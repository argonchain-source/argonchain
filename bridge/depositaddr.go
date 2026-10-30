package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/ui"
	"argonmine/wallet"
)

// Deposit addresses, the daemon side (docs/BRIDGE.md, "Deposit addresses").
// The page asks the leader for the deposit address of an Argonmine address
// on a chain; the leader computes it from the router's forwarder code hash,
// remembers it as a watch, and from then on looks for transfers of the
// chain's tokens into it. Once a transfer has the chain's confirmations it
// reads the balance and, at or above the token's minimum, calls
// router.sweep(token, to). The vault's Deposited event that follows is
// scanned and issued like any other deposit. Followers do none of this.

// Sweep statuses.
const (
	SweepPending   = "pending"   // decided but not on chain: the last attempt failed and is retried (see error)
	SweepSent      = "sent"      // transaction broadcast, waiting for its receipt
	SweepConfirmed = "confirmed" // mined: the funds are in the vault
	SweepFailed    = "failed"    // reverted, not mined in time, or nothing left to sweep; a later sweep takes over
)

const (
	watchCapDefault  = 100_000             // watches across all chains
	watchChunk       = 100                 // recipients per eth_getLogs
	watchExpiry      = 30 * 24 * time.Hour // a watch that never received anything is dropped after this
	sweepStale       = time.Hour           // a sent sweep without a receipt after this is given up
	sweepHistory     = 20                  // sweeps kept per watch
	slotTransfersCap = 32                  // unswept transfers kept per watch and token
	sweepBackoffMax  = 10 * time.Minute    // longest wait between failed attempts
	routerRetryAfter = 30 * time.Second    // after a router check could not reach the chain
	pruneEvery       = time.Hour           // how often expired watches are dropped
	depositAddrNote  = "Send only %s on %s to this address, from any wallet or exchange. Any other token, or the same token on another chain, is lost. Less than the minimum stays at the address until more arrives."
)

// DepositWatch is one deposit address the leader watches: an Argonmine
// address on one chain. It carries what has arrived per token and the
// sweeps made for it, so a restart loses nothing.
type DepositWatch struct {
	Chain   uint64                `json:"chain_id"`
	To      string                `json:"to"`      // Argonmine address, lowercase
	Address string                `json:"address"` // deposit address, 0x lowercase
	Router  string                `json:"router"`  // the router it was derived under, 0x lowercase
	Created int64                 `json:"created"`
	Touched int64                 `json:"touched"`          // last time the page asked for it
	Check   bool                  `json:"check,omitempty"`  // read every token's balance next round (new watch: transfers may predate it)
	Tokens  map[string]*WatchSlot `json:"tokens,omitempty"` // by 0x token address
	Sweeps  []Sweep               `json:"sweeps,omitempty"` // oldest first, at most sweepHistory
}

// WatchSlot is one token at one deposit address.
type WatchSlot struct {
	Balance   string          `json:"balance,omitempty"`    // token units at ReadBlock
	ReadBlock uint64          `json:"read_block,omitempty"` // block of the last balance read (or of the last sweep)
	Transfers []WatchTransfer `json:"transfers,omitempty"`  // transfers in after ReadBlock, oldest first
	LastTx    string          `json:"last_tx,omitempty"`    // newest transfer in
	Seen      int64           `json:"seen,omitempty"`       // unix time something was last seen arriving
	Check     bool            `json:"check,omitempty"`      // read the balance next round whatever the transfers say
	Failures  int             `json:"failures,omitempty"`   // failed sweep attempts in a row
	RetryAt   int64           `json:"retry_at,omitempty"`   // unix time before which no attempt is made
}

// WatchTransfer is one transfer into a deposit address not yet covered by a
// balance read.
type WatchTransfer struct {
	Block  uint64 `json:"block"`
	Amount string `json:"amount"`
	Tx     string `json:"tx"`
	Log    uint64 `json:"log"`
}

