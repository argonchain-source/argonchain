package chain

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/consensus"
)

// Status of a block in the index.
type Status uint8

const (
	// StatusHeaderOnly: the header passed stateless and contextual checks but
	// the body is not stored (a side chain with no more work than the tip).
	// Bodies are fetched only if the branch ever becomes the heaviest.
	StatusHeaderOnly Status = 0
	// StatusHaveData: header valid and full block stored, never connected.
	StatusHaveData Status = 1
	// StatusValid: fully validated by connecting it at least once.
	StatusValid Status = 2
	// StatusInvalid: the block or an ancestor failed validation.
	StatusInvalid Status = 3
)

// MaxSideHeaders bounds header-only index entries (DoS protection; each
// costs the attacker a full proof of work). When the cap is full a new entry
// takes the place of the header-only leaf with the least cumulative work
// (see makeSideRoomLocked), so headers forked cheaply far below the tip
// (at the easiest difficulty, from genesis) are the first to go and cannot
// keep out a fork near the tip, whose work is close to the tip's.
const MaxSideHeaders = 100_000

// maxMissingReported bounds the hashes one MissingDataError lists (the
// oldest first); the rest are reported once those are supplied.
const maxMissingReported = 1024

// powMemoSize bounds the headers CheckHeaderPoW remembers. It covers the
// headers a headers-first sync verifies ahead of their bodies (p2p keeps at
// most a few batches of 2,000 queued).
const powMemoSize = 16_384

// powMemo is a bounded set of header hashes whose proof of work this
// process has verified, oldest forgotten first.
type powMemo struct {
	mu   sync.Mutex
	set  map[consensus.Hash]struct{}
	ring []consensus.Hash
	next int
	full bool
}

func newPowMemo(size int) *powMemo {
	return &powMemo{set: make(map[consensus.Hash]struct{}, size), ring: make([]consensus.Hash, size)}
}

func (m *powMemo) has(h consensus.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.set[h]
	return ok
}

func (m *powMemo) add(h consensus.Hash) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.set[h]; ok {
		return
	}
	if m.full {
		delete(m.set, m.ring[m.next])
	}
	m.ring[m.next] = h
	m.set[h] = struct{}{}
	m.next++
	if m.next == len(m.ring) {
		m.next, m.full = 0, true
	}
}

// Node is an entry in the block index.
type Node struct {
	Hash   consensus.Hash
	Header consensus.Header
	Height uint32
	Work   *big.Int // cumulative work including this block
	Status Status
	Parent *Node
	// seq orders nodes by arrival in this process (loaded nodes first, by
	// height): among branches of equal work the one seen first wins.
	seq uint64
}

// Ancestor returns the ancestor at the given height (or nil).
func (n *Node) Ancestor(height uint32) *Node {
	if height > n.Height {
		return nil
	}
	for n != nil && n.Height > height {
		n = n.Parent
	}
	return n
}

var (
	ErrDuplicateBlock   = errors.New("chain: block already known")
	ErrInvalidAncestor  = errors.New("chain: block descends from an invalid block")
	ErrMissingBlockData = errors.New("chain: block data missing for reorg")
	ErrTooManyHeaders   = errors.New("chain: too many side-chain headers")
	ErrTxNotFound       = errors.New("chain: transaction not found")
	// ErrHeaderNeedsBody: ProcessHeader was given a header whose branch has
	// more work than the tip. Such a block is only accepted with its body.
	ErrHeaderNeedsBody = errors.New("chain: header's branch is heavier than the tip; process the full block")
)

// MissingDataError reports a heavier branch that cannot be evaluated until
// the listed block bodies are supplied. errors.Is(err, ErrMissingBlockData)
// holds for it.
type MissingDataError struct {
	Hashes []consensus.Hash
}

func (e *MissingDataError) Error() string {
	return fmt.Sprintf("chain: %d block bodies missing for reorg", len(e.Hashes))
}

// Is lets errors.Is(err, ErrMissingBlockData) match.
func (e *MissingDataError) Is(target error) bool { return target == ErrMissingBlockData }

// StorageError wraps a database failure. It is never a statement about the
// validity of a block: callers must not mark blocks invalid or ban peers
// because of it (SPEC §9: storage never influences validity).
type StorageError struct {
	Op  string
	Err error
}

func (e *StorageError) Error() string { return "chain: storage " + e.Op + ": " + e.Err.Error() }
func (e *StorageError) Unwrap() error { return e.Err }

// IsStorage reports whether err is (or wraps) a StorageError.
func IsStorage(err error) bool {
	var se *StorageError
	return errors.As(err, &se)
}

// Chain is the node's view of the blockchain.
type Chain struct {
	mu          sync.RWMutex
	p           *consensus.Params
	db          *leveldb.DB
	nodes       map[consensus.Hash]*Node
	children    map[consensus.Hash][]*Node // parent hash -> child nodes in the index
	nextSeq     uint64
	main        []*Node // main chain by height; main[len-1] == tip
	tip         *Node
	sideHeaders int
	maxSide     int        // MaxSideHeaders (lowered by tests)
	leaves      sideLeaves // header-only leaves, least work first (lazily pruned)
	subscribers []func(tip *Node)
	pow         *powMemo
}

