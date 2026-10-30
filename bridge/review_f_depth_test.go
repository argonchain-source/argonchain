package bridge

// Review F (second pass, 2026-10-28): the payout depth of payouts split
// across transactions.

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"argonmine/consensus"
)

// swapChain pays swap addresses from one chain of transactions: each spends
// the uARG change and the ARG change of the one before it (the first, the
// user's newest uARG coin and the coinbase of a block), so several of them
// fit in one block.
type swapChain struct {
	e          *pocEnv
	uTx, aTx   string
	uN, aN     uint32
	uAmt, aAmt consensus.Amount
}

func (e *pocEnv) newSwapChain(cbHeight uint32) *swapChain {
	t := e.t
	t.Helper()
	asset := e.leader.Asset().String()
	info, _ := e.client.ChainInfo()
	c := &swapChain{e: e}
	for h := info.Blocks; h > 0 && c.uTx == ""; h-- {
		bh, _ := e.client.BlockHash(h)
		blk, _ := e.client.Block(bh)
		for _, tx := range blk.Tx {
			for _, o := range tx.Outputs {
				if o.Asset == asset && o.Address == e.uAddr {
					c.uTx, c.uN, c.uAmt = tx.TxID, uint32(o.N), o.Amount
				}
			}
		}
	}
	if c.uTx == "" {
		t.Fatal("the user has no uARG")
	}
	cb, _ := e.client.Block(mustBlockHash(t, e.client, cbHeight))
	c.aTx, c.aN, c.aAmt = cb.Tx[0].TxID, 0, cb.Tx[0].Outputs[0].Amount
	return c
}

// pay sends units of uARG to a swap address from the chain.
func (c *swapChain) pay(swap []byte, units consensus.Amount) string {
	e, t := c.e, c.e.t
	t.Helper()
	asset := e.leader.Asset()
	change := e.user.Output(c.uAmt - units)
	change.Asset = asset
	const fee = 20000
	tx := &consensus.Tx{Version: consensus.TxVersionAssets,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: mustHash(t, c.uTx), Index: c.uN}}, {PrevOut: consensus.OutPoint{TxID: mustHash(t, c.aTx), Index: c.aN}}},
		Outputs:   []consensus.TxOut{{Amount: units, Type: consensus.OutputTypeP2PK, Payload: swap, Asset: asset}, change, e.user.Output(c.aAmt - fee)},
		Witnesses: make([][]byte, 2)}
	spent := []consensus.TxOut{{Amount: c.uAmt, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly, Asset: asset}, {Amount: c.aAmt, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly}}
	for i := range tx.Inputs {
		if err := e.user.Sign(consensus.RegTest.ChainTag, tx, spent, i); err != nil {
			t.Fatal(err)
		}
	}
	id, err := e.client.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
	if err != nil {
		t.Fatal(err)
	}
	c.uTx, c.uN, c.uAmt = id, 1, c.uAmt-units
	c.aTx, c.aN, c.aAmt = id, 2, c.aAmt-fee
	return id + ":0"
}

