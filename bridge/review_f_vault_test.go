package bridge

// Review F (second pass, 2026-10-28): vault replacement and rollback.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewF_VaultRollbackRefused (fixed 2026-10-28): a guardian recorded
// every vault that differed from the one in its epoch as a replacement at
// once (registerVault), before checking it, with AboveID = the highest
// deposit id on record for the chain. Going back to the vault it had before
// (the chains file of a replacement attempt reverted, here after the
// replacement was refused for numbering too low, the documented failure of
// "Replacing a vault") was therefore a replacement of the replacement: the
// original vault numbers its deposits from 1, never above its own ids, so it
// was refused for good, on every guardian that saw the attempt, and the
// burns from before the rollback could no longer be paid from it (each
// change started a new epoch at the tip).
//
// Now a vault that differs is a candidate kept in memory, and its epoch is
// recorded only once it passes the check: the refused one never takes the
// place of the original, whose epoch (its L1Start, its first id, its scan)
// is as it was when the operators go back to it, and a burn made while the
// refused vault was configured waits, then is paid from the original. The
// reset-vault command drops a candidate. A vault that passed is in the next
// epoch's history, and going back past it is still refused, with the reason.
func TestReviewF_VaultRollbackRefused(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	cid := e.chains[0].ID
	orig := HexAddress(e.chains[0].Vault)
	var from [20]byte
	first := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.leader.Inbound(cid, first); d == nil || d.Status != InboundConfirmed {
		t.Fatalf("first deposit not issued: %+v", d)
	}
	epochOf := func(g *Service) Epoch {
		ep, ok := g.st.GetEpoch(cid)
		if !ok {
			t.Fatal("no epoch on record")
		}
		return *ep
	}
	before := epochOf(e.leader)
	if !strings.EqualFold(before.Vault, orig) || before.FirstID != 1 || before.L1Start != 0 || len(before.History) != 0 {
		t.Fatalf("the original vault's epoch: %+v", before)
	}

	// A replacement vault deployed with firstDepositId 1 (too low): refused,
	// as documented, and the epoch on record does not move.
	e.stop()
	bad := append([]Chain{}, e.chains...)
	bad[0].Vault[19] ^= 0x55
	evBad := newFakeEVM(cid, bad[0].Vault, e.evmAddrs, 2, 100, 100)
	evBad.head = e.evm.head
	e.start("g", bad, evBad)
	// A user burns the uARG while it is configured: the burn names the
	// chain, not the vault.
	burn := e.burnNewest(9)
	for i := 0; i < 3; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	e.round()
	for i, g := range []*Service{e.leader, e.f1, e.f2} {
		cs := g.Status().Chains[0]
		if !strings.Contains(cs.Error, "not above deposit") || cs.Paused == "" || cs.FirstDepositID != 0 {
			t.Fatalf("guardian %d: the replacement was not refused: %q / %q (first id %d)", i, cs.Error, cs.Paused, cs.FirstDepositID)
		}
		if ep := epochOf(g); !strings.EqualFold(ep.Vault, orig) || ep.L1Start != before.L1Start || ep.FirstID != 1 {
			t.Fatalf("guardian %d: the refused vault moved the epoch on record: %+v", i, ep)
		}
	}
	if r, _ := e.leader.st.GetRedemption(burn); r == nil || r.Status != RedemptionPending || !strings.Contains(r.Error, "not in use yet") {
		t.Fatalf("the burn while the refused vault is configured: %+v", r)
	}
	if _, err := e.f1.SignReleaseLocally(context.Background(), burn); err == nil || !strings.Contains(err.Error(), "not in use yet") {
		t.Fatalf("a follower signed a release from the refused vault: %v", err)
	}
	if len(evBad.releases)+len(e.evm.releases) != 0 {
		t.Fatal("a release was sent while the vault was refused")
	}

	// reset-vault drops the refused candidate; the next round builds it
	// again (and refuses it again).
	if code, ans := adminCall(t, e.urls[0], e.cs[0].local, "reset-vault", map[string]any{"chain_id": cid}); code != 200 || !strings.Contains(ans, "dropped the candidate epoch of vault "+HexAddress(bad[0].Vault)) || !strings.Contains(ans, "it was refused") {
		t.Fatalf("reset-vault: %d %s", code, ans)
	}
	if cs := e.leader.Status().Chains[0]; strings.Contains(cs.Paused, "not above deposit") {
		t.Fatalf("the reason stayed after the reset: %q", cs.Paused)
	}
	e.round()
	if cs := e.leader.Status().Chains[0]; !strings.Contains(cs.Error, "not above deposit") {
		t.Fatalf("after the reset the candidate was not checked again: %q", cs.Error)
	}
	if code, ans := adminCall(t, e.urls[0], e.cs[0].local, "reset-vault", map[string]any{"chain_id": 999}); code != 400 || !strings.Contains(ans, "not served") {
		t.Fatalf("reset-vault of an unknown chain: %d %s", code, ans)
	}

	// The operators go back to the vault that worked, which holds the
	// backing, and a user deposits into it.
	e.stop()
	e.start("g", e.chains, e.evm)
	second := e.evm.addDeposit(usdc, from, usd(50, 6), e.uAddr)
	e.evm.advance(10)
	for i := 0; i < 3; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	e.round()
	cs := e.leader.Status().Chains[0]
	d, _ := e.leader.Inbound(cid, second)
	if d == nil || d.Status != InboundConfirmed || cs.Error != "" || cs.Paused != "" || cs.FirstDepositID != 1 {
		t.Fatalf("after going back to the original vault: chain error %q, paused %q; deposit #%d: %+v", cs.Error, cs.Paused, second, d)
	}
	if r, _ := e.leader.st.GetRedemption(burn); r.Status != RedemptionConfirmed || len(e.evm.releases) != 1 || len(evBad.releases) != 0 {
		t.Fatalf("the burn made while the refused vault was configured: %+v (releases %d)", r, len(e.evm.releases))
	}
	for i, g := range []*Service{e.leader, e.f1, e.f2} {
		if ep := epochOf(g); ep.L1Start != before.L1Start || ep.FirstID != 1 || len(ep.History) != 0 {
			t.Fatalf("guardian %d: the original epoch was not kept as it was: %+v", i, ep)
		}
	}
	// Offline, reset-vault reports that a stopped guardian keeps no candidate.
	e.stop()
	if res, err := AdminOffline(filepath.Join(e.dir, "g1"), "reset-vault", []byte(`{"chain_id":31337}`), nil); err != nil || !strings.Contains(res.Done, "stopped") || !strings.Contains(res.Done, orig) {
		t.Fatalf("reset-vault offline: %+v %v", res, err)
	}

	// A good replacement (numbering above every id on record) is used, and
	// the original epoch is kept in its history.
	good := append([]Chain{}, e.chains...)
	good[0].Vault[19] ^= 0x66
	evGood := newFakeEVM(cid, good[0].Vault, e.evmAddrs, 2, 100, 100)
	evGood.firstID = 1000
	evGood.head = e.evm.head
	e.start("g", good, evGood)
	for i := 0; i < 2; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	e.round()
	ep := epochOf(e.leader)
	if !strings.EqualFold(ep.Vault, HexAddress(good[0].Vault)) || ep.FirstID != 1000 || ep.L1Start == 0 || len(ep.History) != 1 || !strings.EqualFold(ep.History[0].Vault, orig) || ep.History[0].FirstID != 1 {
		t.Fatalf("the good replacement's epoch: %+v", ep)
	}
	// Going back past a vault that was used is a replacement like any other,
	// refused for its numbering, with the reason.
	e.stop()
	e.start("g", e.chains, e.evm)
	for i := 0; i < 2; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	e.round()
	if cs := e.leader.Status().Chains[0]; !strings.Contains(cs.Error, "not above deposit") || !strings.Contains(cs.Error, "it was the chain's vault before") {
		t.Fatalf("going back past a used vault: %q", cs.Error)
	}
	if ep := epochOf(e.leader); !strings.EqualFold(ep.Vault, HexAddress(good[0].Vault)) {
		t.Fatalf("the epoch on record moved: %+v", ep)
	}
}