// sideLeaves is a min-heap of header-only nodes that may be evicted when
// the side-header cap is full: least cumulative work first and, among equal
// work, the latest arrival first. Entries go stale (the node got a child or
// a body, was marked invalid or evicted) and are skipped when they surface.
type sideLeaves []*Node

func (h sideLeaves) Len() int { return len(h) }
func (h sideLeaves) Less(i, j int) bool {
	if c := h[i].Work.Cmp(h[j].Work); c != 0 {
		return c < 0
	}
	return h[i].seq > h[j].seq
}
func (h sideLeaves) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *sideLeaves) Push(x any)   { *h = append(*h, x.(*Node)) }
func (h *sideLeaves) Pop() any {
	old := *h
	n := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return n
}

// Open loads (or initialises) a chain database at path.
func Open(p *consensus.Params, path string) (*Chain, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, &StorageError{"open", err}
	}
	c := &Chain{p: p, db: db, nodes: map[consensus.Hash]*Node{}, children: map[consensus.Hash][]*Node{},
		pow: newPowMemo(powMemoSize), maxSide: MaxSideHeaders}
	if err := c.load(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

// Close flushes and closes the database.
func (c *Chain) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.db.Close()
}

// load rebuilds the in-memory index from disk, or writes genesis.
func (c *Chain) load() error {
	iter := c.db.NewIterator(util.BytesPrefix([]byte{prefixNode}), nil)
	raw := map[consensus.Hash]*Node{}
	for iter.Next() {
		n, err := decodeNode(iter.Value())
		if err != nil {
			iter.Release()
			return err
		}
		raw[n.Hash] = n
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return &StorageError{"iterate", err}
	}
	if len(raw) == 0 {
		return c.initGenesis()
	}

	byHeight := make([][]*Node, 0)
	for _, n := range raw {
		for uint32(len(byHeight)) <= n.Height {
			byHeight = append(byHeight, nil)
		}
		byHeight[n.Height] = append(byHeight[n.Height], n)
	}
	for h := range byHeight {
		// Arrival order is not stored: loaded nodes count as seen in order
		// of height, then hash, so ties are decided the same way each start.
		sort.Slice(byHeight[h], func(i, j int) bool {
			return bytes.Compare(byHeight[h][i].Hash[:], byHeight[h][j].Hash[:]) < 0
		})
		for _, n := range byHeight[h] {
			target, err := consensus.CompactToTarget(n.Header.Bits)
			if err != nil {
				return fmt.Errorf("chain: stored header has bad bits: %w", err)
			}
			work := consensus.BlockWork(target)
			if h > 0 {
				parent, ok := raw[n.Header.PrevHash]
				if !ok {
					return fmt.Errorf("chain: node %s has unknown parent", n.Hash)
				}
				n.Parent = parent
				work.Add(work, parent.Work)
			} else if n.Hash != c.p.GenesisHash {
				return fmt.Errorf("chain: database genesis %s is not this network's", n.Hash)
			}
			n.Work = work
			c.addNodeLocked(n)
			if n.Status == StatusHeaderOnly {
				c.sideHeaders++
			}
		}
	}
	c.rebuildLeavesLocked()

	tipBytes, err := c.db.Get([]byte(keyTip), nil)
	if err != nil {
		return &StorageError{"read tip", err}
	}
	var tipHash consensus.Hash
	copy(tipHash[:], tipBytes)
	tip, ok := c.nodes[tipHash]
	if !ok {
		return errors.New("chain: tip not in index")
	}
	c.setTip(tip)
	if err := c.ensureTxIndex(); err != nil {
		return err
	}
	// Fork choice may have been interrupted by a crash: adopt a heavier
	// branch whose bodies we already have.
	if best := c.bestCandidateLocked(); best != nil {
		if err := c.reorganize(best); err != nil && !errors.Is(err, ErrMissingBlockData) && !IsStorage(err) {
			// Consensus failure on that branch: already marked invalid.
			return nil
		} else if IsStorage(err) {
			return err
		}
	}
	return nil
}

// setTip installs tip and rebuilds the main-chain slice from it.
func (c *Chain) setTip(tip *Node) {
	c.tip = tip
	if cap(c.main) < int(tip.Height)+1 {
		c.main = make([]*Node, tip.Height+1, tip.Height+1024)
	}
	c.main = c.main[:tip.Height+1]
	for n := tip; n != nil; n = n.Parent {
		c.main[n.Height] = n
	}
}

