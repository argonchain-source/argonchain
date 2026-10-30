package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/consensus"
	"argonmine/wallet"
)

// The inbound bridge (docs/BRIDGE.md): a dollar-stablecoin deposit (USDC or
// USDT) into a vault contract on any of several EVM chains is paid out as
// uARG on this chain, issued by the guardians' aggregate key through the
// consensus asset layer; a uARG burn with a memo naming a chain, a token and
// a recipient is paid out from that chain's vault with threshold guardian
// signatures. ARG never crosses.

// Config for one guardian daemon.
type Config struct {
	Params  *consensus.Params
	Node    *wallet.Client        // this chain's node RPC
	Chains  []Chain               // the EVM chains and their tokens; the first chain's first token is the BRG1 default
	EVMs    map[uint64]EVM        // one client per chain id in Chains: its first RPC provider, which also sends (from the gas key, on the leader)
	Custody *Custody              // guardians' aggregate key: issues uARG and holds the ARG fee float
	EVMKey  *secp256k1.PrivateKey // signs release approvals; never sends a transaction (the gas key does)
	// ExtraEVMs are a chain's other RPC providers: heads, block hashes,
	// deposit logs and receipts are read from every provider and a deposit is
	// acted on only when they all agree.
	ExtraEVMs map[uint64][]EVM

	Leader bool
	Peers  []string // other guardians' base URLs (leader only)
	Secret string   // shared HMAC secret for the guardian API, at least MinSecretLen characters
	// LeaderKey is the leader's 33-byte compressed L1 guardian key, one of
	// the guardians: a follower accepts guardian requests signed by it and
	// by nothing else (auth.go). Required on a follower that holds a key.
	LeaderKey []byte

	Threshold       int    // EVM signatures needed to release
	L1Confirmations uint32 // blocks before a burn is released
	AssetSymbol     string // the asset issued, "UARG"
	FeeRate         consensus.Amount
	IssueInterval   time.Duration
	ScanInterval    time.Duration
	// MaxIssue and MaxIssueDay limit what this guardian issues, in uARG
	// units: per issuance and over any 24 hours (0 = no limit). A deposit
	// above MaxIssue waits for the guardians, with the reason in its error.
	MaxIssue    consensus.Amount
	MaxIssueDay consensus.Amount
	// ReleaseTiers scale a burn's payout depth with its amount; a burn above
	// the last tier is held for a manual release. Empty: L1Confirmations for
	// every burn, which is also the floor of every tier.
	ReleaseTiers []Tier
	// L1ReorgPause: an Argonmine reorganisation that undoes more than this
	// many blocks pauses every issuance and release on this guardian until
	// an operator resumes them (0: never).
	L1ReorgPause uint32
	// ReleaseTimeout is how long a release transaction may stay unmined
	// before it is sent again at higher fees (default 5 minutes).
	ReleaseTimeout time.Duration
	// IssueRebroadcast is how long an issuance may be neither mined nor in
	// the node's mempool before it is broadcast again (default 10 minutes;
	// after 30 its deposits are issued anew).
	IssueRebroadcast time.Duration
	// StuckAfter: an item waiting longer than this is listed in the status
	// under stuck and logged at warn level once an hour (default 1 hour).
	StuckAfter time.Duration
	// Now is the clock (tests); default time.Now.
	Now func() time.Time
	// ClientIPHeader names the request header a proxy in front puts the
	// client's address in (its right-most entry, the one the proxy appended,
	// is used), e.g. X-Forwarded-For: the key of the per-client limits.
	// Empty: the TCP peer, or the right-most X-Forwarded-For entry when the
	// peer is a loopback, private or carrier-grade NAT (100.64.0.0/10)
	// address, as a platform's edge proxy is (clientIP).
	ClientIPHeader string

	StatePath string
	Logf      func(string, ...any)
}

// chainState is one configured chain, its client and what the last scan found.
type chainState struct {
	Chain
	evm       EVM
	providers []EVM // evm first, then the extra providers

	mu           sync.Mutex
	latest       uint64              // the lowest head of the providers at the last scan
	final        uint64              // the lowest final block of the providers at the last scan
	disagree     string              // the providers disagree: nothing on the chain is acted on
	previewTo    uint64              // the last block previewed above the final one (memory only)
	tokenErr     map[[20]byte]string // per token, once its decimals() was read: "" or why the token is refused
	limits       map[[20]byte]*ReleaseLimits
	gasBalance   *big.Int // the gas key's balance (leader), from the last scan
	gasRoleErr   string   // the gas key is a guardian or the owner of the vault: nothing is sent from it
	rolesChecked bool
	scanErr      string                // last scan failure, or empty
	gasErr       string                // the leader could not pay for a release here, or empty
	reserves     map[[20]byte]*big.Int // vault balance per token, from the last scan
	obligations  map[[20]byte]*big.Int // what backs issued uARG per token, from the last scan
	depositBps   uint16                // the vault's fees in basis points, from the last scan
	releaseBps   uint16
	feesKnown    bool // the vault's fees have been read at least once
	// idErr says why the vault's deposits are not scanned or issued: its
	// first deposit id is not above every deposit id on record for the
	// chain (checkVaultIdentity), or empty.
	idErr string
	// cand is the epoch the configured vault starts once it passes
	// checkVaultIdentity, while the epoch on record is another vault's (or
	// none): kept in memory only, so a refused or mistyped vault never
	// replaces the epoch in use (vaultCandidate). nil when the configured
	// vault is the one in use.
	cand  *Epoch
	setup bool // the vault's finalized() is false: its owner may change its fees and limits without the timelock
	// guardianSeen caches the vault's answer to "is this address a
	// guardian?" for release signatures, for an hour per address.
	guardianSeen map[[20]byte]guardianRole

	// Deposit addresses (depositaddr.go): the router client, or nil when the
	// chain has no router, and the result of the one-time router check.
	router          RouterEVM
	routerHash      [32]byte  // the router's forwarderCodeHash(), once checked
	routerOK        bool      // checked: the router serves this vault and derives addresses as DepositAddress does
	routerErr       string    // the check failed: deposit addresses stay off on this chain
	routerTransient string    // the last check could not reach the chain
	routerRetry     time.Time // not before this is the check tried again
}

