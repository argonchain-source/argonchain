// Package mempool holds valid, unconfirmed transactions and selects them for
// block templates. Everything here is policy (SPEC §10); a node with a
// different mempool policy still agrees on the chain.
package mempool

import (
	"container/heap"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
)

// Config holds the policy knobs.
type Config struct {
	MinFeeRate consensus.Amount // units per serialized byte
	DustLimit  consensus.Amount // minimum P2PK output amount
	MaxBytes   int              // pool size cap in serialized bytes
	MaxTxSize  int              // standardness size cap
	// AssetOutputFee is added to the minimum relay fee for every asset
	// output beyond FreeAssetOutputs in one transaction. Asset outputs have
	// no dust limit (asset units have no ARG value), so without it an asset
	// UTXO would cost only its bytes, several times less than an ARG one.
	AssetOutputFee consensus.Amount
}

// DefaultConfig is SPEC §10.
func DefaultConfig() Config {
	return Config{MinFeeRate: 1, DustLimit: 546, MaxBytes: 300 << 20, MaxTxSize: consensus.MaxTxSize, AssetOutputFee: 546}
}

const (
	maxReplacements = 100             // conflicting txs (with descendants) one tx may replace
	rejectTTL       = 2 * time.Minute // how long a rejected txid is remembered
	rejectCacheCap  = 20_000

	// FreeAssetOutputs asset outputs per transaction pay no AssetOutputFee:
	// a payment and its change, or one issuance output, cost what they did.
	FreeAssetOutputs = 2

	// Chains of unconfirmed transactions (Bitcoin's defaults): a transaction
	// with all its unconfirmed ancestors, and every ancestor with all its
	// unconfirmed descendants, at most this many transactions and bytes.
	MaxChainTxs   = 25
	MaxChainBytes = 101_000
)

var (
	ErrAlreadyInPool    = errors.New("mempool: transaction already in pool")
	ErrCoinbase         = errors.New("mempool: coinbase transactions are not relayed")
	ErrTooLarge         = errors.New("mempool: transaction exceeds policy size")
	ErrInsufficientFee  = errors.New("mempool: fee below minimum relay fee")
	ErrDust             = errors.New("mempool: output below dust limit")
	ErrConflict         = errors.New("mempool: conflicts with pool transaction and does not pay enough to replace it")
	ErrTooManyConflicts = errors.New("mempool: replacement would evict too many transactions")
	ErrPoolFull         = errors.New("mempool: pool full and fee rate too low to evict others")
	ErrRecentlyRejected = errors.New("mempool: recently rejected")
	ErrTooLongChain     = errors.New("mempool: too many unconfirmed ancestors or descendants")
)

// MinRelayFee is the smallest fee cfg relays tx for: MinFeeRate per
// serialized byte plus AssetOutputFee per asset output beyond
// FreeAssetOutputs. Wallets building asset transactions should pay at least
// this much.
func MinRelayFee(cfg Config, tx *consensus.Tx) consensus.Amount {
	return cfg.MinFeeRate*consensus.Amount(tx.SerializedSize()) + AssetSurcharge(cfg, tx)
}

// AssetSurcharge is the part of MinRelayFee owed for asset outputs: a
// wallet paying feeRate per byte adds this on top.
func AssetSurcharge(cfg Config, tx *consensus.Tx) consensus.Amount {
	n := 0
	for i := range tx.Outputs {
		if tx.Outputs[i].Asset != consensus.NativeAsset {
			n++
		}
	}
	if n <= FreeAssetOutputs {
		return 0
	}
	return cfg.AssetOutputFee * consensus.Amount(n-FreeAssetOutputs)
}

// Entry is a transaction in the pool with its policy metadata.
type Entry struct {
	Tx    *consensus.Tx
	TxID  consensus.Hash
	Fee   consensus.Amount
	Size  int
	Added time.Time
	// parents are pool transactions whose outputs this one spends; children
	// are pool transactions spending this one's outputs.
	parents  map[consensus.Hash]struct{}
	children map[consensus.Hash]struct{}
	pending  int // scratch for kahn: parents not yet placed (pool lock held)
}