func (c *Chain) initGenesis() error {
	g := c.p.Genesis
	if err := consensus.CheckHeaderStateless(c.p, &g.Header); err != nil {
		return fmt.Errorf("chain: genesis invalid: %w", err)
	}
	target, _ := consensus.CompactToTarget(g.Header.Bits)
	n := &Node{Hash: c.p.GenesisHash, Header: g.Header, Height: 0, Work: consensus.BlockWork(target), Status: StatusValid}

	ov := consensus.NewOverlay(dbView{c.db})
	if err := consensus.ConnectBlock(c.p, g, 0, ov); err != nil {
		return fmt.Errorf("chain: genesis connect: %w", err)
	}
	batch := new(leveldb.Batch)
	batch.Put(nodeKey(n.Hash), encodeNode(n))
	batch.Put(blockKey(n.Hash), g.Serialize())
	batch.Put(undoKey(n.Hash), encodeUndo(nil))
	batch.Put(heightKey(0), n.Hash[:])
	batch.Put(txKey(g.Txs[0].TxID()), txLocValue(n.Hash, 0))
	batch.Put([]byte(keyTxIndexed), []byte{1})
	batch.Put([]byte(keyTip), n.Hash[:])
	if err := c.db.Write(batch, nil); err != nil {
		return &StorageError{"write genesis", err}
	}
	c.addNodeLocked(n)
	c.setTip(n)
	return nil
}

// addNodeLocked enters n into the index (and its parent's children),
// stamping its arrival order.
func (c *Chain) addNodeLocked(n *Node) {
	c.nextSeq++
	n.seq = c.nextSeq
	c.nodes[n.Hash] = n
	if n.Parent != nil {
		c.children[n.Parent.Hash] = append(c.children[n.Parent.Hash], n)
	}
}

// removeNodeLocked takes n (which has no children) out of the index again.
func (c *Chain) removeNodeLocked(n *Node) {
	delete(c.nodes, n.Hash)
	if n.Parent == nil {
		return
	}
	kids := c.children[n.Parent.Hash]
	for i, k := range kids {
		if k == n {
			kids = append(kids[:i], kids[i+1:]...)
			break
		}
	}
	if len(kids) == 0 {
		delete(c.children, n.Parent.Hash)
	} else {
		c.children[n.Parent.Hash] = kids
	}
}

// evictableLocked reports whether n is a header-only entry nothing in the
// index builds on, so it can be dropped without orphaning anything.
func (c *Chain) evictableLocked(n *Node) bool {
	return c.nodes[n.Hash] == n && n.Status == StatusHeaderOnly && len(c.children[n.Hash]) == 0
}

// rebuildLeavesLocked recomputes the eviction heap from the index.
func (c *Chain) rebuildLeavesLocked() {
	c.leaves = c.leaves[:0]
	for _, n := range c.nodes {
		if c.evictableLocked(n) {
			c.leaves = append(c.leaves, n)
		}
	}
	heap.Init(&c.leaves)
}

// addSideHeaderLocked enters a header-only node into the index.
func (c *Chain) addSideHeaderLocked(n *Node) {
	c.addNodeLocked(n)
	c.sideHeaders++
	c.pushLeafLocked(n)
}

// pushLeafLocked offers n as an eviction candidate (checked again when it
// surfaces), compacting the heap once stale entries dominate it.
func (c *Chain) pushLeafLocked(n *Node) {
	heap.Push(&c.leaves, n)
	if len(c.leaves) > 2*c.sideHeaders+1024 {
		c.rebuildLeavesLocked()
	}
}

// makeSideRoomLocked frees a header-only slot, if the cap is full, for a new
// header-only entry of cumulative work work whose parent is parent: it
// evicts header-only leaves, least work first, as long as they have no more
// work than the newcomer (equal work goes: the cap cannot be held against
// an equal claim). A header forked near the tip carries almost the tip's
// work, so cheap headers forked far below it (the attacker's way to fill
// the cap) never keep it out. The parent is never evicted for its own
// child. Returns ErrTooManyHeaders if no slot can be freed.
func (c *Chain) makeSideRoomLocked(work *big.Int, parent *Node) error {
	var spared *Node
	defer func() {
		if spared != nil {
			heap.Push(&c.leaves, spared)
		}
	}()
	for c.sideHeaders >= c.maxSide {
		if len(c.leaves) == 0 {
			return ErrTooManyHeaders
		}
		n := c.leaves[0]
		if !c.evictableLocked(n) {
			heap.Pop(&c.leaves)
			continue
		}
		if n == parent {
			heap.Pop(&c.leaves)
			spared = n
			continue
		}
		if n.Work.Cmp(work) > 0 {
			return ErrTooManyHeaders // every candidate has more work than the newcomer
		}
		heap.Pop(&c.leaves)
		if err := c.evictSideLocked(n); err != nil {
			heap.Push(&c.leaves, n)
			return err
		}
	}
	return nil
}

// evictSideLocked drops the header-only leaf n from the index and disk. Its
// parent becomes a candidate if nothing else builds on it.
func (c *Chain) evictSideLocked(n *Node) error {
	if err := c.db.Delete(nodeKey(n.Hash), nil); err != nil {
		return &StorageError{"evict header", err}
	}
	c.removeNodeLocked(n)
	c.sideHeaders--
	if p := n.Parent; p != nil && c.evictableLocked(p) {
		c.pushLeafLocked(p)
	}
	return nil
}

