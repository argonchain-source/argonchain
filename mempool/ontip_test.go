package mempool

import (
	"testing"

	"argonmine/chain"
	"argonmine/consensus"
)

// onTip skips a tip that is an ancestor of the last one seen only when the
// chain has really moved on from it (a late notification). If the chain is
// back at that ancestor (blocks disconnected), the pool may hold
// transactions spending coins that no longer exist: full Revalidate.
func TestOnTipAncestorRevalidatesUnlessLate(t *testing.T) {
	h := newHarness(t, 104)
	op, out := h.matureCoin(0)
	if err := h.pool.Accept(h.spend(h.k, op, out, newKey(t).out(49*coin))); err != nil {
		t.Fatal(err)
	}
	older := h.ch.Tip()
	h.mine(nil, 0)
	if h.pool.lastTip != h.ch.Tip() {
		t.Fatal("pool not at the new tip")
	}

	// Late: the notification for an ancestor arrives after the pool has
	// seen the chain's current tip.
	before := h.fullRevalidations()
	h.pool.onTip(older)
	if h.fullRevalidations() != before {
		t.Fatal("a late notification for an ancestor re-checked the pool")
	}
	if h.pool.lastTip != h.ch.Tip() {
		t.Fatal("a late notification moved the pool's tip back")
	}

	// Real: the pool last saw a descendant of the chain's tip (blocks were
	// disconnected since), and the chain's tip is the one notified.
	tip := h.ch.Tip()
	h.pool.mu.Lock()
	h.pool.lastTip = &chain.Node{Hash: consensus.TaggedHash("gone", nil), Height: tip.Height + 1, Parent: tip}
	h.pool.mu.Unlock()
	before = h.fullRevalidations()
	h.pool.onTip(tip)
	if h.fullRevalidations() != before+1 {
		t.Fatal("the chain went back to an ancestor and the pool was not revalidated")
	}
	if h.pool.lastTip != tip {
		t.Fatal("pool tip not updated")
	}
}