// TestReviewF_MistypedVault: a vault address with no contract (mistyped, or
// not deployed yet) answered firstDepositId() with "0x", read as 1: the
// address passed as the chain's first vault and was recorded, and the real
// vault became a replacement starting at the tip, so nothing burned before
// the typo was fixed could be paid from it. Now there must be a contract:
// the chain's status says there is none, nothing is scanned or recorded,
// and the real vault, once named, is the chain's first vault.
func TestReviewF_MistypedVault(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	cid := e.chains[0].ID
	e.stop()
	typo := append([]Chain{}, e.chains...)
	typo[0].Vault[19] ^= 0x33
	evTypo := newFakeEVM(cid, typo[0].Vault, e.evmAddrs, 2, 100, 100)
	evTypo.noCode = true
	evTypo.head = e.evm.head
	e.start("m", typo, evTypo)
	var from [20]byte
	dep := e.evm.addDeposit(e.chains[0].Tokens[0].Address, from, usd(100, 6), e.uAddr) // into the real vault
	e.evm.advance(10)
	for i := 0; i < 2; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	for i, g := range []*Service{e.leader, e.f1, e.f2} {
		cs := g.Status().Chains[0]
		if !strings.Contains(cs.Error, "no contract at the vault address") || !strings.Contains(cs.Paused, "no contract") || cs.FirstDepositID != 0 {
			t.Fatalf("guardian %d: a vault address without a contract: %q / %q", i, cs.Error, cs.Paused)
		}
		if ep, ok := g.st.GetEpoch(cid); ok {
			t.Fatalf("guardian %d recorded the mistyped vault: %+v", i, ep)
		}
		if _, _, ok := g.st.Cursor(EVMCursor(cid)); ok {
			t.Fatalf("guardian %d scanned the mistyped vault", i)
		}
	}
	e.stop()
	e.start("m", e.chains, e.evm)
	for i := 0; i < 2; i++ {
		e.round()
		e.mine(1, e.uAddr)
	}
	e.round()
	for i, g := range []*Service{e.leader, e.f1, e.f2} {
		ep, ok := g.st.GetEpoch(cid)
		if !ok || ep.L1Start != 0 || ep.Previous != "" || ep.FirstID != 1 {
			t.Fatalf("guardian %d: the real vault is not the chain's first vault: %+v", i, ep)
		}
	}
	if d, _ := e.leader.Inbound(cid, dep); d == nil || d.Status != InboundConfirmed {
		t.Fatalf("the deposit into the real vault: %+v", d)
	}
}

