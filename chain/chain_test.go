package chain

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
)

type key struct {
	priv  *btcec.PrivateKey
	xonly []byte
}

func newKey(t *testing.T) *key {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &key{priv: priv, xonly: schnorr.SerializePubKey(priv.PubKey())}
}

func (k *key) out(a consensus.Amount) consensus.TxOut {
	return consensus.TxOut{Amount: a, Type: consensus.OutputTypeP2PK, Payload: append([]byte{}, k.xonly...)}
}

func openTemp(t *testing.T) *Chain {
	t.Helper()
	c, err := Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// makeBlock builds and mines a child of parent paying the coinbase to k.
func makeBlock(t *testing.T, p *consensus.Params, parent *Node, k *key, txs []*consensus.Tx, fees consensus.Amount, tag byte) *consensus.Block {
	t.Helper()
	h := parent.Height + 1
	cb := &consensus.Tx{Version: 1, LockHeight: h,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs:   []consensus.TxOut{k.out(consensus.Subsidy(h) + fees)},
		Witnesses: [][]byte{{tag}},
	}
	blk := &consensus.Block{Txs: append([]*consensus.Tx{cb}, txs...)}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		t.Fatal(err)
	}
	blk.Header = consensus.Header{Version: 1, PrevHash: parent.Hash, TxRoot: txRoot, WitnessRoot: wRoot,
		Time: parent.Header.Time + 600, Bits: consensus.NextBits(p, parent.Header.Time, parent.Height)}
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, p.PowLimit) == nil {
			break
		}
	}
	return blk
}

func mustProcess(t *testing.T, c *Chain, blk *consensus.Block) *Node {
	t.Helper()
	if _, err := c.ProcessBlock(blk, -1); err != nil {
		t.Fatalf("process %s: %v", blk.Header.Hash(), err)
	}
	n, _ := c.Node(blk.Header.Hash())
	return n
}

// supplyMissing re-delivers the bodies a MissingDataError asked for.
func supplyMissing(t *testing.T, c *Chain, err error, bodies map[consensus.Hash]*consensus.Block) {
	t.Helper()
	var md *MissingDataError
	if !errors.As(err, &md) {
		t.Fatalf("expected MissingDataError, got %v", err)
	}
	for _, h := range md.Hashes {
		blk, ok := bodies[h]
		if !ok {
			t.Fatalf("asked for unknown body %s", h)
		}
		// Each body may complete the branch or reveal that others are still
		// missing; both are fine while we are supplying them in order.
		if _, err := c.ProcessBlock(blk, -1); err != nil && !errors.Is(err, ErrMissingBlockData) {
			t.Fatalf("supply %s: %v", h, err)
		}
	}
}

func TestOpenCreatesGenesis(t *testing.T) {
	c := openTemp(t)
	tip := c.Tip()
	if tip.Height != 0 || tip.Hash != consensus.RegTest.GenesisHash || tip.Status != StatusValid {
		t.Fatal("bad genesis tip")
	}
	if _, err := c.Block(tip.Hash); err != nil {
		t.Fatal("genesis block not stored")
	}
}

func TestExtendAndPersist(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	k := newKey(t)
	var last *consensus.Block
	for i := 0; i < 5; i++ {
		last = makeBlock(t, c.Params(), c.Tip(), k, nil, 0, 0)
		changed, err := c.ProcessBlock(last, -1)
		if err != nil || !changed {
			t.Fatalf("block %d: changed=%v err=%v", i+1, changed, err)
		}
	}
	if c.Tip().Height != 5 {
		t.Fatal("height")
	}
	if _, err := c.ProcessBlock(last, -1); !errors.Is(err, ErrDuplicateBlock) {
		t.Fatalf("dup: %v", err)
	}
	cbOut := consensus.OutPoint{TxID: last.Txs[0].TxID(), Index: 0}
	if _, ok := c.LookupCoin(cbOut); !ok {
		t.Fatal("coin missing")
	}
	tipHash := c.Tip().Hash
	tipWork := c.Tip().Work
	c.Close()

	c2, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if c2.Tip().Hash != tipHash || c2.Tip().Height != 5 {
		t.Fatal("tip not persisted")
	}
	if c2.Tip().Work.Cmp(tipWork) != 0 {
		t.Fatal("work not recomputed")
	}
	if _, ok := c2.LookupCoin(cbOut); !ok {
		t.Fatal("coin not persisted")
	}
	n, ok := c2.NodeAtHeight(3)
	if !ok || n.Height != 3 || !c2.IsMainChain(n) {
		t.Fatal("height index")
	}
	if _, ok := c2.NodeAtHeight(6); ok {
		t.Fatal("height beyond tip")
	}
}

