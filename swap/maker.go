package swap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/bridge"
	"argonmine/consensus"
	"argonmine/wallet"
)

// Config sets up a maker.
type Config struct {
	Params *consensus.Params
	Node   *wallet.Client
	Wallet *wallet.Wallet // the maker's: its coins, its address, the wARG issuer key
	UARG   consensus.Hash // the bridge's uARG asset id

	Price      *big.Rat // uARG per ARG, before the spread
	SpreadBps  uint32   // taken on each side of ARG and wARG against uARG
	WrapFeeBps uint32   // taken on ARG to wARG and back
	// MaxIn caps one swap, in units of what the wallet pays, per coin (ARG,
	// UARG, WARG); a coin missing from the map is not limited. MinIn is the
	// smallest swap, for every coin.
	MaxIn   map[string]consensus.Amount
	MinIn   consensus.Amount
	FeeRate consensus.Amount // units per byte the maker pays
	// SplitARG keeps the maker's ARG in several coins of about this size,
	// so that swaps in the same block do not wait for each other's change
	// (a coin is spendable again only once its swap confirms); 0: off.
	SplitARG consensus.Amount
	// SplitUARG does the same for the uARG the maker pays out.
	SplitUARG consensus.Amount
	QuoteTTL  time.Duration
	// Closed shuts every swap with this message (shown by /api/info, and
	// every quote is refused with it) while the maker keeps running; empty
	// when it trades.
	Closed string
	// ClosedPairs closes single directions, keyed {from, to} in protocol
	// names (ARG, UARG, WARG): /api/info leaves them out and a quote for one
	// is refused, while the others trade. See ParseClosedPairs.
	ClosedPairs map[[2]string]bool

	LedgerPath string // the wARG ledger (issued and burned per transaction)
	// Treasury receives the swap fees (fees.go); empty: they stay with the
	// maker. FeeSweepMin is the earned amount of a coin that is paid out at
	// once (default 1 coin). FeeBookPath is the fee book (default: next to
	// the ledger).
	Treasury    string
	FeeSweepMin consensus.Amount
	FeeBookPath string
	// ClientIPHeader names the header a proxy in front puts the client's
	// address in (its right-most entry is used); empty: the TCP peer.
	ClientIPHeader string
	// RatePerSec and Burst limit quote and swap requests per client
	// (defaults 2 and 10).
	RatePerSec, Burst float64
	Logf              func(string, ...any)
	Now               func() time.Time
}

// Maker is the swap service.
type Maker struct {
	cfg     Config
	addr    string // the maker's address (receive address 0)
	payload []byte // its key, which also issues wARG
	issuer  [32]byte
	warg    consensus.Hash

	mu     sync.Mutex
	quotes map[string]*pending
	// inflight holds a quote from the moment its swap is accepted until the
	// wallet has recorded the broadcast spend (or the swap failed): its coins
	// stay locked and its ARG reserved, so no other quote counts them free.
	inflight map[string]*pending
	locked   map[consensus.OutPoint]string // the maker's coins in open or in-flight quotes
	ledger   *ledger
	fees     *feeBook
	limiter  *limiter
}

type pending struct {
	q       Quote
	tx      *consensus.Tx
	spent   []consensus.TxOut
	mine    []int // the maker's inputs
	coins   []consensus.OutPoint
	expires time.Time
	ip      string
	need    consensus.Amount // reserved ARG (see argNeed)
	uargOut consensus.Amount // reserved uARG
	issued  consensus.Amount
	burned  consensus.Amount
}

// Errors a wallet can act on.
var (
	errBadRequest = errors.New("swap: bad request")
	errNoQuote    = errors.New("swap: the quote is unknown or was used; ask for a new one")
	errExpired    = errors.New("swap: the quote expired; ask for a new one")
)

type apiErr struct {
	code int
	msg  string
}

func (e *apiErr) Error() string { return e.msg }