// Sweep is one router.sweep for a deposit address and token.
type Sweep struct {
	Token       string `json:"token"`
	TokenSymbol string `json:"token_symbol"`
	Decimals    int    `json:"decimals"`
	Amount      string `json:"amount"` // token units swept (gross; the vault keeps its fee)
	Tx          string `json:"tx"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	Attempts    int    `json:"attempts"`
	DepositID   uint64 `json:"deposit_id,omitempty"` // the vault's deposit id, from the Swept event
	Created     int64  `json:"created"`
	Updated     int64  `json:"updated"`
}

func (w *DepositWatch) slot(token string) *WatchSlot {
	if w.Tokens == nil {
		w.Tokens = map[string]*WatchSlot{}
	}
	sl := w.Tokens[token]
	if sl == nil {
		sl = &WatchSlot{}
		w.Tokens[token] = sl
	}
	return sl
}

// openSweep is the sweep of token not yet settled (pending or sent), or nil.
func (w *DepositWatch) openSweep(token string) *Sweep {
	for i := len(w.Sweeps) - 1; i >= 0; i-- {
		if w.Sweeps[i].Token == token && (w.Sweeps[i].Status == SweepPending || w.Sweeps[i].Status == SweepSent) {
			return &w.Sweeps[i]
		}
	}
	return nil
}

// received reports whether anything ever arrived at the address.
func (w *DepositWatch) received() bool {
	if len(w.Sweeps) > 0 {
		return true
	}
	for _, sl := range w.Tokens {
		if sl.Seen > 0 || len(sl.Transfers) > 0 || (sl.Balance != "" && sl.Balance != "0") {
			return true
		}
	}
	return false
}

func (w *DepositWatch) trimSweeps() {
	for len(w.Sweeps) > sweepHistory {
		drop := -1
		for i := range w.Sweeps {
			if s := w.Sweeps[i].Status; s == SweepConfirmed || s == SweepFailed {
				drop = i
				break
			}
		}
		if drop < 0 {
			return
		}
		w.Sweeps = append(w.Sweeps[:drop], w.Sweeps[drop+1:]...)
	}
}

// amount is what sits at the address as far as the leader knows: the last
// balance read plus the transfers seen since.
func (sl *WatchSlot) amount() *big.Int {
	v := mustBig(sl.Balance)
	for _, t := range sl.Transfers {
		v.Add(v, mustBig(t.Amount))
	}
	return v
}

// coveredUpTo drops the transfers a balance read (or a sweep) at block
// already accounts for.
func (sl *WatchSlot) coveredUpTo(block uint64) {
	if block > sl.ReadBlock {
		sl.ReadBlock = block
	}
	kept := sl.Transfers[:0]
	for _, t := range sl.Transfers {
		if t.Block > sl.ReadBlock {
			kept = append(kept, t)
		}
	}
	sl.Transfers = kept
	if len(sl.Transfers) == 0 {
		sl.Transfers = nil
	}
}

func (sl *WatchSlot) fail(now time.Time) {
	sl.Failures++
	d := 30 * time.Second
	for i := 1; i < sl.Failures && d < sweepBackoffMax; i++ {
		d *= 2
	}
	if d > sweepBackoffMax {
		d = sweepBackoffMax
	}
	sl.RetryAt = now.Add(d).Unix()
}

// ---- state: "w/" ‖ chain (8) ‖ to → DepositWatch; cursor "watch/<chain>" ----
//
// These keys are additive: a daemon of the same state format that does not
// know them ignores them, so the format is unchanged.

func watchPrefix(chain uint64) []byte { return append([]byte("w/"), be64(chain)...) }
func watchKey(chain uint64, to string) []byte {
	return append(watchPrefix(chain), to...)
}

// WatchCursor is the cursor name of the transfer scan of a chain.
func WatchCursor(chainID uint64) string { return "watch/" + strconv.FormatUint(chainID, 10) }

func (s *State) PutWatch(w *DepositWatch) error { return s.put(watchKey(w.Chain, w.To), w) }
func (s *State) DelWatch(chain uint64, to string) error {
	return s.db.Delete(watchKey(chain, to), nil)
}
func (s *State) GetWatch(chain uint64, to string) (*DepositWatch, bool) {
	var w DepositWatch
	if !s.get(watchKey(chain, to), &w) {
		return nil, false
	}
	return &w, true
}

// Watches lists the watches of one chain, or of every chain with all.
func (s *State) Watches(chain uint64, all bool) []DepositWatch {
	prefix := watchPrefix(chain)
	if all {
		prefix = []byte("w/")
	}
	iter := s.db.NewIterator(util.BytesPrefix(prefix), nil)
	defer iter.Release()
	var out []DepositWatch
	for iter.Next() {
		var w DepositWatch
		if json.Unmarshal(iter.Value(), &w) == nil {
			out = append(out, w)
		}
	}
	return out
}

// ---- the router check ----

// errRouterOff is a chain whose router failed the startup checks: deposit
// addresses stay off there until the configuration is fixed.
type errRouterOff struct{ msg string }

func (e errRouterOff) Error() string { return e.msg }

// routerReady returns the forwarder code hash of the chain's router, after
// checking once that the router deposits into this chain's vault and that
// its own depositAddress view agrees with DepositAddress. A disagreement
// turns the feature off for the chain; a chain that cannot be reached is
// tried again after a short pause.
func (s *Service) routerReady(ctx context.Context, c *chainState) ([32]byte, error) {
	c.mu.Lock()
	if c.routerOK {
		h := c.routerHash
		c.mu.Unlock()
		return h, nil
	}
	if c.routerErr != "" {
		e := c.routerErr
		c.mu.Unlock()
		return [32]byte{}, errRouterOff{e}
	}
	if time.Now().Before(c.routerRetry) {
		e := c.routerTransient
		c.mu.Unlock()
		return [32]byte{}, errors.New(e)
	}
	c.mu.Unlock()
	if c.router == nil {
		return [32]byte{}, errRouterOff{"deposit addresses are not available on " + c.Name}
	}
	transient := func(err error) ([32]byte, error) {
		msg := fmt.Sprintf("%s cannot be reached to derive deposit addresses (%v)", c.Name, err)
		c.mu.Lock()
		c.routerTransient, c.routerRetry = msg, time.Now().Add(routerRetryAfter)
		c.mu.Unlock()
		return [32]byte{}, errors.New(msg)
	}
	permanent := func(msg string) ([32]byte, error) {
		msg = "deposit addresses are off on " + c.Name + ": " + msg
		c.mu.Lock()
		c.routerErr = msg
		c.mu.Unlock()
		s.cfg.Logf("bridge: %s", msg)
		return [32]byte{}, errRouterOff{msg}
	}
	vault, err := c.router.RouterVault(ctx, c.Router)
	if err != nil {
		if isRPCAnswer(err) {
			return permanent(err.Error())
		}
		return transient(err)
	}
	if vault != c.Vault {
		return permanent(fmt.Sprintf("the router %s deposits into %s, not into this chain's vault %s", HexAddress(c.Router), HexAddress(vault), HexAddress(c.Vault)))
	}
	hash, err := c.router.ForwarderCodeHash(ctx, c.Router)
	if err != nil {
		if isRPCAnswer(err) {
			return permanent(err.Error())
		}
		return transient(err)
	}
	if hash == ([32]byte{}) {
		return permanent("the router returns an empty forwarder code hash")
	}
	probe := strings.ToLower(s.cfg.Custody.Address())
	view, err := c.router.DepositAddressView(ctx, c.Router, probe)
	if err != nil {
		return transient(err)
	}
	if mine := DepositAddress(c.Router, hash, probe); view != mine {
		return permanent(fmt.Sprintf("the router derives %s for %s but the formula gives %s", HexAddress(view), probe, HexAddress(mine)))
	}
	c.mu.Lock()
	c.routerHash, c.routerOK, c.routerTransient = hash, true, ""
	c.mu.Unlock()
	s.cfg.Logf("bridge: deposit addresses on %s: router %s, forwarder code hash %x", c.Name, HexAddress(c.Router), hash)
	return hash, nil
}

// isRPCAnswer reports whether err is the node's own answer about the
// contract (no code there, or the call reverts) rather than a failure to
// reach the node.
func isRPCAnswer(err error) bool {
	if strings.Contains(err.Error(), "is it a DepositRouter") {
		return true
	}
	var re *RPCError
	return errors.As(err, &re) && (re.Code == 3 || strings.Contains(strings.ToLower(re.Message), "revert"))
}

// ---- registration ----

// apiErr is an error with the HTTP status the API answers it with.
type apiErr struct {
	code int
	msg  string
}

func (e *apiErr) Error() string { return e.msg }

// depositChain resolves and checks the chain and the Argonmine address of
// a deposit-address request.
func (s *Service) depositChain(chainQ, toQ string) (*chainState, string, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(chainQ), 10, 64)
	if err != nil || id == 0 {
		return nil, "", &apiErr{http.StatusBadRequest, "chain must be a chain id"}
	}
	c, ok := s.byID[id]
	if !ok {
		return nil, "", &apiErr{http.StatusBadRequest, fmt.Sprintf("chain %d is not served by this bridge", id)}
	}
	to := strings.ToLower(strings.TrimSpace(toQ))
	if _, err := wallet.DecodeAddress(s.cfg.Params.HRP, to); err != nil {
		return nil, "", &apiErr{http.StatusBadRequest, "to must be an address on this network (" + s.cfg.Params.HRP + "1…)"}
	}
	if IsSwapAddress(s.cfg.Params.HRP, to) {
		return nil, "", &apiErr{http.StatusBadRequest, "to is a swap address; deposit to your own wallet address"}
	}
	if !c.HasRouter() {
		return nil, "", &apiErr{http.StatusBadRequest, "deposit addresses are not available on " + c.Name}
	}
	if !s.cfg.Leader {
		return nil, "", &apiErr{http.StatusServiceUnavailable, "deposit addresses are served by the leader guardian"}
	}
	return c, to, nil
}

// depositAddressOf derives the deposit address of to on c.
func (s *Service) depositAddressOf(ctx context.Context, c *chainState, to string) ([20]byte, error) {
	hash, err := s.routerReady(ctx, c)
	if err != nil {
		var off errRouterOff
		if errors.As(err, &off) {
			return [20]byte{}, &apiErr{http.StatusServiceUnavailable, off.msg}
		}
		return [20]byte{}, &apiErr{http.StatusServiceUnavailable, err.Error() + "; try again shortly"}
	}
	return DepositAddress(c.Router, hash, to), nil
}

// RegisterDepositAddress returns the deposit address of to on the chain and
// starts watching it (once: asking again only refreshes the watch).
func (s *Service) RegisterDepositAddress(ctx context.Context, chainID uint64, to string) (*DepositWatch, error) {
	c, to, err := s.depositChain(strconv.FormatUint(chainID, 10), to)
	if err != nil {
		return nil, err
	}
	addr, err := s.depositAddressOf(ctx, c, to)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if w, ok := s.st.GetWatch(c.ID, to); ok {
		w.Touched = now
		rederive(w, c, addr)
		if err := s.st.PutWatch(w); err != nil {
			return nil, err
		}
		return w, nil
	}
	total := 0
	for _, n := range s.watchCount {
		total += n
	}
	if total >= s.watchCap {
		return nil, &apiErr{http.StatusServiceUnavailable, fmt.Sprintf("the bridge is already watching its maximum of %d deposit addresses; try again later or deposit with a wallet", s.watchCap)}
	}
	w := &DepositWatch{Chain: c.ID, To: to, Address: HexAddress(addr), Router: HexAddress(c.Router), Created: now, Touched: now, Check: true}
	if err := s.st.PutWatch(w); err != nil {
		return nil, err
	}
	s.watchCount[c.ID]++
	s.cfg.Logf("bridge: watching deposit address %s on %s for %s", w.Address, c.Name, to)
	return w, nil
}

// rederive moves a watch to the address derived under the chain's current
// router, should the configuration have changed the router since the watch
// was made: what was seen at the old address no longer applies.
func rederive(w *DepositWatch, c *chainState, addr [20]byte) bool {
	if w.Router == HexAddress(c.Router) && w.Address == HexAddress(addr) {
		return false
	}
	w.Router, w.Address, w.Tokens, w.Check = HexAddress(c.Router), HexAddress(addr), nil, true
	return true
}

// saveWatch writes back what the sweeper changed (tokens, sweeps, the check
// flag) without losing a registration that touched the watch meanwhile.
func (s *Service) saveWatch(w *DepositWatch) error {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	cur, ok := s.st.GetWatch(w.Chain, w.To)
	if !ok {
		return nil // dropped meanwhile
	}
	cur.Tokens, cur.Sweeps, cur.Check, cur.Address, cur.Router = w.Tokens, w.Sweeps, w.Check, w.Address, w.Router
	return s.st.PutWatch(cur)
}

// countWatches loads the number of watches per chain.
func (s *Service) countWatches() {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	s.watchCount = map[uint64]int{}
	for _, w := range s.st.Watches(0, true) {
		s.watchCount[w.Chain]++
	}
}

func (s *Service) watchesOn(chain uint64) int {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	return s.watchCount[chain]
}

// pruneWatches drops watches that never received anything and that nobody
// has asked for in watchExpiry. A watch that received something stays: its
// owner may send to the same address again without visiting the page.
func (s *Service) pruneWatches(now time.Time) int {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	n := 0
	for _, w := range s.st.Watches(0, true) {
		last := w.Created
		if w.Touched > last {
			last = w.Touched
		}
		if w.received() || now.Sub(time.Unix(last, 0)) < watchExpiry {
			continue
		}
		if s.st.DelWatch(w.Chain, w.To) == nil {
			s.watchCount[w.Chain]--
			n++
		}
	}
	if n > 0 {
		s.cfg.Logf("bridge: dropped %d deposit address watches unused for %v", n, watchExpiry)
	}
	return n
}

// ---- the leader's round ----

// processDepositAddresses scans every chain with a router for transfers
// into watched addresses and sweeps what has arrived. Leader only.
func (s *Service) processDepositAddresses(ctx context.Context) error {
	var errs []error
	for _, c := range s.chains {
		if c.router == nil {
			continue
		}
		if err := s.depositAddressRound(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("%s: deposit addresses: %w", c.Name, err))
		}
	}
	if time.Since(s.lastPrune) >= pruneEvery {
		s.pruneWatches(time.Now())
		s.lastPrune = time.Now()
	}
	return errors.Join(errs...)
}

func (s *Service) depositAddressRound(ctx context.Context, c *chainState) error {
	hash, err := s.routerReady(ctx, c)
	if err != nil {
		var off errRouterOff
		if errors.As(err, &off) {
			return nil // reported once in the log and in the status; nothing to do here
		}
		return err
	}
	head, err := c.evm.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("evm: %w", err)
	}
	if err := s.scanTransfers(ctx, c, head); err != nil {
		return err
	}
	var first error
	failed := 0
	router := HexAddress(c.Router)
	for _, w := range s.st.Watches(c.ID, false) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.Router != router {
			addr := DepositAddress(c.Router, hash, w.To)
			s.cfg.Logf("bridge: the router on %s changed: %s now watches %s instead of %s", c.Name, w.To, HexAddress(addr), w.Address)
			rederive(&w, c, addr)
			if err := s.saveWatch(&w); err != nil {
				return err
			}
			continue // scanned and read from the next round on
		}
		if err := s.processWatch(ctx, c, &w, head); err != nil {
			if first == nil {
				first = err
			}
			failed++
		}
	}
	if failed > 1 {
		return fmt.Errorf("%w (and %d more)", first, failed-1)
	}
	return first
}

// scanTransfers looks for transfers of the chain's tokens into watched
// addresses from the cursor to head, in ranges of 2000 blocks and in
// batches of watchChunk recipients per query.
func (s *Service) scanTransfers(ctx context.Context, c *chainState, head uint64) error {
	cur := WatchCursor(c.ID)
	from := c.RouterBlock
	if h, _, ok := s.st.Cursor(cur); ok && h+1 > from {
		from = h + 1
	}
	if from > head {
		return nil
	}
	watches := s.st.Watches(c.ID, false)
	if len(watches) == 0 {
		// Nothing to look for; a watch added later reads its balances once,
		// so a transfer from before it is not missed.
		return s.st.SetCursor(cur, head, "")
	}
	addrs := make([][20]byte, 0, len(watches))
	owner := map[[20]byte]string{}
	for _, w := range watches {
		a, err := ParseAddress(w.Address)
		if err != nil {
			continue
		}
		addrs = append(addrs, a)
		owner[a] = w.To
	}
	for lo := from; lo <= head; lo += 2000 {
		hi := lo + 1999
		if hi > head {
			hi = head
		}
		var found []TransferEvent
		for _, t := range c.Tokens {
			for k := 0; k < len(addrs); k += watchChunk {
				end := k + watchChunk
				if end > len(addrs) {
					end = len(addrs)
				}
				evs, err := c.router.Transfers(ctx, t.Address, addrs[k:end], lo, hi)
				if err != nil {
					return fmt.Errorf("transfer logs: %w", err)
				}
				for _, ev := range evs {
					if _, ok := owner[ev.To]; ok && ev.Token == t.Address && ev.Amount != nil && ev.Amount.Sign() > 0 {
						found = append(found, ev)
					}
				}
			}
		}
		if len(found) > 0 {
			s.recordTransfers(c, owner, found)
		}
		if err := s.st.SetCursor(cur, hi, ""); err != nil {
			return err
		}
	}
	return nil
}

// recordTransfers adds transfers to their watches.
func (s *Service) recordTransfers(c *chainState, owner map[[20]byte]string, evs []TransferEvent) {
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].Block != evs[j].Block {
			return evs[i].Block < evs[j].Block
		}
		return evs[i].LogIndex < evs[j].LogIndex
	})
	now := time.Now().Unix()
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	byTo := map[string][]TransferEvent{}
	for _, ev := range evs {
		byTo[owner[ev.To]] = append(byTo[owner[ev.To]], ev)
	}
	for to, list := range byTo {
		w, ok := s.st.GetWatch(c.ID, to)
		if !ok {
			continue
		}
		for _, ev := range list {
			t, _ := c.Token(ev.Token)
			sl := w.slot(HexAddress(ev.Token))
			if ev.Block <= sl.ReadBlock {
				continue // already in the balance last read
			}
			dup := false
			for _, x := range sl.Transfers {
				if x.Tx == ev.TxHash && x.Log == ev.LogIndex {
					dup = true
				}
			}
			if dup {
				continue
			}
			sl.Transfers = append(sl.Transfers, WatchTransfer{Block: ev.Block, Amount: ev.Amount.String(), Tx: ev.TxHash, Log: ev.LogIndex})
			if len(sl.Transfers) > slotTransfersCap { // merge the two oldest: the sum and the older block are what matter
				a, b := sl.Transfers[0], sl.Transfers[1]
				merged := WatchTransfer{Block: a.Block, Amount: new(big.Int).Add(mustBig(a.Amount), mustBig(b.Amount)).String(), Tx: b.Tx, Log: b.Log}
				sl.Transfers = append([]WatchTransfer{merged}, sl.Transfers[2:]...)
			}
			sl.LastTx, sl.Seen = ev.TxHash, now
			sym, dec := "?", 0
			if t != nil {
				sym, dec = t.Symbol, t.Decimals
			}
			s.cfg.Logf("bridge: %s %s received at deposit address %s on %s (for %s, block %d)", FormatBigTokenAmount(ev.Amount, dec), sym, w.Address, c.Name, w.To, ev.Block)
		}
		s.st.PutWatch(w)
	}
}

// processWatch settles or starts the sweeps of one watch, token by token.
func (s *Service) processWatch(ctx context.Context, c *chainState, w *DepositWatch, head uint64) error {
	changed := false
	if w.Check {
		for i := range c.Tokens {
			w.slot(HexAddress(c.Tokens[i].Address)).Check = true
		}
		w.Check, changed = false, true
	}
	var errs []error
	for i := range c.Tokens {
		ch, err := s.processSlot(ctx, c, w, &c.Tokens[i], head)
		changed = changed || ch
		if err != nil {
			errs = append(errs, err)
		}
	}
	if changed {
		w.trimSweeps()
		if err := s.saveWatch(w); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// processSlot handles one token at one deposit address: it follows a sent
// sweep to its receipt, and otherwise, once a transfer has the chain's
// confirmations (or a check is due), reads the balance and sweeps it if it
// reaches the token's minimum. At most one sweep per address and token is
// ever in flight. It reports whether the watch changed.
func (s *Service) processSlot(ctx context.Context, c *chainState, w *DepositWatch, t *Token, head uint64) (bool, error) {
	key := HexAddress(t.Address)
	now := time.Now()
	open := w.openSweep(key)
	if open != nil && open.Status == SweepSent {
		status, block, err := c.evm.Receipt(ctx, open.Tx)
		if err != nil {
			return false, fmt.Errorf("sweep receipt %s: %w", open.Tx, err)
		}
		sl := w.slot(key)
		switch status {
		case ReceiptOK:
			open.Status, open.Error, open.Updated = SweepConfirmed, "", now.Unix()
			if ev, err := c.router.SweptLog(ctx, c.Router, open.Tx); err == nil && ev != nil {
				open.DepositID = ev.DepositID
				if ev.Amount != nil && ev.Amount.Sign() > 0 {
					open.Amount = ev.Amount.String()
				}
			}
			// The sweep took everything the address held at that block.
			sl.Balance = "0"
			sl.coveredUpTo(block)
			sl.Check, sl.Failures, sl.RetryAt = true, 0, 0 // one more read catches a transfer in the same block
			s.cfg.Logf("bridge: swept %s %s from %s on %s into the vault for %s (deposit #%d, %s)", FormatBigTokenAmount(mustBig(open.Amount), t.Decimals), t.Symbol, w.Address, c.Name, w.To, open.DepositID, open.Tx)
			return true, nil
		case ReceiptFailed:
			open.Status, open.Error, open.Updated = SweepFailed, "the sweep transaction reverted", now.Unix()
			sl.fail(now)
			sl.Check = true
			return true, nil
		default:
			if now.Sub(time.Unix(open.Updated, 0)) > sweepStale {
				open.Status, open.Error, open.Updated = SweepFailed, "not mined within an hour", now.Unix()
				sl.fail(now)
				sl.Check = true
				return true, nil
			}
			return false, nil
		}
	}
	sl := w.Tokens[key]
	due := open != nil // a pending sweep is retried
	if sl != nil && (sl.Check || (len(sl.Transfers) > 0 && sl.Transfers[0].Block+c.Confirmations <= head)) {
		due = true
	}
	if !due || (sl != nil && sl.RetryAt > now.Unix()) {
		return false, nil
	}
	addr, err := ParseAddress(w.Address)
	if err != nil {
		return false, err
	}
	bal, err := c.router.BalanceOf(ctx, t.Address, addr, head)
	if err != nil {
		return false, fmt.Errorf("balance of %s at %s: %w", t.Symbol, w.Address, err)
	}
	sl = w.slot(key)
	sl.Balance = bal.String()
	sl.coveredUpTo(head)
	sl.Check = false
	if bal.Sign() > 0 && sl.Seen == 0 {
		sl.Seen = now.Unix() // arrived before the watch existed
	}
	if bal.Sign() == 0 || bal.Cmp(t.MinDeposit) < 0 {
		// Below the minimum: not an error, the funds wait for more.
		if open != nil {
			open.Status, open.Error, open.Updated = SweepFailed, "the address no longer holds the minimum", now.Unix()
		}
		return true, nil
	}
	if why := c.gasRefused(); why != "" {
		return true, nil // nothing is sent from a gas key that is a guardian or the vault's owner; the status says why
	}
	tx, err := c.router.Sweep(ctx, c.Router, t.Address, w.To)
	switch {
	case err == nil:
		if open == nil {
			w.Sweeps = append(w.Sweeps, Sweep{Token: key, TokenSymbol: t.Symbol, Decimals: t.Decimals, Created: now.Unix()})
			open = &w.Sweeps[len(w.Sweeps)-1]
		}
		open.Status, open.Tx, open.Amount, open.Error, open.Updated = SweepSent, tx, bal.String(), "", now.Unix()
		open.Attempts++
		sl.Failures, sl.RetryAt = 0, 0
		c.setGasErr("")
		s.cfg.Logf("bridge: sweeping %s %s from %s on %s for %s (%s)", FormatBigTokenAmount(bal, t.Decimals), t.Symbol, w.Address, c.Name, w.To, tx)
		return true, nil
	case errors.Is(err, ErrSweepBelowMinimum):
		// The vault's own minimum is above the token's configured one: wait for more.
		if open != nil {
			open.Status, open.Error, open.Updated = SweepFailed, "below the vault's minimum deposit; waiting for more", now.Unix()
		}
		s.cfg.Logf("bridge: %s %s at %s on %s is below the vault's minimum; waiting for more", FormatBigTokenAmount(bal, t.Decimals), t.Symbol, w.Address, c.Name)
		return true, nil
	case errors.Is(err, ErrNothingToSweep):
		sl.Balance = "0"
		if open != nil {
			open.Status, open.Error, open.Updated = SweepFailed, "nothing left to sweep", now.Unix()
		}
		return true, nil
	}
	if open == nil {
		w.Sweeps = append(w.Sweeps, Sweep{Token: key, TokenSymbol: t.Symbol, Decimals: t.Decimals, Amount: bal.String(), Created: now.Unix()})
		open = &w.Sweeps[len(w.Sweeps)-1]
	}
	open.Status, open.Error, open.Amount, open.Updated = SweepPending, publicErr(err.Error()), bal.String(), now.Unix()
	open.Attempts++
	sl.fail(now)
	if gasProblem(err) {
		s.noGas(c, "sweep transactions", err)
	}
	return true, fmt.Errorf("sweep %s at %s: %w", t.Symbol, w.Address, err)
}

// ---- the public API ----

// DepositAddressToken is one token a deposit address accepts.
type DepositAddressToken struct {
	Symbol     string `json:"symbol"`
	Address    string `json:"address"`
	Decimals   int    `json:"decimals"`
	MinDeposit string `json:"min_deposit"` // token units
}

// DepositAddressInfo answers GET /api/deposit-address.
type DepositAddressInfo struct {
	ChainID       uint64                `json:"chain_id"`
	Chain         string                `json:"chain"`
	To            string                `json:"to"`
	Address       string                `json:"address"`  // EIP-55
	Router        string                `json:"router"`   // 0x lowercase
	Explorer      string                `json:"explorer"` // the deposit address on the chain's explorer, or empty
	Confirmations uint64                `json:"confirmations"`
	DepositFeeBps uint16                `json:"deposit_fee_bps"`
	Tokens        []DepositAddressToken `json:"tokens"`
	Note          string                `json:"note"`
}

// DepositAddressBalance is what sits at a deposit address in one token.
type DepositAddressBalance struct {
	Symbol     string `json:"symbol"`
	Token      string `json:"token"`
	Decimals   int    `json:"decimals"`
	Amount     string `json:"amount"`      // token units: the last balance read plus the transfers seen since
	MinDeposit string `json:"min_deposit"` // token units
	LastTx     string `json:"last_tx"`     // newest transfer in, or empty
}

// DepositAddressStatus answers GET /api/deposit-address/status.
type DepositAddressStatus struct {
	ChainID       uint64                  `json:"chain_id"`
	Chain         string                  `json:"chain"`
	To            string                  `json:"to"`
	Address       string                  `json:"address"`
	Watched       bool                    `json:"watched"`
	Confirmations uint64                  `json:"confirmations"`
	Head          uint64                  `json:"head"` // last block of the chain the guardians scanned
	Balances      []DepositAddressBalance `json:"balances"`
	Sweeps        []Sweep                 `json:"sweeps"`   // newest first
	Deposits      []Inbound               `json:"deposits"` // the vault deposits made through the router for to, newest first
}

func (s *Service) depositAddressInfo(c *chainState, to string, addr [20]byte) DepositAddressInfo {
	bps, _ := c.fees()
	info := DepositAddressInfo{ChainID: c.ID, Chain: c.Name, To: to, Address: ChecksumAddress(addr), Router: HexAddress(c.Router),
		Confirmations: c.Confirmations, DepositFeeBps: bps}
	if c.Explorer != "" {
		info.Explorer = c.Explorer + "/address/" + info.Address
	}
	var syms []string
	for _, t := range c.Tokens {
		info.Tokens = append(info.Tokens, DepositAddressToken{Symbol: t.Symbol, Address: HexAddress(t.Address), Decimals: t.Decimals, MinDeposit: t.MinDeposit.String()})
		syms = append(syms, t.Symbol)
	}
	list := strings.Join(syms, " or ")
	if len(syms) > 2 {
		list = strings.Join(syms[:len(syms)-1], ", ") + " or " + syms[len(syms)-1]
	}
	info.Note = fmt.Sprintf(depositAddrNote, list, c.Name)
	return info
}

func (s *Service) depositAddressStatus(c *chainState, to string, addr [20]byte) DepositAddressStatus {
	head, _, _ := s.st.Cursor(EVMCursor(c.ID))
	c.mu.Lock()
	if c.latest > head {
		head = c.latest
	}
	c.mu.Unlock()
	out := DepositAddressStatus{ChainID: c.ID, Chain: c.Name, To: to, Address: ChecksumAddress(addr), Confirmations: c.Confirmations, Head: head,
		Balances: []DepositAddressBalance{}, Sweeps: []Sweep{}, Deposits: []Inbound{}}
	w, ok := s.st.GetWatch(c.ID, to)
	out.Watched = ok
	for _, t := range c.Tokens {
		b := DepositAddressBalance{Symbol: t.Symbol, Token: HexAddress(t.Address), Decimals: t.Decimals, Amount: "0", MinDeposit: t.MinDeposit.String()}
		if ok {
			if sl := w.Tokens[b.Token]; sl != nil {
				b.Amount, b.LastTx = sl.amount().String(), sl.LastTx
			}
		}
		out.Balances = append(out.Balances, b)
	}
	if ok {
		for i := len(w.Sweeps) - 1; i >= 0; i-- {
			sw := w.Sweeps[i]
			sw.Error = publicErr(sw.Error)
			out.Sweeps = append(out.Sweeps, sw)
		}
	}
	router := HexAddress(c.Router)
	for _, d := range s.st.InboundsTo([]string{to}, 0) {
		if d.Chain == c.ID && strings.EqualFold(d.From, router) {
			out.Deposits = append(out.Deposits, publicInbound(d))
			if len(out.Deposits) == 20 {
				break
			}
		}
	}
	return out
}

func writeAPIErr(w http.ResponseWriter, err error) {
	var ae *apiErr
	if errors.As(err, &ae) {
		apiError(w, ae.code, ae.msg)
		return
	}
	apiError(w, http.StatusInternalServerError, err.Error())
}

// handleDepositAddress answers GET /api/deposit-address?chain=<id>&to=<addr>.
func (s *Service) handleDepositAddress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		apiError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.limiter.allow(s.clientIP(r), time.Now()) {
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusTooManyRequests, "too many requests; wait a few seconds")
		return
	}
	q := r.URL.Query()
	c, to, err := s.depositChain(q.Get("chain"), q.Get("to"))
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	wt, err := s.RegisterDepositAddress(ctx, c.ID, to)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	addr, _ := ParseAddress(wt.Address)
	writeJSON(w, s.depositAddressInfo(c, to, addr))
}

// handleDepositAddressStatus answers GET /api/deposit-address/status?chain=<id>&to=<addr>.
func (s *Service) handleDepositAddressStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, to, err := s.depositChain(q.Get("chain"), q.Get("to"))
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	addr, err := s.depositAddressOf(ctx, c, to)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, s.depositAddressStatus(c, to, addr))
}

// ---- rate limiting: a token bucket per client IP ----

type rateLimiter struct {
	mu    sync.Mutex
	rate  float64 // tokens per second
	burst float64
	m     map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// limiterMaxClients bounds the memory of one limiter (about 100 bytes per
// client). Clients whose bucket has refilled are forgotten first; past
// that, the tenth seen longest ago. A flood from many addresses never
// resets everyone's bucket at once.
const limiterMaxClients = 10_000

func newRateLimiter(perSecond, burst float64) *rateLimiter {
	return &rateLimiter{rate: perSecond, burst: burst, m: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= limiterMaxClients {
			l.evictLocked(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictLocked makes room for a new client: it forgets the clients whose
// bucket has refilled (forgetting them changes nothing), then, if the table
// is still full, the tenth seen longest ago.
func (l *rateLimiter) evictLocked(now time.Time) {
	for k, x := range l.m {
		if x.tokens+now.Sub(x.last).Seconds()*l.rate >= l.burst {
			delete(l.m, k)
		}
	}
	if len(l.m) < limiterMaxClients {
		return
	}
	type seen struct {
		key  string
		last time.Time
	}
	all := make([]seen, 0, len(l.m))
	for k, x := range l.m {
		all = append(all, seen{k, x.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for _, x := range all[:len(all)/10+1] {
		delete(l.m, x.key)
	}
}

// clientIP names the client of a request for the per-client limits.
// With -client-ip-header set, it is that header's right-most entry (for
// X-Forwarded-For the hop the proxy itself appended; a client can only add
// entries to the left of it). Otherwise it is the TCP peer, unless the peer
// is a proxy on a loopback, private or carrier-grade NAT (100.64.0.0/10)
// address, as a platform's edge is: then the right-most X-Forwarded-For
// entry. A missing or malformed value falls back to the peer. IPv6 clients
// are grouped by /64, the block one subscriber usually holds.
func (s *Service) clientIP(r *http.Request) string { return clientIPFrom(r, s.cfg.ClientIPHeader) }

func clientIPFrom(r *http.Request, header string) string {
	header = strings.TrimSpace(header)
	if header != "" {
		if ip, ok := lastHeaderIP(r, header); ok {
			return ipKey(ip)
		}
	}
	peer, ok := parseClientIP(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if header == "" && trustedProxy(peer) {
		if ip, ok := lastHeaderIP(r, "X-Forwarded-For"); ok {
			return ipKey(ip)
		}
	}
	return ipKey(peer)
}

// cgnat is the shared address space (RFC 6598) some platforms' edge proxies
// connect from.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func trustedProxy(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)
}

// lastHeaderIP is the right-most address in a header (every value joined).
func lastHeaderIP(r *http.Request, header string) (netip.Addr, bool) {
	vals := r.Header.Values(header)
	if len(vals) == 0 {
		return netip.Addr{}, false
	}
	all := strings.Split(strings.Join(vals, ","), ",")
	return parseClientIP(all[len(all)-1])
}

// parseClientIP accepts "1.2.3.4", "1.2.3.4:5", "::1", "[::1]:5".
func parseClientIP(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if ip, err := netip.ParseAddr(s); err == nil {
		return ip.Unmap(), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			return ip.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func ipKey(ip netip.Addr) string {
	if ip.Is6() {
		if p, err := ip.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip.String()
}

// ---- QR codes ----

var evmAddrRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// serveQR answers /qr.svg?d=<text>: a deposit address (0x, 40 hex digits,
// any case) is drawn here, since ui.QRSVG takes only bech32 characters (an
// Argonmine address), which it serves as the apps do.
func serveQR(w http.ResponseWriter, r *http.Request) {
	d := strings.TrimSpace(r.URL.Query().Get("d"))
	if !evmAddrRe.MatchString(d) {
		ui.ServeQR(w, d)
		return
	}
	q, err := qrcode.New(d, qrcode.Medium)
	if err != nil {
		http.Error(w, "bad data", http.StatusBadRequest)
		return
	}
	bits := q.Bitmap()
	n := len(bits)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, n, n)
	for y, row := range bits {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(b.String()))
}
