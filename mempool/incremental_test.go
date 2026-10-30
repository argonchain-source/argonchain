package mempool

import (
	"sort"
	"testing"

	"argonmine/consensus"
)

func sortedIDs(p *Pool) []string {
	var out []string
	for _, id := range p.TxIDs() {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}

// After a block connect handled block by block, a full Revalidate
// must find nothing more to remove. The block confirms a pool parent and,
// in the same block, spends the parent's output differently from the pool
// child (an outpoint created and spent inside the block); confirms two
// links of a three-transaction pool chain; and double-spends the root of a
// third chain.
func TestIncrementalConnectMatchesRevalidate(t *testing.T) {
	h := newHarness(t, 106)
	a, b := newKey(t), newKey(t)
	op0, out0 := h.matureCoin(0)
	op1, out1 := h.matureCoin(1)
	op2, out2 := h.matureCoin(2)
	op3, out3 := h.matureCoin(3)
	// Chain 1: P -> C (pool); block has P and Z, Z spending P:0 (conflicts C).
	txP := h.spend(h.k, op0, out0, a.out(49*coin))
	txC := h.spend(a, consensus.OutPoint{TxID: txP.TxID()}, txP.Outputs[0], a.out(48*coin))
	txZ := h.spend(a, consensus.OutPoint{TxID: txP.TxID()}, txP.Outputs[0], b.out(47*coin))
	// Chain 2: Q -> R -> S (pool); block confirms Q and R; S stays.
	txQ := h.spend(h.k, op1, out1, a.out(49*coin))
	txR := h.spend(a, consensus.OutPoint{TxID: txQ.TxID()}, txQ.Outputs[0], b.out(48*coin))
	txS := h.spend(b, consensus.OutPoint{TxID: txR.TxID()}, txR.Outputs[0], a.out(47*coin))
	// Chain 3: T -> U (pool); block has T2 spending T's input (conflict).
	txT := h.spend(h.k, op2, out2, a.out(49*coin))
	txU := h.spend(a, consensus.OutPoint{TxID: txT.TxID()}, txT.Outputs[0], b.out(48*coin))
	txT2 := h.spend(h.k, op2, out2, b.out(46*coin))
	// Unrelated, stays.
	txV := h.spend(h.k, op3, out3, b.out(49*coin))
	for _, tx := range []*consensus.Tx{txP, txC, txQ, txR, txS, txT, txU, txV} {
		if err := h.pool.Accept(tx); err != nil {
			t.Fatal(err)
		}
	}
	before := h.fullRevalidations()
	h.mine([]*consensus.Tx{txP, txZ, txQ, txR, txT2}, 1*coin+2*coin+1*coin+1*coin+4*coin)
	if h.fullRevalidations() != before {
		t.Fatal("expected the incremental path")
	}
	inc := sortedIDs(h.pool)
	h.pool.Revalidate()
	full := sortedIDs(h.pool)
	t.Logf("after the block: %d in pool incrementally, %d after a full Revalidate", len(inc), len(full))
	if len(inc) != len(full) {
		t.Fatalf("incremental update left %d transactions a full Revalidate removes", len(inc)-len(full))
	}
	want := map[consensus.Hash]bool{txS.TxID(): true, txV.TxID(): true}
	for _, id := range h.pool.TxIDs() {
		if !want[id] {
			t.Errorf("unexpected tx %s in pool", id)
		}
	}
	if h.pool.Count() != len(want) {
		t.Errorf("pool has %d, want %d", h.pool.Count(), len(want))
	}
	txs, _ := h.pool.Select(consensus.MaxBlockSize)
	if len(txs) != 2 {
		t.Errorf("template has %d transactions, want 2", len(txs))
	}
}