func badReq(format string, a ...any) error {
	return &apiErr{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

// New makes a maker from its configuration.
func New(cfg Config) (*Maker, error) {
	if cfg.Params == nil || cfg.Node == nil || cfg.Wallet == nil || cfg.Price == nil || cfg.Price.Sign() <= 0 {
		return nil, errors.New("swap: params, node, wallet and a positive price are required")
	}
	if cfg.UARG == consensus.NativeAsset {
		return nil, errors.New("swap: the uARG asset id is required")
	}
	if cfg.SpreadBps >= 10_000 || cfg.WrapFeeBps >= 10_000 {
		return nil, errors.New("swap: fees must be below 10000 basis points")
	}
	if cfg.FeeRate < 1 {
		cfg.FeeRate = 1
	}
	if cfg.QuoteTTL <= 0 {
		cfg.QuoteTTL = 30 * time.Second
	}
	if cfg.MinIn <= 0 {
		cfg.MinIn = 1_000_000 // 0.01
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RatePerSec <= 0 {
		cfg.RatePerSec = 2
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 10
	}
	addr, err := cfg.Wallet.ReceiveAddress()
	if err != nil {
		return nil, err
	}
	out, err := wallet.DecodeAddress(cfg.Params.HRP, addr)
	if err != nil {
		return nil, err
	}
	m := &Maker{cfg: cfg, addr: addr, payload: out.Payload, quotes: map[string]*pending{}, inflight: map[string]*pending{}, locked: map[consensus.OutPoint]string{},
		limiter: newLimiter(cfg.RatePerSec, cfg.Burst)}
	copy(m.issuer[:], out.Payload)
	m.warg = consensus.AssetID(m.issuer, WARGSymbol)
	if m.ledger, err = openLedger(cfg.LedgerPath); err != nil {
		return nil, err
	}
	if cfg.Treasury != "" {
		if _, err := wallet.DecodeAddress(cfg.Params.HRP, cfg.Treasury); err != nil {
			return nil, fmt.Errorf("swap: treasury address: %w", err)
		}
		if m.cfg.FeeSweepMin <= 0 {
			m.cfg.FeeSweepMin = consensus.UnitsPerCoin
		}
		path := cfg.FeeBookPath
		if path == "" && cfg.LedgerPath != "" {
			path = strings.TrimSuffix(cfg.LedgerPath, ".json") + ".fees.json"
		}
		if m.fees, err = openFeeBook(path); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// ParseClosedPairs reads a list of closed directions, "ARG>UARG,WARG>UARG"
// (coin names in any case), for Config.ClosedPairs. Each side must be one
// of ARG, UARG, WARG and the two must differ; "" gives no closed pair.
func ParseClosedPairs(s string) (map[[2]string]bool, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[[2]string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		from, to, ok := strings.Cut(item, ">")
		if !ok {
			return nil, fmt.Errorf("closed pair %q: write it as FROM>TO, e.g. ARG>UARG", item)
		}
		f, t := Normalize(from), Normalize(to)
		if f == "" || t == "" {
			return nil, fmt.Errorf("closed pair %q: each side must be one of ARG, UARG, WARG", item)
		}
		if f == t {
			return nil, fmt.Errorf("closed pair %q: the two sides must differ", item)
		}
		out[[2]string{f, t}] = true
	}
	return out, nil
}

// Address is the maker's address; WARG its wARG asset id.
func (m *Maker) Address() string      { return m.addr }
func (m *Maker) WARG() consensus.Hash { return m.warg }

func (m *Maker) asset(name string) consensus.Hash {
	switch name {
	case UARG:
		return m.cfg.UARG
	case WARG:
		return m.warg
	}
	return consensus.NativeAsset
}

// Run keeps the maker's wallet in step with the chain and checks the wARG
// ledger, until ctx ends.
func (m *Maker) Run(ctx context.Context) {
	sync := time.NewTicker(10 * time.Second)
	recon := time.NewTicker(time.Minute)
	defer sync.Stop()
	defer recon.Stop()
	m.Sync()
	m.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sync.C:
			m.Sync()
		case <-recon.C:
			m.reconcile()
			m.reconcileFees()
			m.splitARG()
			m.splitUARG()
			m.sweepFees()
		}
	}
}

// splitARG splits the maker's largest ARG coin into coins of SplitARG
// while it has fewer than 8 spendable ARG coins; splitUARG does the same
// for uARG with SplitUARG. They run only when no quote is open, so they
// never spend a coin a quote holds.
func (m *Maker) splitARG() {
	m.split("ARG", "", m.cfg.SplitARG, func(rs []wallet.Recipient) (string, error) { return m.cfg.Wallet.SendMany(rs, m.cfg.FeeRate) })
}

func (m *Maker) splitUARG() {
	id := m.cfg.UARG.String()
	m.split("UARG", id, m.cfg.SplitUARG, func(rs []wallet.Recipient) (string, error) { return m.cfg.Wallet.SendAssetMany(id, rs, m.cfg.FeeRate) })
}

func (m *Maker) split(name, asset string, size consensus.Amount, send func([]wallet.Recipient) (string, error)) {
	if size <= 0 {
		return
	}
	coins, err := m.cfg.Wallet.SpendableCoins(asset)
	if err != nil || len(coins) == 0 || len(coins) >= 8 {
		return
	}
	n := int(coins[0].Amount/size) - 1
	if n > 10 {
		n = 10
	}
	if n < 2 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if len(m.quotes) > 0 || len(m.inflight) > 0 {
		return
	}
	rs := make([]wallet.Recipient, n)
	for i := range rs {
		rs[i] = wallet.Recipient{Address: m.addr, Amount: size}
	}
	txid, err := send(rs)
	if err != nil {
		m.cfg.Logf("swap: splitting %s into %d coins of %s: %v", name, n, wallet.FormatAmount(size), err)
		return
	}
	m.cfg.Logf("swap: split %s into %d coins of %s (%s)", name, n, wallet.FormatAmount(size), txid)
}

// Sync updates the maker's wallet and drops expired quotes.
func (m *Maker) Sync() {
	if _, err := m.cfg.Wallet.Sync(); err != nil {
		m.cfg.Logf("swap: wallet sync: %v", err)
	}
	m.mu.Lock()
	m.expireLocked()
	m.mu.Unlock()
}

func (m *Maker) expireLocked() {
	now := m.cfg.Now()
	for id, p := range m.quotes {
		if now.After(p.expires) {
			m.dropLocked(id, p)
		}
	}
}

func (m *Maker) dropLocked(id string, p *pending) {
	delete(m.quotes, id)
	for _, op := range p.coins {
		if m.locked[op] == id {
			delete(m.locked, op)
		}
	}
}

// ---- prices ----

const bps = 10_000

func (m *Maker) price(from, to string) (*big.Rat, error) {
	keep := func(b uint32) *big.Rat { return big.NewRat(int64(bps-b), bps) }
	switch {
	case (from == ARG && to == WARG) || (from == WARG && to == ARG):
		return keep(m.cfg.WrapFeeBps), nil
	case (from == ARG || from == WARG) && to == UARG:
		return new(big.Rat).Mul(m.cfg.Price, keep(m.cfg.SpreadBps)), nil
	case from == UARG && (to == ARG || to == WARG):
		return new(big.Rat).Mul(new(big.Rat).Inv(m.cfg.Price), keep(m.cfg.SpreadBps)), nil
	}
	return nil, badReq("the maker does not trade %s for %s", from, to)
}

func applyRate(in consensus.Amount, r *big.Rat) consensus.Amount {
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(in)), r)
	q := new(big.Int).Quo(v.Num(), v.Denom())
	if !q.IsInt64() {
		return 0
	}
	return consensus.Amount(q.Int64())
}

func rateString(r *big.Rat) string { return r.FloatString(8) }

// ---- reserve and inventory ----

// wargSupply is the wARG in circulation: issued minus burned, in every
// swap the maker broadcast that has not been found to have failed.
func (m *Maker) wargSupply() consensus.Amount { return m.ledger.supply() }

// balances of the maker's spendable coins, less what open quotes reserve.
func (m *Maker) freeLocked() (argFree, uargFree consensus.Amount, err error) {
	args, err := m.cfg.Wallet.SpendableCoins("")
	if err != nil {
		return 0, 0, err
	}
	uargs, err := m.cfg.Wallet.SpendableCoins(m.cfg.UARG.String())
	if err != nil {
		return 0, 0, err
	}
	for _, c := range args {
		argFree += c.Amount
	}
	for _, c := range uargs {
		uargFree += c.Amount
	}
	argFree -= m.wargSupply()
	if m.fees != nil { // the treasury's share is not the maker's to trade
		argFree -= m.fees.owed(ARG)
		uargFree -= m.fees.owed(UARG)
	}
	for _, set := range []map[string]*pending{m.quotes, m.inflight} {
		for _, p := range set {
			if p.need > 0 {
				argFree -= p.need
			}
			uargFree -= p.uargOut
		}
	}
	return argFree, uargFree, nil
}

// argNeed is the ARG a swap takes from the reserve's free part: what the
// maker pays in ARG (the payout and the fee), plus wARG it issues, less
// wARG it burns and less ARG it receives that backs the wARG it issues in
// the same transaction.
func argNeed(argOut, argIn, issued, burned consensus.Amount) consensus.Amount {
	backs := argIn
	if backs > issued {
		backs = issued
	}
	return argOut + issued - burned - backs
}

// Info answers GET /api/info.
func (m *Maker) Info() (*Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	argFree, uargFree, err := m.freeLocked()
	if err != nil {
		return nil, err
	}
	info := &Info{Network: m.cfg.Params.Name, Maker: m.addr, Issuer: hex.EncodeToString(m.issuer[:]), UARG: m.cfg.UARG.String(), WARG: m.warg.String(),
		Price: m.cfg.Price.FloatString(8), SpreadBps: m.cfg.SpreadBps, WrapFeeBps: m.cfg.WrapFeeBps, QuoteTTL: int(m.cfg.QuoteTTL / time.Second),
		WARGSupply: m.wargSupply()}
	if bal, err := m.cfg.Wallet.SpendableCoins(""); err == nil {
		for _, c := range bal {
			info.Reserve += c.Amount
		}
	}
	if h, err := m.cfg.Node.ChainInfo(); err == nil {
		info.Height = h.Blocks
	}
	if m.cfg.Closed != "" {
		info.Closed = m.cfg.Closed
		return info, nil // no pairs: nothing is traded
	}
	const feeMargin = 100_000
	for _, from := range []string{ARG, WARG, UARG} {
		for _, to := range []string{ARG, WARG, UARG} {
			r, err := m.price(from, to)
			if err != nil || m.cfg.ClosedPairs[[2]string{from, to}] {
				continue
			}
			p := Pair{From: from, To: to, Rate: rateString(r), Min: m.cfg.MinIn, Max: m.cfg.MaxIn[from]}
			switch {
			case to == UARG:
				p.Available = uargFree
			case from == ARG && to == WARG:
				p.Available = p.Max // backed by the ARG paid in
				if p.Available == 0 {
					p.Available = consensus.MaxAssetAmount
				}
			case to == ARG && from == WARG:
				p.Available = m.wargSupply() // every wARG out there can come back
				if argFree > feeMargin {
					p.Available += argFree - feeMargin
				}
			default:
				if argFree > feeMargin {
					p.Available = argFree - feeMargin
				}
			}
			if p.Available < 0 {
				p.Available = 0
			}
			info.Pairs = append(info.Pairs, p)
		}
	}
	return info, nil
}

// ---- quotes ----

type userCoin struct {
	op  consensus.OutPoint
	out consensus.TxOut
}

// Quote builds the transaction for a request and holds the maker's coins
// for it until it expires or is used.
func (m *Maker) Quote(req *QuoteRequest, ip string) (*Quote, error) {
	from, to := Normalize(req.From), Normalize(req.To)
	if from == "" || to == "" || from == to {
		return nil, badReq("from and to must be two of ARG, UARG, WARG")
	}
	if m.cfg.Closed != "" {
		return nil, &apiErr{http.StatusServiceUnavailable, m.cfg.Closed}
	}
	if m.cfg.ClosedPairs[[2]string{from, to}] {
		return nil, &apiErr{http.StatusServiceUnavailable, "This direction is closed for now"}
	}
	rate, err := m.price(from, to)
	if err != nil {
		return nil, err
	}
	if req.Amount < m.cfg.MinIn {
		return nil, badReq("the smallest swap is %s %s", wallet.FormatAmount(m.cfg.MinIn), from)
	}
	if max, ok := m.cfg.MaxIn[from]; ok && max > 0 && req.Amount > max {
		return nil, badReq("the largest swap is %s %s", wallet.FormatAmount(max), from)
	}
	if len(req.Inputs) == 0 || len(req.Inputs) > 50 {
		return nil, badReq("name 1 to 50 coins to pay with")
	}
	recv, err := m.walletAddress(req.Receive, "receive")
	if err != nil {
		return nil, err
	}
	change, err := m.walletAddress(req.Change, "change")
	if err != nil {
		return nil, err
	}
	out := applyRate(req.Amount, rate)
	if out <= 0 {
		return nil, badReq("the swap is too small to pay anything")
	}
	if to == ARG && out < argDust {
		return nil, badReq("the swap is too small: less than %d units of ARG would arrive", argDust)
	}

	// The maker's own coins as of now, and the wallet's as the chain has them.
	if _, err := m.cfg.Wallet.Sync(); err != nil {
		m.cfg.Logf("swap: wallet sync before a quote: %v", err)
	}
	fromAsset := m.asset(from)
	info, err := m.cfg.Node.ChainInfo()
	if err != nil {
		return nil, err
	}
	var coins []userCoin
	var userIn consensus.Amount
	seen := map[consensus.OutPoint]bool{}
	for _, in := range req.Inputs {
		h, err := consensus.HashFromHex(in.TxID)
		if err != nil {
			return nil, badReq("bad input txid")
		}
		op := consensus.OutPoint{TxID: h, Index: in.Index}
		if seen[op] {
			return nil, badReq("a coin is named twice")
		}
		seen[op] = true
		c, err := m.coin(op, info.Blocks)
		if err != nil {
			return nil, err
		}
		if c.Asset != fromAsset {
			return nil, badReq("coin %s:%d is not %s", in.TxID, in.Index, from)
		}
		if string(c.Payload) == string(m.payload) {
			return nil, badReq("coin %s:%d is the maker's", in.TxID, in.Index)
		}
		coins = append(coins, userCoin{op, c})
		userIn += c.Amount
	}
	if userIn < req.Amount {
		return nil, badReq("the coins named hold %s %s, less than the %s asked", wallet.FormatAmount(userIn), from, wallet.FormatAmount(req.Amount))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if len(m.quotes) >= 200 {
		return nil, &apiErr{http.StatusServiceUnavailable, "the maker is busy; try again in a few seconds"}
	}
	n := 0
	for _, p := range m.quotes {
		if p.ip == ip {
			n++
		}
	}
	if n >= 3 {
		return nil, &apiErr{http.StatusTooManyRequests, "too many open quotes; use or let one expire first"}
	}
	for _, c := range coins {
		for _, set := range []map[string]*pending{m.quotes, m.inflight} {
			for _, p := range set {
				for _, i := range p.q.UserInputs {
					if p.tx.Inputs[i].PrevOut == c.op {
						return nil, badReq("a coin named is already in an open quote")
					}
				}
			}
		}
	}
	p, err := m.buildLocked(from, to, req.Amount, out, coins, recv, change)
	if err != nil {
		return nil, err
	}
	argFree, uargFree, err := m.freeLocked()
	if err != nil {
		return nil, err
	}
	if p.uargOut > uargFree {
		return nil, &apiErr{http.StatusConflict, fmt.Sprintf("the maker can pay at most %s UARG right now", wallet.FormatAmount(max0(uargFree)))}
	}
	if p.need > argFree {
		return nil, &apiErr{http.StatusConflict, "the maker cannot pay that much ARG right now"}
	}
	id := randomID()
	p.q.ID, p.q.Rate, p.ip = id, rateString(rate), ip
	p.expires = m.cfg.Now().Add(m.cfg.QuoteTTL)
	p.q.Expires = p.expires.Unix()
	m.quotes[id] = p
	for _, op := range p.coins {
		m.locked[op] = id
	}
	q := p.q
	return &q, nil
}

func max0(a consensus.Amount) consensus.Amount {
	if a < 0 {
		return 0
	}
	return a
}

func (m *Maker) walletAddress(addr, what string) ([]byte, error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	out, err := wallet.DecodeAddress(m.cfg.Params.HRP, addr)
	if err != nil {
		return nil, badReq("the %s address is not an address of this network", what)
	}
	if bridge.IsSwapAddress(m.cfg.Params.HRP, addr) {
		return nil, badReq("the %s address is a swap address", what)
	}
	if string(out.Payload) == string(m.payload) {
		return nil, badReq("the %s address is the maker's", what)
	}
	return out.Payload, nil
}

// coin looks up an unspent, spendable coin on the chain.
func (m *Maker) coin(op consensus.OutPoint, tip uint32) (consensus.TxOut, error) {
	raw, err := m.cfg.Node.Call("gettxout", op.TxID.String(), op.Index)
	if err != nil {
		return consensus.TxOut{}, err
	}
	var c struct {
		Amount        consensus.Amount `json:"amount"`
		Type          uint8            `json:"type"`
		Payload       string           `json:"payload"`
		Asset         string           `json:"asset"`
		Coinbase      bool             `json:"coinbase"`
		Confirmations uint32           `json:"confirmations"`
	}
	if string(raw) == "null" || json.Unmarshal(raw, &c) != nil || c.Payload == "" {
		return consensus.TxOut{}, badReq("coin %s:%d is not an unspent confirmed coin", op.TxID, op.Index)
	}
	if c.Type != consensus.OutputTypeP2PK {
		return consensus.TxOut{}, badReq("coin %s:%d cannot be spent", op.TxID, op.Index)
	}
	if c.Coinbase && c.Confirmations < consensus.CoinbaseMaturity {
		return consensus.TxOut{}, badReq("coin %s:%d is a mining reward that is not mature yet", op.TxID, op.Index)
	}
	p, err := hex.DecodeString(c.Payload)
	if err != nil || len(p) != consensus.P2PKPayloadLen {
		return consensus.TxOut{}, badReq("coin %s:%d has a bad key", op.TxID, op.Index)
	}
	o := consensus.TxOut{Amount: c.Amount, Type: c.Type, Payload: p}
	if c.Asset != "" {
		if o.Asset, err = consensus.HashFromHex(c.Asset); err != nil {
			return consensus.TxOut{}, err
		}
	}
	return o, nil
}

// relayFee is mempool.MinRelayFee at the maker's fee rate: bytes times the
// rate, plus 546 units for every asset output beyond the first two.
func (m *Maker) relayFee(tx *consensus.Tx) consensus.Amount {
	n := 0
	for i := range tx.Outputs {
		if tx.Outputs[i].Asset != consensus.NativeAsset {
			n++
		}
	}
	fee := m.cfg.FeeRate * consensus.Amount(tx.SerializedSize())
	if n > 2 {
		fee += consensus.Amount(n-2) * 546
	}
	return fee
}

// buildLocked assembles the swap transaction: the wallet's coins first,
// then the maker's coins of what it pays, then the maker's ARG for the fee.
func (m *Maker) buildLocked(from, to string, amountIn, amountOut consensus.Amount, coins []userCoin, recv, change []byte) (*pending, error) {
	fromAsset, toAsset := m.asset(from), m.asset(to)
	var userIn consensus.Amount
	for _, c := range coins {
		userIn += c.out.Amount
	}
	makerGets := amountIn
	userChange := userIn - amountIn
	if from == ARG && userChange > 0 && userChange < argDust {
		makerGets += userChange // a change below the dust limit cannot be paid back
		userChange = 0
	}

	p := &pending{q: Quote{From: from, To: to, AmountIn: amountIn, AmountOut: amountOut}}
	tx := &consensus.Tx{Version: consensus.TxVersionAssets}
	var spent []consensus.TxOut
	addIn := func(op consensus.OutPoint, o consensus.TxOut) {
		tx.Inputs = append(tx.Inputs, consensus.TxIn{PrevOut: op})
		spent = append(spent, o)
	}
	for _, c := range coins {
		p.q.UserInputs = append(p.q.UserInputs, len(tx.Inputs))
		addIn(c.op, c.out)
	}

	// Fixed outputs: what the wallet gets, its change, what the maker gets.
	fixed := []consensus.TxOut{{Amount: amountOut, Type: consensus.OutputTypeP2PK, Payload: recv, Asset: toAsset}}
	if userChange > 0 {
		fixed = append(fixed, consensus.TxOut{Amount: userChange, Type: consensus.OutputTypeP2PK, Payload: change, Asset: fromAsset})
	}
	switch from {
	case WARG:
		p.burned = makerGets // no output: the wARG is burned
	default:
		fixed = append(fixed, consensus.TxOut{Amount: makerGets, Type: consensus.OutputTypeP2PK, Payload: m.payload, Asset: fromAsset})
	}

	// The maker's side of what it pays.
	free := func(asset string) ([]wallet.Coin, error) {
		all, err := m.cfg.Wallet.SpendableCoins(asset)
		if err != nil {
			return nil, err
		}
		out := all[:0]
		for _, c := range all {
			h, _ := consensus.HashFromHex(c.TxID)
			if _, held := m.locked[consensus.OutPoint{TxID: h, Index: c.Index}]; !held {
				out = append(out, c)
			}
		}
		return out, nil
	}
	addMaker := func(c wallet.Coin) {
		h, _ := consensus.HashFromHex(c.TxID)
		op := consensus.OutPoint{TxID: h, Index: c.Index}
		p.mine = append(p.mine, len(tx.Inputs))
		p.coins = append(p.coins, op)
		addIn(op, c.Out)
	}
	switch to {
	case WARG:
		tx.Issuances = []consensus.Issuance{{Asset: m.warg, Issuer: m.issuer, Symbol: WARGSymbol, Amount: amountOut}}
		p.issued = amountOut
	case UARG:
		cs, err := free(m.cfg.UARG.String())
		if err != nil {
			return nil, err
		}
		var have consensus.Amount
		for _, c := range cs {
			if have >= amountOut {
				break
			}
			addMaker(c)
			have += c.Amount
		}
		if have < amountOut {
			return nil, &apiErr{http.StatusConflict, fmt.Sprintf("the maker can pay at most %s UARG right now", wallet.FormatAmount(have))}
		}
		if have > amountOut {
			fixed = append(fixed, consensus.TxOut{Amount: have - amountOut, Type: consensus.OutputTypeP2PK, Payload: m.payload, Asset: m.cfg.UARG})
		}
		p.uargOut = amountOut
	}

	// ARG: the payout when the wallet buys ARG, and the fee, with change.
	argPay := consensus.Amount(0)
	if to == ARG {
		argPay = amountOut
	}
	cs, err := free("")
	if err != nil {
		return nil, err
	}
	nIss := len(tx.Issuances)
	var argIn consensus.Amount
	for k, c := range cs {
		if k >= 50 {
			break
		}
		addMaker(c)
		argIn += c.Amount
		tx.Outputs = append(append([]consensus.TxOut{}, fixed...), consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: m.payload})
		tx.Witnesses = make([][]byte, len(tx.Inputs)+nIss)
		for i := range tx.Witnesses {
			tx.Witnesses[i] = make([]byte, consensus.SchnorrSigLen)
		}
		fee := m.relayFee(tx)
		if argIn >= argPay+fee+argDust {
			tx.Outputs[len(tx.Outputs)-1].Amount = argIn - argPay - fee
			p.need = argNeed(argPay+fee, argInOf(from, makerGets), p.issued, p.burned)
			return m.finish(p, tx, spent)
		}
		tx.Outputs = append([]consensus.TxOut{}, fixed...)
		fee = m.relayFee(tx)
		if argIn >= argPay+fee {
			p.need = argNeed(argIn, argInOf(from, makerGets), p.issued, p.burned) // the rest goes to the fee
			return m.finish(p, tx, spent)
		}
	}
	return nil, &apiErr{http.StatusConflict, "the maker cannot pay the network fee right now"}
}

func argInOf(from string, makerGets consensus.Amount) consensus.Amount {
	if from == ARG {
		return makerGets
	}
	return 0
}

func (m *Maker) finish(p *pending, tx *consensus.Tx, spent []consensus.TxOut) (*pending, error) {
	for i := range tx.Witnesses {
		tx.Witnesses[i] = nil // unsigned
	}
	p.tx, p.spent = tx, spent
	p.q.Tx = hex.EncodeToString(tx.Serialize())
	for _, s := range spent {
		p.q.Spent = append(p.q.Spent, spentJSON(s))
	}
	return p, nil
}

// ---- swaps ----

// Swap completes a quote with the wallet's signatures: it checks them,
// signs the maker's inputs (and the wARG issuance), broadcasts the
// transaction and returns its id.
func (m *Maker) Swap(req *SwapRequest) (string, error) {
	m.mu.Lock()
	p, ok := m.quotes[req.QuoteID]
	if !ok {
		m.mu.Unlock()
		return "", &apiErr{http.StatusNotFound, errNoQuote.Error()}
	}
	if m.cfg.Now().After(p.expires) {
		m.dropLocked(req.QuoteID, p)
		m.mu.Unlock()
		return "", &apiErr{http.StatusGone, errExpired.Error()}
	}
	// One attempt per quote. Until the spend is recorded (or the attempt
	// fails) the quote's coins stay locked and its ARG reserved.
	delete(m.quotes, req.QuoteID)
	m.inflight[req.QuoteID] = p
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.inflight, req.QuoteID)
		for _, op := range p.coins {
			if m.locked[op] == req.QuoteID {
				delete(m.locked, op)
			}
		}
		m.mu.Unlock()
	}()

	tx := p.tx
	tx.Witnesses = make([][]byte, len(tx.Inputs)+len(tx.Issuances))
	want := map[int]bool{}
	for _, i := range p.q.UserInputs {
		want[i] = true
	}
	if len(req.Sigs) != len(want) {
		return "", badReq("sign every one of your inputs, and nothing else")
	}
	cache := consensus.NewSigHashCache(m.cfg.Params.ChainTag, tx, p.spent)
	for _, s := range req.Sigs {
		if !want[s.Index] || tx.Witnesses[s.Index] != nil {
			return "", badReq("signature for input %d is not expected", s.Index)
		}
		sig, err := hex.DecodeString(s.Sig)
		if err != nil || len(sig) != consensus.SchnorrSigLen {
			return "", badReq("signature for input %d is malformed", s.Index)
		}
		pub, err := schnorr.ParsePubKey(p.spent[s.Index].Payload)
		if err != nil {
			return "", badReq("input %d has a bad key", s.Index)
		}
		ps, err := schnorr.ParseSignature(sig)
		h := cache.Hash(uint32(s.Index))
		if err != nil || !ps.Verify(h[:], pub) {
			return "", badReq("signature for input %d does not verify", s.Index)
		}
		tx.Witnesses[s.Index] = sig
	}
	if err := m.cfg.Wallet.SignInputs(tx, p.spent, p.mine); err != nil {
		return "", fmt.Errorf("swap: maker signing: %w", err)
	}
	if len(tx.Issuances) > 0 {
		if err := m.cfg.Wallet.SignIssuance(tx, p.spent); err != nil {
			return "", fmt.Errorf("swap: maker issuance signing: %w", err)
		}
	}
	if err := consensus.CheckTxStateless(tx); err != nil {
		return "", fmt.Errorf("swap: %w", err)
	}
	if err := consensus.VerifyTxSignatures(m.cfg.Params.ChainTag, tx, p.spent); err != nil {
		return "", badReq("signatures do not verify: %v", err)
	}
	// Recorded before broadcast, so a crash never leaves issued wARG out of
	// the ledger; reconcile drops it if the transaction never made it.
	txid := tx.TxID().String()
	if p.issued > 0 || p.burned > 0 {
		if err := m.ledger.add(ledgerEntry{TxID: txid, Issued: p.issued, Burned: p.burned, Time: m.cfg.Now().Unix()}); err != nil {
			return "", err
		}
	}
	if _, err := m.cfg.Node.SendRawTransaction(hex.EncodeToString(tx.Serialize())); err != nil {
		// Only a refusal by the node means the swap is not out there: after
		// a transport failure it may have been accepted, so its wARG stays in
		// the ledger and reconcile drops it if it never confirms.
		var re *wallet.RPCError
		if (p.issued > 0 || p.burned > 0) && errors.As(err, &re) {
			m.ledger.remove(txid)
		}
		return "", badReq("the network refused the swap: %v", err)
	}
	if err := m.cfg.Wallet.RecordSpend(txid, tx, p.spent); err != nil {
		m.cfg.Logf("swap: recording %s: %v", txid, err)
	}
	m.bookFee(txid, p.q)
	m.cfg.Logf("swap: %s %s %s for %s %s (%s)", txid, wallet.FormatAmount(p.q.AmountIn), p.q.From, wallet.FormatAmount(p.q.AmountOut), p.q.To, p.ip)
	return txid, nil
}