// FeeRate in units per byte as a float, for ordering only.
func (e *Entry) FeeRate() float64 { return float64(e.Fee) / float64(e.Size) }

// Pool is the mempool.
type Pool struct {
	mu     sync.Mutex
	ch     *chain.Chain
	cfg    Config
	txs    map[consensus.Hash]*Entry
	spends map[consensus.OutPoint]consensus.Hash // outpoint -> spending txid
	bytes  int
	// rejected remembers failures that do not depend on the witness, by
	// txid: every transaction with that txid fails the same way. Failures
	// that depend on the witness (signature, fee rate, size) are remembered
	// by wtxid in rejectedW, so a copy with a garbage witness cannot block
	// the genuine transaction.
	rejected  map[consensus.Hash]rejection
	rejectedW map[consensus.Hash]rejection
	gen       uint64 // bumped on every change to the set of transactions
	// lastTip is the chain tip the pool was last brought up to date with
	// (by a block-by-block update or a full Revalidate).
	lastTip *chain.Node
	// sel is the last Select result; it depends only on the pool's
	// contents and maxBytes.
	sel selectCache
	// fullRevalidations counts full Revalidate passes (for tests).
	fullRevalidations int
}

type selectCache struct {
	valid        bool
	gen          uint64
	count, bytes int
	maxBytes     int
	txs          []*consensus.Tx
	fees         consensus.Amount
}

// maxIncrementalBlocks: a new tip up to this many blocks ahead of the last
// one seen, on top of it, is applied block by block; anything else (a
// reorganisation, a disconnect, a long jump) gets a full Revalidate.
const maxIncrementalBlocks = 16

type rejection struct {
	err  error
	when time.Time
}

// New creates a pool bound to a chain and subscribes to tip changes.
func New(ch *chain.Chain, cfg Config) *Pool {
	p := &Pool{ch: ch, cfg: cfg, txs: map[consensus.Hash]*Entry{}, spends: map[consensus.OutPoint]consensus.Hash{},
		rejected: map[consensus.Hash]rejection{}, rejectedW: map[consensus.Hash]rejection{}, lastTip: ch.Tip()}
	ch.Subscribe(p.onTip)
	return p
}

// poolView resolves outpoints against pool outputs first, then the chain.
// Outpoints spent by pool transactions (other than those in ignore) are
// reported as missing.
type poolView struct {
	p          *Pool
	nextHeight uint32
	ignore     map[consensus.Hash]struct{}
}

func (v poolView) Lookup(op consensus.OutPoint) (consensus.Coin, bool) {
	if spender, ok := v.p.spends[op]; ok {
		if _, ig := v.ignore[spender]; !ig {
			return consensus.Coin{}, false
		}
	}
	if e, ok := v.p.txs[op.TxID]; ok {
		if _, ig := v.ignore[op.TxID]; ig {
			return consensus.Coin{}, false
		}
		if int(op.Index) >= len(e.Tx.Outputs) || e.Tx.Outputs[op.Index].Type != consensus.OutputTypeP2PK {
			return consensus.Coin{}, false
		}
		return consensus.Coin{Out: e.Tx.Outputs[op.Index], Height: v.nextHeight, Coinbase: false}, true
	}
	return v.p.ch.LookupCoin(op)
}

// RecentlyRejected reports whether txid failed acceptance within rejectTTL
// for a reason that does not depend on its witness, so relays can skip
// fetching it again. A witness-dependent failure (one bad copy) does not
// count: another peer may hold the valid transaction.
func (p *Pool) RecentlyRejected(txid consensus.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.rejected[txid]
	return ok && time.Since(r.when) < rejectTTL
}

func remember(m map[consensus.Hash]rejection, key consensus.Hash, err error) map[consensus.Hash]rejection {
	if len(m) >= rejectCacheCap {
		now := time.Now()
		for id, r := range m {
			if now.Sub(r.when) > rejectTTL {
				delete(m, id)
			}
		}
		if len(m) >= rejectCacheCap {
			m = map[consensus.Hash]rejection{}
		}
	}
	m[key] = rejection{err: err, when: time.Now()}
	return m
}