// TestPayoutDepthWindow (review F, fixed 2026-10-28): the payout depth went
// by what one transaction pays out, so a payout split across transactions,
// each under the first tier, was paid at the first tier's depth however
// much the split added up to. Now each guardian chooses the depth for the
// larger of the transaction's payout and what the bridge pays out of the W
// blocks up to the payout's height (W, the deepest tier's depth), from its
// own records. Twelve payments of 45 uARG, each under the 50 uARG tier, in
// four blocks: the first waits 3 blocks, the next eight wait 6 (the window
// reaches 225 and 405 uARG), and the last three, which take the window to
// 540 uARG, above the last tier, are held for a manual release, on the
// leader and on both followers. What was already paid is not touched.
func TestPayoutDepthWindow(t *testing.T) {
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
	var payee [20]byte
	payee[19] = 0xAB
	p, _ := SwapPayload(SwapDest{ChainID: c.ID, Symbol: "USDC", To: payee})
	e.issueDeposit(c.Tokens[0].Address, usd(600, 6)) // 594 uARG to the user

	ch := e.newSwapChain(9)
	var blocks [][]string
	for _, n := range []int{1, 4, 4, 3} {
		var ids []string
		for i := 0; i < n; i++ {
			ids = append(ids, ch.pay(p[:], 45*unit))
		}
		e.mine(1, e.uAddr)
		blocks = append(blocks, ids)
	}
	a, b, cc, d := blocks[0], blocks[1], blocks[2], blocks[3]
	e.round() // confirmations: a 4, b 3, c 2, d 1
	all := []*Service{e.leader, e.f1, e.f2}
	for _, g := range all {
		for _, id := range append(append(append(append([]string{}, a...), b...), cc...), d...) {
			if r, ok := g.st.GetRedemption(id); !ok || r.TxPayout != 45*unit || r.Amount != 45*unit {
				t.Fatalf("%s: %+v", id, r)
			}
		}
	}
	want := map[string]uint32{a[0]: 3}
	for _, id := range append(append([]string{}, b...), cc...) {
		want[id] = 6
	}
	for i := 0; i < 2; i++ {
		e.leader.Once(ctx)
	}
	for id, n := range want {
		if r, _ := e.leader.st.GetRedemption(id); r.Confirmations != n {
			t.Fatalf("leader: %s needs %d confirmations, want %d (%+v)", id, r.Confirmations, n, r)
		}
	}
	for _, id := range d {
		r, _ := e.leader.st.GetRedemption(id)
		if r.Status != RedemptionPending || !strings.Contains(r.Error, "pays out 540.00000000 UARG in Argonmine blocks") || !strings.Contains(r.Error, "manual release") {
			t.Fatalf("leader: %s above the last tier: %+v", id, r)
		}
	}
	if len(e.evm.releases) != 1 {
		t.Fatalf("after 4 blocks: %d releases, want the first payment's only", len(e.evm.releases))
	}
	// Each follower, from its own records: the payments of the second block
	// need 6 blocks.
	for i, f := range []*Service{e.f1, e.f2} {
		if _, err := f.SignReleaseLocally(ctx, b[2]); err == nil || !strings.Contains(err.Error(), "payment has 3 of 6 confirmations (the depth for 225.00000000 UARG)") {
			t.Fatalf("follower %d signed a payment of the second block after 3 blocks: %v", i+1, err)
		}
	}

	// Two blocks later (second block 5 deep, third 4, last 3): nothing more
	// is paid, and the followers still refuse, each for the window up to the
	// payment; the last block's payments are held.
	e.mine(2, e.uAddr)
	for i := 0; i < 2; i++ {
		e.round()
	}
	if len(e.evm.releases) != 1 {
		t.Fatalf("second block at 5 confirmations: %d releases, want 1", len(e.evm.releases))
	}
	for i, f := range []*Service{e.f1, e.f2} {
		if _, err := f.SignReleaseLocally(ctx, b[0]); err == nil || !strings.Contains(err.Error(), "payment has 5 of 6 confirmations (the depth for 225.00000000 UARG)") {
			t.Fatalf("follower %d signed a payment of the second block after 5 blocks: %v", i+1, err)
		}
		if _, err := f.SignReleaseLocally(ctx, cc[3]); err == nil || !strings.Contains(err.Error(), "payment has 4 of 6 confirmations (the depth for 405.00000000 UARG)") {
			t.Fatalf("follower %d signed a payment of the third block after 4 blocks: %v", i+1, err)
		}
		if _, err := f.SignReleaseLocally(ctx, d[1]); err == nil || !strings.Contains(err.Error(), "pays out 540.00000000 UARG") || !strings.Contains(err.Error(), "largest automatic payout") {
			t.Fatalf("follower %d signed a payment of the last block (held above the last tier): %v", i+1, err)
		}
	}
	// One more: the second block's four are paid; one after, the third's.
	e.mine(1, e.uAddr)
	for i := 0; i < 4; i++ {
		e.round()
	}
	if len(e.evm.releases) != 5 {
		t.Fatalf("second block at 6 confirmations: %d releases, want 5", len(e.evm.releases))
	}
	e.mine(1, e.uAddr)
	for i := 0; i < 4; i++ {
		e.round()
	}
	if len(e.evm.releases) != 9 {
		t.Fatalf("third block at 6 confirmations: %d releases, want 9", len(e.evm.releases))
	}
	for _, id := range append(append(append([]string{}, a...), b...), cc...) {
		if r, _ := e.leader.st.GetRedemption(id); r.Status != RedemptionConfirmed {
			t.Fatalf("%s: %+v", id, r)
		}
	}
	for _, id := range d {
		if r, _ := e.leader.st.GetRedemption(id); r.Status != RedemptionPending || !strings.Contains(r.Error, "manual release") {
			t.Fatalf("%s: %+v", id, r)
		}
	}
	// Approved on every guardian: paid at the last tier's depth.
	for i, u := range e.urls {
		for _, id := range d {
			if code, ans := adminCall(t, u, e.cs[i].local, "approve-release", map[string]string{"txid": id}); code != 200 {
				t.Fatalf("approve %s on %d: %d %s", id, i, code, ans)
			}
		}
	}
	e.mine(1, e.uAddr) // the last block 6 deep
	for i := 0; i < 4; i++ {
		e.round()
	}
	if len(e.evm.releases) != 12 {
		t.Fatalf("approved payments: %d releases, want 12", len(e.evm.releases))
	}
}