func (c *chainState) setScanErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.scanErr = ""
	} else {
		c.scanErr = err.Error()
	}
}

// setIDErr records why the vault's deposits are refused ("" when they are
// not) and reports whether that changed.
func (c *chainState) setIDErr(msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := c.idErr != msg
	c.idErr = msg
	return changed
}

func (c *chainState) identityErr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idErr
}

func (c *chainState) setGasErr(msg string) {
	c.mu.Lock()
	c.gasErr = msg
	c.mu.Unlock()
}

func (c *chainState) setReserve(token [20]byte, reserve, obligations *big.Int) {
	c.mu.Lock()
	c.reserves[token] = reserve
	c.obligations[token] = obligations
	c.mu.Unlock()
}

func (c *chainState) setFees(depositBps, releaseBps uint16) {
	c.mu.Lock()
	c.depositBps, c.releaseBps, c.feesKnown = depositBps, releaseBps, true
	c.mu.Unlock()
}

// fees returns the vault's fee rates from the last scan (0 until the chain
// has been scanned once).
func (c *chainState) fees() (depositBps, releaseBps uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.depositBps, c.releaseBps
}

// Service is the guardian daemon.
type Service struct {
	cfg    Config
	st     *State
	hc     *http.Client
	asset  consensus.Hash
	issuer [32]byte
	chains []*chainState // in configuration order
	byID   map[uint64]*chainState

	mu       sync.Mutex
	sessions map[string]*pendingSigning // follower-side MuSig2 sessions by request id
	signMu   sync.Mutex                 // one partial signature or release signature at a time (the signing memory)
	lastErr  string
	stop     chan struct{}
	wg       sync.WaitGroup
	started  bool
	closed   bool

	// Deposit addresses (depositaddr.go).
	watchMu    sync.Mutex     // registrations and the sweeper's writes of watches
	watchCount map[uint64]int // watches per chain
	watchCap   int            // across all chains
	limiter    *rateLimiter   // per client IP, on GET /api/deposit-address
	lastPrune  time.Time      // leader loop only

	leaderKey   *btcec.PublicKey // the key a follower takes guardian requests from
	replay      *replayCache     // nonces of accepted guardian requests
	adminReplay *replayCache     // nonces of accepted operator commands
	warned      map[string]time.Time

	// Hand-offs (web.go) and the public API's cached answers.
	handoffs    *handoffStore
	handoffPost *rateLimiter
	handoffGet  *rateLimiter
	snapMu      sync.Mutex
	snap        *apiSnapshot

	// Withdrawals (withdraw.go) and the mempool they are followed in.
	mempool   *mempoolWatch
	wdPost    *rateLimiter
	wdGet     *rateLimiter
	wdViews   *wdViewCache
	wdMu      sync.Mutex                 // creating and pruning withdrawals
	wdCount   int                        // open withdrawals (q/t/), under wdMu
	wdClients map[string][]wdClientEntry // the withdrawals each client made recently, under wdMu
}

// PeerTimeout bounds one call of the leader to another guardian; a
// guardian's HTTP server must allow its handlers longer than this.
const PeerTimeout = 30 * time.Second