// rejectTx caches a failure that every transaction with this txid shares.
func (p *Pool) rejectTx(txid consensus.Hash, err error) error {
	p.rejected = remember(p.rejected, txid, err)
	return err
}

// rejectWitness caches a failure of this exact serialization only.
func (p *Pool) rejectWitness(wtxid consensus.Hash, err error) error {
	p.rejectedW = remember(p.rejectedW, wtxid, err)
	return err
}

// Accept validates tx and adds it to the pool, replacing lower-fee conflicts
// when allowed. Cheap checks and the fee check run before signature
// verification so that a peer cannot buy free CPU with zero-fee spam.
func (p *Pool) Accept(tx *consensus.Tx) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := consensus.CheckTxStateless(tx); err != nil {
		return err
	}
	if tx.IsCoinbase() {
		return ErrCoinbase
	}
	size := tx.SerializedSize()
	if size > p.cfg.MaxTxSize {
		return ErrTooLarge
	}
	txid := tx.TxID()
	if _, ok := p.txs[txid]; ok {
		return ErrAlreadyInPool
	}
	wtxid := tx.WTxID()
	if r, ok := p.rejected[txid]; ok && time.Since(r.when) < rejectTTL {
		return fmt.Errorf("%w: %v", ErrRecentlyRejected, r.err)
	}
	if r, ok := p.rejectedW[wtxid]; ok && time.Since(r.when) < rejectTTL {
		return fmt.Errorf("%w: %v", ErrRecentlyRejected, r.err)
	}
	for i := range tx.Outputs {
		o := &tx.Outputs[i]
		if o.Type == consensus.OutputTypeP2PK && o.Asset == consensus.NativeAsset && o.Amount < p.cfg.DustLimit {
			return p.rejectTx(txid, ErrDust)
		}
	}

	// Conflicts: pool transactions spending any of our inputs, plus their descendants.
	conflicts := map[consensus.Hash]struct{}{}
	for _, in := range tx.Inputs {
		if spender, ok := p.spends[in.PrevOut]; ok {
			p.collectDescendants(spender, conflicts)
		}
	}
	if len(conflicts) > maxReplacements {
		return p.rejectTx(txid, ErrTooManyConflicts)
	}

	nextHeight := p.ch.Tip().Height + 1
	view := poolView{p: p, nextHeight: nextHeight, ignore: conflicts}
	fee, spent, err := consensus.CheckTxInputs(tx, view, nextHeight)
	if err != nil {
		if errors.Is(err, consensus.ErrTxMissingInput) {
			return err // its parent may simply not have arrived yet; do not cache
		}
		return p.rejectTx(txid, err)
	}
	// From here on the verdict depends on the size, hence on the witness.
	minFee := p.cfg.MinFeeRate*consensus.Amount(size) + AssetSurcharge(p.cfg, tx)
	if fee < minFee {
		return p.rejectWitness(wtxid, fmt.Errorf("%w: fee %d < %d", ErrInsufficientFee, fee, minFee))
	}
	rate := float64(fee) / float64(size)
	if len(conflicts) > 0 {
		// Replace-by-fee: pay for everything replaced plus our own relay, and
		// beat every replaced transaction's fee rate.
		var replaced consensus.Amount
		for id := range conflicts {
			e := p.txs[id]
			replaced += e.Fee
			if rate <= e.FeeRate() {
				return p.rejectWitness(wtxid, ErrConflict)
			}
		}
		if fee < replaced+minFee {
			return p.rejectWitness(wtxid, ErrConflict)
		}
	}

	parents := map[consensus.Hash]struct{}{}
	for _, in := range tx.Inputs {
		if _, ok := p.txs[in.PrevOut.TxID]; ok {
			parents[in.PrevOut.TxID] = struct{}{}
		}
	}
	// Not cached: the limit frees up as soon as an ancestor confirms.
	ancestors, err := p.checkChainLimits(parents, size, conflicts)
	if err != nil {
		return err
	}

	// Only now the expensive part.
	if err := consensus.VerifyTxSignatures(p.ch.Params().ChainTag, tx, spent); err != nil {
		return p.rejectWitness(wtxid, err)
	}

	// Replaced transactions leave first, so eviction never hits innocents
	// when the replacement itself frees enough room.
	for id := range conflicts {
		if _, still := p.txs[id]; still {
			p.remove(id)
		}
	}
	if p.bytes+size > p.cfg.MaxBytes {
		if !p.evictLocked(size, rate, ancestors) {
			return ErrPoolFull
		}
	}

	e := &Entry{Tx: tx, TxID: txid, Fee: fee, Size: size, Added: time.Now(), parents: parents, children: map[consensus.Hash]struct{}{}}
	for parent := range parents {
		p.txs[parent].children[txid] = struct{}{}
	}
	for _, in := range tx.Inputs {
		p.spends[in.PrevOut] = txid
	}
	p.txs[txid] = e
	p.bytes += size
	p.gen++
	return nil
}

