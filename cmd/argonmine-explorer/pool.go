package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
)

// The mining pool. It lives beside the solo work server and speaks the same
// protocol under the /pool prefix (GET /pool/api/work, POST
// /pool/api/work/submit): a pool job is a block template whose coinbase pays
// the pool's address, served with the miner's share target in place of the
// block target (the block target travels beside it as block_target). A hash
// at or below the share target is a share, worth the expected number of
// hashes it took (its difficulty); one at or below the block target is also
// a block, handed to the node like a solo one.
//
// Accounting is per round: the shares sent between two pool blocks. When a
// block matures (consensus.CoinbaseMaturity) its reward less the pool fee is
// split over the round's shares and credited to each miner's balance; when
// a balance reaches the minimum payout it is paid from the pool wallet, many
// miners in one transaction. A block that ends up off the main chain pays
// nothing, and its round's shares are carried into the next round so that
// nobody's work is lost to an orphan.
//
// Everything the pool owes and has paid is in one state file, saved on
// every block and payout and a few seconds after any share, and published
// whole at /api/pool/ledger so that anyone can check it against the chain.
//
// Share difficulty is tuned per miner (vardiff) towards one share every
// poolShareInterval, so a phone and a server both report their work at the
// same rate and are both credited exactly for the hashes they did.

const (
	poolTag           = "argonmine-pool" // coinbase witness of every pool job
	poolShareInterval = 15 * time.Second // wanted time between a miner's shares
	poolStartDiff     = 2000             // a new miner's hashes per share
	poolMinDiff       = 500              // floor: a phone at 50 H/s reports every 10 s
	poolMaxDiff       = uint64(1) << 40  // ceiling (never above the block difficulty anyway)
	poolAdjustEvery   = 30 * time.Second // vardiff retunes at most this often
	poolRateWindow    = 10 * time.Minute // hashrate and "active" window
	poolOrphanDepth   = 6                // a pending block missing from the chain this deep is orphaned
	poolTick          = 30 * time.Second // maturity and payout interval
	poolSaveEvery     = 5 * time.Second  // shares are persisted this often
	poolMaxRecipients = 200              // miners paid per transaction
	poolFeeRate       = 1                // payout transaction fee, units per byte
	poolFeeReserve    = 1_000_000        // 0.01 ARG kept back for the payout's own fee
	poolRoundsKept    = 1000             // rounds and payouts remembered in the state file
	poolDownText      = "the pool is not available"
)

// poolConfig enables the pool.
type poolConfig struct {
	Address   string           // the pool wallet's payout address
	FeeBps    int              // pool fee in basis points (100 = 1%)
	MinPayout consensus.Amount // a balance is paid once it reaches this
	StatePath string           // ledger file ("" = memory only)
	Wallet    *wallet.Wallet   // signs payouts; nil pays nobody (tests)
}

// poolRound is the shares between two pool blocks, and the block that ended it.
type poolRound struct {
	Height      uint32            `json:"height"`
	BlockHash   string            `json:"block_hash"`
	Reward      consensus.Amount  `json:"reward"` // the coinbase's payment to the pool
	FoundBy     string            `json:"found_by"`
	Shares      map[string]uint64 `json:"shares"` // weighted: expected hashes
	TotalShares uint64            `json:"total_shares"`
	Found       int64             `json:"found"`
	Status      string            `json:"status"`        // pending | matured | orphaned
	Fee         consensus.Amount  `json:"fee,omitempty"` // the pool's cut, once matured
}

// poolPayout is one payout transaction.
type poolPayout struct {
	TxID    string                      `json:"txid"`
	Time    int64                       `json:"time"`
	Amounts map[string]consensus.Amount `json:"amounts"`
	Total   consensus.Amount            `json:"total"`
}

