package bridge

// Review B (custody and payout core): deposits across a vault replacement.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
	"argonmine/wallet"
)

// TestVaultReplacementDepositIDs (review B, fixed 2026-10-28): a deposit is
// identified by (chain id, deposit id) only, in the state, in the deposit
// references of every issuance (BRI1) and in the settlement from them. It
// used to be that every vault numbered its deposits from 1, so after a
// vault was replaced on the same chain the new vault's deposit #1 was taken
// for the old one: with kept state every guardian paused with a
// reorganisation alert and never issued it (and the EVM cursor stayed at
// the old vault's position, so new deposits below it were never scanned);
// with rebuilt state the old vault's issuance BRI1(chain, 1) marked it
// confirmed without paying it.
//
// Now a vault numbers its deposits from its constructor's firstDepositId,
// and the guardians check it:
//
//	(b1) rebuilt state, l1_start_height set, the new vault deployed with a
//	     first id above the old ids: the new deposit is issued, and the old
//	     issuance's reference is kept for an id no deposit of the new vault
//	     has;
//	(b2) rebuilt state, a new vault numbering from 1 again: an old issuance
//	     mined below l1_start_height that references an id the new vault
//	     uses is never applied to it; issuance on the chain is paused for an
//	     operator, and nothing is issued for the colliding deposit;
//	(a1) kept state, a new vault numbering from 1 again: refused before
//	     anything of it is scanned, with the reason (and the first id it
//	     needs) in the chain's status; no pause, nothing issued, the old
//	     records untouched. The highest id on record includes the ones
//	     named by issuances on Argonmine: here #1001, which the vault of
//	     (b1) issued while these guardians were stopped;
//	(a2) kept state, a new vault numbering above every id on record: its
//	     scan starts again from start_block (a deposit below the old
//	     vault's cursor is found), its deposits are issued, the old
//	     vault's pending deposit is held for the guardians instead of being
//	     taken for a reorganised one, and nothing pauses.
var reviewBCtx = context.Background()

