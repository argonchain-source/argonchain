package mempool

import (
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
)

// Review D, mempool revalidation cost: every new tip re-checked every pool
// entry against the chain under the pool lock, and every block template
// ordered the whole pool. A block connect now touches only what the block
// confirms or conflicts with (a full Revalidate is kept for reorganisations
// and disconnects), and Select stops ordering once the template is full and
// reuses its result while the pool is unchanged.

// mineOn builds and mines (without processing) a child of parent.
func (h *harness) mineOn(parent *chain.Node, txs []*consensus.Tx, fees consensus.Amount, tag byte) *consensus.Block {
	h.t.Helper()
	height := parent.Height + 1
	cb := &consensus.Tx{Version: 1, LockHeight: height,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs:   []consensus.TxOut{h.k.out(consensus.Subsidy(height) + fees)},
		Witnesses: [][]byte{{byte(height), tag}},
	}
	blk := &consensus.Block{Txs: append([]*consensus.Tx{cb}, txs...)}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		h.t.Fatal(err)
	}
	blk.Header = consensus.Header{Version: 1, PrevHash: parent.Hash, TxRoot: txRoot, WitnessRoot: wRoot,
		Time: parent.Header.Time + 600, Bits: consensus.NextBits(consensus.RegTest, parent.Header.Time, parent.Height)}
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, consensus.RegTest.PowLimit) == nil {
			break
		}
	}
	return blk
}

func (h *harness) fullRevalidations() int {
	h.pool.mu.Lock()
	defer h.pool.mu.Unlock()
	return h.pool.fullRevalidations
}

// A block connect removes exactly what the full Revalidate would (the
// confirmed transaction, a conflict and the conflict's child; the confirmed
// transaction's child and an unrelated one stay) without re-checking the
// pool; a reorganisation still gets the full Revalidate.
func TestReviewD_BlockConnectDoesNotRecheckPool(t *testing.T) {
	h := newHarness(t, 104)
	a, b := newKey(t), newKey(t)
	op0, out0 := h.matureCoin(0)
	op1, out1 := h.matureCoin(1)
	op2, out2 := h.matureCoin(2)
	op3, out3 := h.matureCoin(3)
	txA := h.spend(h.k, op0, out0, a.out(49*coin))
	txAC := h.spend(a, consensus.OutPoint{TxID: txA.TxID()}, txA.Outputs[0], a.out(48*coin))
	txB := h.spend(h.k, op1, out1, b.out(49*coin))
	txBC := h.spend(b, consensus.OutPoint{TxID: txB.TxID()}, txB.Outputs[0], b.out(48*coin))
	txD := h.spend(h.k, op2, out2, a.out(49*coin))
	for _, tx := range []*consensus.Tx{txA, txAC, txB, txBC, txD} {
		if err := h.pool.Accept(tx); err != nil {
			t.Fatal(err)
		}
	}
	txB2 := h.spend(h.k, op1, out1, a.out(47*coin)) // conflicts with txB
	before := h.fullRevalidations()
	h.mine([]*consensus.Tx{txA, txB2}, 4*coin)
	if got := h.fullRevalidations(); got != before {
		t.Errorf("a block connect re-checked the whole pool (%d full revalidations)", got-before)
	}
	for _, c := range []struct {
		name string
		tx   *consensus.Tx
		want bool
	}{{"confirmed", txA, false}, {"child of confirmed", txAC, true}, {"conflict", txB, false},
		{"child of conflict", txBC, false}, {"unrelated", txD, true}} {
		if h.pool.Has(c.tx.TxID()) != c.want {
			t.Errorf("%s: in pool %v, want %v", c.name, !c.want, c.want)
		}
	}
	// What is left is exactly what a full Revalidate keeps, and mines.
	h.pool.Revalidate()
	if h.pool.Count() != 2 {
		t.Fatalf("full Revalidate changed the pool: %d entries", h.pool.Count())
	}
	sel, fees := h.pool.Select(consensus.MaxBlockSize)
	if len(sel) != 2 {
		t.Fatalf("selected %d", len(sel))
	}
	before = h.fullRevalidations()
	tip := h.mine(sel, fees)
	if h.pool.Count() != 0 || h.fullRevalidations() != before {
		t.Fatalf("after mining the template: %d left, %d full revalidations", h.pool.Count(), h.fullRevalidations()-before)
	}

	// A reorganisation replacing the tip: full Revalidate against the new chain.
	txE := h.spend(h.k, op3, out3, a.out(49*coin))
	if err := h.pool.Accept(txE); err != nil {
		t.Fatal(err)
	}
	tipNode, _ := h.ch.Node(tip.Header.Hash())
	side1 := h.mineOn(tipNode.Parent, nil, 0, 1)
	if _, err := h.ch.ProcessBlockOpt(side1, -1, true); err != nil {
		t.Fatal(err)
	}
	s1, _ := h.ch.Node(side1.Header.Hash())
	before = h.fullRevalidations()
	if _, err := h.ch.ProcessBlock(h.mineOn(s1, nil, 0, 2), -1); err != nil {
		t.Fatal(err)
	}
	if h.ch.Tip().Parent != s1 {
		t.Fatal("no reorganisation")
	}
	if h.fullRevalidations() != before+1 || !h.pool.Has(txE.TxID()) || h.pool.Count() != 1 {
		t.Fatalf("after the reorg: %d full revalidations, pool %d", h.fullRevalidations()-before, h.pool.Count())
	}
}