// CheckHeaderPoW runs consensus.CheckHeaderStateless on h (version, time
// bound, and the proof of work: one Argon2id evaluation), unless this Chain
// has already verified this exact header earlier in this process. Headers
// are identified by their hash, a tagged SHA-256 of all 120 header bytes,
// and only headers that passed the check here are remembered (a bounded
// set), so nothing a peer says is ever taken on trust. Callers that check
// headers ahead of their bodies (headers-first sync) use it so that the
// body does not pay for a second Argon2id. Safe for concurrent use; it
// takes no chain lock.
func (c *Chain) CheckHeaderPoW(h *consensus.Header) error {
	hash := h.Hash()
	if c.pow.has(hash) {
		return nil
	}
	if err := consensus.CheckHeaderStateless(c.p, h); err != nil {
		return err
	}
	c.pow.add(hash)
	return nil
}

// Params returns the network parameters.
func (c *Chain) Params() *consensus.Params { return c.p }

// Tip returns the best chain tip.
func (c *Chain) Tip() *Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tip
}

// Node returns the index entry for hash.
func (c *Chain) Node(h consensus.Hash) (*Node, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.nodes[h]
	return n, ok
}

// NodeAtHeight returns the main-chain node at height in O(1).
func (c *Chain) NodeAtHeight(height uint32) (*Node, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if int(height) >= len(c.main) {
		return nil, false
	}
	return c.main[height], true
}

// IsMainChain reports whether n is on the best chain, in O(1).
func (c *Chain) IsMainChain(n *Node) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return int(n.Height) < len(c.main) && c.main[n.Height] == n
}

// HasData reports whether the full block for h is stored.
func (c *Chain) HasData(h consensus.Hash) bool {
	st, ok := c.Status(h)
	return ok && (st == StatusHaveData || st == StatusValid)
}

// Status returns the index status of h (read under the chain lock).
func (c *Chain) Status(h consensus.Hash) (Status, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.nodes[h]
	if !ok {
		return 0, false
	}
	return n.Status, true
}

// MissingBodies walks the branch ending at h down to where it leaves the
// main chain and returns, oldest first, up to max of the blocks on it whose
// bodies are not stored (header-only): the bodies to fetch, in order,
// before the branch can be connected. known is false if h is not in the
// index; invalid is true if the branch contains an invalid block.
func (c *Chain) MissingBodies(h consensus.Hash, max int) (hashes []consensus.Hash, known, invalid bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.nodes[h]
	if !ok {
		return nil, false, false
	}
	var run []consensus.Hash // newest first
	for ; n != nil && !(int(n.Height) < len(c.main) && c.main[n.Height] == n); n = n.Parent {
		switch n.Status {
		case StatusInvalid:
			return nil, true, true
		case StatusHeaderOnly:
			run = append(run, n.Hash)
		}
	}
	for i := len(run) - 1; i >= 0 && len(hashes) < max; i-- {
		hashes = append(hashes, run[i])
	}
	return hashes, true, false
}

// Block loads a stored block.
func (c *Chain) Block(h consensus.Hash) (*consensus.Block, error) {
	b, err := c.RawBlock(h)
	if err != nil {
		return nil, err
	}
	return consensus.DecodeBlock(b)
}

// RawBlock returns the stored serialized block (for relaying without a
// decode/encode round trip).
func (c *Chain) RawBlock(h consensus.Hash) ([]byte, error) {
	b, err := c.db.Get(blockKey(h), nil)
	if err != nil {
		if errors.Is(err, leveldb.ErrNotFound) {
			return nil, ErrMissingBlockData
		}
		return nil, &StorageError{"read block", err}
	}
	return b, nil
}

// Undo returns the coins a connected block spent (its undo data), keyed by
// outpoint. Coins created and spent inside the same block are not included.
func (c *Chain) Undo(h consensus.Hash) (map[consensus.OutPoint]consensus.Coin, error) {
	b, err := c.db.Get(undoKey(h), nil)
	if err != nil {
		return nil, err
	}
	return decodeUndo(b)
}

// UTXO returns a read-only view of the tip's UTXO set.
func (c *Chain) UTXO() consensus.UTXOView { return dbView{c.db} }

// LookupCoin is a convenience over UTXO().Lookup.
func (c *Chain) LookupCoin(op consensus.OutPoint) (consensus.Coin, bool) {
	return dbView{c.db}.Lookup(op)
}

// TxLocation returns the main-chain block hash and index of a confirmed
// transaction.
func (c *Chain) TxLocation(txid consensus.Hash) (consensus.Hash, uint32, bool) {
	v, err := c.db.Get(txKey(txid), nil)
	if err != nil || len(v) != 36 {
		return consensus.Hash{}, 0, false
	}
	var h consensus.Hash
	copy(h[:], v[:32])
	return h, binary.LittleEndian.Uint32(v[32:]), true
}