func TestVaultReplacementDepositIDs(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	cid := e.chains[0].ID
	var from [20]byte
	old := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	oldRec, _ := e.leader.Inbound(cid, old)
	if old != 1 || oldRec.Status != InboundConfirmed {
		t.Fatalf("old deposit not confirmed: %+v", oldRec)
	}
	if st := e.leader.Status(); st.Chains[0].FirstDepositID != 1 || st.Chains[0].MaxDepositID != 1 {
		t.Fatalf("status of the first vault: first %d, max %d", st.Chains[0].FirstDepositID, st.Chains[0].MaxDepositID)
	}
	// A second deposit into the old vault, not final yet when the vault is
	// replaced: on record as pending.
	pend := e.evm.addDeposit(usdc, from, usd(30, 6), e.uAddr)
	e.scanAll()
	if d, _ := e.leader.Inbound(cid, pend); d == nil || d.Status != InboundPending {
		t.Fatalf("old pending deposit: %+v", d)
	}

	user := func() string {
		priv, _ := btcec.NewPrivateKey()
		return (&wallet.Key{Priv: priv, XOnly: schnorr.SerializePubKey(priv.PubKey())}).Address(consensus.RegTest.HRP)
	}
	newVault := func(last byte, first uint64) ([]Chain, *fakeEVM) {
		cs := append([]Chain{}, e.chains...)
		cs[0].Vault[19] = last
		ev := newFakeEVM(cs[0].ID, cs[0].Vault, e.evmAddrs, 2, 100, 100)
		ev.firstID = first
		ev.head = e.evm.head // the same chain, later blocks
		return cs, ev
	}
	// rebuilt starts three guardians from empty state directories.
	rebuilt := func(prefix string, cs []Chain, ev *fakeEVM) (l *Service, all []*Service, round func()) {
		g1 := e.mk(1, false, nil, prefix+"1", cs, ev)
		g2 := e.mk(2, false, nil, prefix+"2", cs, ev)
		s1, s2 := httptest.NewServer(g1.Handler()), httptest.NewServer(g2.Handler())
		t.Cleanup(s1.Close)
		t.Cleanup(s2.Close)
		l = e.mk(0, true, []string{s1.URL, s2.URL}, prefix+"0", cs, ev)
		all = []*Service{l, g1, g2}
		round = func() {
			for _, g := range all {
				g.scanEVM(reviewBCtx)
				if err := g.scanL1(reviewBCtx); err != nil {
					t.Fatal(err)
				}
			}
			l.Once(reviewBCtx)
		}
		return l, all, round
	}

	// (b1) Fresh state everywhere, l1_start_height set, the new vault
	// numbering from 1001.
	info, _ := e.client.ChainInfo()
	u2 := user()
	cb, evB := newVault(0x78, 1001)
	cb[0].L1StartHeight = info.Blocks + 1
	idB := evB.addDeposit(usdc, from, usd(500, 6), u2)
	evB.advance(10)
	if idB != 1001 {
		t.Fatalf("new vault's first id %d", idB)
	}
	l2, gs, roundB := rebuilt("n", cb, evB)
	roundB()
	e.mine(1, e.uAddr)
	roundB()
	for i, g := range gs {
		d, _ := g.Inbound(cid, idB)
		if d == nil || d.Status != InboundConfirmed || d.IssueTx == "" || d.IssueTx == oldRec.IssueTx {
			t.Fatalf("(b1) guardian %d: the new vault's deposit %+v", i, d)
		}
		if _, ok := g.Inbound(cid, old); ok {
			t.Fatalf("(b1) guardian %d has a deposit #%d of the new vault", i, old)
		}
		if sr, ok := g.st.Settled(InboundRef{Chain: cid, ID: old}); !ok || sr.TxID != oldRec.IssueTx || sr.Height == 0 {
			t.Fatalf("(b1) guardian %d: the old issuance's reference: %+v", i, sr)
		}
		if p := g.st.Pauses(); len(p) != 0 {
			t.Fatalf("(b1) guardian %d paused: %+v", i, p)
		}
	}
	if b := e.uargBalance(u2); b != 495*consensus.UnitsPerCoin {
		t.Fatalf("(b1) the new depositor holds %d uARG units for 500 USDC", b)
	}
	if st := l2.Status(); st.Chains[0].FirstDepositID != 1001 || st.Chains[0].MaxDepositID != 1001 {
		t.Fatalf("(b1) status: first %d, max %d", st.Chains[0].FirstDepositID, st.Chains[0].MaxDepositID)
	}
	for _, g := range gs {
		g.Stop()
	}

	// (b2) Fresh state again, l1_start_height set, but the vault numbers
	// from 1 like the old one did: the old issuance's BRI1(chain, 1) is
	// never applied to the new deposit #1.
	info, _ = e.client.ChainInfo()
	u3 := user()
	cb2, evB2 := newVault(0x7b, 1)
	cb2[0].L1StartHeight = info.Blocks + 1
	id1 := evB2.addDeposit(usdc, from, usd(500, 6), u3)
	evB2.advance(10)
	_, gs2, roundB2 := rebuilt("m", cb2, evB2)
	roundB2()
	e.mine(1, e.uAddr)
	roundB2()
	for i, g := range gs2 {
		d, _ := g.Inbound(cid, id1)
		if d == nil || d.Status != InboundPending || d.IssueTx != "" {
			t.Fatalf("(b2) guardian %d: the colliding deposit %+v", i, d)
		}
		p := g.st.Pauses()
		if len(p) != 1 || p[0].Scope != pauseScopeEVM(cid) || !strings.Contains(p[0].Reason, "never applied to this vault's deposit") {
			t.Fatalf("(b2) guardian %d: pauses %+v", i, p)
		}
		t.Logf("(b2) guardian %d paused: %s", i, p[0].Reason)
	}
	if b := e.uargBalance(u3); b != 0 {
		t.Fatalf("(b2) the colliding deposit was issued: %d", b)
	}
	for _, g := range gs2 {
		g.Stop()
	}

	// (a1) The same guardians, their state kept, a new vault numbering
	// from 1 again: refused.
	e.stop()
	u4 := user()
	ca, evA := newVault(0x79, 1)
	evA.addDeposit(usdc, from, usd(500, 6), u4)
	evA.advance(10)
	e.start("g", ca, evA)
	for i := 0; i < 2; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	e.round()
	for i, g := range []*Service{e.leader, e.f1, e.f2} {
		st := g.Status()
		cs := st.Chains[0]
		if !strings.Contains(cs.Error, "numbers its deposits from 1, not above deposit 31337#1001") || !strings.Contains(cs.Error, "firstDepositId 1002 or more") || cs.Paused == "" {
			t.Fatalf("(a1) guardian %d: chain status %q / %q", i, cs.Error, cs.Paused)
		}
		if len(st.Paused) != 0 {
			t.Fatalf("(a1) guardian %d paused: %+v", i, st.Paused)
		}
		if d, _ := g.Inbound(cid, old); d.EVMTx != oldRec.EVMTx || d.Status != InboundConfirmed {
			t.Fatalf("(a1) guardian %d: the old deposit #1 %+v", i, d)
		}
		if d, _ := g.Inbound(cid, pend); d.Status != InboundPending {
			t.Fatalf("(a1) guardian %d: the old pending deposit %+v", i, d)
		}
	}
	if b := e.uargBalance(u4); b != 0 {
		t.Fatalf("(a1) a deposit of the refused vault was issued: %d", b)
	}

	// (a2) Redeployed with firstDepositId 1002: a deposit below the old
	// vault's cursor and one above it, both issued.
	cur, _, _ := e.leader.st.Cursor(EVMCursor(cid)) // still the old vault's: nothing of the refused one was scanned
	e.stop()
	u5 := user()
	ca2, evA2 := newVault(0x7a, 1002)
	evA2.head = 49
	low := evA2.addDeposit(usdc, from, usd(200, 6), u5) // block 50, far below the old cursor
	evA2.head = e.evm.head
	high := evA2.addDeposit(usdc, from, usd(100, 6), u5)
	evA2.advance(10)
	if low != 1002 || high != 1003 || cur <= 50 {
		t.Fatalf("ids %d %d, old cursor %d", low, high, cur)
	}
	e.start("g", ca2, evA2)
	for i := 0; i < 2; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	for i, g := range []*Service{e.leader, e.f1, e.f2} {
		for _, id := range []uint64{low, high} {
			if d, _ := g.Inbound(cid, id); d == nil || d.Status != InboundConfirmed {
				t.Fatalf("(a2) guardian %d: deposit %d %+v", i, id, d)
			}
		}
		if d, _ := g.Inbound(cid, old); d.EVMTx != oldRec.EVMTx || d.Status != InboundConfirmed {
			t.Fatalf("(a2) guardian %d: the old deposit #1 %+v", i, d)
		}
		if p := g.st.Pauses(); len(p) != 0 {
			t.Fatalf("(a2) guardian %d paused: %+v", i, p)
		}
		st := g.Status()
		if cs := st.Chains[0]; cs.Error != "" || cs.Paused != "" || cs.FirstDepositID != 1002 || cs.MaxDepositID != 1003 {
			t.Fatalf("(a2) guardian %d: chain status %+v", i, cs)
		}
	}
	// The old vault's deposit that was still pending: held for the
	// guardians, neither issued nor taken for one that left the chain.
	if d, _ := e.leader.Inbound(cid, pend); d.Status != InboundPending || !strings.Contains(d.Error, "earlier vault") {
		t.Fatalf("(a2) the old pending deposit: %+v", d)
	}
	if b := e.uargBalance(u5); b != (198+99)*consensus.UnitsPerCoin {
		t.Fatalf("(a2) the new depositor holds %d uARG units for 300 USDC", b)
	}
}