// poolState is the ledger, persisted whole.
type poolState struct {
	Balances       map[string]consensus.Amount `json:"balances"` // owed, matured, not yet paid
	Paid           map[string]consensus.Amount `json:"paid"`     // paid so far, per miner
	Round          map[string]uint64           `json:"round"`    // the current round's shares
	RoundStart     int64                       `json:"round_start"`
	Rounds         []poolRound                 `json:"rounds"`
	Payouts        []poolPayout                `json:"payouts"`
	TotalPaid      consensus.Amount            `json:"total_paid"`
	Fees           consensus.Amount            `json:"fees"` // kept by the pool from matured rounds
	BlocksFound    int                         `json:"blocks_found"`
	SharesAccepted uint64                      `json:"shares_accepted"`
	SharesRejected uint64                      `json:"shares_rejected"`
}

type poolRec struct {
	t time.Time
	w uint64
}

// poolMiner is one address's vardiff and rate bookkeeping (memory only).
type poolMiner struct {
	diff        uint64
	lastAdjust  time.Time
	sharesSince uint64
	lastShare   time.Time
	recent      []poolRec
}

type pool struct {
	ws   *workServer
	logf func(string, ...any)

	mu      sync.Mutex
	cfg     poolConfig
	pay     consensus.TxOut // the pool address, decoded
	on      bool
	miners  map[string]*poolMiner
	state   poolState
	recent  []poolRec // every share in the window, for the pool hashrate
	dirty   bool
	lastErr string
	paying  bool // a payout is being built and sent
}

var big2_256 = new(big.Int).Lsh(big.NewInt(1), 256)

// newPool returns a disabled pool; enable turns it on.
func newPool(ws *workServer) *pool {
	p := &pool{ws: ws, logf: ws.logf, miners: map[string]*poolMiner{}}
	p.state = poolState{Balances: map[string]consensus.Amount{}, Paid: map[string]consensus.Amount{}, Round: map[string]uint64{}, RoundStart: time.Now().Unix()}
	return p
}

// enable configures the pool and loads its ledger.
func (p *pool) enable(cfg poolConfig) error {
	out, err := wallet.DecodeAddress(p.ws.params.HRP, cfg.Address)
	if err != nil {
		return fmt.Errorf("pool: payout address: %w", err)
	}
	if cfg.FeeBps < 0 || cfg.FeeBps > 5000 {
		return errors.New("pool: the fee must be between 0 and 50 percent")
	}
	if cfg.MinPayout < wallet.DustLimit {
		cfg.MinPayout = wallet.DustLimit
	}
	st := poolState{Balances: map[string]consensus.Amount{}, Paid: map[string]consensus.Amount{}, Round: map[string]uint64{}, RoundStart: time.Now().Unix()}
	if cfg.StatePath != "" {
		if b, err := os.ReadFile(cfg.StatePath); err == nil {
			if err := json.Unmarshal(b, &st); err != nil {
				return fmt.Errorf("pool: ledger %s: %w", cfg.StatePath, err)
			}
			if st.Balances == nil {
				st.Balances = map[string]consensus.Amount{}
			}
			if st.Paid == nil {
				st.Paid = map[string]consensus.Amount{}
			}
			if st.Round == nil {
				st.Round = map[string]uint64{}
			}
			if st.RoundStart == 0 {
				st.RoundStart = time.Now().Unix()
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pool: ledger %s: %w", cfg.StatePath, err)
		}
	}
	p.mu.Lock()
	p.cfg, p.pay, p.state, p.on = cfg, out, st, true
	p.mu.Unlock()
	return nil
}

func (p *pool) enabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.on
}

// run persists shares and runs the maturity and payout tick until stop closes.
func (p *pool) run(stop <-chan struct{}) {
	if !p.enabled() {
		return
	}
	save := time.NewTicker(poolSaveEvery)
	tick := time.NewTicker(poolTick)
	defer save.Stop()
	defer tick.Stop()
	p.tick()
	for {
		select {
		case <-stop:
			p.save()
			return
		case <-save.C:
			p.saveIfDirty()
		case <-tick.C:
			p.tick()
		}
	}
}