// New opens the state and returns a stopped service.
func New(cfg Config) (*Service, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.L1Confirmations == 0 {
		cfg.L1Confirmations = 30
	}
	if cfg.FeeRate < 1 {
		cfg.FeeRate = 1
	}
	if cfg.IssueInterval == 0 {
		cfg.IssueInterval = 2 * time.Minute
	}
	if cfg.ScanInterval == 0 {
		cfg.ScanInterval = 10 * time.Second
	}
	if cfg.AssetSymbol == "" {
		cfg.AssetSymbol = "UARG"
	}
	if cfg.ReleaseTimeout <= 0 {
		cfg.ReleaseTimeout = 5 * time.Minute
	}
	if cfg.IssueRebroadcast <= 0 {
		cfg.IssueRebroadcast = 10 * time.Minute
	}
	if cfg.StuckAfter <= 0 {
		cfg.StuckAfter = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if !consensus.ValidSymbol(cfg.AssetSymbol) {
		return nil, errors.New("bridge: invalid asset symbol")
	}
	if cfg.Threshold < 1 {
		return nil, errors.New("bridge: threshold must be at least 1")
	}
	if len(cfg.Chains) == 0 {
		return nil, errors.New("bridge: at least one chain is required")
	}
	s := &Service{cfg: cfg, hc: &http.Client{Timeout: PeerTimeout}, sessions: map[string]*pendingSigning{}, stop: make(chan struct{}), byID: map[uint64]*chainState{},
		watchCount: map[uint64]int{}, watchCap: watchCapDefault, limiter: newRateLimiter(1.0/3, 20),
		replay: newReplayCache(replayCacheMax), adminReplay: newReplayCache(1000), warned: map[string]time.Time{}, handoffs: newHandoffStore(),
		handoffPost: newRateLimiter(1.0/20, 10), handoffGet: newRateLimiter(1.0/2, 20),
		mempool: newMempoolWatch(), wdPost: newRateLimiter(1.0/6, 10), wdGet: newRateLimiter(2, 60), wdViews: newWdViewCache(), wdClients: map[string][]wdClientEntry{}}
	for _, c := range cfg.Chains {
		if len(c.Tokens) == 0 {
			return nil, fmt.Errorf("bridge: chain %s has no tokens", c.Name)
		}
		evm, ok := cfg.EVMs[c.ID]
		if !ok || evm == nil {
			return nil, fmt.Errorf("bridge: no EVM client for chain %d (%s)", c.ID, c.Name)
		}
		if evm.ChainID() != c.ID || evm.Vault() != c.Vault {
			return nil, fmt.Errorf("bridge: the client for chain %d (%s) is for another chain or vault", c.ID, c.Name)
		}
		if _, dup := s.byID[c.ID]; dup {
			return nil, fmt.Errorf("bridge: chain %d listed twice", c.ID)
		}
		cs := &chainState{Chain: c, evm: evm, providers: []EVM{evm}, reserves: map[[20]byte]*big.Int{}, obligations: map[[20]byte]*big.Int{},
			tokenErr: map[[20]byte]string{}, limits: map[[20]byte]*ReleaseLimits{}}
		for _, x := range cfg.ExtraEVMs[c.ID] {
			if x == nil || x.ChainID() != c.ID || x.Vault() != c.Vault {
				return nil, fmt.Errorf("bridge: an extra provider of chain %d (%s) is for another chain or vault", c.ID, c.Name)
			}
			cs.providers = append(cs.providers, x)
		}
		if cfg.EVMKey != nil {
			own := AddressOf(cfg.EVMKey.PubKey())
			for _, p := range cs.providers {
				if sp, ok := p.(SenderEVM); ok && sp.Sender() == own {
					return nil, fmt.Errorf("bridge: chain %d (%s) would send transactions from this guardian's own EVM key %s; give the leader a separate gas key (-gas-key)", c.ID, c.Name, HexAddress(own))
				}
			}
		}
		if c.HasRouter() {
			rc, ok := evm.(RouterEVM)
			if !ok {
				return nil, fmt.Errorf("bridge: chain %d (%s) has a router but its client cannot serve deposit addresses", c.ID, c.Name)
			}
			cs.router = rc
		}
		s.chains = append(s.chains, cs)
		s.byID[c.ID] = cs
	}
	if err := s.checkAuthConfig(); err != nil {
		return nil, err
	}
	st, err := openState(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	s.st = st
	switch {
	case st.MigratedFrom == 3:
		cfg.Logf("bridge: state directory migrated from format 3: every record is kept; rescanning Argonmine from genesis for the custody coins")
	case st.Rebuilt && st.MigratedFrom == 0 && st.wasEmpty:
		cfg.Logf("bridge: state directory is new: scanning every chain from the start")
	case st.Rebuilt:
		cfg.Logf("bridge: state directory of format %d archived under a/%d/ (nothing deleted): rescanning every chain from the start", st.MigratedFrom, st.MigratedFrom)
	}
	copy(s.issuer[:], cfg.Custody.XOnly())
	s.asset = consensus.AssetID(s.issuer, cfg.AssetSymbol)
	s.countWatches()
	if s.wdCount, err = st.prepareOpenIndex(); err != nil {
		st.Close()
		return nil, err
	}
	st.swapChanged = s.wdViews.changed
	s.reclassifyPending()
	for _, p := range st.Pauses() {
		cfg.Logf("bridge: PAUSED %s since %s: %s (argonmine-bridge resume lifts it)", p.Scope, time.Unix(p.Since, 0).UTC().Format(time.RFC3339), p.Reason)
	}
	return s, nil
}

func (s *Service) now() time.Time { return s.cfg.Now() }

// reclassifyPending recomputes the pending deposits recorded with decimals
// other than the configuration's (a token configured wrongly, refused,
// then fixed): their uARG follows from the token amount on chain.
func (s *Service) reclassifyPending() {
	for _, d := range s.st.Inbounds() {
		if d.Status != InboundPending {
			continue
		}
		c, ok := s.byID[d.Chain]
		if !ok {
			continue
		}
		addr, err := ParseAddress(d.Token)
		if err != nil {
			continue
		}
		t, ok := c.Token(addr)
		if !ok || t.Decimals == d.Decimals {
			continue
		}
		from, _ := ParseAddress(d.From)
		ev := DepositEvent{ID: d.ID, Token: addr, From: from, Amount: mustBig(d.TokenAmount), Fee: mustBig(d.TokenFee), To: d.To,
			Block: d.Block, BlockHash: d.BlockHash, TxHash: d.EVMTx, LogIndex: d.LogIndex}
		nd := s.classifyDeposit(c, ev)
		nd.Created = d.Created
		s.st.PutInbound(nd)
		s.cfg.Logf("bridge: deposit %s#%d recomputed with %d decimals (it was recorded with %d): %s uARG", c.Name, d.ID, t.Decimals, d.Decimals, wallet.FormatAmount(nd.Amount))
	}
}

// checkAuthConfig enforces who may coordinate: the leader signs its
// requests with its own guardian key, a follower that can sign must know
// the leader's key, and the shared secret is long enough wherever guardians
// talk to each other.
func (s *Service) checkAuthConfig() error {
	cfg := &s.cfg
	talks := (cfg.Leader && len(cfg.Peers) > 0) || (!cfg.Leader && cfg.Custody.CanSign())
	if talks && len(cfg.Secret) < MinSecretLen {
		return fmt.Errorf("bridge: the shared secret must be at least %d characters", MinSecretLen)
	}
	if cfg.Leader {
		if !cfg.Custody.CanSign() {
			return errors.New("bridge: the leader needs its guardian key (-key)")
		}
		if len(cfg.LeaderKey) > 0 && !bytes.Equal(cfg.LeaderKey, cfg.Custody.LocalKey()) {
			return errors.New("bridge: -leader-pubkey on the leader must be its own key")
		}
		return nil
	}
	if !cfg.Custody.CanSign() {
		return nil // an observer signs nothing
	}
	if len(cfg.LeaderKey) == 0 {
		return errors.New("bridge: a follower needs the leader's public key (-leader-pubkey)")
	}
	if bytes.Equal(cfg.LeaderKey, cfg.Custody.LocalKey()) {
		return errors.New("bridge: -leader-pubkey names this guardian's own key; run it with -leader, or name the leader")
	}
	k, err := parseLeaderKey(cfg.Custody, cfg.LeaderKey)
	if err != nil {
		return err
	}
	s.leaderKey = k
	return nil
}

// Asset is the id of the uARG asset this bridge issues.
func (s *Service) Asset() consensus.Hash { return s.asset }

// Chains lists the configured chains.
func (s *Service) Chains() []Chain {
	out := make([]Chain, len(s.chains))
	for i, c := range s.chains {
		out[i] = c.Chain
	}
	return out
}

// Start runs the scanning loop (and the issue/release loop on the leader).
func (s *Service) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { <-s.stop; cancel() }()
		if !s.cfg.Leader {
			s.indexOnly(ctx)
			return
		}
		// The leader serves the public page: it follows withdrawals in the
		// mempool as well as in blocks.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.mempoolLoop(ctx)
		}()
		lastIssue := time.Time{}
		for {
			withIssue := time.Since(lastIssue) >= s.cfg.IssueInterval
			if err := s.tick(ctx, withIssue); err != nil {
				s.cfg.Logf("bridge: %v", err)
			}
			if withIssue {
				lastIssue = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.cfg.ScanInterval):
			}
		}
	}()
}

