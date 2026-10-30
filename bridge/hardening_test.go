package bridge

// Tests for the second security pass: EVM finality, reorganisations and
// RPC providers (C4), payout depth tiers and the L1 reorganisation pause
// (C7), token decimals and the per-deposit cap (C8), stuck items, fee
// bumps, limits and pauses of the vault (C9), the gas key (C13), and the
// public RPC in the status.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/rpc"
	"argonmine/wallet"
)

// testClock is a clock the tests move by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Now()} }

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// issueDeposit makes a deposit of gross token units on the env's chain and
// takes it through issuance and settlement; it returns the deposit.
func (e *pocEnv) issueDeposit(token [20]byte, gross *big.Int) *Inbound {
	t := e.t
	t.Helper()
	var from [20]byte
	id := e.evm.addDeposit(token, from, gross, e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d, _ := e.leader.Inbound(e.chains[0].ID, id)
	if d == nil || d.Status != InboundConfirmed {
		t.Fatalf("deposit not issued: %+v", d)
	}
	return d
}

// burnNewest burns the user's newest uARG coin for USDC to recipient 0xCD,
// paying the fee from the coinbase of block cbHeight, and mines it.
func (e *pocEnv) burnNewest(cbHeight uint32) string {
	var recipient [20]byte
	recipient[19] = 0xCD
	burn := e.burnFirstCoin(EncodeMemo(recipient), cbHeight)
	e.mine(1, e.uAddr)
	return burn
}

// adminCall sends an operator command to a guardian's handler, signed with
// key.
func adminCall(t *testing.T, base string, key *btcec.PrivateKey, action string, body any) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	hdr, err := AdminHeaders(consensus.RegTest.Name, key, "/admin/"+action, raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return postGuardian(t, base+"/admin/"+action, raw, hdr)
}

// resumeAll resumes every pause on the three guardians, as their operators
// would.
func (e *pocEnv) resumeAll() {
	for i, u := range e.urls {
		if code, ans := adminCall(e.t, u, e.cs[i].local, "resume", map[string]string{"scope": "all"}); code != 200 {
			e.t.Fatalf("resume on guardian %d: %d %s", i, code, ans)
		}
	}
}

// TestEVMFinality: a deposit above the final block is recorded (so wallets
// can follow it) and never acted on, by the leader or by a follower asked
// directly; with the finalized tag, depth alone is not enough.
func TestEVMFinality(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t, func(c *Config) { c.Chains[0].Finality = FinalityFinalized })
	ctx := context.Background()
	e.fund()
	e.evm.finalLag = 50
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	id := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10) // 10 blocks deep: past the 5 confirmations, not finalized
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d, _ := e.leader.Inbound(e.chains[0].ID, id)
	if d == nil || d.Status != InboundPending || d.BlockHash == "" {
		t.Fatalf("deposit above the final block: %+v", d)
	}
	st := e.leader.Status()
	if c := st.Chains[0]; c.Finality != FinalityFinalized || c.Final >= d.Block || c.Head != e.evm.head {
		t.Fatalf("chain status: finality %s final %d head %d (deposit in %d)", c.Finality, c.Final, c.Head, d.Block)
	}
	if len(e.leader.st.Issues()) != 0 || e.uargBalance(e.uAddr) != 0 {
		t.Fatal("a deposit above the final block was issued")
	}
	_, _, req := issueTx(e.leader, e.matureCoins(e.f1)[0], []*Inbound{d})
	var out struct{}
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "not final") {
		t.Fatalf("a follower signed a deposit above the finalized block: %v", err)
	}
	// Finalized: issued.
	e.evm.advance(50)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.leader.Inbound(e.chains[0].ID, id); d.Status != InboundConfirmed || e.uargBalance(e.uAddr) != d.Amount {
		t.Fatalf("finalized deposit: %+v", d)
	}
}