// Transaction loads a confirmed transaction and the index node of its block.
func (c *Chain) Transaction(txid consensus.Hash) (*consensus.Tx, *Node, error) {
	bh, idx, ok := c.TxLocation(txid)
	if !ok {
		return nil, nil, ErrTxNotFound
	}
	n, ok := c.Node(bh)
	if !ok {
		return nil, nil, ErrTxNotFound
	}
	blk, err := c.Block(bh)
	if err != nil {
		return nil, nil, err
	}
	if int(idx) >= len(blk.Txs) {
		return nil, nil, errCorrupt
	}
	return blk.Txs[idx], n, nil
}

// ensureTxIndex builds the transaction index for databases created before
// it existed or after an interrupted build. Idempotent.
func (c *Chain) ensureTxIndex() error {
	if _, err := c.db.Get([]byte(keyTxIndexed), nil); err == nil {
		return nil
	}
	batch := new(leveldb.Batch)
	for _, n := range c.main {
		blk, err := c.Block(n.Hash)
		if err != nil {
			return err
		}
		for i, tx := range blk.Txs {
			batch.Put(txKey(tx.TxID()), txLocValue(n.Hash, uint32(i)))
		}
		if batch.Len() >= 10_000 {
			if err := c.db.Write(batch, nil); err != nil {
				return &StorageError{"write tx index", err}
			}
			batch.Reset()
		}
	}
	batch.Put([]byte(keyTxIndexed), []byte{1})
	if err := c.db.Write(batch, nil); err != nil {
		return &StorageError{"write tx index", err}
	}
	return nil
}

// Subscribe registers a callback invoked (under no lock) after each tip change.
func (c *Chain) Subscribe(fn func(tip *Node)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscribers = append(c.subscribers, fn)
}

// MedianTimePast returns the MTP of the chain ending at n (n and up to 10
// ancestors), as required for the block after n.
func MedianTimePast(n *Node) uint64 {
	times := make([]uint64, 0, consensus.MedianTimeSpan)
	for i := 0; i < consensus.MedianTimeSpan && n != nil; i++ {
		times = append(times, n.Header.Time)
		n = n.Parent
	}
	return consensus.MedianTimePast(times)
}

// NextBits returns the compact target the next block on the tip must use.
func (c *Chain) NextBits() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return consensus.NextBits(c.p, c.tip.Header.Time, c.tip.Height)
}

// ProcessBlock validates and stores a block, extending or reorganising the
// best chain when appropriate. now is the local time for the future-timestamp
// check (-1 skips it). Returns true if the tip changed.
//
// Blocks on a side chain with no more work than the tip are recorded header
// only; their bodies are dropped and requested again if the branch ever wins
// (see MissingDataError). Pass keepBody=true for blocks fetched through
// headers-first sync of a branch known to be heavier, to avoid that second
// download.
func (c *Chain) ProcessBlock(blk *consensus.Block, now int64) (bool, error) {
	return c.ProcessBlockOpt(blk, now, false)
}

// ProcessBlockOpt is ProcessBlock with the keepBody option.
func (c *Chain) ProcessBlockOpt(blk *consensus.Block, now int64, keepBody bool) (bool, error) {
	c.mu.Lock()
	oldTip := c.tip
	err := c.processBlockLocked(blk, now, keepBody)
	changed := c.tip != oldTip
	newTip := c.tip
	subs := c.subscribers
	c.mu.Unlock()
	if changed {
		for _, fn := range subs {
			fn(newTip)
		}
	}
	return changed, err
}

// ProcessHeader records a header without its body, as a header-only side
// entry, if its branch has no more work than the tip: exactly what
// ProcessBlock records for such a block, without having to download the
// body first. The header gets the same checks as a block's header
// (contextual rules against its parent, then the proof of work) and counts
// against MaxSideHeaders (when the cap is full it takes the place of a
// header-only leaf with no more work, or is refused with ErrTooManyHeaders;
// see makeSideRoomLocked). A header that would make its branch heavier
// than the tip is refused with ErrHeaderNeedsBody and nothing is stored:
// such a block must come with its body (ProcessBlock). A known header
// returns ErrDuplicateBlock. now is as for ProcessBlock.
func (c *Chain) ProcessHeader(h *consensus.Header, now int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.processHeaderLocked(h, now)
}

// ProcessHeaders is ProcessHeader for a run of headers, each the parent of
// the next, under one lock (so that no entry of the run is evicted to make
// room for the next). Headers already known count as done. It returns how
// many leading headers of hs are now in the index and the error that
// stopped it, if any.
func (c *Chain) ProcessHeaders(hs []consensus.Header, now int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range hs {
		if err := c.processHeaderLocked(&hs[i], now); err != nil && !errors.Is(err, ErrDuplicateBlock) {
			return i, err
		}
	}
	return len(hs), nil
}