// reconcile confirms ledger entries that made it into a block and drops
// those that did not after half an hour (refused, replaced or forgotten).
func (m *Maker) reconcile() {
	for _, e := range m.ledger.pending() {
		raw, err := m.cfg.Node.Call("getrawtransaction", e.TxID)
		if err == nil {
			var t struct {
				Confirmations uint32 `json:"confirmations"`
			}
			if json.Unmarshal(raw, &t) == nil && t.Confirmations >= 6 {
				m.ledger.confirm(e.TxID)
			}
			continue
		}
		if m.cfg.Now().Unix()-e.Time > 1800 {
			m.cfg.Logf("swap: %s never confirmed; dropped from the wARG ledger (issued %d, burned %d)", e.TxID, e.Issued, e.Burned)
			m.ledger.remove(e.TxID)
		}
	}
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- the wARG ledger ----

type ledgerEntry struct {
	TxID      string           `json:"txid"`
	Issued    consensus.Amount `json:"issued"`
	Burned    consensus.Amount `json:"burned"`
	Time      int64            `json:"time"`
	Confirmed bool             `json:"confirmed"`
}

type ledger struct {
	mu      sync.Mutex
	path    string
	Entries []ledgerEntry `json:"entries"`
}

func openLedger(path string) (*ledger, error) {
	l := &ledger{path: path}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("swap: ledger %s: %w", path, err)
	}
	return l, nil
}