// checkChainLimits enforces MaxChainTxs/MaxChainBytes for a newcomer of the
// given size spending the given pool parents, ignoring transactions about
// to be replaced. It returns the newcomer's ancestors.
func (p *Pool) checkChainLimits(parents map[consensus.Hash]struct{}, size int, ignore map[consensus.Hash]struct{}) (map[consensus.Hash]struct{}, error) {
	anc := map[consensus.Hash]struct{}{}
	ancBytes := size
	stack := make([]consensus.Hash, 0, len(parents))
	for id := range parents {
		stack = append(stack, id)
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := anc[id]; seen {
			continue
		}
		if _, ig := ignore[id]; ig {
			continue
		}
		e, ok := p.txs[id]
		if !ok {
			continue
		}
		anc[id] = struct{}{}
		ancBytes += e.Size
		if len(anc)+1 > MaxChainTxs || ancBytes > MaxChainBytes {
			return nil, fmt.Errorf("%w: more than %d transactions or %d bytes with its ancestors", ErrTooLongChain, MaxChainTxs, MaxChainBytes)
		}
		for parent := range e.parents {
			stack = append(stack, parent)
		}
	}
	for a := range anc {
		n, b := p.descendantStats(a, ignore)
		if n+1 > MaxChainTxs || b+size > MaxChainBytes {
			return nil, fmt.Errorf("%w: an ancestor would have more than %d transactions or %d bytes of descendants", ErrTooLongChain, MaxChainTxs, MaxChainBytes)
		}
	}
	return anc, nil
}

// descendantStats counts id and its pool descendants (not in ignore) and
// their bytes, stopping early once past the chain limits.
func (p *Pool) descendantStats(id consensus.Hash, ignore map[consensus.Hash]struct{}) (int, int) {
	seen := map[consensus.Hash]struct{}{}
	stack := []consensus.Hash{id}
	n, b := 0, 0
	for len(stack) > 0 && n <= MaxChainTxs && b <= MaxChainBytes {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[cur]; ok {
			continue
		}
		seen[cur] = struct{}{}
		if _, ig := ignore[cur]; ig {
			continue
		}
		e, ok := p.txs[cur]
		if !ok {
			continue
		}
		n++
		b += e.Size
		for c := range e.children {
			stack = append(stack, c)
		}
	}
	return n, b
}

// evictLocked frees room for a transaction of the given size and fee rate by
// removing the lowest-fee-rate leaves, in one amortised pass down to 95% of
// the cap. Returns false if the newcomer's rate is not above the lowest.
// The newcomer's own ancestors are never evicted.
func (p *Pool) evictLocked(need int, rate float64, keep map[consensus.Hash]struct{}) bool {
	leaves := p.leavesByRate(keep)
	if len(leaves) == 0 || leaves[0].FeeRate() >= rate {
		return false
	}
	target := p.cfg.MaxBytes*95/100 - need
	for _, e := range leaves {
		if p.bytes <= target {
			break
		}
		if e.FeeRate() >= rate {
			break
		}
		if _, still := p.txs[e.TxID]; still {
			p.remove(e.TxID)
		}
	}
	return p.bytes+need <= p.cfg.MaxBytes
}