// ---- jobs and shares ----

// targetFor turns a share difficulty (expected hashes) into a target, never
// harder than the block target.
func targetFor(diff uint64, blockTarget *big.Int) *big.Int {
	if diff == 0 {
		diff = 1
	}
	t := new(big.Int).Div(big2_256, new(big.Int).SetUint64(diff))
	if t.Cmp(blockTarget) < 0 {
		return new(big.Int).Set(blockTarget)
	}
	return t
}

// diffOf is the expected hashes to meet a target.
func diffOf(target *big.Int) uint64 {
	if target.Sign() <= 0 {
		return poolMaxDiff
	}
	d := new(big.Int).Div(big2_256, target)
	if !d.IsUint64() || d.Uint64() > poolMaxDiff {
		return poolMaxDiff
	}
	if d.Uint64() < 1 {
		return 1
	}
	return d.Uint64()
}

func (p *pool) minerLocked(addr string, now time.Time) *poolMiner {
	m := p.miners[addr]
	if m == nil {
		m = &poolMiner{diff: poolStartDiff, lastAdjust: now}
		p.miners[addr] = m
	}
	return m
}

// adjustLocked retunes a miner's share difficulty towards one share per
// poolShareInterval, by at most 4x either way per step.
func (p *pool) adjustLocked(m *poolMiner, now time.Time) {
	elapsed := now.Sub(m.lastAdjust)
	if elapsed < poolAdjustEvery {
		return
	}
	var factor float64
	if m.sharesSince == 0 {
		if elapsed < 4*poolShareInterval {
			return // not enough evidence yet
		}
		factor = 0.5
	} else {
		observed := elapsed.Seconds() / float64(m.sharesSince)
		factor = poolShareInterval.Seconds() / observed
	}
	m.lastAdjust, m.sharesSince = now, 0
	if factor > 4 {
		factor = 4
	}
	if factor < 0.25 {
		factor = 0.25
	}
	if factor > 0.8 && factor < 1.25 {
		return // close enough; no oscillation
	}
	d := uint64(float64(m.diff) * factor)
	if d < poolMinDiff {
		d = poolMinDiff
	}
	if d > poolMaxDiff {
		d = poolMaxDiff
	}
	m.diff = d
}

// Job builds a pool job for miner: the coinbase pays the pool, the target
// is the miner's share target.
func (p *pool) Job(miner string) (*workJobJSON, error) {
	if !p.enabled() {
		return nil, errors.New(poolDownText)
	}
	if _, err := wallet.DecodeAddress(p.ws.params.HRP, miner); err != nil {
		return nil, errWorkInput{"invalid payout address"}
	}
	t, err := p.ws.template()
	if err != nil {
		p.ws.noteErr("getblocktemplate", err)
		return nil, errors.New(workNodeDownText)
	}
	value, treasury, payTreasury, err := p.ws.minerValue(t)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	pay := p.pay
	now := time.Now()
	m := p.minerLocked(miner, now)
	p.adjustLocked(m, now)
	diff := m.diff
	p.mu.Unlock()

	outs := []consensus.TxOut{{Amount: value, Type: pay.Type, Payload: pay.Payload}}
	if payTreasury {
		outs = append(outs, treasury)
	}
	target := targetFor(diff, t.target)
	if target.Cmp(t.target) == 0 {
		diff = diffOf(t.target) // the share is a block: worth the block's difficulty
	}
	return p.ws.jobFor(t, miner, outs, poolTag, &poolShare{target: target, diff: diff})
}

// credit records an accepted share of weight w for addr.
func (p *pool) credit(addr string, w uint64) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Round[addr] += w
	p.state.SharesAccepted++
	m := p.minerLocked(addr, now)
	m.sharesSince++
	m.lastShare = now
	m.recent = trimRecs(append(m.recent, poolRec{now, w}), now)
	p.recent = trimRecs(append(p.recent, poolRec{now, w}), now)
	p.dirty = true
}