func (c *Chain) processHeaderLocked(h *consensus.Header, now int64) error {
	hash := h.Hash()
	if _, known := c.nodes[hash]; known {
		return ErrDuplicateBlock
	}
	parent, ok := c.nodes[h.PrevHash]
	if !ok {
		return consensus.ErrBlockUnknownParent
	}
	if parent.Status == StatusInvalid {
		return ErrInvalidAncestor
	}
	if err := consensus.CheckHeaderContext(c.p, h, parent.Hash, parent.Header.Time, parent.Height, MedianTimePast(parent), now); err != nil {
		return err
	}
	if err := c.CheckHeaderPoW(h); err != nil {
		return err
	}
	target, _ := consensus.CompactToTarget(h.Bits)
	work := consensus.BlockWork(target)
	work.Add(work, parent.Work)
	if work.Cmp(c.tip.Work) > 0 {
		return ErrHeaderNeedsBody
	}
	if err := c.makeSideRoomLocked(work, parent); err != nil {
		return err
	}
	n := &Node{Hash: hash, Header: *h, Height: parent.Height + 1, Work: work, Parent: parent, Status: StatusHeaderOnly}
	if err := c.db.Put(nodeKey(hash), encodeNode(n), nil); err != nil {
		return &StorageError{"write header", err}
	}
	c.addSideHeaderLocked(n)
	return nil
}

func (c *Chain) processBlockLocked(blk *consensus.Block, now int64, keepBody bool) error {
	hash := blk.Header.Hash()
	if n, known := c.nodes[hash]; known {
		if n.Status != StatusHeaderOnly {
			return ErrDuplicateBlock
		}
		return c.attachBodyLocked(n, blk)
	}
	parent, ok := c.nodes[blk.Header.PrevHash]
	if !ok {
		return consensus.ErrBlockUnknownParent
	}
	if parent.Status == StatusInvalid {
		return ErrInvalidAncestor
	}
	// A body that is not the one its header commits to is nobody's block:
	// nothing is stored or marked for it, whatever the header.
	if err := consensus.CheckBlockCommitments(blk); err != nil {
		return err
	}

	// Cheap contextual checks first, the Argon2id proof of work last (skipped
	// only for a header this Chain verified itself; see CheckHeaderPoW).
	if err := consensus.CheckHeaderContext(c.p, &blk.Header, parent.Hash, parent.Header.Time, parent.Height, MedianTimePast(parent), now); err != nil {
		return err
	}
	if err := c.CheckHeaderPoW(&blk.Header); err != nil {
		return err
	}
	if blk.SerializedSize() > consensus.MaxBlockSize {
		return consensus.ErrBlockTooLarge
	}

	target, _ := consensus.CompactToTarget(blk.Header.Bits)
	work := consensus.BlockWork(target)
	work.Add(work, parent.Work)
	n := &Node{Hash: hash, Header: blk.Header, Height: parent.Height + 1, Work: work, Parent: parent}

	extendsTip := parent == c.tip
	heavier := work.Cmp(c.tip.Work) > 0
	if !extendsTip && !heavier && !keepBody {
		// Light side chain: remember the header, drop the body.
		if err := c.makeSideRoomLocked(work, parent); err != nil {
			return err
		}
		n.Status = StatusHeaderOnly
		if err := c.db.Put(nodeKey(hash), encodeNode(n), nil); err != nil {
			return &StorageError{"write header", err}
		}
		c.addSideHeaderLocked(n)
		return nil
	}

	n.Status = StatusHaveData
	batch := new(leveldb.Batch)
	batch.Put(nodeKey(hash), encodeNode(n))
	batch.Put(blockKey(hash), blk.Serialize())
	if err := c.db.Write(batch, nil); err != nil {
		return &StorageError{"write block", err}
	}
	c.addNodeLocked(n)

	switch {
	case extendsTip:
		if err := c.connectTip(n, blk); err != nil {
			if IsStorage(err) || errors.Is(err, consensus.ErrBlockMutated) {
				// Not a verdict on the block: forget it so a retry can connect it.
				c.removeNodeLocked(n)
				return err
			}
			c.markInvalid(n)
			return err
		}
		return nil
	case heavier:
		return c.reorganizeFor(n)
	default:
		return nil // kept for a sync in progress; evaluated when the branch gets heavier
	}
}

// attachBodyLocked stores the body of a header-only node and re-evaluates
// fork choice.
func (c *Chain) attachBodyLocked(n *Node, blk *consensus.Block) error {
	if err := consensus.CheckBlockCommitments(blk); err != nil {
		return err // not this header's body: the header stays as it is
	}
	if blk.SerializedSize() > consensus.MaxBlockSize {
		return consensus.ErrBlockTooLarge
	}
	n.Status = StatusHaveData
	batch := new(leveldb.Batch)
	batch.Put(nodeKey(n.Hash), encodeNode(n))
	batch.Put(blockKey(n.Hash), blk.Serialize())
	if err := c.db.Write(batch, nil); err != nil {
		n.Status = StatusHeaderOnly
		return &StorageError{"write block", err}
	}
	c.sideHeaders--
	best := c.bestCandidateLocked()
	if best == nil {
		return nil
	}
	err := c.reorganize(best)
	if err == nil || errors.Is(err, ErrMissingBlockData) || IsStorage(err) {
		return err
	}
	// A consensus failure on some block of that branch: it has been marked
	// invalid. This body itself was fine, so report nothing against it
	// unless it is the invalid block or descends from one.
	if n.Status == StatusInvalid {
		return ErrInvalidAncestor
	}
	return nil
}