// indexOnly keeps a follower's view of every chain current so it can verify
// what the leader asks it to sign.
func (s *Service) indexOnly(ctx context.Context) {
	for {
		if err := s.tick(ctx, false); err != nil {
			s.cfg.Logf("bridge: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.ScanInterval):
		}
	}
}

// Stop ends the loops (if running) and closes the state.
func (s *Service) Stop() {
	s.mu.Lock()
	if s.started {
		s.started = false
		close(s.stop)
	}
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
	if !closed {
		s.st.Close()
	}
}

func (s *Service) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.lastErr = ""
	} else {
		s.lastErr = err.Error()
	}
}

// Once performs one full round: scan every chain, issue confirmed deposits,
// release confirmed burns. Used by tests and operators.
func (s *Service) Once(ctx context.Context) error {
	return s.tick(ctx, true)
}

func (s *Service) tick(ctx context.Context, withIssue bool) error {
	var errs []error
	// EVM chains first, then this chain: a deposit is then on record before
	// the issuance that paid it is seen, whatever state the daemon started from.
	if err := s.scanEVM(ctx); err != nil {
		errs = append(errs, err)
	}
	// The leader sweeps deposit addresses every round, not only with the
	// issuance batch: a sweep is the user's deposit reaching the vault.
	if s.cfg.Leader {
		if err := s.processDepositAddresses(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.scanL1(ctx); err != nil {
		errs = append(errs, err)
	}
	if withIssue && s.cfg.Leader {
		if err := s.processInbound(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := s.processRedemptions(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	s.warnStuck()
	err := errors.Join(errs...)
	s.setErr(err)
	return err
}

// applyReleaseFee fills a redemption's fee and net payout from the chain's
// release fee as last read from its vault: fee = gross × bps / 10000 rounded
// down, paid = gross − fee, exactly as the contract computes them. The
// gross (TokenAmount) is what release() is called with and what the
// guardians sign; it is never changed by the fee.
func (r *Redemption) applyReleaseFee(c *chainState) {
	gross, ok := new(big.Int).SetString(r.TokenAmount, 10)
	if !ok {
		gross = new(big.Int)
	}
	_, releaseBps := c.fees()
	fee := FeeOf(gross, releaseBps)
	r.TokenFee = fee.String()
	r.TokenPaid = new(big.Int).Sub(gross, fee).String()
}

// ---- scanning the EVM chains ----

// scanEVM scans every chain. A chain that fails is reported in its status
// and retried next tick; the others are not held up.
func (s *Service) scanEVM(ctx context.Context) error {
	var errs []error
	for _, c := range s.chains {
		err := s.scanChain(ctx, c)
		c.setScanErr(err)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// classifyDeposit turns a Deposited event into the record of what it pays:
// the token must be allowlisted on that chain, the deposit at least the
// token's minimum (measured before the vault's fee, as the user deposited
// it) and in range once converted, the destination an address of this
// network. Anything else is unclaimable and needs a human. The event's
// amount is the net after the vault's deposit fee: exactly that much,
// converted, is issued; the fee is recorded and stays in the vault.
func (s *Service) classifyDeposit(c *chainState, ev DepositEvent) *Inbound {
	now := time.Now().Unix()
	d := &Inbound{Chain: c.ID, ChainName: c.Name, ID: ev.ID, EVMTx: strings.ToLower(ev.TxHash), From: HexAddress(ev.From), Token: HexAddress(ev.Token),
		To: strings.ToLower(strings.TrimSpace(ev.To)), Block: ev.Block, BlockHash: strings.ToLower(ev.BlockHash), LogIndex: ev.LogIndex, TokenAmount: "0", TokenFee: "0", Created: now, Updated: now}
	if ev.Amount != nil {
		d.TokenAmount = ev.Amount.String()
	}
	if ev.Fee != nil {
		d.TokenFee = ev.Fee.String()
	}
	t, ok := c.Token(ev.Token)
	if !ok {
		d.Status, d.Error = InboundUnclaimable, "token is not accepted on this chain"
		return d
	}
	d.TokenSymbol, d.Decimals = t.Symbol, t.Decimals
	units, ok := UnitsFromToken(ev.Amount, t.Decimals)
	gross := new(big.Int)
	if ev.Amount != nil {
		gross.Add(gross, ev.Amount)
	}
	if ev.Fee != nil {
		gross.Add(gross, ev.Fee)
	}
	switch {
	case ev.Amount == nil || ev.Amount.Sign() <= 0:
		d.Status, d.Error = InboundUnclaimable, "amount out of range"
	case gross.Cmp(t.MinDeposit) < 0:
		d.Status, d.Error = InboundUnclaimable, "below the minimum deposit"
	case !ok:
		d.Status, d.Error = InboundUnclaimable, "amount out of range"
	default:
		if _, err := wallet.DecodeAddress(s.cfg.Params.HRP, d.To); err != nil {
			d.Status, d.Error = InboundUnclaimable, "destination is not a valid address on this network"
			return d
		}
		d.Amount = units
		d.Status = InboundPending
		// Already paid by an issuance mined before this deposit was on record
		// here? Its deposit references say so (never its address and amount),
		// unless the issuance predates the vault and the id is one it uses as
		// well: then the deposit waits for an operator (refPredatesVault).
		ref := InboundRef{Chain: c.ID, ID: ev.ID}
		if sr, ok := s.st.Settled(ref); ok {
			if why := s.refPredatesVault(ref, sr.TxID, sr.Height); why != "" {
				d.Error = why
				s.pauseChain(c, why)
				return d
			}
			d.Status, d.IssueTx = InboundConfirmed, sr.TxID
			return d
		}
		// Held, not refused: a deposit above the token's cap, or in a token
		// whose decimals are refused, waits with the reason; its funds stay
		// in the vault (docs/BRIDGE.md, "Per-deposit cap").
		if why := depositHold(c, t, gross); why != "" {
			d.Error = why
		}
	}
	return d
}

// depositHold is why a deposit of gross token units of t waits for a human
// ("" when it does not): above the token's per-deposit cap, or in a token
// refused for its decimals.
func depositHold(c *chainState, t *Token, gross *big.Int) string {
	if t.MaxDeposit != nil && gross.Cmp(t.MaxDeposit) > 0 {
		return fmt.Sprintf("above the per-deposit cap of %s %s on %s: held for the guardians (the funds are in the vault; it is issued once the cap allows it, or refunded by hand)", FormatBigTokenAmount(t.MaxDeposit, t.Decimals), t.Symbol, c.Name)
	}
	c.mu.Lock()
	why := c.tokenErr[t.Address]
	c.mu.Unlock()
	return why
}

// depositBlocker is why a pending deposit cannot be issued now on this
// guardian ("" when it can): its token is not checked or refused, it is
// above the cap, or it was recorded with other decimals than configured.
func (s *Service) depositBlocker(c *chainState, d *Inbound) string {
	addr, err := ParseAddress(d.Token)
	if err != nil {
		return "bad token on record"
	}
	t, ok := c.Token(addr)
	if !ok {
		return "token is not accepted on this chain"
	}
	if t.Decimals != d.Decimals {
		return fmt.Sprintf("recorded with %d decimals, configured with %d", d.Decimals, t.Decimals)
	}
	if first := s.vaultFirstID(c); d.ID < first {
		return fmt.Sprintf("a deposit into an earlier vault of %s (the current vault %s numbers its deposits from %d): it is not issued from the current vault's chain; it needs the guardians (docs/BRIDGE.md, \"Replacing a vault\")", c.Name, HexAddress(c.Vault), first)
	}
	if why := c.tokenRefused(addr); why != "" {
		return why
	}
	gross := new(big.Int).Add(mustBig(d.TokenAmount), mustBig(d.TokenFee))
	if t.MaxDeposit != nil && gross.Cmp(t.MaxDeposit) > 0 {
		return depositHold(c, t, gross)
	}
	return ""
}

// ---- status ----

// TokenStatus is one accepted token of a chain in the public summary.
type TokenStatus struct {
	Symbol       string           `json:"symbol"`
	Address      string           `json:"address"`
	Decimals     int              `json:"decimals"`
	Mint         bool             `json:"mint"`                  // test token with a public mint: the page shows a faucet
	MinDeposit   string           `json:"min_deposit"`           // token units (decimal string)
	Reserve      string           `json:"reserve"`               // token units the vault holds (decimal string)
	ReserveUnits consensus.Amount `json:"reserve_units"`         // the same in uARG units (8-decimal dollars), rounded down
	Obligations  string           `json:"obligations"`           // token units that back issued uARG: the vault's obligations(token) (decimal string)
	Surplus      string           `json:"surplus"`               // token units above that: the fees the vault has collected, reserve − obligations (decimal string)
	MaxDeposit   string           `json:"max_deposit,omitempty"` // token units: larger deposits are held for the guardians (empty: no cap)
	// AvailableNow is the most one redemption in this token can be paid on
	// this chain right now, gross in token units: the vault's balance less
	// the releases in flight, within its per-release and 24-hour limits ("0"
	// while the vault is paused, empty until read). A larger burn waits.
	AvailableNow    string `json:"available_now,omitempty"`
	MaxRelease      string `json:"max_release,omitempty"`      // the vault's per-release limit, token units (empty: none)
	PeriodAvailable string `json:"period_available,omitempty"` // what the vault's 24-hour window still allows, token units (empty: no limit)
	Error           string `json:"error,omitempty"`            // why the token is refused (its decimals), or empty
	// MinRelease is the smallest payout in the token, gross token units
	// (decimal string): a redemption below it is never sent but held as
	// unreleasable ("below the minimum withdrawal"). The page asks for at
	// least this, and at least 1 uARG.
	MinRelease string `json:"min_release"`
}

// ChainStatus is one chain in the public summary.
type ChainStatus struct {
	Name          string `json:"name"`
	ChainID       uint64 `json:"chain_id"`
	Vault         string `json:"vault"`
	RPC           string `json:"rpc,omitempty"` // the chain's public_rpc, for the page to hand to wallets; absent when none is configured
	Explorer      string `json:"explorer"`
	NativeSymbol  string `json:"native_symbol"`
	Confirmations uint64 `json:"confirmations"`
	Head          uint64 `json:"head"`            // the chain's head at the last scan; progress of a deposit = head - block + 1
	DepositFeeBps uint16 `json:"deposit_fee_bps"` // the vault's fee on deposits, basis points (100 = 1%)
	ReleaseFeeBps uint16 `json:"release_fee_bps"` // the vault's fee on payouts, basis points
	Error         string `json:"error"`           // last scan error, or the leader's gas problem, or empty
	// Deposit addresses: the chain's DepositRouter (empty when the chain has
	// none, or when its check failed: then RouterError says why), and how
	// many deposit addresses the leader watches there.
	Router           string `json:"router"`
	RouterError      string `json:"router_error,omitempty"`
	DepositAddresses int    `json:"deposit_addresses"`
	Finality         string `json:"finality"`  // "finalized" or "depth"
	Final            uint64 `json:"final"`     // the last final block this guardian scanned: deposits at or below it can be issued
	Providers        int    `json:"providers"` // RPC providers that must agree
	// Paused says why deposits on this chain are not issued now (a reorg
	// alert waiting for an operator, the providers disagreeing, a pause of
	// everything), or is empty.
	Paused      string        `json:"paused,omitempty"`
	VaultPaused bool          `json:"vault_paused"`          // the vault's own pause: payouts wait
	GasBalance  string        `json:"gas_balance,omitempty"` // the gas key's balance in wei (leader)
	GasLow      bool          `json:"gas_low,omitempty"`     // below the chain's min_gas_balance: fund the gas key
	Tokens      []TokenStatus `json:"tokens"`
	// VaultSetup: the vault's finalized() is false, so its owner can still
	// change its tokens, fees and limits at once, without the timelock.
	VaultSetup bool `json:"vault_setup,omitempty"`
	// FirstDepositID is the first deposit id of the vault as this guardian
	// checked it (0 until then), and MaxDepositID the highest deposit id it
	// has on record for the chain from any vault: a vault that replaces
	// this one must be deployed with a firstDepositId above the largest
	// max_deposit_id of the guardians (docs/BRIDGE.md, "Replacing a vault").
	FirstDepositID uint64 `json:"first_deposit_id"`
	MaxDepositID   uint64 `json:"max_deposit_id"`
}

// Status is the public summary.
type Status struct {
	Network         string           `json:"network"`
	Asset           string           `json:"asset"`
	AssetSymbol     string           `json:"asset_symbol"`
	Custody         string           `json:"custody_address"`
	CustodyARG      consensus.Amount `json:"custody_arg"` // fee float
	Guardians       []string         `json:"guardians"`
	Threshold       int              `json:"threshold"`
	L1Confirmations uint32           `json:"l1_confirmations"`
	// ReleaseTiers: a burn of up to up_to uARG units waits confirmations
	// blocks (at least l1_confirmations); above the last tier it is held for
	// a manual release.
	ReleaseTiers  []Tier           `json:"l1_confirmation_tiers"`
	L1ReorgPause  uint32           `json:"l1_reorg_pause"` // an Argonmine reorganisation deeper than this pauses everything (0: never)
	L1Height      uint64           `json:"l1_height"`      // last block the guardians scanned on this chain; progress of a burn = l1_height - height + 1
	Chains        []ChainStatus    `json:"chains"`
	ReserveUnits  consensus.Amount `json:"reserve_units"` // every vault's reserve, summed, in uARG units
	BackingUnits  consensus.Amount `json:"backing_units"` // of that, what backs issued uARG (the vaults' obligations), in uARG units
	SurplusUnits  consensus.Amount `json:"surplus_units"` // and what the vaults have collected in fees, in uARG units
	Issued        consensus.Amount `json:"issued"`        // uARG issued and confirmed
	Redeemed      consensus.Amount `json:"redeemed"`      // uARG burned and paid out
	Deposits      int              `json:"deposits"`
	Redemptions   int              `json:"redemptions"`
	Pending       int              `json:"pending"`
	Held          int              `json:"held"`            // pending deposits that wait with a reason (a limit, a cap, a refused token): see their error
	Reorged       int              `json:"reorged"`         // deposits whose event left their chain before they were issued
	IssueLimit    consensus.Amount `json:"issue_limit"`     // most uARG one issuance creates (0: no limit)
	IssueDayLimit consensus.Amount `json:"issue_day_limit"` // most uARG this guardian signs over any 24 hours (0: no limit)
	IssuedDay     consensus.Amount `json:"issued_day"`      // uARG this guardian signed in the last 24 hours
	// Paused lists what this guardian will not do until an operator resumes
	// it: "l1" (every issuance and release, after a deep Argonmine
	// reorganisation) and "evm/<chain id>" (issuance on a chain, after a
	// deposit already issued left it).
	Paused     []Pause     `json:"paused"`
	Stuck      []StuckItem `json:"stuck"`                 // what has waited longer than -stuck-after, oldest first
	GasAddress string      `json:"gas_address,omitempty"` // the leader's gas key, which sends releases and sweeps
	Leader     bool        `json:"leader"`
	LastError  string      `json:"last_error"`
}

// Status returns the public summary. It reads what the last scan found and
// does not touch the network; the API serves it from a short cache
// (web.go). Every error in it is stripped of URLs and secrets.
func (s *Service) Status() Status { return s.statusOf(s.st.Inbounds(), s.st.Redemptions()) }

func (s *Service) statusOf(deps []Inbound, rs []Redemption) Status {
	s.mu.Lock()
	lastErr := s.lastErr
	s.mu.Unlock()
	l1Height, _, _ := s.st.Cursor("l1")
	st := Status{
		Network: s.cfg.Params.Name, Asset: s.asset.String(), AssetSymbol: s.cfg.AssetSymbol,
		Custody: s.cfg.Custody.Address(), Guardians: s.cfg.Custody.Signers(), Threshold: s.cfg.Threshold,
		L1Confirmations: s.cfg.L1Confirmations, ReleaseTiers: append([]Tier{}, s.cfg.ReleaseTiers...), L1ReorgPause: s.cfg.L1ReorgPause,
		L1Height: l1Height, Leader: s.cfg.Leader, LastError: publicErr(lastErr),
		IssueLimit: s.cfg.MaxIssue, IssueDayLimit: s.cfg.MaxIssueDay, IssuedDay: sumSigned(s.st.SignedSince(time.Now().Add(-issueDay).Unix()), nil),
		Paused: []Pause{}, Stuck: s.stuckItems(deps, rs),
	}
	if st.Stuck == nil {
		st.Stuck = []StuckItem{}
	}
	for _, p := range s.st.Pauses() {
		p.Reason = publicErr(p.Reason)
		st.Paused = append(st.Paused, p)
	}
	// Releases sent and not mined yet will take from the vaults.
	inFlight := map[string]*big.Int{}
	for _, r := range rs {
		if r.Status == RedemptionReleased {
			k := strconv.FormatUint(r.Chain, 10) + "/" + strings.ToLower(r.Token)
			if inFlight[k] == nil {
				inFlight[k] = new(big.Int)
			}
			inFlight[k].Add(inFlight[k], mustBig(r.TokenAmount))
		}
	}
	for _, c := range s.chains {
		final, _, _ := s.st.Cursor(EVMCursor(c.ID))
		if sender := gasSender(c); sender != ([20]byte{}) && s.cfg.Leader {
			st.GasAddress = HexAddress(sender)
		}
		paused := s.issuancePaused(c)
		firstID, maxID := s.vaultFirstID(c), s.st.MaxDepositID(c.ID)
		c.mu.Lock()
		head := c.latest
		if head == 0 {
			head = final
		}
		cs := ChainStatus{Name: c.Name, ChainID: c.ID, Vault: HexAddress(c.Vault), RPC: c.PublicRPC, Explorer: c.Explorer, NativeSymbol: c.NativeSymbol, Confirmations: c.Confirmations, Head: head,
			DepositFeeBps: c.depositBps, ReleaseFeeBps: c.releaseBps, Error: publicErr(c.scanErr),
			Finality: c.Finality, Final: final, Providers: len(c.providers), Paused: publicErr(paused),
			VaultSetup: c.setup, FirstDepositID: firstID, MaxDepositID: maxID}
		if cs.Error == "" {
			cs.Error = publicErr(c.gasErr)
		}
		if cs.Error == "" {
			cs.Error = publicErr(c.gasRoleErr)
		}
		if c.gasBalance != nil {
			cs.GasBalance = c.gasBalance.String()
			cs.GasLow = c.MinGasBalance != nil && c.gasBalance.Cmp(c.MinGasBalance) < 0
		}
		if c.HasRouter() {
			if c.routerErr != "" {
				cs.RouterError = publicErr(c.routerErr)
			} else {
				cs.Router = HexAddress(c.Router)
			}
		}
		c.mu.Unlock()
		for _, t := range c.Tokens {
			ts := TokenStatus{Symbol: t.Symbol, Address: HexAddress(t.Address), Decimals: t.Decimals, Mint: t.Mint, MinDeposit: t.MinDeposit.String(), Reserve: "0", Obligations: "0", Surplus: "0"}
			if t.MinRelease != nil {
				ts.MinRelease = t.MinRelease.String()
			}
			if t.MaxDeposit != nil {
				ts.MaxDeposit = t.MaxDeposit.String()
			}
			flight := inFlight[strconv.FormatUint(c.ID, 10)+"/"+HexAddress(t.Address)]
			if flight == nil {
				flight = new(big.Int)
			}
			if a := c.availableNow(t.Address, flight); a != nil {
				ts.AvailableNow = a.String()
			}
			c.mu.Lock()
			if why, done := c.tokenErr[t.Address]; done && why != "" {
				ts.Error = publicErr(why)
			}
			if lim := c.limits[t.Address]; lim != nil {
				cs.VaultPaused = cs.VaultPaused || lim.Paused
				if lim.MaxRelease != nil {
					ts.MaxRelease = lim.MaxRelease.String()
				}
				if lim.PeriodAvailable != nil {
					ts.PeriodAvailable = lim.PeriodAvailable.String()
				}
			}
			if r := c.reserves[t.Address]; r != nil {
				ts.Reserve = r.String()
				if u, ok := UnitsFromToken(r, t.Decimals); ok {
					ts.ReserveUnits = u
				}
				// Surplus is what the vault holds beyond its obligations:
				// the fees it has kept (and anything sent to it directly).
				o := c.obligations[t.Address]
				if o == nil {
					o = new(big.Int)
				}
				if o.Cmp(r) > 0 {
					o = r // cannot be: obligations are paid out of the reserve
				}
				surplus := new(big.Int).Sub(r, o)
				ts.Obligations, ts.Surplus = o.String(), surplus.String()
				if u, ok := UnitsFromToken(o, t.Decimals); ok {
					st.BackingUnits += u
				}
				if u, ok := UnitsFromToken(surplus, t.Decimals); ok {
					st.SurplusUnits += u
				}
			}
			c.mu.Unlock()
			st.ReserveUnits += ts.ReserveUnits
			cs.Tokens = append(cs.Tokens, ts)
		}
		cs.DepositAddresses = s.watchesOn(c.ID)
		st.Chains = append(st.Chains, cs)
	}
	for _, u := range s.st.UTXOs() {
		st.CustodyARG += u.Amount
	}
	st.Deposits = len(deps)
	for _, d := range deps {
		switch d.Status {
		case InboundPending, InboundIssued:
			st.Pending++
			if d.Status == InboundPending && d.Error != "" {
				st.Held++
			}
		case InboundConfirmed:
			st.Issued += d.Amount
		case InboundReorged:
			st.Reorged++
		}
	}
	st.Redemptions = len(rs)
	for _, r := range rs {
		switch r.Status {
		case RedemptionPending, RedemptionReleased:
			st.Pending++
		case RedemptionConfirmed:
			st.Redeemed += r.Amount
		}
	}
	return st
}

// Inbounds, Redemptions, Inbound and InboundByTx expose the state for the web page.
func (s *Service) Inbounds() []Inbound       { return s.st.Inbounds() }
func (s *Service) Redemptions() []Redemption { return s.st.Redemptions() }
func (s *Service) Inbound(chain, id uint64) (*Inbound, bool) {
	return s.st.GetInbound(chain, id)
}

// InboundByTx finds a deposit by its EVM transaction hash on any chain.
func (s *Service) InboundByTx(hash string) (*Inbound, bool) { return s.st.InboundByEVMTx(hash) }