// TestEVMReorgRelocatesAndReorgs: a reorganisation below the final block
// takes one deposit's transaction out of the chain and moves another's to a
// later block. The first is marked reorged and never issued; the second is
// re-located by its transaction and issued from where it is now.
func TestEVMReorgRelocatesAndReorgs(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	gone := e.evm.addDeposit(usdc, from, usd(300, 6), e.uAddr)
	moved := e.evm.addDeposit(usdc, from, usd(200, 6), e.uAddr)
	e.evm.advance(10)
	e.scanAll() // both final and recorded, nothing issued yet
	dm, _ := e.leader.Inbound(e.chains[0].ID, moved)
	if dm.Status != InboundPending || dm.BlockHash == "" {
		t.Fatalf("before the reorganisation: %+v", dm)
	}
	oldBlock, oldHash := dm.Block, dm.BlockHash
	e.evm.dropDeposit(gone)
	e.evm.moveDeposit(moved, oldBlock+3)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	for i, s := range []*Service{e.leader, e.f1, e.f2} {
		g, _ := s.Inbound(e.chains[0].ID, gone)
		if g.Status != InboundReorged || !strings.Contains(g.Error, "no longer in block") {
			t.Fatalf("guardian %d: the deposit that left the chain: %+v", i, g)
		}
		m, _ := s.Inbound(e.chains[0].ID, moved)
		if m.Block != oldBlock+3 || m.BlockHash == oldHash || m.Status == InboundReorged {
			t.Fatalf("guardian %d: the moved deposit was not re-located: %+v", i, m)
		}
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	m, _ := e.leader.Inbound(e.chains[0].ID, moved)
	if m.Status != InboundConfirmed || e.uargBalance(e.uAddr) != m.Amount {
		t.Fatalf("moved deposit: %+v, user holds %d", m, e.uargBalance(e.uAddr))
	}
	if st := e.leader.Status(); st.Reorged != 1 || len(st.Paused) != 0 {
		t.Fatalf("status: reorged %d, paused %+v", st.Reorged, st.Paused)
	}
}

// TestEVMReorgAfterIssueAlerts: a deposit already issued whose event leaves
// the chain raises an alert and pauses issuance on that chain on every
// guardian, until each operator resumes it.
func TestEVMReorgAfterIssueAlerts(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	d := e.issueDeposit(usdc, usd(100, 6))
	e.issueDeposit(usdc, usd(70, 6)) // the vault's counter stays above the id that leaves
	e.evm.dropDeposit(d.ID)
	e.evm.advance(1)
	e.scanAll()
	scope := pauseScopeEVM(e.chains[0].ID)
	for i, s := range []*Service{e.leader, e.f1, e.f2} {
		st := s.Status()
		if len(st.Paused) != 1 || st.Paused[0].Scope != scope || !strings.Contains(st.Paused[0].Reason, "was issued") || !strings.Contains(st.Chains[0].Paused, "paused") {
			t.Fatalf("guardian %d: no alert: %+v / %q", i, st.Paused, st.Chains[0].Paused)
		}
		if x, _ := s.Inbound(e.chains[0].ID, d.ID); x.Status != InboundConfirmed || !strings.Contains(x.Error, "left") {
			t.Fatalf("guardian %d: the issued deposit: %+v", i, x)
		}
	}
	// A new deposit on the paused chain waits; a follower asked directly refuses.
	var from [20]byte
	id2 := e.evm.addDeposit(usdc, from, usd(50, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.leader.Inbound(e.chains[0].ID, id2)
	if d2.Status != InboundPending {
		t.Fatalf("issued on a paused chain: %+v", d2)
	}
	_, _, req := issueTx(e.leader, e.matureCoins(e.f1)[0], []*Inbound{d2})
	var out struct{}
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("a paused follower signed: %v", err)
	}
	// The operators resume: issued.
	e.resumeAll()
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	e.round()
	if d2, _ = e.leader.Inbound(e.chains[0].ID, id2); d2.Status != InboundConfirmed {
		t.Fatalf("after resume: %+v", d2)
	}
}

// TestProviderDisagreement: with two RPC providers, a deposit only one of
// them reports, or a block they see differently, pauses the chain (it shows
// in the status) and nothing on it is issued, by the leader or by a
// follower asked directly; once they agree the deposit is issued.
func TestProviderDisagreement(t *testing.T) {
	t.Parallel()
	var second *fakeEVM
	e := newPocEnv(t, func(c *Config) {
		if second == nil {
			second = newFakeEVM(c.Chains[0].ID, c.Chains[0].Vault, nil, 2, 100, 100)
		}
		c.ExtraEVMs = map[uint64][]EVM{c.Chains[0].ID: {second}}
	})
	ctx := context.Background()
	e.fund()
	sync := func() { // the second provider sees exactly what the first does
		e.evm.mu.Lock()
		second.mu.Lock()
		second.deposits = append([]DepositEvent{}, e.evm.deposits...)
		second.forks = append([]uint64{}, e.evm.forks...)
		second.head = e.evm.head
		second.mu.Unlock()
		e.evm.mu.Unlock()
	}
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	id := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr) // only the first provider says so
	e.evm.advance(10)
	second.advance(11)
	err := e.round()
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("round: %v", err)
	}
	st := e.leader.Status()
	if !strings.Contains(st.Chains[0].Paused, "disagree") || st.Chains[0].Providers != 2 {
		t.Fatalf("chain status: %+v", st.Chains[0])
	}
	if d, _ := e.leader.Inbound(e.chains[0].ID, id); d != nil || len(e.leader.st.Issues()) != 0 {
		t.Fatalf("acted on while the providers disagree: %+v", d)
	}
	// They agree: the deposit is recorded on every guardian.
	sync()
	e.scanAll()
	d, _ := e.f1.Inbound(e.chains[0].ID, id)
	if d == nil || d.Status != InboundPending {
		t.Fatalf("after agreement: %+v", d)
	}
	// The second provider now sees another branch: the chain stops again.
	second.dropDeposit(id)
	if err := e.round(); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("round with different branches: %v", err)
	}
	if len(e.leader.st.Issues()) != 0 {
		t.Fatal("issued while the providers disagree")
	}
	_, _, req := issueTx(e.leader, e.matureCoins(e.f1)[0], []*Inbound{d})
	var out struct{}
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("a follower signed while its providers disagree: %v", err)
	}
	// Agreement again: issued.
	sync()
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.leader.Inbound(e.chains[0].ID, id); d.Status != InboundConfirmed {
		t.Fatalf("after agreement: %+v", d)
	}
	if st := e.leader.Status(); st.Chains[0].Paused != "" {
		t.Fatalf("still paused: %q", st.Chains[0].Paused)
	}
}