func (l *ledger) saveLocked() error {
	if l.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, l.path)
}

func (l *ledger) supply() consensus.Amount {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s consensus.Amount
	for _, e := range l.Entries {
		s += e.Issued - e.Burned
	}
	return s
}

func (l *ledger) add(e ledgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Entries = append(l.Entries, e)
	return l.saveLocked()
}

func (l *ledger) remove(txid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, e := range l.Entries {
		if e.TxID == txid {
			l.Entries = append(l.Entries[:i], l.Entries[i+1:]...)
			break
		}
	}
	l.saveLocked()
}

func (l *ledger) confirm(txid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.Entries {
		if l.Entries[i].TxID == txid {
			l.Entries[i].Confirmed = true
		}
	}
	l.saveLocked()
}

func (l *ledger) pending() []ledgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ledgerEntry
	for _, e := range l.Entries {
		if !e.Confirmed {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// ---- HTTP ----

// Handler serves the maker's API.
func (m *Maker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, &apiErr{http.StatusMethodNotAllowed, "GET only"})
			return
		}
		info, err := m.Info()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, info)
	})
	mux.HandleFunc("/api/quote", func(w http.ResponseWriter, r *http.Request) {
		ip := m.clientIP(r)
		if !m.limiter.allow(ip) {
			writeErr(w, &apiErr{http.StatusTooManyRequests, "too many requests; wait a few seconds"})
			return
		}
		var req QuoteRequest
		if err := readJSON(w, r, &req); err != nil {
			writeErr(w, err)
			return
		}
		q, err := m.Quote(&req, ip)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, q)
	})
	mux.HandleFunc("/api/swap", func(w http.ResponseWriter, r *http.Request) {
		if !m.limiter.allow(m.clientIP(r)) {
			writeErr(w, &apiErr{http.StatusTooManyRequests, "too many requests; wait a few seconds"})
			return
		}
		var req SwapRequest
		if err := readJSON(w, r, &req); err != nil {
			writeErr(w, err)
			return
		}
		txid, err := m.Swap(&req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, SwapResult{TxID: txid})
	})
	return mux
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Method != http.MethodPost {
		return &apiErr{http.StatusMethodNotAllowed, "POST only"}
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return badReq("the request is too large")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return badReq("the request is not valid JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	msg := "the maker could not do that right now"
	var ae *apiErr
	if errors.As(err, &ae) {
		code, msg = ae.code, ae.msg
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (m *Maker) clientIP(r *http.Request) string {
	if h := m.cfg.ClientIPHeader; h != "" {
		if v := r.Header.Get(h); v != "" {
			parts := strings.Split(v, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter is a token bucket per client.
type limiter struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	b     map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, b: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.b) > 10_000 {
		l.b = map[string]*bucket{}
	}
	bk, ok := l.b[key]
	if !ok {
		bk = &bucket{tokens: l.burst, last: now}
		l.b[key] = bk
	}
	bk.tokens += now.Sub(bk.last).Seconds() * l.rate
	if bk.tokens > l.burst {
		bk.tokens = l.burst
	}
	bk.last = now
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}