func TestUnknownParentAndSideChain(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	orphanParent := &Node{Hash: consensus.TaggedHash("x", nil), Height: 10, Header: consensus.Header{Time: c.Tip().Header.Time}}
	orphan := makeBlock(t, c.Params(), orphanParent, k, nil, 0, 0)
	if _, err := c.ProcessBlock(orphan, -1); !errors.Is(err, consensus.ErrBlockUnknownParent) {
		t.Fatalf("orphan: %v", err)
	}

	// Main: g -> a1 -> a2. Side: g -> b1 (less work): header kept, body dropped.
	a1 := mustProcess(t, c, makeBlock(t, c.Params(), c.Tip(), k, nil, 0, 1))
	a2 := mustProcess(t, c, makeBlock(t, c.Params(), a1, k, nil, 0, 2))
	g, _ := c.Node(consensus.RegTest.GenesisHash)
	b1blk := makeBlock(t, c.Params(), g, k, nil, 0, 3)
	changed, err := c.ProcessBlock(b1blk, -1)
	if err != nil || changed {
		t.Fatalf("side chain: changed=%v err=%v", changed, err)
	}
	b1, _ := c.Node(b1blk.Header.Hash())
	if b1.Status != StatusHeaderOnly || c.IsMainChain(b1) || !c.IsMainChain(a2) {
		t.Fatal("side chain bookkeeping")
	}
	if c.HasData(b1.Hash) {
		t.Fatal("side-chain body should not be stored")
	}
	if _, err := c.Block(b1.Hash); !errors.Is(err, ErrMissingBlockData) {
		t.Fatalf("body lookup: %v", err)
	}
	// Re-delivering the same body while it is still lighter keeps it header-only.
	if _, err := c.ProcessBlock(b1blk, -1); err != nil {
		t.Fatal(err)
	}
	// With keepBody (headers-first sync) the body is stored even on a side chain.
	g2 := makeBlock(t, c.Params(), g, k, nil, 0, 4)
	if _, err := c.ProcessBlockOpt(g2, -1, true); err != nil {
		t.Fatal(err)
	}
	if !c.HasData(g2.Header.Hash()) {
		t.Fatal("keepBody ignored")
	}
}