func (p *pool) rejectedShare() {
	p.mu.Lock()
	p.state.SharesRejected++
	p.dirty = true
	p.mu.Unlock()
}

// blockFound closes the round on a pool block the node accepted.
func (p *pool) blockFound(j *workJob, hash consensus.Hash, foundBy string) {
	var reward consensus.Amount
	if cb, err := consensus.DecodeTx(j.coinbase); err == nil && len(cb.Outputs) > 0 {
		reward = cb.Outputs[0].Amount
	}
	p.mu.Lock()
	shares := make(map[string]uint64, len(p.state.Round))
	var total uint64
	for a, n := range p.state.Round {
		shares[a] = n
		total += n
	}
	p.state.Rounds = append(p.state.Rounds, poolRound{
		Height: j.tmpl.height, BlockHash: hash.String(), Reward: reward, FoundBy: foundBy,
		Shares: shares, TotalShares: total, Found: time.Now().Unix(), Status: "pending",
	})
	if len(p.state.Rounds) > poolRoundsKept {
		p.state.Rounds = append([]poolRound{}, p.state.Rounds[len(p.state.Rounds)-poolRoundsKept:]...)
	}
	p.state.BlocksFound++
	p.state.Round = map[string]uint64{}
	p.state.RoundStart = time.Now().Unix()
	p.dirty = true
	p.saveLocked()
	p.mu.Unlock()
	p.logf("pool: block %s at height %d pays %s to the pool; %d weighted shares in the round", hash, j.tmpl.height, wallet.FormatAmount(reward), total)
}

func trimRecs(rs []poolRec, now time.Time) []poolRec {
	cut := now.Add(-poolRateWindow)
	i := 0
	for i < len(rs) && rs[i].t.Before(cut) {
		i++
	}
	if len(rs)-i > 100_000 {
		i = len(rs) - 100_000
	}
	return rs[i:]
}

// rateOf is the hashrate the window's shares attest to.
func rateOf(rs []poolRec, now time.Time) float64 {
	if len(rs) == 0 {
		return 0
	}
	cut := now.Add(-poolRateWindow)
	var total uint64
	first := now
	for _, r := range rs {
		if r.t.Before(cut) {
			continue
		}
		total += r.w
		if r.t.Before(first) {
			first = r.t
		}
	}
	span := now.Sub(first).Seconds()
	if span < 60 {
		span = 60
	}
	return float64(total) / span
}

// ---- maturity and payouts ----

// tick matures rounds, detects orphans and pays balances due. Errors are
// kept for the status page and retried next tick.
func (p *pool) tick() {
	if !p.enabled() || p.ws.client == nil {
		return
	}
	if err := p.mature(); err != nil {
		p.setErr(err)
		return
	}
	if err := p.payout(); err != nil {
		p.setErr(err)
		return
	}
	p.mu.Lock()
	p.lastErr = ""
	p.mu.Unlock()
}

func (p *pool) setErr(err error) {
	p.mu.Lock()
	changed := p.lastErr != err.Error()
	p.lastErr = err.Error()
	p.mu.Unlock()
	if changed {
		p.logf("pool: %v", err)
	}
}

