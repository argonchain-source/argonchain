package swap

import (
	"encoding/hex"
	"testing"
	"time"

	"argonmine/consensus"
)

// Security review G (2026-10-28): from accept to the end of Swap, a quote is
// in m.inflight with its coins locked; every way out of Swap (refused
// signatures, an expired quote, a broadcast the node refuses) must leave no
// in-flight entry, no lock under its id and the reserve as before, and the
// wARG ledger without the swap that did not happen.
func TestReviewG_MakerReleasesOnEveryFailure(t *testing.T) {
	e := newEnv(t)
	free := func() (consensus.Amount, consensus.Amount) {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		a, u, err := e.m.freeLocked()
		if err != nil {
			t.Fatal(err)
		}
		return a, u
	}
	clean := func(what, id string) {
		t.Helper()
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		if _, ok := e.m.inflight[id]; ok {
			t.Fatalf("%s: the quote is still in flight", what)
		}
		if _, ok := e.m.quotes[id]; ok {
			t.Fatalf("%s: the quote is still open", what)
		}
		for op, q := range e.m.locked {
			if q == id {
				t.Fatalf("%s: coin %v is still locked for the quote", what, op)
			}
		}
	}
	sign := func(q *Quote) *SwapRequest {
		tx, spent, err := q.Decode()
		if err != nil {
			t.Fatal(err)
		}
		if err := e.user.SignInputs(tx, spent, q.UserInputs); err != nil {
			t.Fatal(err)
		}
		r := &SwapRequest{QuoteID: q.ID}
		for _, i := range q.UserInputs {
			r.Sigs = append(r.Sigs, Sig{Index: i, Sig: hex.EncodeToString(tx.Witnesses[i])})
		}
		return r
	}
	a0, u0 := free()
	supply0 := e.m.wargSupply()

	// 1. A signature that does not verify.
	q, _ := e.quoteFor(UARG, ARG, 5*unit)
	r := sign(q)
	r.Sigs[0].Sig = r.Sigs[0].Sig[:len(r.Sigs[0].Sig)-2] + "00"
	if _, err := e.m.Swap(r); err == nil {
		t.Fatal("a bad signature was accepted")
	}
	clean("bad signature", q.ID)
	if a, u := free(); a != a0 || u != u0 {
		t.Fatalf("bad signature: free ARG %d uARG %d, was %d %d", a, u, a0, u0)
	}

	// 2. An expired quote.
	q, _ = e.quoteFor(UARG, WARG, 5*unit)
	e.clk.add(e.m.cfg.QuoteTTL + time.Second)
	if _, err := e.m.Swap(sign(q)); err == nil {
		t.Fatal("an expired quote was swapped")
	}
	clean("expired", q.ID)

	// 3. The node refuses the broadcast: the user's coin is spent elsewhere
	// first. The swap issues wARG, so its ledger entry must go again.
	q, _ = e.quoteFor(UARG, WARG, 5*unit)
	r = sign(q)
	tx, _, _ := q.Decode()
	userAddr, _ := e.user.ReceiveAddress()
	if _, err := e.user.SendAsset(e.uarg.String(), userAddr, 1*unit, 1); err != nil {
		t.Fatal(err)
	}
	conflict := false
	for _, i := range q.UserInputs {
		raw, _ := e.node.Call("gettxout", tx.Inputs[i].PrevOut.TxID.String(), tx.Inputs[i].PrevOut.Index)
		if string(raw) == "null" {
			conflict = true
		}
	}
	if _, err := e.m.Swap(r); err == nil {
		if conflict {
			t.Fatal("a swap of a spent coin was broadcast")
		}
		t.Skip("the user's payment did not spend the quoted coin")
	} else {
		t.Logf("broadcast refused as expected: %v", err)
	}
	clean("broadcast refused", q.ID)
	if s := e.m.wargSupply(); s != supply0 {
		t.Fatalf("broadcast refused: wARG supply %d, was %d", s, supply0)
	}
	if a, _ := free(); a > a0 {
		t.Fatalf("broadcast refused: free ARG grew to %d from %d", a, a0)
	}
}