// TestFirstDepositIDNeedsCode: the RPC client reads firstDepositId() only
// from an address that holds a contract; "0x" from a contract is no answer
// either. A contract without the view (it reverts) is a vault from before
// it: 1.
func TestFirstDepositIDNeedsCode(t *testing.T) {
	t.Parallel()
	var code, answer string
	revert := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal(b, &req)
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "eth_chainId":
			out["result"] = "0x7a69"
		case "eth_getCode":
			out["result"] = code
		case "eth_call":
			if revert {
				out["error"] = map[string]any{"code": 3, "message": "execution reverted"}
			} else {
				out["result"] = answer
			}
		default:
			out["error"] = map[string]any{"code": -32601, "message": "no such method"}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	var vault [20]byte
	vault[19] = 0x99
	ev := NewRPCEVM(srv.URL, 31337, vault, nil)
	ctx := context.Background()
	code, answer = "0x", "0x"
	if _, err := ev.FirstDepositID(ctx); !errors.Is(err, ErrNoVaultCode) {
		t.Fatalf("no contract: %v", err)
	}
	code = "0x6080"
	if _, err := ev.FirstDepositID(ctx); err == nil || !strings.Contains(err.Error(), "answers firstDepositId() with nothing") {
		t.Fatalf("an empty answer: %v", err)
	}
	answer = "0x" + strings.Repeat("0", 60) + "03e8"
	if id, err := ev.FirstDepositID(ctx); err != nil || id != 1000 {
		t.Fatalf("a vault numbering from 1000: %d %v", id, err)
	}
	revert = true
	if id, err := ev.FirstDepositID(ctx); err != nil || id != 1 {
		t.Fatalf("a vault without the view: %d %v", id, err)
	}
}

// TestEpochRestorable: going back to an earlier vault takes its epoch back
// as it was only across vaults that never passed their check (an epoch
// recorded before it: Rescan, no first id); across one that passed, it is a
// replacement like any other.
func TestEpochRestorable(t *testing.T) {
	a := Epoch{Vault: "0xaaaa", L1Start: 0, FirstID: 1}
	b := Epoch{Vault: "0xbbbb", L1Start: 50, Previous: "0xaaaa", AboveID: 9, FirstID: 10}
	never := func(v, prev string) Epoch {
		return Epoch{Vault: v, L1Start: 90, Previous: prev, AboveID: 20, Rescan: true}
	}

	// On record: c, never checked, after a and b.
	c := never("0xcccc", "0xbbbb")
	c.History = []Epoch{a, b}
	if got, ok := c.restorable("0xBBBB"); !ok || got.Vault != "0xbbbb" || got.L1Start != 50 || got.FirstID != 10 || len(got.History) != 1 || got.History[0].Vault != "0xaaaa" {
		t.Fatalf("back to b: %+v %v", got, ok)
	}
	if _, ok := c.restorable("0xaaaa"); ok {
		t.Fatal("back to a across b, which was used")
	}
	// Two vaults in a row that never passed.
	d := never("0xdddd", "0xcccc")
	d.History = []Epoch{a, b, never("0xcccc", "0xbbbb")}
	if got, ok := d.restorable("0xbbbb"); !ok || got.L1Start != 50 || len(got.History) != 1 {
		t.Fatalf("back to b across two that never passed: %+v %v", got, ok)
	}
	// An epoch on record that passed restores nothing; neither does an
	// unknown vault.
	b2 := b
	b2.History = []Epoch{a}
	if _, ok := b2.restorable("0xaaaa"); ok {
		t.Fatal("back to a from b, which passed")
	}
	if _, ok := c.restorable("0xeeee"); ok {
		t.Fatal("an unknown vault")
	}
	// An epoch from before the check existed (no first id, no rescan) counts
	// as used.
	old := Epoch{Vault: "0xbbbb", L1Start: 50}
	unchecked := never("0x1", "0x2")
	if !old.passed() || unchecked.passed() || !b.passed() {
		t.Fatal("passed()")
	}
	if h := b2.succeeded(); len(h) != 2 || h[0].Vault != "0xaaaa" || h[1].Vault != "0xbbbb" || h[1].History != nil {
		t.Fatalf("succeeded(): %+v", h)
	}
}