// mature checks every pending round against the chain.
func (p *pool) mature() error {
	info, err := p.ws.client.ChainInfo()
	if err != nil {
		return fmt.Errorf("getblockchaininfo: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.state.Rounds {
		r := &p.state.Rounds[i]
		if r.Status != "pending" {
			continue
		}
		h, err := p.ws.client.BlockHash(r.Height)
		if err != nil || h != r.BlockHash {
			if info.Blocks > r.Height+poolOrphanDepth {
				// Off the main chain: the reward never existed. The round's
				// work counts towards the next block instead.
				r.Status = "orphaned"
				for a, n := range r.Shares {
					p.state.Round[a] += n
				}
				p.dirty = true
				p.logf("pool: block %s at height %d is not on the chain; its %d shares move to the current round", r.BlockHash, r.Height, r.TotalShares)
			}
			continue
		}
		if info.Blocks+1-r.Height < consensus.CoinbaseMaturity {
			continue
		}
		fee := r.Reward * consensus.Amount(p.cfg.FeeBps) / 10_000
		net := r.Reward - fee
		var given consensus.Amount
		if r.TotalShares > 0 {
			for addr, n := range r.Shares {
				amt := consensus.Amount(new(big.Int).Div(new(big.Int).Mul(big.NewInt(int64(net)), new(big.Int).SetUint64(n)), new(big.Int).SetUint64(r.TotalShares)).Int64())
				p.state.Balances[addr] += amt
				given += amt
			}
		}
		r.Fee = r.Reward - given // the fee plus rounding dust
		r.Status = "matured"
		p.state.Fees += r.Fee
		p.dirty = true
		p.logf("pool: block at height %d matured: %s shared among %d miners, fee %s", r.Height, wallet.FormatAmount(given), len(r.Shares), wallet.FormatAmount(r.Fee))
	}
	p.saveIfDirtyLocked()
	return nil
}

// payout sends what is due, as far as the wallet's spendable coins reach.
// Balances are deducted and saved before the transaction goes out, so a
// crash in between can lose a record but never pay twice.
func (p *pool) payout() error {
	p.mu.Lock()
	w := p.cfg.Wallet
	if w == nil || p.paying {
		p.mu.Unlock()
		return nil
	}
	var due []wallet.Recipient
	for addr, bal := range p.state.Balances {
		if bal >= p.cfg.MinPayout && bal >= wallet.DustLimit {
			due = append(due, wallet.Recipient{Address: addr, Amount: bal})
		}
	}
	p.mu.Unlock()
	if len(due) == 0 {
		return nil
	}
	sort.Slice(due, func(i, j int) bool {
		return due[i].Amount > due[j].Amount || due[i].Amount == due[j].Amount && due[i].Address < due[j].Address
	})
	if len(due) > poolMaxRecipients {
		due = due[:poolMaxRecipients]
	}
	tip, err := w.Sync()
	if err != nil {
		return fmt.Errorf("wallet sync: %w", err)
	}
	spendable, _, _ := w.Balance(tip)
	budget := spendable - poolFeeReserve
	var recips []wallet.Recipient
	var total consensus.Amount
	for _, r := range due {
		if total+r.Amount > budget {
			break
		}
		recips = append(recips, r)
		total += r.Amount
	}
	if len(recips) == 0 {
		return nil // rewards still maturing in the wallet
	}

	p.mu.Lock()
	p.paying = true
	for _, r := range recips {
		p.state.Balances[r.Address] -= r.Amount
		if p.state.Balances[r.Address] <= 0 {
			delete(p.state.Balances, r.Address)
		}
	}
	p.dirty = true
	p.saveLocked()
	p.mu.Unlock()

	txid, err := w.SendMany(recips, poolFeeRate)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.paying = false
	if err != nil {
		for _, r := range recips {
			p.state.Balances[r.Address] += r.Amount
		}
		p.dirty = true
		p.saveLocked()
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil
		}
		return fmt.Errorf("payout: %w", err)
	}
	pay := poolPayout{TxID: txid, Time: time.Now().Unix(), Amounts: map[string]consensus.Amount{}, Total: total}
	for _, r := range recips {
		pay.Amounts[r.Address] = r.Amount
		p.state.Paid[r.Address] += r.Amount
	}
	p.state.Payouts = append(p.state.Payouts, pay)
	if len(p.state.Payouts) > poolRoundsKept {
		p.state.Payouts = append([]poolPayout{}, p.state.Payouts[len(p.state.Payouts)-poolRoundsKept:]...)
	}
	p.state.TotalPaid += total
	p.dirty = true
	p.saveLocked()
	p.logf("pool: paid %s to %d miners in %s", wallet.FormatAmount(total), len(recips), txid)
	return nil
}