func TestReorg(t *testing.T) {
	c := openTemp(t)
	ka, kb := newKey(t), newKey(t)
	g := c.Tip()
	a1blk := makeBlock(t, c.Params(), g, ka, nil, 0, 1)
	a1 := mustProcess(t, c, a1blk)
	a2blk := makeBlock(t, c.Params(), a1, ka, nil, 0, 2)
	mustProcess(t, c, a2blk)
	a1coin := consensus.OutPoint{TxID: a1blk.Txs[0].TxID(), Index: 0}
	a2coin := consensus.OutPoint{TxID: a2blk.Txs[0].TxID(), Index: 0}

	// Side: g -> b1 -> b2 -> b3. b1 and b2 are lighter: header-only.
	b1blk := makeBlock(t, c.Params(), g, kb, nil, 0, 3)
	b1 := mustProcess(t, c, b1blk)
	b2blk := makeBlock(t, c.Params(), b1, kb, nil, 0, 4)
	b2 := mustProcess(t, c, b2blk)
	if c.Tip().Hash != a2blk.Header.Hash() {
		t.Fatal("equal work must not reorg")
	}
	b3blk := makeBlock(t, c.Params(), b2, kb, nil, 0, 5)
	changed, err := c.ProcessBlock(b3blk, -1)
	if changed || !errors.Is(err, ErrMissingBlockData) {
		t.Fatalf("heavier branch without bodies: changed=%v err=%v", changed, err)
	}
	bodies := map[consensus.Hash]*consensus.Block{b1blk.Header.Hash(): b1blk, b2blk.Header.Hash(): b2blk}
	supplyMissing(t, c, err, bodies)
	if c.Tip().Hash != b3blk.Header.Hash() || c.Tip().Height != 3 {
		t.Fatalf("tip after reorg: height %d", c.Tip().Height)
	}
	if _, ok := c.LookupCoin(a1coin); ok {
		t.Fatal("a1 coin should be gone")
	}
	if _, ok := c.LookupCoin(a2coin); ok {
		t.Fatal("a2 coin should be gone")
	}
	for i, blk := range []*consensus.Block{b1blk, b2blk, b3blk} {
		if _, ok := c.LookupCoin(consensus.OutPoint{TxID: blk.Txs[0].TxID()}); !ok {
			t.Fatalf("b%d coin missing", i+1)
		}
	}
	n1, _ := c.NodeAtHeight(1)
	if n1.Hash != b1blk.Header.Hash() {
		t.Fatal("height index not updated")
	}
	a1n, _ := c.Node(a1blk.Header.Hash())
	if a1n.Status != StatusValid || c.IsMainChain(a1n) {
		t.Fatal("old branch should remain valid but off main chain")
	}

	// Reorg back: a3 (equal work: header-only), a4 (heavier: needs a3's body).
	a2n, _ := c.Node(a2blk.Header.Hash())
	a3blk := makeBlock(t, c.Params(), a2n, ka, nil, 0, 6)
	a3 := mustProcess(t, c, a3blk)
	if c.Tip().Hash != b3blk.Header.Hash() || a3.Status != StatusHeaderOnly {
		t.Fatal("3 vs 3 must not reorg")
	}
	a4blk := makeBlock(t, c.Params(), a3, ka, nil, 0, 7)
	_, err = c.ProcessBlock(a4blk, -1)
	supplyMissing(t, c, err, map[consensus.Hash]*consensus.Block{a3blk.Header.Hash(): a3blk})
	if c.Tip().Height != 4 || c.Tip().Hash != a4blk.Header.Hash() || !c.IsMainChain(a1n) {
		t.Fatal("reorg back failed")
	}
	if _, ok := c.LookupCoin(a1coin); !ok {
		t.Fatal("a1 coin should be restored")
	}
}

func TestReorgWithSpendsAndUndo(t *testing.T) {
	c := openTemp(t)
	k, r := newKey(t), newKey(t)
	var first *consensus.Block
	for i := 0; i < 101; i++ {
		blk := makeBlock(t, c.Params(), c.Tip(), k, nil, 0, byte(i))
		if first == nil {
			first = blk
		}
		mustProcess(t, c, blk)
	}
	fork := c.Tip() // height 101
	op := consensus.OutPoint{TxID: first.Txs[0].TxID(), Index: 0}
	coin, _ := c.LookupCoin(op)

	spend := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}, Outputs: []consensus.TxOut{r.out(49 * consensus.UnitsPerCoin)}, Witnesses: make([][]byte, 1)}
	msg := consensus.SigHash(c.Params().ChainTag, spend, []consensus.TxOut{coin.Out}, 0)
	sig, _ := schnorr.Sign(k.priv, msg[:])
	spend.Witnesses[0] = sig.Serialize()
	mustProcess(t, c, makeBlock(t, c.Params(), fork, k, []*consensus.Tx{spend}, 1*consensus.UnitsPerCoin, 200))
	newOp := consensus.OutPoint{TxID: spend.TxID(), Index: 0}
	if _, ok := c.LookupCoin(op); ok {
		t.Fatal("spent coin present")
	}
	if _, ok := c.LookupCoin(newOp); !ok {
		t.Fatal("new coin missing")
	}

	// Branch B from fork, longer: b1 header-only, b2 heavier -> needs b1 body.
	b1blk := makeBlock(t, c.Params(), fork, r, nil, 0, 201)
	b1 := mustProcess(t, c, b1blk)
	b2blk := makeBlock(t, c.Params(), b1, r, nil, 0, 202)
	_, err := c.ProcessBlock(b2blk, -1)
	supplyMissing(t, c, err, map[consensus.Hash]*consensus.Block{b1blk.Header.Hash(): b1blk})
	if c.Tip().Height != 103 {
		t.Fatal("reorg to B failed")
	}
	restored, ok := c.LookupCoin(op)
	if !ok || restored.Out.Amount != coin.Out.Amount || !restored.Coinbase || restored.Height != 1 {
		t.Fatal("undo did not restore the coin exactly")
	}
	if _, ok := c.LookupCoin(newOp); ok {
		t.Fatal("branch A output should be gone")
	}
}