// TestPayoutVolumeIndex: the amounts by height follow every write of a
// redemption (new, changed, deleted, undone) and come back the same when the
// state is opened again; the window is read with at most one lookup per
// height, so thousands of redemptions cost nothing per payout.
func TestPayoutVolumeIndex(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "st")
	st, err := openState(dir)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	type rec struct {
		h uint32
		a consensus.Amount
	}
	want := map[string]rec{}
	put := func(id string, h uint32, a consensus.Amount) {
		vout := uint32(0)
		r := &Redemption{TxID: id[:64], Height: h, Amount: a, Status: RedemptionPending}
		if len(id) > 64 {
			r.Vout = &vout
		}
		if err := st.PutRedemption(r); err != nil {
			t.Fatal(err)
		}
		want[r.ID()] = rec{h, a}
	}
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("%064x", i)
		if i%3 == 0 {
			id += ":0"
		}
		put(id, uint32(1000+rng.Intn(400)), consensus.Amount(1+rng.Intn(1e9)))
	}
	// Status changes keep the amount; deletions take it away.
	for id := range want {
		if rng.Intn(4) == 0 {
			got, _ := st.GetRedemption(id)
			got.Status = RedemptionConfirmed
			st.PutRedemption(got)
		} else if rng.Intn(10) == 0 {
			st.write([]kvOp{{key: redemptionKey(id)}}, false)
			delete(want, id)
		}
	}
	// Other keys starting with 'x' would be no redemptions: there are none,
	// but a record that is not JSON is not counted.
	st.write([]kvOp{{key: redemptionKey(fmt.Sprintf("%064x", 999999)), value: []byte("not json")}}, false)
	brute := func(from, to uint32) consensus.Amount {
		var s consensus.Amount
		for _, r := range want {
			if r.h >= from && r.h <= to {
				s += r.a
			}
		}
		return s
	}
	check := func(st *State, what string) {
		for _, w := range [][2]uint32{{0, 0}, {1000, 1000}, {1100, 1219}, {990, 1005}, {1395, 1500}, {0, 1 << 31}, {1200, 1100}} {
			if got, exp := st.PayoutVolume(w[0], w[1]), brute(w[0], w[1]); got != exp {
				t.Fatalf("%s: volume of %d..%d = %d, want %d", what, w[0], w[1], got, exp)
			}
		}
	}
	check(st, "live")
	st.Close()
	st, err = openState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	check(st, "reopened")
}