// ---- persistence ----

func (p *pool) save() {
	p.mu.Lock()
	p.saveLocked()
	p.mu.Unlock()
}

func (p *pool) saveIfDirty() {
	p.mu.Lock()
	p.saveIfDirtyLocked()
	p.mu.Unlock()
}

func (p *pool) saveIfDirtyLocked() {
	if p.dirty {
		p.saveLocked()
	}
}

// saveLocked writes the ledger atomically (temp file, then rename).
func (p *pool) saveLocked() {
	p.dirty = false
	if p.cfg.StatePath == "" {
		return
	}
	b, err := json.MarshalIndent(&p.state, "", " ")
	if err != nil {
		return
	}
	tmp := p.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		p.logf("pool: ledger: %v", err)
		return
	}
	if err := os.Rename(tmp, p.cfg.StatePath); err != nil {
		p.logf("pool: ledger: %v", err)
	}
}

// ---- views ----

type poolStats struct {
	Enabled        bool             `json:"enabled"`
	Network        string           `json:"network"`
	Address        string           `json:"address"`
	FeePercent     float64          `json:"fee_percent"`
	MinPayout      consensus.Amount `json:"min_payout"`
	ShareInterval  int              `json:"share_interval"`
	Hashrate       float64          `json:"hashrate"`
	Miners         int              `json:"miners"`
	RoundShares    uint64           `json:"round_shares"`
	RoundStart     int64            `json:"round_start"`
	BlocksFound    int              `json:"blocks_found"`
	Pending        int              `json:"pending"`
	TotalPaid      consensus.Amount `json:"total_paid"`
	Owed           consensus.Amount `json:"owed"`
	Fees           consensus.Amount `json:"fees"`
	SharesAccepted uint64           `json:"shares_accepted"`
	SharesRejected uint64           `json:"shares_rejected"`
	Rounds         []poolRound      `json:"rounds"`
	Payouts        []poolPayout     `json:"payouts"`
	Error          string           `json:"error,omitempty"`
}

// stats is the public pool status, with the last 20 rounds and payouts.
func (p *pool) stats() poolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	s := poolStats{Enabled: p.on, Network: p.ws.params.Name}
	if !p.on {
		return s
	}
	s.Address, s.FeePercent, s.MinPayout, s.ShareInterval = p.cfg.Address, float64(p.cfg.FeeBps)/100, p.cfg.MinPayout, int(poolShareInterval/time.Second)
	s.Hashrate = rateOf(p.recent, now)
	for _, m := range p.miners {
		if now.Sub(m.lastShare) < poolRateWindow {
			s.Miners++
		}
	}
	for _, n := range p.state.Round {
		s.RoundShares += n
	}
	s.RoundStart, s.BlocksFound, s.TotalPaid, s.Fees = p.state.RoundStart, p.state.BlocksFound, p.state.TotalPaid, p.state.Fees
	s.SharesAccepted, s.SharesRejected, s.Error = p.state.SharesAccepted, p.state.SharesRejected, p.lastErr
	for _, b := range p.state.Balances {
		s.Owed += b
	}
	for _, r := range p.state.Rounds {
		if r.Status == "pending" {
			s.Pending++
		}
	}
	from := len(p.state.Rounds) - 20
	if from < 0 {
		from = 0
	}
	s.Rounds = make([]poolRound, 0, 20)
	for _, r := range p.state.Rounds[from:] {
		r.Shares = nil
		s.Rounds = append(s.Rounds, r)
	}
	from = len(p.state.Payouts) - 20
	if from < 0 {
		from = 0
	}
	s.Payouts = append([]poolPayout{}, p.state.Payouts[from:]...)
	return s
}