func TestInvalidBlockOnReorgIsRolledBack(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	g := c.Tip()
	a1 := mustProcess(t, c, makeBlock(t, c.Params(), g, k, nil, 0, 1))
	tipBefore := c.Tip().Hash

	// Side branch b1 (header-only) -> bad (overpaying coinbase, heavier).
	b1blk := makeBlock(t, c.Params(), g, k, nil, 0, 2)
	b1 := mustProcess(t, c, b1blk)
	bad := makeBlock(t, c.Params(), b1, k, nil, 1, 3) // fees=1 with no txs -> overpays
	_, err := c.ProcessBlock(bad, -1)
	if !errors.Is(err, ErrMissingBlockData) {
		t.Fatalf("expected missing body for b1 first, got %v", err)
	}
	// Supplying b1 triggers the reorg attempt; bad fails and is marked invalid;
	// the original chain is restored. b1 itself was fine: no error for it.
	if _, err := c.ProcessBlock(b1blk, -1); err != nil {
		t.Fatalf("supplying a valid body must not fail: %v", err)
	}
	if c.Tip().Hash != tipBefore || c.Tip().Height != 1 {
		t.Fatal("original chain not restored")
	}
	badN, _ := c.Node(bad.Header.Hash())
	if badN.Status != StatusInvalid {
		t.Fatal("bad block not marked invalid")
	}
	if c.HasData(badN.Hash) {
		t.Fatal("invalid block body should be dropped")
	}
	b1n, _ := c.Node(b1blk.Header.Hash())
	if b1n.Status != StatusValid {
		t.Fatalf("b1 should stay valid (connected once), got %d", b1n.Status)
	}
	// Children of an invalid block are refused immediately.
	child := makeBlock(t, c.Params(), badN, k, nil, 0, 4)
	if _, err := c.ProcessBlock(child, -1); !errors.Is(err, ErrInvalidAncestor) {
		t.Fatalf("child of invalid: %v", err)
	}
	// A directly invalid block extending the tip is reported as such.
	bad2 := makeBlock(t, c.Params(), a1, k, nil, 1, 5)
	if _, err := c.ProcessBlock(bad2, -1); !errors.Is(err, consensus.ErrBlockBadCoinbaseValue) {
		t.Fatalf("direct invalid: %v", err)
	}
	mustProcess(t, c, makeBlock(t, c.Params(), a1, k, nil, 0, 6))
	if c.Tip().Height != 2 {
		t.Fatal("extend after rollback")
	}
}

func TestBadHeaderRejectedBeforeStorage(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	blk := makeBlock(t, c.Params(), c.Tip(), k, nil, 0, 1)
	blk.Header.Time = c.Tip().Header.Time // == MTP of genesis alone -> too old
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, c.Params().PowLimit) == nil {
			break
		}
	}
	if _, err := c.ProcessBlock(blk, -1); !errors.Is(err, consensus.ErrTimeTooOld) {
		t.Fatalf("got %v", err)
	}
	if _, known := c.Node(blk.Header.Hash()); known {
		t.Fatal("header-invalid block must not enter the index")
	}
}