// leavesByRate returns entries with no pool children, cheapest first,
// leaving out those in keep.
func (p *Pool) leavesByRate(keep map[consensus.Hash]struct{}) []*Entry {
	var leaves []*Entry
	for id, e := range p.txs {
		if _, k := keep[id]; k {
			continue
		}
		if len(e.children) == 0 {
			leaves = append(leaves, e)
		}
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].FeeRate() < leaves[j].FeeRate() })
	return leaves
}

// collectDescendants adds id and every pool transaction that depends on it.
func (p *Pool) collectDescendants(id consensus.Hash, out map[consensus.Hash]struct{}) {
	stack := []consensus.Hash{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, done := out[cur]; done {
			continue
		}
		e, ok := p.txs[cur]
		if !ok {
			continue
		}
		out[cur] = struct{}{}
		for c := range e.children {
			stack = append(stack, c)
		}
	}
}

// remove deletes id and all its descendants.
func (p *Pool) remove(id consensus.Hash) {
	all := map[consensus.Hash]struct{}{}
	p.collectDescendants(id, all)
	for d := range all {
		p.removeOne(d)
	}
}

// removeOne deletes a single entry, unlinking it from its pool parents and
// children. Children stay; the caller decides whether they are still valid.
func (p *Pool) removeOne(id consensus.Hash) {
	e, ok := p.txs[id]
	if !ok {
		return
	}
	for parent := range e.parents {
		if pe, ok := p.txs[parent]; ok {
			delete(pe.children, id)
		}
	}
	for c := range e.children {
		if ce, ok := p.txs[c]; ok {
			delete(ce.parents, id)
		}
	}
	for _, in := range e.Tx.Inputs {
		if p.spends[in.PrevOut] == id {
			delete(p.spends, in.PrevOut)
		}
	}
	delete(p.txs, id)
	p.bytes -= e.Size
	p.gen++
}

// onTip brings the pool up to date with a new chain tip. When the tip is
// one or a few blocks on top of the last tip seen, only what those blocks
// change is touched: the transactions they confirm, and the pool
// transactions spending an outpoint they spend, with their descendants
// (O(block), not O(pool)). Connecting blocks can invalidate nothing else:
// lock heights and coinbase maturity only become easier as the height
// grows, and a pool transaction's inputs stay as they were unless a block
// spends them. A reorganisation or disconnect (lower maturity, coins
// coming back, others gone) gets the full Revalidate.
func (p *Pool) onTip(tip *chain.Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A new tip can change any cached verdict (maturity, lock height, conflicts).
	p.rejected = map[consensus.Hash]rejection{}
	p.rejectedW = map[consensus.Hash]rejection{}
	last := p.lastTip
	if last != nil {
		if tip == last {
			return // the pool was already brought up to date with this tip
		}
		if tip.Height < last.Height && last.Ancestor(tip.Height) == tip && p.ch.Tip() != tip {
			// A late notification: the chain has moved on since (to last or
			// beyond), and the pool already reflects a later tip. If the
			// chain really is back at this ancestor (blocks disconnected),
			// the full Revalidate below runs instead.
			return
		}
		if tip.Height > last.Height && tip.Height-last.Height <= maxIncrementalBlocks &&
			tip.Ancestor(last.Height) == last && p.ch.IsMainChain(tip) {
			blocks := make([]*consensus.Block, tip.Height-last.Height)
			ok := true
			for n := tip; n != last; n = n.Parent {
				blk, err := p.ch.Block(n.Hash)
				if err != nil {
					ok = false
					break
				}
				blocks[n.Height-last.Height-1] = blk
			}
			if ok {
				for _, blk := range blocks {
					p.removeForBlockLocked(blk)
				}
				p.lastTip = tip
				return
			}
		}
	}
	p.revalidateLocked()
}

// removeForBlockLocked drops what connecting blk makes invalid: the
// transactions it confirms (their children stay: their inputs are now in
// the chain) and every pool transaction spending an outpoint blk spends,
// with all its descendants.
func (p *Pool) removeForBlockLocked(blk *consensus.Block) {
	for _, tx := range blk.Txs {
		if tx.IsCoinbase() {
			continue
		}
		txid := tx.TxID()
		if _, ok := p.txs[txid]; ok {
			p.removeOne(txid)
		}
		for _, in := range tx.Inputs {
			if spender, ok := p.spends[in.PrevOut]; ok {
				p.remove(spender)
			}
		}
	}
}