type poolMinerStats struct {
	Address     string           `json:"address"`
	Hashrate    float64          `json:"hashrate"`
	ShareDiff   uint64           `json:"share_diff"`
	RoundShares uint64           `json:"round_shares"`
	RoundTotal  uint64           `json:"round_total"`
	Balance     consensus.Amount `json:"balance"`
	Paid        consensus.Amount `json:"paid"`
	Pending     consensus.Amount `json:"pending"` // this miner's estimated part of the blocks still maturing
	LastShare   int64            `json:"last_share,omitempty"`
}

// miner is one address's view.
func (p *pool) miner(addr string) poolMinerStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := poolMinerStats{Address: addr, ShareDiff: poolStartDiff}
	if !p.on {
		return m
	}
	m.RoundShares, m.Balance, m.Paid = p.state.Round[addr], p.state.Balances[addr], p.state.Paid[addr]
	for _, n := range p.state.Round {
		m.RoundTotal += n
	}
	if ms := p.miners[addr]; ms != nil {
		m.Hashrate = rateOf(ms.recent, time.Now())
		m.ShareDiff = ms.diff
		if !ms.lastShare.IsZero() {
			m.LastShare = ms.lastShare.Unix()
		}
	}
	for _, r := range p.state.Rounds {
		if r.Status == "pending" && r.TotalShares > 0 {
			if n := r.Shares[addr]; n > 0 {
				net := r.Reward - r.Reward*consensus.Amount(p.cfg.FeeBps)/10_000
				m.Pending += consensus.Amount(new(big.Int).Div(new(big.Int).Mul(big.NewInt(int64(net)), new(big.Int).SetUint64(n)), new(big.Int).SetUint64(r.TotalShares)).Int64())
			}
		}
	}
	return m
}

// ledger is the whole state file, for anyone who wants to audit it.
func (p *pool) ledger() poolState {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, _ := json.Marshal(&p.state)
	var out poolState
	json.Unmarshal(b, &out)
	return out
}

// ---- HTTP ----

// apiPoolWork serves GET /pool/api/work?address=<miner>.
func (s *server) apiPoolWork(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	miner := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("address")))
	job, err := s.work.pool.Job(miner)
	if err != nil {
		code := http.StatusServiceUnavailable
		var in errWorkInput
		if errors.As(err, &in) {
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}
	if s.miners != nil {
		s.miners.note(miner, s.clientIP(r), time.Now())
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(job)
}

func writeJSONCORS(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// apiPoolStats serves GET /api/pool/stats.
func (s *server) apiPoolStats(w http.ResponseWriter, r *http.Request) {
	writeJSONCORS(w, s.work.pool.stats())
}

// apiPoolMiner serves GET /api/pool/miner/<address>.
func (s *server) apiPoolMiner(w http.ResponseWriter, r *http.Request) {
	addr := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/pool/miner/"))
	if _, err := wallet.DecodeAddress(s.params.HRP, addr); err != nil {
		http.Error(w, "invalid address", http.StatusBadRequest)
		return
	}
	writeJSONCORS(w, s.work.pool.miner(addr))
}

// apiPoolLedger serves GET /api/pool/ledger: the whole ledger.
func (s *server) apiPoolLedger(w http.ResponseWriter, r *http.Request) {
	if !s.work.pool.enabled() {
		http.Error(w, poolDownText, http.StatusNotFound)
		return
	}
	writeJSONCORS(w, s.work.pool.ledger())
}

// poolPage serves GET /pool.
func (s *server) poolPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/pool" {
		s.notFound(w, r.URL.Path)
		return
	}
	st := s.work.pool.stats()
	s.render(w, "pool", "Mining pool", map[string]any{"Stats": st, "StatsJSON": mustJSON(st), "Maturity": consensus.CoinbaseMaturity})
}

// mustJSON renders v for a <script> element: the template inserts it as
// is, and no closing tag can form inside it.
func mustJSON(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return template.JS(strings.ReplaceAll(string(b), "</", "<\\/"))
}

// isPoolAddress reports whether addr is the pool's payout address.
func (s *server) isPoolAddress(addr string) bool {
	p := s.work.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.on && p.cfg.Address == addr
}
