package bridge

// Review B (custody and payout core): the smaller findings.

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSignedSinceCountsDepositsOnce (review B, fixed 2026-10-28): the
// daily issuance limit used to add up every issuance a guardian signed, so
// a round retried, or an issuance built anew after one was dropped,
// counted the same deposits again. Now each deposit counts once, with its
// amount on record; an issuance paying a deposit not on record counts once
// with its whole amount.
func TestSignedSinceCountsDepositsOnce(t *testing.T) {
	st, err := openState(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, d := range []Inbound{{Chain: 7, ID: 1, Amount: 10}, {Chain: 7, ID: 2, Amount: 20}} {
		if err := st.PutInbound(&d); err != nil {
			t.Fatal(err)
		}
	}
	for _, si := range []SignedIssue{
		{TxID: "aa", Inputs: []string{"c1:0"}, Inbound: []InboundRef{{7, 1}, {7, 2}}, Amount: 30, Time: 100},
		{TxID: "bb", Inputs: []string{"c1:0"}, Inbound: []InboundRef{{7, 1}}, Amount: 10, Time: 200}, // the same deposit, signed again
		{TxID: "cc", Inputs: []string{"c2:0"}, Inbound: []InboundRef{{7, 9}}, Amount: 7, Time: 300},  // not on record here
		{TxID: "dd", Inputs: []string{"c3:0"}, Inbound: []InboundRef{{7, 2}}, Amount: 20, Time: 50},  // before the window
	} {
		if err := st.RecordSignedIssue(&si); err != nil {
			t.Fatal(err)
		}
	}
	w := st.SignedSince(100)
	if got := sumSigned(w, nil); got != 37 {
		t.Fatalf("signed since 100: %d, want 10 + 20 + 7 (each deposit once)", got)
	}
	if got := sumSigned(w, map[InboundRef]bool{{7, 1}: true}); got != 27 {
		t.Fatalf("leaving deposit 7#1 out: %d", got)
	}
	if got := sumSigned(st.SignedSince(201), nil); got != 7 {
		t.Fatalf("signed since 201: %d", got)
	}
}

// TestIssueDeadDepth (review B, fixed 2026-10-28): an issuance signed
// earlier counted as unable ever to confirm once another transaction that
// spent one of its custody coins was l1_confirmations deep (3 here), so a
// reorganisation of a few blocks could bring it back next to a later
// issuance for the same deposits. Now the spend must be at least 100
// blocks deep.
func TestIssueDeadDepth(t *testing.T) {
	s := offlineService(t)
	s.cfg.L1Confirmations = 3
	old := &SignedIssue{TxID: "aa", Inputs: []string{"c1:0"}}
	if err := s.st.put(spendKey("c1", 0), CustodySpend{TxID: "other", Height: 500}); err != nil {
		t.Fatal(err)
	}
	for tip, want := range map[uint32]bool{502: false, 598: false, 599: true, 700: true} {
		if got := s.issueDead(old, tip); got != want {
			t.Errorf("spent at 500, tip %d: dead %v, want %v", tip, got, want)
		}
	}
	// Spent by the issuance itself: never dead.
	if err := s.st.put(spendKey("c1", 0), CustodySpend{TxID: "aa", Height: 500}); err != nil {
		t.Fatal(err)
	}
	if s.issueDead(old, 2000) {
		t.Error("an issuance counted dead because its own coin was spent by it")
	}
	s.cfg.L1Confirmations = 150 // a deeper l1_confirmations still applies
	if err := s.st.put(spendKey("c1", 0), CustodySpend{TxID: "other", Height: 500}); err != nil {
		t.Fatal(err)
	}
	if s.issueDead(old, 620) || !s.issueDead(old, 649) {
		t.Error("l1_confirmations above 100 is not the depth")
	}
}

// TestVaultSetupReported (review B, fixed 2026-10-28): the daemon reads
// the vault's finalized() with its scan and reports a vault still in setup
// mode (its owner can change fees and limits at once) in the status.
func TestVaultSetupReported(t *testing.T) {
	s := offlineService(t)
	ctx := context.Background()
	c := s.chains[0]
	fe := c.evm.(*fakeEVM)
	fe.mu.Lock()
	fe.setup = true
	fe.mu.Unlock()
	if err := s.scanChain(ctx, c); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if !st.Chains[0].VaultSetup || st.Chains[1].VaultSetup {
		t.Fatalf("vault_setup: %v %v", st.Chains[0].VaultSetup, st.Chains[1].VaultSetup)
	}
	if st.Chains[0].FirstDepositID != 1 || st.Chains[1].FirstDepositID != 0 {
		t.Fatalf("first_deposit_id: %d %d (the second chain is not scanned yet)", st.Chains[0].FirstDepositID, st.Chains[1].FirstDepositID)
	}
	if st.Chains[1].Paused == "" {
		t.Fatal("a chain whose vault is not checked yet is not reported as waiting")
	}
	fe.mu.Lock()
	fe.setup = false
	fe.mu.Unlock()
	if err := s.scanChain(ctx, c); err != nil {
		t.Fatal(err)
	}
	if s.Status().Chains[0].VaultSetup {
		t.Fatal("vault_setup after finalizeSetup")
	}
}