// reorganizeFor switches to the branch ending at n (which has more work than
// the tip), translating failures into the right verdict for n itself.
func (c *Chain) reorganizeFor(n *Node) error {
	err := c.reorganize(n)
	if err == nil || errors.Is(err, ErrMissingBlockData) || IsStorage(err) {
		return err
	}
	if n.Status == StatusInvalid {
		// n itself or an ancestor failed; both make n unacceptable.
		return err
	}
	return nil
}

// bestCandidateLocked returns the heaviest non-invalid node whose body is
// stored and which has more work than the tip, or nil. Among candidates of
// equal work the one that arrived first wins (as the tip itself wins over a
// later branch of equal work), not whichever map order happens to visit.
func (c *Chain) bestCandidateLocked() *Node {
	var best *Node
	for _, n := range c.nodes {
		if n.Status == StatusInvalid || n.Status == StatusHeaderOnly || n == c.tip {
			continue
		}
		if n.Work.Cmp(c.tip.Work) <= 0 {
			continue
		}
		if best == nil {
			best = n
			continue
		}
		if cmp := n.Work.Cmp(best.Work); cmp > 0 || (cmp == 0 && n.seq < best.seq) {
			best = n
		}
	}
	return best
}

// connectTip validates blk (child of the current tip) against the UTXO set
// and commits it atomically. Consensus errors are returned as-is; database
// failures as *StorageError.
func (c *Chain) connectTip(n *Node, blk *consensus.Block) error {
	ov := consensus.NewOverlay(dbView{c.db})
	if err := consensus.ConnectBlock(c.p, blk, n.Height, ov); err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	for op := range ov.Spent() {
		batch.Delete(coinKey(op))
	}
	for op, coin := range ov.Added() {
		batch.Put(coinKey(op), encodeCoin(coin))
	}
	prev := n.Status
	n.Status = StatusValid
	batch.Put(undoKey(n.Hash), encodeUndo(ov.Spent()))
	batch.Put(nodeKey(n.Hash), encodeNode(n))
	batch.Put(heightKey(n.Height), n.Hash[:])
	for i, tx := range blk.Txs {
		batch.Put(txKey(tx.TxID()), txLocValue(n.Hash, uint32(i)))
	}
	batch.Put([]byte(keyTip), n.Hash[:])
	if err := c.db.Write(batch, nil); err != nil {
		n.Status = prev
		return &StorageError{"write connect", err}
	}
	c.tip = n
	if int(n.Height) < len(c.main) {
		c.main = c.main[:n.Height]
	}
	c.main = append(c.main, n)
	return nil
}

// disconnectTip removes the current tip, restoring spent coins from undo
// data and deleting the coins it created.
func (c *Chain) disconnectTip() error {
	n := c.tip
	if n.Parent == nil {
		return errors.New("chain: cannot disconnect genesis")
	}
	blk, err := c.Block(n.Hash)
	if err != nil {
		return err
	}
	undoBytes, err := c.db.Get(undoKey(n.Hash), nil)
	if err != nil {
		return &StorageError{"read undo", err}
	}
	spent, err := decodeUndo(undoBytes)
	if err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	for _, tx := range blk.Txs {
		txid := tx.TxID()
		batch.Delete(txKey(txid))
		for i := range tx.Outputs {
			if tx.Outputs[i].Type == consensus.OutputTypeP2PK {
				batch.Delete(coinKey(consensus.OutPoint{TxID: txid, Index: uint32(i)}))
			}
		}
	}
	for op, coin := range spent {
		batch.Put(coinKey(op), encodeCoin(coin))
	}
	batch.Delete(heightKey(n.Height))
	batch.Put([]byte(keyTip), n.Parent.Hash[:])
	if err := c.db.Write(batch, nil); err != nil {
		return &StorageError{"write disconnect", err}
	}
	c.tip = n.Parent
	c.main = c.main[:n.Height]
	return nil
}