func TestTxIndex(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	k, kb := newKey(t), newKey(t)
	g := c.Tip()
	if _, n, err := c.Transaction(consensus.RegTest.Genesis.Txs[0].TxID()); err != nil || n.Height != 0 {
		t.Fatalf("genesis tx: %v", err)
	}
	a1blk := makeBlock(t, c.Params(), g, k, nil, 0, 1)
	a1 := mustProcess(t, c, a1blk)
	a2blk := makeBlock(t, c.Params(), a1, k, nil, 0, 2)
	mustProcess(t, c, a2blk)
	txid := a2blk.Txs[0].TxID()
	tx, n, err := c.Transaction(txid)
	if err != nil || n.Height != 2 || tx.TxID() != txid {
		t.Fatalf("lookup: %v", err)
	}
	if _, _, err := c.Transaction(consensus.TaggedHash("nope", nil)); !errors.Is(err, ErrTxNotFound) {
		t.Fatal("missing tx should be not found")
	}

	// Reorg to a heavier branch with a different payout key.
	b1blk := makeBlock(t, c.Params(), g, kb, nil, 0, 3)
	b1 := mustProcess(t, c, b1blk)
	b2blk := makeBlock(t, c.Params(), b1, kb, nil, 0, 4)
	b2 := mustProcess(t, c, b2blk)
	b3blk := makeBlock(t, c.Params(), b2, kb, nil, 0, 5)
	_, err = c.ProcessBlock(b3blk, -1)
	supplyMissing(t, c, err, map[consensus.Hash]*consensus.Block{b1blk.Header.Hash(): b1blk, b2blk.Header.Hash(): b2blk})
	if c.Tip().Hash != b3blk.Header.Hash() {
		t.Fatal("reorg")
	}
	if _, _, ok := c.TxLocation(txid); ok {
		t.Fatal("orphaned tx still indexed")
	}
	if _, n, err := c.Transaction(b3blk.Txs[0].TxID()); err != nil || n.Height != 3 {
		t.Fatal("new branch tx not indexed")
	}

	// Rebuild when the marker is missing (upgrade path).
	if err := c.db.Delete([]byte(keyTxIndexed), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.db.Delete(txKey(b3blk.Txs[0].TxID()), nil); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c2, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, n, err := c2.Transaction(b3blk.Txs[0].TxID()); err != nil || n.Height != 3 {
		t.Fatal("index not rebuilt on open")
	}
	if _, _, ok := c2.TxLocation(txid); ok {
		t.Fatal("rebuilt index contains orphaned tx")
	}
}

func TestSideHeaderCapAndStatusPersistence(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	k := newKey(t)
	g := c.Tip()
	mustProcess(t, c, makeBlock(t, c.Params(), g, k, nil, 0, 1))
	side := makeBlock(t, c.Params(), g, k, nil, 0, 2)
	mustProcess(t, c, side)
	c.Close()
	c2, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	n, ok := c2.Node(side.Header.Hash())
	if !ok || n.Status != StatusHeaderOnly || c2.sideHeaders != 1 {
		t.Fatalf("header-only status not persisted: %+v count=%d", n, c2.sideHeaders)
	}
}

func TestSubscribe(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	var got []uint32
	c.Subscribe(func(tip *Node) { got = append(got, tip.Height) })
	mustProcess(t, c, makeBlock(t, c.Params(), c.Tip(), k, nil, 0, 1))
	mustProcess(t, c, makeBlock(t, c.Params(), c.Tip(), k, nil, 0, 2))
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("subscriber calls %v", got)
	}
}