// Revalidate drops every entry that is no longer valid against the current
// chain tip (confirmed, double-spent by a block, now-immature, ...).
// Children of a confirmed transaction stay: their inputs are now in the
// chain. Tip changes call it after a reorganisation or disconnect; a plain
// block connect only removes what the block affects (see onTip).
func (p *Pool) Revalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A new tip can change any verdict (maturity, lock height, conflicts).
	p.rejected = map[consensus.Hash]rejection{}
	p.rejectedW = map[consensus.Hash]rejection{}
	p.revalidateLocked()
}

func (p *Pool) revalidateLocked() {
	p.fullRevalidations++
	tip := p.ch.Tip()
	p.lastTip = tip
	nextHeight := tip.Height + 1
	// Parents come before children, so a child is checked after its parent
	// has either stayed or left.
	for _, e := range p.topological() {
		if _, still := p.txs[e.TxID]; !still {
			continue
		}
		view := poolView{p: p, nextHeight: nextHeight, ignore: map[consensus.Hash]struct{}{e.TxID: {}}}
		if _, _, err := consensus.CheckTxInputs(e.Tx, view, nextHeight); err != nil {
			p.removeOne(e.TxID)
		}
	}
}

// ranksBefore orders entries by fee rate, highest first; ties by age, then
// txid, so the order is deterministic.
func ranksBefore(a, b *Entry) bool {
	if ra, rb := a.FeeRate(), b.FeeRate(); ra != rb {
		return ra > rb
	}
	if !a.Added.Equal(b.Added) {
		return a.Added.Before(b.Added)
	}
	return string(a.TxID[:]) < string(b.TxID[:])
}

// entryHeap pops the best-ranked entry first.
type entryHeap []*Entry

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return ranksBefore(h[i], h[j]) }
func (h entryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *entryHeap) Push(x any)        { *h = append(*h, x.(*Entry)) }
func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

// worstHeap keeps the worst-ranked entry on top (for top-k selection).
type worstHeap []*Entry

func (h worstHeap) Len() int           { return len(h) }
func (h worstHeap) Less(i, j int) bool { return ranksBefore(h[j], h[i]) }
func (h worstHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *worstHeap) Push(x any)        { *h = append(*h, x.(*Entry)) }
func (h *worstHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

// topological returns entries with parents before children, highest fee
// rate first among the transactions whose parents are already placed
// (Kahn's algorithm with a fee-rate priority queue, O(n log n)).
func (p *Pool) topological() []*Entry { return p.kahn(p.txs) }

// kahn orders the entries of set (closed under ancestors) topologically.
func (p *Pool) kahn(set map[consensus.Hash]*Entry) []*Entry {
	ready := make(entryHeap, 0, len(set))
	for _, e := range set {
		n := 0
		for parent := range e.parents {
			if _, ok := set[parent]; ok {
				n++
			}
		}
		e.pending = n
		if n == 0 {
			ready = append(ready, e)
		}
	}
	heap.Init(&ready)
	out := make([]*Entry, 0, len(set))
	for ready.Len() > 0 {
		e := heap.Pop(&ready).(*Entry)
		out = append(out, e)
		for c := range e.children {
			ce, ok := set[c]
			if !ok {
				continue
			}
			ce.pending--
			if ce.pending == 0 {
				heap.Push(&ready, ce)
			}
		}
	}
	return out
}

// topK returns the k best-ranked entries plus their ancestors, ordered:
// O(n log k) instead of ordering the whole pool.
func (p *Pool) topK(k int) []*Entry {
	h := make(worstHeap, 0, k)
	for _, e := range p.txs {
		if len(h) < k {
			heap.Push(&h, e)
		} else if ranksBefore(e, h[0]) {
			h[0] = e
			heap.Fix(&h, 0)
		}
	}
	set := make(map[consensus.Hash]*Entry, 2*k)
	stack := append([]*Entry(nil), h...)
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := set[e.TxID]; ok {
			continue
		}
		set[e.TxID] = e
		for parent := range e.parents {
			if pe, ok := p.txs[parent]; ok {
				stack = append(stack, pe)
			}
		}
	}
	return p.kahn(set)
}