// referenceSelect is Select as it was: order the whole pool, then fill.
func referenceSelect(p *Pool, maxBytes int) ([]*consensus.Tx, consensus.Amount) {
	var txs []*consensus.Tx
	var fees consensus.Amount
	used := 0
	included := map[consensus.Hash]struct{}{}
	for _, e := range p.topological() {
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

// The lazy, cached Select returns exactly what ordering the whole pool did,
// for random pools of dependent transactions and many size budgets, and
// follows every change to the pool.
func TestReviewD_SelectMatchesFullOrdering(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	p := &Pool{txs: map[consensus.Hash]*Entry{}, spends: map[consensus.OutPoint]consensus.Hash{}}
	now := time.Now()
	var ids []consensus.Hash
	add := func(i int) {
		id := consensus.TaggedHash("sel", []byte{byte(i), byte(i >> 8), byte(i >> 16)})
		e := &Entry{Tx: &consensus.Tx{Version: uint32(i)}, TxID: id, Fee: consensus.Amount(100 + rng.Intn(5000)),
			Size: 100 + rng.Intn(900), Added: now.Add(time.Duration(rng.Intn(50)) * time.Second),
			parents: map[consensus.Hash]struct{}{}, children: map[consensus.Hash]struct{}{}}
		for k := rng.Intn(3); k > 0 && len(ids) > 0; k-- {
			parent := ids[rng.Intn(len(ids))]
			if _, live := p.txs[parent]; !live {
				continue
			}
			e.parents[parent] = struct{}{}
			p.txs[parent].children[id] = struct{}{}
		}
		p.txs[id] = e
		p.bytes += e.Size
		p.gen++
		ids = append(ids, id)
	}
	same := func(budget int) {
		t.Helper()
		got, gf := p.Select(budget)
		want, wf := referenceSelect(p, budget)
		if gf != wf || len(got) != len(want) {
			t.Fatalf("budget %d: %d txs fee %d, want %d txs fee %d", budget, len(got), gf, len(want), wf)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("budget %d: entry %d differs", budget, i)
			}
		}
	}
	for i := 0; i < 3000; i++ {
		add(i)
	}
	for _, budget := range []int{0, 99, 100, 1000, 5000, 77_777, 300_000, consensus.MaxBlockSize, 10 * consensus.MaxBlockSize} {
		same(budget)
		same(budget) // cached
	}
	// Changes to the pool are seen.
	for i := 0; i < 50; i++ {
		p.remove(ids[rng.Intn(len(ids))])
		add(3000 + i)
		same(200_000)
	}
}

// TestReviewD_RevalidateCostScales measures the work done under the pool
// lock on a new tip and for a block template. A full Revalidate is O(pool);
// a block connect is O(block). The SPEC §10 cap of 300 MB holds about 2
// million minimal transactions; this test fills N and reports the cost.
func TestReviewD_RevalidateCostScales(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	const fanouts, perFan = 8, 2000 // 16,000 pool transactions
	h := newHarness(t, 100+fanouts)
	var fans []*consensus.Tx
	for i := 0; i < fanouts; i++ {
		op, out := h.matureCoin(i)
		outs := make([]consensus.TxOut, perFan)
		each := (out.Amount - 200_000) / perFan
		for j := range outs {
			outs[j] = h.k.out(each)
		}
		fans = append(fans, h.spend(h.k, op, out, outs...))
	}
	var fees consensus.Amount
	h.mine(fans[:4], fees)
	h.mine(fans[4:], fees)
	start := time.Now()
	var first *consensus.Tx
	n := 0
	for _, f := range fans {
		fid := f.TxID()
		for j := range f.Outputs {
			op := consensus.OutPoint{TxID: fid, Index: uint32(j)}
			tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}},
				Outputs: []consensus.TxOut{h.k.out(f.Outputs[j].Amount - 1000)}, Witnesses: make([][]byte, 1)}
			msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, []consensus.TxOut{f.Outputs[j]}, 0)
			sig, _ := schnorr.Sign(h.k.priv, msg[:])
			tx.Witnesses[0] = sig.Serialize()
			if err := h.pool.Accept(tx); err != nil {
				t.Fatalf("accept %d: %v", n, err)
			}
			if first == nil {
				first = tx
			}
			n++
		}
	}
	t.Logf("filled %d txs (%d bytes) in %s", h.pool.Count(), h.pool.Bytes(), time.Since(start))

	t0 := time.Now()
	h.pool.Revalidate()
	rev := time.Since(t0)
	t0 = time.Now()
	h.pool.Select(consensus.MaxBlockSize)
	sel := time.Since(t0)
	t0 = time.Now()
	h.pool.Select(consensus.MaxBlockSize)
	selAgain := time.Since(t0)

	// A block confirming one pool transaction, timed through ProcessBlock
	// (which also connects the block) with the pool's tip update in it.
	blk := h.mineOn(h.ch.Tip(), []*consensus.Tx{first}, 1000, 0)
	before := h.fullRevalidations()
	t0 = time.Now()
	if _, err := h.ch.ProcessBlock(blk, -1); err != nil {
		t.Fatal(err)
	}
	connect := time.Since(t0)
	if h.fullRevalidations() != before || h.pool.Has(first.TxID()) || h.pool.Count() != n-1 {
		t.Fatalf("block connect: %d full revalidations, pool %d", h.fullRevalidations()-before, h.pool.Count())
	}
	per := float64(h.pool.Bytes()) / float64(h.pool.Count())
	scale := float64(300<<20) / per / float64(h.pool.Count())
	t.Logf("%d entries (%.0f B each): full Revalidate %s (a full 300 MB pool, x%.0f: ~%s); block connect incl. pool update %s; Select %s, unchanged pool %s",
		h.pool.Count(), per, rev, scale, time.Duration(float64(rev)*scale).Round(time.Millisecond), connect, sel, selAgain)
}

// BenchmarkReviewD_PowHash: cost of one Argon2id PoW evaluation.
func BenchmarkReviewD_PowHash(b *testing.B) {
	hdr := consensus.MainNet.Genesis.Header
	for i := 0; i < b.N; i++ {
		hdr.Nonce = uint64(i)
		consensus.PowHash(hdr.Serialize())
	}
}