// TestPayoutDepthTiers: the depth a burn waits scales with its amount, on
// the leader and on every follower; above the last tier it is held until
// every guardian's operator approves it.
func TestPayoutDepthTiers(t *testing.T) {
	t.Parallel()
	tiers, err := ParseTiers("50:3,500:6")
	if err != nil {
		t.Fatal(err)
	}
	e := newPocEnv(t, func(c *Config) { c.ReleaseTiers = tiers })
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	e.issueDeposit(usdc, usd(400, 6)) // 396 uARG: the 6-block tier
	mid := e.burnNewest(9)
	e.mine(3, e.uAddr) // 4 confirmations
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if len(e.evm.releases) != 0 {
		t.Fatal("a 396 uARG burn was paid after 4 blocks")
	}
	if r, _ := e.leader.st.GetRedemption(mid); r.Confirmations != 6 {
		t.Fatalf("required depth on record: %d", r.Confirmations)
	}
	if _, err := e.f1.SignReleaseLocally(ctx, mid); err == nil || !strings.Contains(err.Error(), "of 6 confirmations") {
		t.Fatalf("a follower signed a mid-size burn too early: %v", err)
	}
	e.mine(2, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if len(e.evm.releases) != 1 {
		t.Fatalf("mid-size burn after 6 blocks: %d releases", len(e.evm.releases))
	}
	// Above the last tier: held, refused by followers, until approved.
	e.issueDeposit(usdc, usd(1000, 6)) // 990 uARG
	big := e.burnNewest(10)
	e.mine(8, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	r, _ := e.leader.st.GetRedemption(big)
	if r.Status != RedemptionPending || !strings.Contains(r.Error, "manual release") || len(e.evm.releases) != 1 {
		t.Fatalf("burn above the tiers: %+v", r)
	}
	if _, err := e.f2.SignReleaseLocally(ctx, big); err == nil || !strings.Contains(err.Error(), "largest automatic payout") {
		t.Fatalf("a follower signed a burn above the tiers: %v", err)
	}
	for i, u := range e.urls {
		if code, ans := adminCall(t, u, e.cs[i].local, "approve-release", map[string]string{"txid": big}); code != 200 {
			t.Fatalf("approve on %d: %d %s", i, code, ans)
		}
	}
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if len(e.evm.releases) != 2 {
		t.Fatalf("approved burn: %d releases", len(e.evm.releases))
	}
}

// reorgL1 replaces the top depth blocks of the env's node with depth+1
// empty blocks paying someone else.
func (e *pocEnv) reorgL1(depth uint32) {
	t := e.t
	info, _ := e.client.ChainInfo()
	fork := info.Blocks - depth
	chB, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer chB.Close()
	for h := uint32(1); h <= fork; h++ {
		n, _ := e.ch.NodeAtHeight(h)
		b, err := e.ch.Block(n.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chB.ProcessBlock(b, -1); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := btcec.NewPrivateKey()
	pay := consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: schnorr.SerializePubKey(other.PubKey())}
	for i := uint32(0); i <= depth; i++ {
		b := mineAlt(t, chB, pay)
		if _, err := e.ch.ProcessBlockOpt(b, -1, true); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := e.ch.NodeAtHeight(fork + 1); n == nil {
		t.Fatal("the node did not reorganise")
	}
}

// TestL1ReorgPause: an Argonmine reorganisation deeper than -l1-reorg-pause
// stops every release and issuance on every guardian until each operator
// resumes.
func TestL1ReorgPause(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t, func(c *Config) { c.L1ReorgPause = 2 })
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	e.issueDeposit(usdc, usd(100, 6))
	burn := e.burnNewest(9)
	e.mine(3, e.uAddr)
	e.scanAll()
	// Three blocks above the burn's depth are replaced (deeper than 2).
	e.reorgL1(3)
	e.scanAll()
	for i, s := range []*Service{e.leader, e.f1, e.f2} {
		st := s.Status()
		if len(st.Paused) != 1 || st.Paused[0].Scope != pauseL1 || !strings.Contains(st.Paused[0].Reason, "3 blocks deep") || st.L1ReorgPause != 2 {
			t.Fatalf("guardian %d: %+v", i, st.Paused)
		}
	}
	e.mine(3, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if len(e.evm.releases) != 0 {
		t.Fatal("released while paused")
	}
	if _, err := e.f1.SignReleaseLocally(ctx, burn); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("a paused follower signed a release: %v", err)
	}
	// Only from this host, and only signed with the guardian's own key.
	stranger, _ := btcec.NewPrivateKey()
	if code, _ := adminCall(t, e.urls[1], stranger, "resume", map[string]string{}); code != http.StatusUnauthorized {
		t.Fatalf("resume signed by a stranger: %d", code)
	}
	if code, _ := adminCall(t, e.urls[1], e.cs[0].local, "resume", map[string]string{}); code != http.StatusUnauthorized {
		t.Fatalf("resume signed by another guardian: %d", code)
	}
	raw := []byte(`{"scope":"all"}`)
	hdr, _ := AdminHeaders(consensus.RegTest.Name, e.cs[1].local, "/admin/resume", raw, time.Now())
	proxied := hdr.Clone()
	proxied.Set("X-Forwarded-For", "203.0.113.9")
	if code, _ := postGuardian(t, e.urls[1]+"/admin/resume", raw, proxied); code != http.StatusForbidden {
		t.Fatalf("a proxied operator command was taken: %d", code)
	}
	if code, ans := postGuardian(t, e.urls[1]+"/admin/resume", raw, hdr); code != 200 || !strings.Contains(ans, "resumed l1") {
		t.Fatalf("resume: %d %s", code, ans)
	}
	if code, _ := postGuardian(t, e.urls[1]+"/admin/resume", raw, hdr); code != http.StatusUnauthorized {
		t.Fatalf("a replayed operator command was taken: %d", code)
	}
	e.resumeAll()
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if len(e.evm.releases) != 1 {
		t.Fatalf("after resume: %d releases", len(e.evm.releases))
	}
}

// TestTokenDecimalsMismatch: a token whose decimals() differs from the
// configuration is refused: its deposits wait, followers refuse them, and
// the status says why; the other token works. Fixed in the file, the
// waiting deposit is recomputed and issued.
func TestTokenDecimalsMismatch(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	usdc, usdt := e.chains[0].Tokens[0].Address, e.chains[0].Tokens[1].Address
	e.evm.setDecimals(usdt, 8) // the file says 6
	var from [20]byte
	idT := e.evm.addDeposit(usdt, from, big.NewInt(5_000_000_000), e.uAddr) // 50 USDT at 8 decimals
	idC := e.evm.addDeposit(usdc, from, usd(10, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	e.round()
	dT, _ := e.leader.Inbound(e.chains[0].ID, idT)
	dC, _ := e.leader.Inbound(e.chains[0].ID, idC)
	if dC.Status != InboundConfirmed || dT.Status != InboundPending || !strings.Contains(dT.Error, "decimals") {
		t.Fatalf("deposits: USDC %+v, USDT %+v", dC, dT)
	}
	st := e.leader.Status()
	if !strings.Contains(st.Chains[0].Tokens[1].Error, "8 decimals on chain") || st.Chains[0].Tokens[0].Error != "" {
		t.Fatalf("token status: %+v", st.Chains[0].Tokens)
	}
	_, _, req := issueTx(e.leader, e.matureCoins(e.f1)[0], []*Inbound{dT})
	var out struct{}
	if err := e.leader.callPeer(ctx, e.urls[2], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "decimals") {
		t.Fatalf("a follower signed a refused token: %v", err)
	}
	// The file is fixed (8 decimals) and the guardians restarted: the
	// deposit is recomputed from its token amount and issued.
	e.stop()
	fixed := append([]Chain{}, e.chains...)
	fixed[0].Tokens = append([]Token{}, e.chains[0].Tokens...)
	fixed[0].Tokens[1].Decimals = 8
	fixed[0].Tokens[1].MinDeposit = big.NewInt(100_000_000)
	e.start("g", fixed, e.evm)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	e.round()
	if d, _ := e.leader.Inbound(e.chains[0].ID, idT); d.Status != InboundConfirmed || d.Amount != 4_950_000_000 || d.Decimals != 8 {
		t.Fatalf("after the fix: %+v", d)
	}
}

// TestPerDepositCap: a deposit above its token's max_deposit is held with
// the reason (the funds stay in the vault), refused by followers, and can
// be marked unclaimable after a refund by hand; smaller ones go through.
func TestPerDepositCap(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t, func(c *Config) { c.Chains[0].Tokens[0].MaxDeposit = usd(500, 6) })
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	big1 := e.evm.addDeposit(usdc, from, usd(1000, 6), e.uAddr)
	small := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	e.round()
	db, _ := e.leader.Inbound(e.chains[0].ID, big1)
	ds, _ := e.leader.Inbound(e.chains[0].ID, small)
	if db.Status != InboundPending || !strings.Contains(db.Error, "per-deposit cap") || ds.Status != InboundConfirmed {
		t.Fatalf("deposits: %+v / %+v", db, ds)
	}
	st := e.leader.Status()
	if st.Held != 1 || st.Chains[0].Tokens[0].MaxDeposit != "500000000" {
		t.Fatalf("status: held %d, max_deposit %q", st.Held, st.Chains[0].Tokens[0].MaxDeposit)
	}
	_, _, req := issueTx(e.leader, e.matureCoins(e.f1)[0], []*Inbound{db})
	var out struct{}
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "per-deposit cap") {
		t.Fatalf("a follower signed above the cap: %v", err)
	}
	// Refunded by hand: every guardian marks it so it is never issued.
	for i, u := range e.urls {
		if code, ans := adminCall(t, u, e.cs[i].local, "deposit-unclaimable", map[string]any{"chain_id": e.chains[0].ID, "id": big1, "reason": "refunded"}); code != 200 {
			t.Fatalf("mark on %d: %d %s", i, code, ans)
		}
	}
	if d, _ := e.f2.Inbound(e.chains[0].ID, big1); d.Status != InboundUnclaimable || !strings.Contains(d.Error, "refunded") {
		t.Fatalf("after marking: %+v", d)
	}
}

// TestReleaseRebroadcast: a release not mined after the timeout is sent
// again with the same nonce and fees 12.5 percent higher; when the first
// transaction lands late the burn is paid once and confirmed from it. A
// release whose nonce went to another transaction is sent again with a new
// nonce. Fee raises stop at the chain's cap and the item shows as stuck.
func TestReleaseRebroadcast(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	e := newPocEnv(t, func(c *Config) { c.Now = clock.now; c.ReleaseTimeout = 5 * time.Minute; c.StuckAfter = time.Hour })
	e.fund()
	e.evm.hold = true
	usdc := e.chains[0].Tokens[0].Address
	e.issueDeposit(usdc, usd(300, 6))
	burn := e.burnNewest(9)
	e.mine(3, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	r, _ := e.leader.st.GetRedemption(burn)
	if r.Status != RedemptionReleased || r.Send == nil || len(r.Send.Hashes) != 1 {
		t.Fatalf("first send: %+v", r)
	}
	first := r.Send.Hashes[0]
	firstCap := mustBig(r.Send.FeeCap)
	// Before the timeout nothing is sent again.
	clock.add(4 * time.Minute)
	e.leader.Once(context.Background())
	if r, _ = e.leader.st.GetRedemption(burn); len(r.Send.Hashes) != 1 {
		t.Fatal("sent again before the timeout")
	}
	clock.add(2 * time.Minute)
	e.leader.Once(context.Background())
	r, _ = e.leader.st.GetRedemption(burn)
	if len(r.Send.Hashes) != 2 || r.Send.Nonce != 0 || r.Send.Bumps != 1 || r.EVMTx != r.Send.Hashes[1] {
		t.Fatalf("after the timeout: %+v", r.Send)
	}
	if minCap := mulDivUp(firstCap, 1125, 1000); mustBig(r.Send.FeeCap).Cmp(minCap) < 0 || mustBig(r.Send.TipCap).Cmp(big.NewInt(1_125_000_000)) < 0 {
		t.Fatalf("fees not raised by 12.5%%: %s/%s after %s", r.Send.TipCap, r.Send.FeeCap, firstCap)
	}
	// The first transaction lands late: paid once, confirmed from it.
	if e.evm.mineRelease(first) != ReceiptOK {
		t.Fatal("the first transaction did not pay")
	}
	e.leader.Once(context.Background())
	r, _ = e.leader.st.GetRedemption(burn)
	if r.Status != RedemptionConfirmed || r.EVMTx != first || len(e.evm.releases) != 1 {
		t.Fatalf("after the late landing: %+v, %d releases", r, len(e.evm.releases))
	}
	sends := len(e.evm.sends)
	clock.add(time.Hour)
	e.leader.Once(context.Background())
	if len(e.evm.sends) != sends || len(e.evm.releases) != 1 {
		t.Fatal("sent again after the burn was paid")
	}

	// A second burn whose nonce is taken by another transaction: sent again
	// with the next nonce, paid once.
	e.issueDeposit(usdc, usd(200, 6))
	burn2 := e.burnNewest(10)
	e.mine(3, e.uAddr)
	e.round()
	r2, _ := e.leader.st.GetRedemption(burn2)
	if r2.Status != RedemptionReleased {
		t.Fatalf("second burn: %+v", r2)
	}
	e.evm.takeNonce()
	e.leader.Once(context.Background())
	r2, _ = e.leader.st.GetRedemption(burn2)
	if len(r2.Send.Hashes) != 2 || r2.Send.Nonce != r.Send.Nonce+2 {
		t.Fatalf("resent with a new nonce: %+v (first release's nonce %d)", r2.Send, r.Send.Nonce)
	}
	e.evm.mineRelease(r2.Send.Hashes[1])
	e.leader.Once(context.Background())
	if r2, _ = e.leader.st.GetRedemption(burn2); r2.Status != RedemptionConfirmed || len(e.evm.releases) != 2 {
		t.Fatalf("second burn after the resend: %+v", r2)
	}

	// A third whose fees cannot rise under the cap: it waits, is reported
	// stuck after -stuck-after, and is never sent above the cap.
	e.leader.byID[e.chains[0].ID].MaxGasPrice = big.NewInt(3_200_000_000)
	e.issueDeposit(usdc, usd(100, 6))
	burn3 := e.burnNewest(11)
	e.mine(3, e.uAddr)
	e.round()
	clock.add(10 * time.Minute)
	e.leader.Once(context.Background())
	r3, _ := e.leader.st.GetRedemption(burn3)
	if r3.Status != RedemptionReleased || len(r3.Send.Hashes) != 1 || !strings.Contains(r3.Error, "gas price cap") {
		t.Fatalf("third burn at the cap: %+v", r3)
	}
	clock.add(2 * time.Hour)
	found := false
	for _, it := range e.leader.Status().Stuck {
		if it.ID == burn3 && it.Kind == "release" && strings.Contains(it.Reason, "gas price cap") {
			found = true
		}
	}
	if !found {
		t.Fatalf("not reported stuck: %+v", e.leader.Status().Stuck)
	}
	for _, s := range e.evm.sends {
		if mustBig(s.FeeCap).Cmp(big.NewInt(3_200_000_000)) > 0 && s.Nonce == r3.Send.Nonce {
			t.Fatalf("sent above the cap: %+v", s)
		}
	}
}

// TestReleaseLimitsAndPause: a burn above what the vault's 24-hour window
// allows waits (never fails) and is paid when the window has room; a limit
// reached between the check and the send waits too; a paused vault holds
// every payout; the status shows available_now and vault_paused.
func TestReleaseLimitsAndPause(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	e.issueDeposit(usdc, usd(100, 6))
	e.evm.mu.Lock()
	e.evm.periodAvail[usdc] = usd(20, 6)
	e.evm.mu.Unlock()
	burn := e.burnNewest(9) // 99 USDC gross
	e.mine(3, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	r, _ := e.leader.st.GetRedemption(burn)
	if r.Status != RedemptionPending || !strings.Contains(r.Error, "24-hour release window") || len(e.evm.releases) != 0 {
		t.Fatalf("burn over the window: %+v", r)
	}
	st := e.leader.Status()
	if tok := st.Chains[0].Tokens[0]; tok.AvailableNow != "20000000" || tok.PeriodAvailable != "20000000" {
		t.Fatalf("token status: %+v", tok)
	}
	// The window frees up, but the limit is hit at the moment of sending.
	e.evm.mu.Lock()
	e.evm.periodAvail[usdc] = usd(1000, 6)
	e.evm.limitOnce = 1
	e.evm.mu.Unlock()
	e.round()
	if r, _ = e.leader.st.GetRedemption(burn); r.Status != RedemptionPending || !strings.Contains(r.Error, "release limits") {
		t.Fatalf("burn after LimitExceeded: %+v", r)
	}
	e.round()
	if r, _ = e.leader.st.GetRedemption(burn); r.Status != RedemptionReleased && r.Status != RedemptionConfirmed {
		t.Fatalf("burn once the window has room: %+v", r)
	}
	// A paused vault holds payouts.
	e.issueDeposit(usdc, usd(50, 6))
	e.evm.mu.Lock()
	e.evm.paused = true
	e.evm.mu.Unlock()
	burn2 := e.burnNewest(10)
	e.mine(3, e.uAddr)
	e.round()
	r2, _ := e.leader.st.GetRedemption(burn2)
	if r2.Status != RedemptionPending || !strings.Contains(r2.Error, "paused") || len(e.evm.releases) != 1 {
		t.Fatalf("burn on a paused vault: %+v", r2)
	}
	if st := e.leader.Status(); !st.Chains[0].VaultPaused || st.Chains[0].Tokens[0].AvailableNow != "0" {
		t.Fatalf("paused vault status: %+v", st.Chains[0])
	}
	e.evm.mu.Lock()
	e.evm.paused = false
	e.evm.mu.Unlock()
	e.round()
	if len(e.evm.releases) != 2 {
		t.Fatalf("after the pause: %d releases", len(e.evm.releases))
	}
}

// TestGasKey: releases are sent from the client's gas key, never from a
// guardian key: a client that would send from the guardian's own EVM key
// is refused at start, a gas key that is a guardian of the vault or its
// owner sends nothing, and the status shows the gas key and its balance.
func TestGasKey(t *testing.T) {
	t.Parallel()
	gas, _ := secp256k1.GeneratePrivateKey()
	gasAddr := AddressOf(gas.PubKey())
	e := newPocEnv(t)
	e.evm.sender = gasAddr
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	e.issueDeposit(usdc, usd(100, 6))
	e.burnNewest(9)
	e.mine(3, e.uAddr)
	e.round()
	if len(e.evm.releases) != 1 || e.evm.sentFrom[gasAddr] != 1 || len(e.evm.sentFrom) != 1 {
		t.Fatalf("senders: %v", e.evm.sentFrom)
	}
	st := e.leader.Status()
	if st.GasAddress != HexAddress(gasAddr) || st.Chains[0].GasBalance != "1000000000000000000" || st.Chains[0].GasLow {
		t.Fatalf("gas status: %s %+v", st.GasAddress, st.Chains[0])
	}
	e.evm.mu.Lock()
	e.evm.native = big.NewInt(1000)
	e.evm.mu.Unlock()
	e.round()
	if !e.leader.Status().Chains[0].GasLow {
		t.Fatal("a low gas balance is not reported")
	}
	// The guardian's own key as the sender: refused at start.
	own := newFakeEVM(e.chains[0].ID, e.chains[0].Vault, e.evmAddrs, 2, 100, 100)
	own.sender = e.evmAddrs[0]
	_, err := New(Config{Params: consensus.RegTest, Node: e.client, Chains: e.chains, EVMs: map[uint64]EVM{e.chains[0].ID: own}, Custody: e.cs[0], EVMKey: e.evmKeys[0],
		Leader: true, Threshold: 2, StatePath: filepath.Join(t.TempDir(), "x")})
	if err == nil || !strings.Contains(err.Error(), "gas key") {
		t.Fatalf("a client sending from the guardian key was accepted: %v", err)
	}
	// A gas key that is one of the vault's guardians (a guardian key that is
	// not one of these daemons'): nothing is sent from it.
	other, _ := secp256k1.GeneratePrivateKey()
	otherAddr := AddressOf(other.PubKey())
	e.evm.mu.Lock()
	e.evm.guardians[otherAddr] = true
	e.evm.sender = otherAddr
	e.evm.native = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	e.evm.mu.Unlock()
	e.stop()
	e.start("g", e.chains, e.evm)
	e.issueDeposit(usdc, usd(50, 6))
	burn := e.burnNewest(10)
	e.mine(3, e.uAddr)
	e.round()
	if r, _ := e.leader.st.GetRedemption(burn); r.Status != RedemptionPending || !strings.Contains(r.Error, "is a guardian") || e.evm.sentFrom[otherAddr] != 0 {
		t.Fatalf("release from a guardian's key: %+v (sent %v)", r, e.evm.sentFrom)
	}
	if !strings.Contains(e.leader.Status().Chains[0].Error, "is a guardian") {
		t.Fatalf("status: %+v", e.leader.Status().Chains[0])
	}
}

// TestPublicStatusRPC: the status shows a chain's public_rpc and never its
// rpc or extra providers, which may carry an API key; without public_rpc it
// shows none.
func TestPublicStatusRPC(t *testing.T) {
	doc := `{"chains":[{"name":"Keyed","chain_id":31337,"rpc":"https://eth.example/v2/SECRETKEY1","rpcs":["https://other.example/?apikey=SECRETKEY2"],
	  "vault":"0x0000000000000000000000000000000000000077","confirmations":5,
	  "tokens":[{"symbol":"USDC","address":"0x0000000000000000000000000000000000000055","decimals":6}]},
	 {"name":"Public","chain_id":31338,"rpc":"https://eth.example/v2/SECRETKEY3","public_rpc":"https://public.example","vault":"0x0000000000000000000000000000000000000088","confirmations":5,
	  "tokens":[{"symbol":"USDC","address":"0x0000000000000000000000000000000000000066","decimals":6}]}]}`
	chains, err := ParseChains([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(chains[0].RPCs) != 2 || chains[0].PublicRPC != "" || chains[1].PublicRPC != "https://public.example" {
		t.Fatalf("parsed: %+v", chains)
	}
	cs, _ := guardians(t, 1)
	evms := map[uint64]EVM{}
	for _, c := range chains {
		evms[c.ID] = newFakeEVM(c.ID, c.Vault, nil, 1, 0, 0)
	}
	extra := map[uint64][]EVM{chains[0].ID: {newFakeEVM(chains[0].ID, chains[0].Vault, nil, 1, 0, 0)}}
	s, err := New(Config{Params: consensus.RegTest, Node: wallet.NewClient("http://127.0.0.1:1", "x"), Chains: chains, EVMs: evms, ExtraEVMs: extra,
		Custody: cs[0], Leader: true, Threshold: 1, StatePath: filepath.Join(t.TempDir(), "s"), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.scanEVM(context.Background())
	ws := httptest.NewServer(s.Handler())
	defer ws.Close()
	resp, err := http.Get(ws.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("SECRETKEY")) || bytes.Contains(body, []byte("eth.example")) || bytes.Contains(body, []byte("other.example")) {
		t.Fatalf("a private RPC reached the status: %s", body)
	}
	var st Status
	json.Unmarshal(body, &st)
	if st.Chains[0].RPC != "" || st.Chains[1].RPC != "https://public.example" || st.Chains[0].Providers != 2 || st.Chains[0].Finality != FinalityDepth {
		t.Fatalf("chains: %+v", st.Chains)
	}
	if !bytes.Contains(body, []byte(`"rpc":"https://public.example"`)) || bytes.Count(body, []byte(`"rpc":`)) != 1 {
		t.Fatalf("rpc fields: %s", body)
	}
}

// TestHardeningConfig pins the new configuration rules, the payout tiers
// and the fee raises.
func TestHardeningConfig(t *testing.T) {
	base := `{"chains":[{"name":"X","chain_id":%d,"rpc":"http://x",%s"vault":"0x0000000000000000000000000000000000000077",
	  "tokens":[{"symbol":"USDC","address":"0x0000000000000000000000000000000000000055","decimals":6%s}]}]}`
	parse := func(id uint64, chain, token string) ([]Chain, error) {
		return ParseChains([]byte(fmt.Sprintf(base, id, chain, token)))
	}
	for id, want := range map[uint64]string{11155111: FinalityFinalized, 84532: FinalityFinalized, 97: FinalityFinalized, 421614: FinalityFinalized, 31337: FinalityDepth, 777: FinalityDepth} {
		c, err := parse(id, "", "")
		if err != nil || c[0].Finality != want {
			t.Errorf("chain %d: finality %v %v", id, c, err)
		}
	}
	if c, err := parse(1, `"finality":"depth","max_gas_price_gwei":0.5,"min_gas_balance":"0.25",`, `,"max_deposit":"5000000"`); err != nil ||
		c[0].Finality != FinalityDepth || c[0].MaxGasPrice.String() != "500000000" || c[0].MinGasBalance.String() != "250000000000000000" || c[0].Tokens[0].MaxDeposit.String() != "5000000" {
		t.Fatalf("explicit fields: %+v %v", c, err)
	}
	if c, _ := parse(1, "", ""); c[0].MaxGasPrice.String() != "300000000000" || c[0].Tokens[0].MaxDeposit != nil {
		t.Fatalf("defaults: %+v", c[0])
	}
	if c, err := ParseChains([]byte(`{"chains":[{"name":"X","chain_id":5,"rpcs":["http://a","http://b","http://a"],"vault":"0x0000000000000000000000000000000000000077","tokens":[{"symbol":"U","address":"0x0000000000000000000000000000000000000055","decimals":6}]}]}`)); err != nil || c[0].RPC != "http://a" || len(c[0].RPCs) != 2 {
		t.Fatalf("rpcs only: %+v %v", c, err)
	}
	for name, doc := range map[string][2]string{
		"bad finality":   {`"finality":"safe",`, ""},
		"zero gas cap":   {`"max_gas_price_gwei":0,`, ""},
		"bad gas cap":    {`"max_gas_price_gwei":"x",`, ""},
		"bad min gas":    {`"min_gas_balance":"-1",`, ""},
		"cap below min":  {"", `,"max_deposit":"10"`},
		"zero cap":       {"", `,"max_deposit":"0"`},
		"five providers": {`"rpcs":["http://a","http://b","http://c","http://d"],`, ""},
	} {
		if _, err := parse(1, doc[0], doc[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	tiers, err := ParseTiers("1000:6, 10000:30,100000:120")
	if err != nil || len(tiers) != 3 || tiers[1].UpTo != 10000*consensus.UnitsPerCoin || tiers[2].Confirmations != 120 {
		t.Fatalf("tiers: %+v %v", tiers, err)
	}
	if none, err := ParseTiers(""); err != nil || len(none) != 0 {
		t.Fatalf("no tiers: %v %v", none, err)
	}
	for _, bad := range []string{"1000", "1000:0", "x:5", "1000:6,500:30", "1000:30,2000:6"} {
		if _, err := ParseTiers(bad); err == nil {
			t.Errorf("tiers %q accepted", bad)
		}
	}
	// Fee raises: +12.5% on both EIP-1559 fees, +10% legacy, under the cap.
	tip, fc, err := Bump1559(big.NewInt(1000), big.NewInt(10000), big.NewInt(1), big.NewInt(1), nil)
	if err != nil || tip.Int64() != 1125 || fc.Int64() != 11250 {
		t.Fatalf("1559 raise: %v %v %v", tip, fc, err)
	}
	if tip, fc, _ = Bump1559(big.NewInt(1000), big.NewInt(10000), big.NewInt(2000), big.NewInt(9000), nil); tip.Int64() != 2000 || fc.Int64() != 20000 {
		t.Fatalf("1559 raise to the network: %v %v", tip, fc)
	}
	if _, _, err = Bump1559(big.NewInt(1000), big.NewInt(10000), big.NewInt(1), big.NewInt(1), big.NewInt(11000)); err != ErrGasCap {
		t.Fatalf("1559 raise above the cap: %v", err)
	}
	if _, fc, _ = Bump1559(big.NewInt(1000), big.NewInt(10000), big.NewInt(1), big.NewInt(9000), big.NewInt(12000)); fc.Int64() != 12000 {
		t.Fatalf("1559 raise clipped to the cap: %v", fc)
	}
	if gp, err := BumpLegacy(big.NewInt(1000), big.NewInt(1), nil); err != nil || gp.Int64() != 1100 {
		t.Fatalf("legacy raise: %v %v", gp, err)
	}
	if _, err := BumpLegacy(big.NewInt(1000), big.NewInt(1), big.NewInt(1050)); err != ErrGasCap {
		t.Fatalf("legacy raise above the cap: %v", err)
	}
	// A release call decodes back to what was encoded.
	var token, to [20]byte
	token[0], to[1] = 1, 2
	id := [32]byte{3}
	sigs := [][]byte{bytes.Repeat([]byte{4}, 65), bytes.Repeat([]byte{5}, 65)}
	gt, gto, ga, gid, gs, err := DecodeReleaseCall(EncodeReleaseCall(token, to, big.NewInt(77), id, sigs))
	if err != nil || gt != token || gto != to || ga.Int64() != 77 || gid != id || len(gs) != 2 || !bytes.Equal(gs[1], sigs[1]) {
		t.Fatalf("release call: %v", err)
	}
}

// TestIssuanceRebroadcast: an issuance that is neither mined nor in the
// node's mempool is broadcast again after -issue-rebroadcast, and its
// deposits go back to pending after 30 minutes if it is still missing.
func TestIssuanceRebroadcast(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	e := newPocEnv(t, func(c *Config) { c.Now = clock.now; c.IssueRebroadcast = 10 * time.Minute })
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	id := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d, _ := e.leader.Inbound(e.chains[0].ID, id)
	if d.Status != InboundIssued {
		t.Fatalf("not issued: %+v", d)
	}
	// The leader now talks to a node whose mempool lost the issuance.
	emptyNode := func() (*wallet.Client, *mempool.Pool) {
		mp := mempool.New(e.ch, mempool.DefaultConfig())
		srv := rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: "t"}, rpc.Deps{Params: consensus.RegTest, Chain: e.ch, Pool: mp, Started: time.Now(), Logf: func(string, ...any) {}})
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Stop)
		return wallet.NewClient("http://"+srv.Addr(), "t"), mp
	}
	client2, mp2 := emptyNode()
	e.leader.cfg.Node = client2
	clock.add(5 * time.Minute)
	e.leader.Once(context.Background())
	if mp2.Has(mustHash(t, d.IssueTx)) {
		t.Fatal("broadcast again before the timeout")
	}
	clock.add(6 * time.Minute)
	e.leader.Once(context.Background())
	if !mp2.Has(mustHash(t, d.IssueTx)) {
		t.Fatal("the issuance was not broadcast again")
	}
	if x, _ := e.leader.Inbound(e.chains[0].ID, id); x.Status != InboundIssued || x.IssueTx != d.IssueTx {
		t.Fatalf("after the rebroadcast: %+v", x)
	}
	// Still missing after 30 minutes (another node again): issued anew.
	client3, _ := emptyNode()
	e.leader.cfg.Node = client3
	clock.add(25 * time.Minute)
	e.leader.reissueDropped(e.leader.st.Inbounds())
	if x, _ := e.leader.Inbound(e.chains[0].ID, id); x.Status != InboundPending {
		t.Fatalf("after 30 minutes: %+v", x)
	}
}

// TestAdminOffline: with the daemon stopped, the operator commands act on
// its state directory.
func TestAdminOffline(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := openState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.PutPause(&Pause{Scope: pauseL1, Reason: "deep reorganisation", Since: 1})
	st.PutPause(&Pause{Scope: pauseScopeEVM(5), Reason: "alert", Since: 2})
	st.Close()
	res, err := AdminOffline(dir, "resume", []byte(`{"scope":"evm/5"}`), t.Logf)
	if err != nil || res.Done != "resumed evm/5" || len(res.Pauses) != 1 || res.Pauses[0].Scope != pauseL1 {
		t.Fatalf("resume evm/5: %+v %v", res, err)
	}
	if res, err = AdminOffline(dir, "resume", nil, t.Logf); err != nil || len(res.Pauses) != 0 {
		t.Fatalf("resume all: %+v %v", res, err)
	}
	if _, err := AdminOffline(dir, "approve-release", []byte(`{"txid":"zz"}`), t.Logf); err == nil {
		t.Fatal("a bad txid was approved")
	}
	if _, err := AdminOffline(dir, "format-disk", nil, t.Logf); err == nil {
		t.Fatal("an unknown action was taken")
	}
	txid := strings.Repeat("ab", 32)
	if _, err := AdminOffline(dir, "approve-release", []byte(`{"txid":"`+txid+`"}`), t.Logf); err != nil {
		t.Fatal(err)
	}
	st, _ = openState(dir)
	defer st.Close()
	if !st.ReleaseApproved(txid) || len(st.Pauses()) != 0 {
		t.Fatal("the offline commands were not kept")
	}
}
