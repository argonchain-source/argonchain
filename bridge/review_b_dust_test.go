package bridge

// Review B (custody and payout core): the minimum payout, and the payout
// depth of a transaction paid out in several outputs.

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"argonmine/consensus"
)

// TestDustPaymentsNotReleased (review B, fixed 2026-10-28): a redemption
// used to be unreleasable only when it converted to zero token units, so
// one cheap transaction with many dust outputs to a swap address (asset
// outputs are exempt from the dust limit) made the leader send one release
// transaction per output and emptied the gas key. Now every token has a
// minimum release (min_release in the chains file, by default its
// min_deposit): a payout below it is recorded as unreleasable with the
// reason on every guardian, never signed by any of them and never sent,
// while a payout at the minimum is paid. The status shows the minimum per
// token, and the page's POST /api/withdrawals asks for at least as much.
func TestDustPaymentsNotReleased(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	c := e.chains[0]
	e.issueDeposit(c.Tokens[0].Address, usd(2, 6)) // 1.98 uARG
	var to [20]byte
	to[19] = 0xAB
	p, err := SwapPayload(SwapDest{ChainID: c.ID, Symbol: "USDC", To: to})
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	var outs []consensus.TxOut
	for i := 0; i < n; i++ {
		outs = append(outs, consensus.TxOut{Amount: 100, Type: consensus.OutputTypeP2PK, Payload: p[:], Asset: e.leader.Asset()})
	}
	// And one payout of exactly the minimum: 1 uARG, 1 USDC gross.
	outs = append(outs, consensus.TxOut{Amount: consensus.UnitsPerCoin, Type: consensus.OutputTypeP2PK, Payload: p[:], Asset: e.leader.Asset()})
	pay := e.paySwap(outs, 10)
	e.mine(4, e.uAddr)
	before := len(e.evm.sends)
	for i := 0; i < 3; i++ {
		e.round()
	}
	if sent := len(e.evm.sends) - before; sent != 1 || len(e.evm.releases) != 1 || e.evm.releases[0].amount.String() != "1000000" {
		t.Fatalf("one transaction paying %d dust outputs and one of 1 uARG: %d release transactions sent, %d paid", n, sent, len(e.evm.releases))
	}
	for i, s := range []*Service{e.leader, e.f1, e.f2} {
		for v := 0; v < n; v++ {
			id := pay + ":" + itoa(v)
			r, ok := s.st.GetRedemption(id)
			if !ok || r.Status != RedemptionUnreleasable || r.Error != "below the minimum withdrawal of 1 USDC on Local A: not paid out (it needs the guardians)" {
				t.Fatalf("guardian %d: dust payout %s: %+v", i, id, r)
			}
		}
	}
	if r, _ := e.leader.st.GetRedemption(pay + ":" + itoa(n)); r.Status != RedemptionConfirmed {
		t.Fatalf("the payout at the minimum: %+v", r)
	}
	// Every follower refuses to sign a payout below the minimum on its own.
	if _, err := e.f1.SignReleaseLocally(ctx, pay+":0"); err == nil || !strings.Contains(err.Error(), "below the minimum withdrawal of 1 USDC") {
		t.Fatalf("a follower signed a dust payout: %v", err)
	}
	// And so does signRelease itself, whatever the redemption on record says.
	r, _ := e.leader.st.GetRedemption(pay + ":1")
	if _, err := e.leader.signRelease(r, e.leader.byID[c.ID], c.Tokens[0].Address, to, big.NewInt(1)); err == nil || !strings.Contains(err.Error(), "below the minimum withdrawal") {
		t.Fatalf("signRelease signed 1 token unit: %v", err)
	}
	if _, ok := e.f1.st.SignedBurn(pay + ":0"); ok {
		t.Fatal("a dust payout was signed")
	}

	// The minimum per token in the status (USDT's min_deposit is 2 USDT),
	// and the page's withdrawals ask for at least as much (and 1 uARG).
	st := e.leader.Status()
	if ts := st.Chains[0].Tokens; ts[0].MinRelease != "1000000" || ts[1].MinRelease != "2000000" {
		t.Fatalf("status: min_release %q %q", ts[0].MinRelease, ts[1].MinRelease)
	}
	var payee [20]byte
	payee[19] = 0xC1
	if _, err := e.leader.createWithdrawal(c.ID, "USDT", HexAddress(payee), 150_000_000); err == nil || !strings.Contains(err.Error(), "The smallest withdrawal is 2.00000000 uARG") {
		t.Fatalf("a 1.5 uARG USDT withdrawal: %v", err)
	}
	if _, err := e.leader.createWithdrawal(c.ID, "USDT", HexAddress(payee), 2*consensus.UnitsPerCoin); err != nil && strings.Contains(err.Error(), "smallest") {
		t.Fatalf("a 2 uARG USDT withdrawal: %v", err) // (the vault holds no USDT here: refused for liquidity, not the minimum)
	}
	if _, err := e.leader.createWithdrawal(c.ID, "USDC", HexAddress(payee), consensus.UnitsPerCoin); err != nil {
		t.Fatalf("a 1 uARG USDC withdrawal: %v", err)
	}
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

// TestMinRelease: the chains file's min_release and its default, and the
// smallest withdrawal it makes the page ask for.
func TestMinRelease(t *testing.T) {
	chains, err := ParseChains([]byte(testChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if a := chains[0].Tokens; a[0].MinRelease.String() != "1000000" || a[1].MinRelease.String() != "2000000" {
		t.Fatalf("defaults from min_deposit: %s %s", a[0].MinRelease, a[1].MinRelease)
	}
	if b := chains[1].Tokens[0]; b.MinRelease.String() != "1000000000000000000" {
		t.Fatalf("18 decimals: %s", b.MinRelease)
	}
	doc := strings.Replace(testChainsJSON, `"decimals":6,"mint":true},`, `"decimals":6,"mint":true,"min_deposit":"0"},`, 1)
	if cs, err := ParseChains([]byte(doc)); err != nil || cs[0].Tokens[0].MinRelease.String() != "1000000" {
		t.Fatalf("min_deposit 0: one token (%v)", err)
	}
	doc = strings.Replace(testChainsJSON, `"decimals":6,"mint":true},`, `"decimals":6,"mint":true,"min_release":"25000000"},`, 1)
	if cs, err := ParseChains([]byte(doc)); err != nil || cs[0].Tokens[0].MinRelease.String() != "25000000" {
		t.Fatalf("min_release 25 USDC: %v", err)
	}
	for _, bad := range []string{`"0"`, `"-1"`, `"1.5"`, `"x"`} {
		doc = strings.Replace(testChainsJSON, `"decimals":6,"mint":true},`, `"decimals":6,"mint":true,"min_release":`+bad+`},`, 1)
		if _, err := ParseChains([]byte(doc)); err == nil || !strings.Contains(err.Error(), "min_release") {
			t.Fatalf("min_release %s accepted: %v", bad, err)
		}
	}
	unit := consensus.Amount(consensus.UnitsPerCoin)
	for _, c := range []struct {
		dec  int
		min  string
		want consensus.Amount
	}{
		{6, "1", unit},              // below 1 uARG: 1 uARG
		{6, "2000000", 2 * unit},    // 2 USDT
		{6, "2500001", 250_000_100}, // exact for 6 decimals
		{18, "1000000000000000000", unit},
		{18, "1000000000000000001", unit + 1}, // rounded up: 1 uARG pays 10^18 units, one unit short
		{8, "123456789", 123456789},
	} {
		m, _ := new(big.Int).SetString(c.min, 10)
		if got := minWithdrawUnits(&Token{Decimals: c.dec, MinRelease: m}); got != c.want {
			t.Errorf("min_release %s at %d decimals: %d uARG units, want %d", c.min, c.dec, got, c.want)
		}
	}
}

// TestSplitPaymentPayoutDepth (review B, fixed 2026-10-28): the payout
// depth used to be chosen per redemption, and a payment to swap addresses
// is one redemption per output, so a payout above the last tier, split
// into outputs under it, was paid at a shallow depth without the manual
// approval. A reorganisation undoes all outputs of a transaction together:
// now the depth, and the hold above the last tier, follow what the whole
// transaction pays out of the bridge (every swap output and a burn), on
// the leader and on every follower from its own node.
//
// With tiers 50:3,500:6, 98 uARG as two outputs of 49 waits the 6-block
// tier, not 3; 980 uARG as two outputs of 490 is above the last tier and
// held on every guardian until each operator approves both payouts, and is
// then paid at the last tier's depth.
func TestSplitPaymentPayoutDepth(t *testing.T) {
	t.Parallel()
	tiers, err := ParseTiers("50:3,500:6")
	if err != nil {
		t.Fatal(err)
	}
	e := newPocEnv(t, func(c *Config) { c.ReleaseTiers = tiers })
	ctx := context.Background()
	e.fund()
	c := e.chains[0]
	unit := consensus.Amount(consensus.UnitsPerCoin)
	var to [20]byte
	to[19] = 0xAB
	p, _ := SwapPayload(SwapDest{ChainID: c.ID, Symbol: "USDC", To: to})
	out := func(a consensus.Amount) consensus.TxOut {
		return consensus.TxOut{Amount: a, Type: consensus.OutputTypeP2PK, Payload: p[:], Asset: e.leader.Asset()}
	}

	// 98 uARG in two outputs of 49: the 6-block tier.
	e.issueDeposit(c.Tokens[0].Address, usd(100, 6)) // 99 uARG
	small := e.paySwap([]consensus.TxOut{out(49 * unit), out(49 * unit)}, 9)
	e.mine(4, e.uAddr) // 4 confirmations
	e.round()
	for _, id := range []string{small + ":0", small + ":1"} {
		r, _ := e.leader.st.GetRedemption(id)
		if r.Status != RedemptionPending || r.Confirmations != 6 || r.TxPayout != 98*unit {
			t.Fatalf("%s: %+v", id, r)
		}
	}
	if len(e.evm.releases) != 0 {
		t.Fatalf("98 uARG in two outputs paid after 4 blocks: %d releases", len(e.evm.releases))
	}
	if _, err := e.f1.SignReleaseLocally(ctx, small+":0"); err == nil || !strings.Contains(err.Error(), "of 6 confirmations (the depth for 98.00000000 ") {
		t.Fatalf("a follower signed an output of a 98 uARG payment after 4 blocks: %v", err)
	}
	e.mine(2, e.uAddr)
	for i := 0; i < 3; i++ { // the fake pays at once, so a release is counted in flight for a round
		e.round()
	}
	if len(e.evm.releases) != 2 {
		t.Fatalf("98 uARG in two outputs after 6 blocks: %d releases", len(e.evm.releases))
	}

	// 980 uARG in two outputs of 490: above the last tier, held.
	e.issueDeposit(c.Tokens[0].Address, usd(1000, 6)) // 990 uARG
	big := e.paySwap([]consensus.TxOut{out(490 * unit), out(490 * unit)}, 10)
	e.mine(8, e.uAddr)
	for i := 0; i < 2; i++ {
		e.round()
	}
	ids := []string{big + ":0", big + ":1"}
	for _, id := range ids {
		r, _ := e.leader.st.GetRedemption(id)
		if r.Status != RedemptionPending || !strings.Contains(r.Error, "its transaction pays out 980.00000000 ") || !strings.Contains(r.Error, "manual release") || r.TxPayout != 980*unit {
			t.Fatalf("%s: %+v", id, r)
		}
		if _, err := e.f2.SignReleaseLocally(ctx, id); err == nil || !strings.Contains(err.Error(), "largest automatic payout") {
			t.Fatalf("a follower signed an output of a payment above the tiers: %v", err)
		}
	}
	if len(e.evm.releases) != 2 {
		t.Fatalf("a 980 uARG payment in two outputs was paid without approval: %d releases", len(e.evm.releases))
	}
	// Approved on every guardian (each output by its id): paid.
	for i, u := range e.urls {
		for _, id := range ids {
			if code, ans := adminCall(t, u, e.cs[i].local, "approve-release", map[string]string{"txid": id}); code != 200 {
				t.Fatalf("approve %s on %d: %d %s", id, i, code, ans)
			}
		}
	}
	for i := 0; i < 3; i++ {
		e.round()
	}
	if len(e.evm.releases) != 4 {
		t.Fatalf("the approved payment: %d releases", len(e.evm.releases))
	}
	for _, id := range ids {
		if r, _ := e.leader.st.GetRedemption(id); r.Status != RedemptionConfirmed || r.Confirmations != 6 {
			t.Fatalf("%s after approval: %+v", id, r)
		}
	}
}