// Select returns transactions for a block template in valid order, greedily
// by fee rate, whose total size does not exceed maxBytes. Also returns the
// total fees.
//
// The result is what walking the whole pool in topological order (kahn)
// gives, but the walk stops as soon as nothing more can fit, so only the
// entries that make it into (or just miss) the template are ordered; and
// while the pool is unchanged the previous result is reused.
func (p *Pool) Select(maxBytes int) ([]*consensus.Tx, consensus.Amount) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := &p.sel
	if !c.valid || c.gen != p.gen || c.count != len(p.txs) || c.bytes != p.bytes || c.maxBytes != maxBytes {
		txs, fees := p.selectLocked(maxBytes)
		p.sel = selectCache{valid: true, gen: p.gen, count: len(p.txs), bytes: p.bytes, maxBytes: maxBytes, txs: txs, fees: fees}
	}
	return append([]*consensus.Tx(nil), c.txs...), c.fees
}

// selectLocked is Kahn's algorithm over the whole pool (as kahn), run
// lazily: entries are popped in the same order kahn would output them, and
// popping stops once the space left is below the smallest entry.
func (p *Pool) selectLocked(maxBytes int) ([]*consensus.Tx, consensus.Amount) {
	ready := make(entryHeap, 0, len(p.txs)/4+1)
	minSize := -1
	for _, e := range p.txs {
		n := 0
		if len(e.parents) > 0 {
			for parent := range e.parents {
				if _, ok := p.txs[parent]; ok {
					n++
				}
			}
		}
		e.pending = n
		if n == 0 {
			ready = append(ready, e)
		}
		if minSize < 0 || e.Size < minSize {
			minSize = e.Size
		}
	}
	heap.Init(&ready)
	var txs []*consensus.Tx
	var fees consensus.Amount
	used := 0
	included := map[consensus.Hash]struct{}{}
	for ready.Len() > 0 && maxBytes-used >= minSize {
		e := heap.Pop(&ready).(*Entry)
		for c := range e.children {
			ce, ok := p.txs[c]
			if !ok {
				continue
			}
			ce.pending--
			if ce.pending == 0 {
				heap.Push(&ready, ce)
			}
		}
		if used+e.Size > maxBytes {
			continue
		}
		ok := true
		for parent := range e.parents {
			if _, inPool := p.txs[parent]; !inPool {
				continue
			}
			if _, inc := included[parent]; !inc {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		included[e.TxID] = struct{}{}
		txs = append(txs, e.Tx)
		fees += e.Fee
		used += e.Size
	}
	return txs, fees
}

// Ordered returns up to max pool txids with parents before children and
// higher fee rates first: the order to announce them to a peer in, so that
// it never receives a child before its parent.
func (p *Pool) Ordered(max int) []consensus.Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	var all []*Entry
	if max < 0 || len(p.txs) <= max {
		all = p.topological()
	} else {
		all = p.topK(max)
	}
	if max >= 0 && len(all) > max {
		all = all[:max] // a prefix of a topological order keeps every parent
	}
	out := make([]consensus.Hash, len(all))
	for i, e := range all {
		out[i] = e.TxID
	}
	return out
}

// Generation changes whenever a transaction enters or leaves the pool.
func (p *Pool) Generation() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

// Get returns a pool transaction.
func (p *Pool) Get(txid consensus.Hash) (*consensus.Tx, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.txs[txid]
	if !ok {
		return nil, false
	}
	return e.Tx, true
}

// Has reports whether txid is in the pool.
func (p *Pool) Has(txid consensus.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.txs[txid]
	return ok
}

// Count returns the number of pool transactions.
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.txs)
}

// Bytes returns the total serialized size of the pool.
func (p *Pool) Bytes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}

// TxIDs returns all pool txids (unordered).
func (p *Pool) TxIDs() []consensus.Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]consensus.Hash, 0, len(p.txs))
	for id := range p.txs {
		out = append(out, id)
	}
	return out
}