// reorganize switches the best chain to end at target, which has more work
// than the current tip.
//
// The statuses along the new branch are checked first, before any body is
// read: a branch with an invalid block fails at once, and one with
// header-only blocks returns *MissingDataError (oldest first) and changes
// nothing. Then every body of the branch is read back and checked against
// its header; one that is missing, does not decode or does not match (the
// disk is at fault, not the block) is forgotten, so that it is fetched
// again, and reported missing, still before anything changes. Only then is
// the chain switched, one block at a time, each body read again as it is
// connected (memory holds one block however deep the reorganisation). If a
// block on the new branch is invalid it is marked and the original chain is
// restored.
func (c *Chain) reorganize(target *Node) error {
	fork := c.tip
	other := target
	for fork != other {
		if fork.Height > other.Height {
			fork = fork.Parent
		} else if other.Height > fork.Height {
			other = other.Parent
		} else {
			fork = fork.Parent
			other = other.Parent
		}
	}

	var attach []*Node
	for n := target; n != fork; n = n.Parent {
		attach = append(attach, n)
	}
	for i, j := 0, len(attach)-1; i < j; i, j = i+1, j-1 {
		attach[i], attach[j] = attach[j], attach[i]
	}
	// Statuses only: nothing is read from disk for a branch that cannot be
	// connected yet.
	var missing []consensus.Hash
	for _, n := range attach {
		switch n.Status {
		case StatusInvalid:
			c.markInvalid(n)
			return ErrInvalidAncestor
		case StatusHeaderOnly:
			if len(missing) < maxMissingReported {
				missing = append(missing, n.Hash)
			}
		}
	}
	if len(missing) > 0 {
		return &MissingDataError{Hashes: missing}
	}
	// Every stored body, checked against its header (and not kept).
	for _, n := range attach {
		if _, err := c.storedBodyLocked(n); err != nil {
			if IsStorage(err) {
				return err
			}
			if len(missing) < maxMissingReported {
				missing = append(missing, n.Hash)
			}
		}
	}
	if len(missing) > 0 {
		return &MissingDataError{Hashes: missing}
	}

	var detached []*Node
	for n := c.tip; n != fork; n = n.Parent {
		detached = append(detached, n)
	}
	for c.tip != fork {
		if err := c.disconnectTip(); err != nil {
			return err
		}
	}
	for _, n := range attach {
		blk, err := c.storedBodyLocked(n)
		if err == nil {
			err = c.connectTip(n, blk)
		} else if !IsStorage(err) {
			// Unreadable since the check above: forgotten, fetch it again.
			err = &MissingDataError{Hashes: []consensus.Hash{n.Hash}}
		}
		if err == nil {
			continue
		}
		if IsStorage(err) {
			return err
		}
		// A body that is not this block (missing, mutated) says nothing
		// about it; any other failure is a verdict on the block.
		if !errors.Is(err, ErrMissingBlockData) && !errors.Is(err, consensus.ErrBlockMutated) {
			c.markInvalid(n)
		}
		if rerr := c.restoreLocked(fork, detached); rerr != nil {
			return rerr
		}
		return err
	}
	return nil
}

// storedBodyLocked reads n's stored body and checks it against n's header.
// A body that is missing, does not decode or does not match the header (the
// disk is at fault, not the block) is forgotten, so that it is fetched
// again, and ErrMissingBlockData returned; a database failure is returned
// as a *StorageError.
func (c *Chain) storedBodyLocked(n *Node) (*consensus.Block, error) {
	raw, err := c.RawBlock(n.Hash)
	if IsStorage(err) {
		return nil, err
	}
	var blk *consensus.Block
	if err == nil {
		blk, err = consensus.DecodeBlock(raw)
		if err == nil {
			err = consensus.CheckBlockCommitments(blk)
		}
	}
	if err != nil {
		if ferr := c.forgetBodyLocked(n); ferr != nil {
			return nil, ferr
		}
		return nil, ErrMissingBlockData
	}
	return blk, nil
}

// restoreLocked undoes a failed switch: back to fork, then the detached
// blocks (newest first in detached) connected again.
func (c *Chain) restoreLocked(fork *Node, detached []*Node) error {
	for c.tip != fork {
		if err := c.disconnectTip(); err != nil {
			return err
		}
	}
	for j := len(detached) - 1; j >= 0; j-- {
		blk, err := c.Block(detached[j].Hash)
		if err != nil {
			return err
		}
		if err := c.connectTip(detached[j], blk); err != nil {
			return fmt.Errorf("chain: failed to restore previous chain: %w", err)
		}
	}
	return nil
}

// forgetBodyLocked turns n back into a header-only node, so its body is
// fetched again. On a write failure n is left as it was.
func (c *Chain) forgetBodyLocked(n *Node) error {
	prev := n.Status
	n.Status = StatusHeaderOnly
	batch := new(leveldb.Batch)
	batch.Put(nodeKey(n.Hash), encodeNode(n))
	batch.Delete(blockKey(n.Hash))
	if err := c.db.Write(batch, nil); err != nil {
		n.Status = prev
		return &StorageError{"forget block body", err}
	}
	c.sideHeaders++
	c.pushLeafLocked(n)
	return nil
}

// markInvalid flags n and every known descendant as invalid, persistently,
// and drops their stored bodies. It walks the children index: O(size of
// n's subtree), not O(index size x depth).
func (c *Chain) markInvalid(n *Node) {
	batch := new(leveldb.Batch)
	flag := func(m *Node) {
		if m.Status == StatusHeaderOnly {
			c.sideHeaders--
		}
		m.Status = StatusInvalid
		batch.Put(nodeKey(m.Hash), encodeNode(m))
		batch.Delete(blockKey(m.Hash))
		batch.Delete(undoKey(m.Hash))
	}
	flag(n)
	stack := append([]*Node(nil), c.children[n.Hash]...)
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if m.Status != StatusInvalid {
			flag(m)
		}
		stack = append(stack, c.children[m.Hash]...)
	}
	_ = c.db.Write(batch, nil)
}