// ProcessHeader keeps a header whose branch has no more work than the tip
// header-only (under MaxSideHeaders) and refuses one that would make its
// branch heavier: that block needs its body. The branch can still win
// later, its bodies fetched through MissingDataError.
func TestProcessHeaderKeepsLightHeadersOnly(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	g := c.Tip()
	a1 := mustProcess(t, c, makeBlock(t, c.Params(), g, k, nil, 0, 1))
	mustProcess(t, c, makeBlock(t, c.Params(), a1, k, nil, 0, 2))

	b1 := makeBlock(t, c.Params(), g, k, nil, 0, 3)
	if err := c.ProcessHeader(&b1.Header, -1); err != nil {
		t.Fatal(err)
	}
	if err := c.ProcessHeader(&b1.Header, -1); !errors.Is(err, ErrDuplicateBlock) {
		t.Fatalf("again: %v", err)
	}
	b1n, ok := c.Node(b1.Header.Hash())
	if !ok || b1n.Status != StatusHeaderOnly || c.sideHeaders != 1 || c.HasData(b1n.Hash) {
		t.Fatalf("light header not kept header-only: %+v side=%d", b1n, c.sideHeaders)
	}
	b2 := makeBlock(t, c.Params(), b1n, k, nil, 0, 4) // equal work: still light
	if err := c.ProcessHeader(&b2.Header, -1); err != nil {
		t.Fatal(err)
	}
	b2n, _ := c.Node(b2.Header.Hash())
	b3 := makeBlock(t, c.Params(), b2n, k, nil, 0, 5) // heavier than the tip
	if err := c.ProcessHeader(&b3.Header, -1); !errors.Is(err, ErrHeaderNeedsBody) {
		t.Fatalf("heavier header: %v", err)
	}
	if _, ok := c.Node(b3.Header.Hash()); ok {
		t.Fatal("heavier header stored without its body")
	}
	// Contextual rules apply: wrong bits, unknown parent.
	bad := b2.Header
	bad.Bits--
	if err := c.ProcessHeader(&bad, -1); err == nil {
		t.Fatal("header with wrong bits kept")
	}
	orphan := b3.Header
	orphan.PrevHash = consensus.TaggedHash("nowhere", nil)
	if err := c.ProcessHeader(&orphan, -1); !errors.Is(err, consensus.ErrBlockUnknownParent) {
		t.Fatalf("orphan header: %v", err)
	}
	// The cap.
	c.mu.Lock()
	c.sideHeaders = MaxSideHeaders
	c.mu.Unlock()
	if err := c.ProcessHeader(&makeBlock(t, c.Params(), g, k, nil, 0, 6).Header, -1); !errors.Is(err, ErrTooManyHeaders) {
		t.Fatalf("over the cap: %v", err)
	}
	c.mu.Lock()
	c.sideHeaders = 2
	c.mu.Unlock()
	// The branch wins once its heavier block arrives with its body.
	_, err := c.ProcessBlock(b3, -1)
	supplyMissing(t, c, err, map[consensus.Hash]*consensus.Block{b1.Header.Hash(): b1, b2.Header.Hash(): b2})
	if c.Tip().Hash != b3.Header.Hash() {
		t.Fatalf("tip %d, want the b branch", c.Tip().Height)
	}
}

// CheckHeaderPoW remembers only headers that passed, by their full hash.
func TestCheckHeaderPoWMemo(t *testing.T) {
	c := openTemp(t)
	good := makeBlock(t, c.Params(), c.Tip(), newKey(t), nil, 0, 1).Header
	if c.pow.has(good.Hash()) {
		t.Fatal("remembered before checking")
	}
	if err := c.CheckHeaderPoW(&good); err != nil || !c.pow.has(good.Hash()) {
		t.Fatalf("good header: %v, remembered %v", err, c.pow.has(good.Hash()))
	}
	bad := good
	for n := uint64(0); ; n++ {
		bad.Nonce = n
		if consensus.CheckProofOfWork(&bad, c.Params().PowLimit) != nil {
			break
		}
	}
	for i := 0; i < 2; i++ {
		if err := c.CheckHeaderPoW(&bad); err == nil || c.pow.has(bad.Hash()) {
			t.Fatalf("bad header: %v, remembered %v", err, c.pow.has(bad.Hash()))
		}
	}
	m := newPowMemo(3)
	var hs []consensus.Hash
	for i := 0; i < 5; i++ {
		h := consensus.TaggedHash("memo", []byte{byte(i)})
		hs = append(hs, h)
		m.add(h)
	}
	if m.has(hs[0]) || m.has(hs[1]) || !m.has(hs[2]) || !m.has(hs[4]) || len(m.set) != 3 {
		t.Fatal("memo bound")
	}
}

// Among branches of equal work, the one whose blocks arrived first wins,
// whatever the map order (repeated: map order is random).
func TestEqualWorkCandidatesFirstSeenWins(t *testing.T) {
	for round := 0; round < 8; round++ {
		c := openTemp(t)
		k := newKey(t)
		g := c.Tip()
		mustProcess(t, c, makeBlock(t, c.Params(), g, k, nil, 0, 1))
		b1 := makeBlock(t, c.Params(), g, k, nil, 0, 2)
		if _, err := c.ProcessBlock(b1, -1); err != nil {
			t.Fatal(err)
		}
		b1n, _ := c.Node(b1.Header.Hash())
		b2 := makeBlock(t, c.Params(), b1n, k, nil, 0, 3)
		if _, err := c.ProcessBlock(b2, -1); !errors.Is(err, ErrMissingBlockData) {
			t.Fatalf("b2: %v", err)
		}
		c1 := makeBlock(t, c.Params(), g, k, nil, 0, 4)
		if _, err := c.ProcessBlock(c1, -1); err != nil {
			t.Fatal(err)
		}
		c1n, _ := c.Node(c1.Header.Hash())
		c2 := makeBlock(t, c.Params(), c1n, k, nil, 0, 5)
		if _, err := c.ProcessBlock(c2, -1); !errors.Is(err, ErrMissingBlockData) {
			t.Fatalf("c2: %v", err)
		}
		// Both bodies arrive, c's first: b2 (seen first) is still the choice.
		if _, err := c.ProcessBlock(c1, -1); err != nil && !errors.Is(err, ErrMissingBlockData) {
			t.Fatal(err)
		}
		if _, err := c.ProcessBlock(b1, -1); err != nil {
			t.Fatal(err)
		}
		if c.Tip().Hash != b2.Header.Hash() {
			t.Fatalf("round %d: tip is %s, want b2 (first seen of equal work)", round, c.Tip().Hash)
		}
	}
}

// markInvalid flags exactly the subtree, through the children index (it
// used to rescan the whole index once per level of depth).
func TestMarkInvalidUsesChildren(t *testing.T) {
	c := openTemp(t)
	g := c.Tip()
	c.mu.Lock()
	defer c.mu.Unlock()
	seq := 0
	grow := func(from *Node, n int) []*Node {
		var out []*Node
		for i := 0; i < n; i++ {
			seq++
			nd := &Node{Hash: consensus.TaggedHash("inv", []byte{byte(seq), byte(seq >> 8), byte(seq >> 16)}), Height: from.Height + 1,
				Work: new(big.Int).Add(from.Work, big.NewInt(1)), Parent: from, Status: StatusHeaderOnly}
			c.addNodeLocked(nd)
			c.sideHeaders++
			out = append(out, nd)
			from = nd
		}
		return out
	}
	long := grow(g, 20_000)
	sibling := grow(g, 100)
	fork := grow(long[100], 10)
	start := time.Now()
	c.markInvalid(long[0])
	el := time.Since(start)
	for _, set := range [][]*Node{long, fork} {
		for _, n := range set {
			if n.Status != StatusInvalid {
				t.Fatalf("descendant at height %d not invalid", n.Height)
			}
		}
	}
	for _, n := range sibling {
		if n.Status != StatusHeaderOnly {
			t.Fatal("unrelated branch flagged")
		}
	}
	if c.sideHeaders != len(sibling) {
		t.Fatalf("side headers %d, want %d", c.sideHeaders, len(sibling))
	}
	t.Logf("flagged %d descendants in %s (including the database write)", len(long)+len(fork), el)
}

// The body path skips the Argon2id only for a header this Chain verified
// itself: here a header with no valid proof of work is planted in the memo
// (which in production only CheckHeaderPoW fills, after a passing check)
// to show processBlock consults it; without the entry it is refused.
func TestProcessBlockUsesPoWMemo(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	blk := makeBlock(t, c.Params(), c.Tip(), k, nil, 0, 1)
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, c.Params().PowLimit) != nil {
			break
		}
	}
	if _, err := c.ProcessBlock(blk, -1); !errors.Is(err, consensus.ErrHighHash) {
		t.Fatalf("block without proof of work: %v", err)
	}
	c.pow.add(blk.Header.Hash())
	if _, err := c.ProcessBlock(blk, -1); err != nil {
		t.Fatalf("remembered header checked again: %v", err)
	}
}
