package main

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"argonmine/bridge"
	"argonmine/consensus"
	"argonmine/ui"
	"argonmine/wallet"
)

// M5: only the bridge's asset id is "uARG" with the logo; any other asset
// calling itself UARG, WARG or ARG is "SYMBOL (unverified)" with its issuer
// key and no logo, wherever it appears.
func TestUARGLabelling(t *testing.T) {
	const (
		fakeU    = "f00000000000000000000000000000000000000000000000000000000000000f"
		fakeW    = "f10000000000000000000000000000000000000000000000000000000000001f"
		fakeA    = "f20000000000000000000000000000000000000000000000000000000000002f"
		gold     = "f30000000000000000000000000000000000000000000000000000000000003f"
		mallory  = "2222222222222222222222222222222222222222222222222222222222222222"
		testHold = "rarg1holder"
	)
	ix := testIndexer(t)
	issue := func(txid, asset, issuer, sym string) txJSON {
		return txJSON{TxJSON: wallet.TxJSON{TxID: txid, Outputs: []wallet.TxOutJSON{testOut(testHold, 5*coin, asset)}},
			Issuances: []issuanceJSON{{Asset: asset, Issuer: issuer, Symbol: sym, Amount: 5 * coin}}}
	}
	if err := ix.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin), issue("i1", testAsset, testIssuer, "UARG"), issue("i2", fakeU, mallory, "UARG"),
		issue("i3", fakeW, mallory, "WARG"), issue("i4", fakeA, mallory, "ARG"), issue("i5", gold, mallory, "GOLD"))); err != nil {
		t.Fatal(err)
	}

	// Without a bridge answer every UARG is unverified, the real one too.
	ui.SetCanonicalAsset("UARG", "")
	if a, _ := ix.asset(testAsset); a.Display != "UARG (unverified)" || !a.Unverified || a.Logo != "" {
		t.Fatalf("no canonical id: %+v", a)
	}

	genuineUARG(t)
	want := map[string]struct {
		display    string
		unverified bool
		logo       bool
	}{
		testAsset: {"uARG", false, true},
		fakeU:     {"UARG (unverified)", true, false},
		fakeW:     {"WARG (unverified)", true, false},
		fakeA:     {"ARG (unverified)", true, false},
		gold:      {"GOLD", false, false},
	}
	for id, w := range want {
		a, ok := ix.asset(id)
		if !ok || a.Display != w.display || a.Unverified != w.unverified || (a.Logo != "") != w.logo {
			t.Errorf("asset %s…: %q unverified %v logo %q; want %+v", id[:4], a.Display, a.Unverified, a.Logo, w)
		}
	}
	for _, h := range ix.holdings(ix.totals(testHold)) {
		w := want[h.Asset]
		if h.Display != w.display || h.Unverified != w.unverified || (h.Logo != "") != w.logo || (h.Unverified && h.Issuer != mallory) {
			t.Errorf("holding %s…: %+v", h.Asset[:4], h)
		}
	}
	for _, e := range ix.addressHistory(testHold, 10) {
		for _, m := range e.Assets {
			if w := want[m.Asset]; m.Display != w.display || m.Unverified != w.unverified {
				t.Errorf("history move %s…: %+v", m.Asset[:4], m)
			}
		}
	}

	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	render := func(name string, data any) string {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, name, page{Title: name, Brand: "Argonmine", Coin: "ARG", Network: "regtest", Data: data}); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		return buf.String()
	}
	list := render("assets", ix.assets())
	if strings.Count(list, uargLogo) != 1 || !strings.Contains(list, "UARG (unverified)") || !strings.Contains(list, "issuer "+shorten(mallory, 10, 6)) {
		t.Errorf("asset list: logo %d times, unverified label %v", strings.Count(list, uargLogo), strings.Contains(list, "UARG (unverified)"))
	}
	real, _ := ix.asset(testAsset)
	if p := render("asset", assetData{Info: real}); !strings.Contains(p, uargLogo) || strings.Contains(p, "Unverified:") {
		t.Error("genuine uARG page: no logo or a warning")
	}
	fake, _ := ix.asset(fakeU)
	if p := render("asset", assetData{Info: fake}); strings.Contains(p, uargLogo) || !strings.Contains(p, "Unverified: this asset calls itself UARG") || !strings.Contains(p, shorten(mallory, 10, 6)) {
		t.Error("look-alike page: logo shown or no warning with the issuer")
	}
	addr := render("address", addressData{Address: testHold, Holdings: ix.holdings(ix.totals(testHold)), History: ix.addressHistory(testHold, 10)})
	if strings.Count(addr, uargLogo) != 1 || !strings.Contains(addr, "UARG (unverified) balance") || !strings.Contains(addr, "uARG balance") {
		t.Error("address page holdings mislabelled")
	}
}

// I4: only the output that follows the work server's referral rule is a
// referral; referral and mining figures are lifetime totals.
func TestReferralLabelAndTotals(t *testing.T) {
	ix := testIndexer(t)
	const ref, other = "rarg1referrer", "rarg1other"
	reward := consensus.Subsidy(1)
	share := reward / 100 * 5
	cb := func(txid string, outs ...wallet.TxOutJSON) txJSON {
		return txJSON{TxJSON: wallet.TxJSON{TxID: txid, Coinbase: true, Inputs: []wallet.TxInJSON{{Coinbase: true}}, Outputs: outs}}
	}
	blocks := []*blockJSON{
		testBlock(0, "", cb("c0", testOut(minerAddr, reward-share, ""), testOut(ref, share, ""))),                                   // 5%: referral
		testBlock(1, "hash0", cb("c1", testOut(minerAddr, reward/2, ""), testOut(other, reward-reward/2, ""))),                      // 50/50 is 50%: referral rule allows up to 50
		testBlock(2, "hash1", cb("c2", testOut(minerAddr, reward-123, ""), testOut(other, 123, ""))),                                // no whole percent: a pool split
		testBlock(3, "hash2", cb("c3", testOut(minerAddr, reward-2*share, ""), testOut(ref, share, ""), testOut(other, share, ""))), // three outputs: none
	}
	for _, b := range blocks {
		if err := ix.apply(b); err != nil {
			t.Fatal(err)
		}
	}
	labels := func(addr string) (refs, mined int) {
		for _, e := range ix.addressHistory(addr, 10) {
			if e.Referral {
				refs++
			} else if e.Coinbase {
				mined++
			}
		}
		return
	}
	if r, m := labels(ref); r != 1 || m != 1 {
		t.Errorf("referrer: %d referral, %d mined; want 1 and 1 (the 3-output coinbase is not a referral)", r, m)
	}
	if r, m := labels(other); r != 1 || m != 2 {
		t.Errorf("other: %d referral, %d mined; want 1 and 2", r, m)
	}
	if r, m := labels(minerAddr); r != 0 || m != 4 {
		t.Errorf("miner: %d referral, %d mined", r, m)
	}
	rt := ix.totals(ref)
	if rt.ReferralCount != 1 || rt.ReferralTotal != share || rt.Mined != 1 || rt.MinedTotal != share {
		t.Errorf("referrer totals %+v", rt)
	}
	mt := ix.totals(minerAddr)
	st := addressStats(mt)
	if st.Mined != 4 || st.FirstHeight != 0 || st.LastHeight != 3 || st.FirstTime != blocks[0].Time || st.LastTime != blocks[3].Time {
		t.Errorf("miner stats %+v", st)
	}
	// Maturing: every reward is younger than 100 blocks at height 3.
	if m := ix.maturing(minerAddr, 3); m != mt.Received {
		t.Errorf("maturing %d, want %d", m, mt.Received)
	}
	if m := ix.maturing(minerAddr, 3+int64(consensus.CoinbaseMaturity)); m != 0 {
		t.Errorf("maturing after 100 blocks: %d", m)
	}
	if m := ix.maturing(minerAddr, 2+int64(consensus.CoinbaseMaturity)); m != reward-2*share {
		t.Errorf("maturing with only block 3 young: %d", m)
	}
	// Rewinding a block undoes the figures, including the last-seen height.
	if err := ix.rewind(3); err != nil {
		t.Fatal(err)
	}
	rt = ix.totals(ref)
	if rt.ReferralCount != 1 || rt.Mined != 0 || rt.LastHeight != 0 {
		t.Errorf("referrer after rewind %+v", rt)
	}
	if st := addressStats(ix.totals(minerAddr)); st.Mined != 3 || st.LastHeight != 2 {
		t.Errorf("miner after rewind %+v", st)
	}
	if err := ix.rewind(2); err != nil {
		t.Fatal(err)
	}
	if ot := ix.totals(other); ot.ReferralCount != 1 || ot.Mined != 0 || ot.LastHeight != 1 {
		t.Errorf("other after rewind %+v", ot)
	}
}

// L5: asset totals saturate instead of wrapping negative.
func TestAssetTotalsSaturate(t *testing.T) {
	genuineUARG(t)
	ix := testIndexer(t)
	big := consensus.MaxAssetAmount // 2^62
	issue := func(txid string) txJSON {
		return txJSON{TxJSON: wallet.TxJSON{TxID: txid, Outputs: []wallet.TxOutJSON{testOut(aliceAddr, big, testAsset)}},
			Issuances: []issuanceJSON{{Asset: testAsset, Issuer: testIssuer, Symbol: "UARG", Amount: big}}}
	}
	var txs []txJSON
	for i := 0; i < 4; i++ {
		txs = append(txs, issue("big"+string(rune('0'+i))))
	}
	if err := ix.apply(testBlock(0, "", append([]txJSON{coinbaseTx("cb0", 50*coin)}, txs...)...)); err != nil {
		t.Fatal(err)
	}
	a, _ := ix.asset(testAsset)
	if a.Issued != math.MaxInt64 || a.Supply < 0 {
		t.Fatalf("issued %d supply %d: not saturated", a.Issued, a.Supply)
	}
	at := ix.totals(aliceAddr).Assets[testAsset]
	if at.Received != math.MaxInt64 {
		t.Fatalf("received %d: not saturated", at.Received)
	}
	if h := ix.holdings(ix.totals(aliceAddr)); len(h) != 1 || h[0].Balance < 0 {
		t.Fatalf("holding %+v", h)
	}
	if satAdd(math.MaxInt64, 1) != math.MaxInt64 || satSub(math.MinInt64, 1) != math.MinInt64 || satSub(5, math.MinInt64) != math.MaxInt64 || satAdd(2, 3) != 5 {
		t.Fatal("satAdd/satSub")
	}
}

func TestSwapAddressLabel(t *testing.T) {
	to, _ := bridge.ParseAddress("0x198c2d42c71e8046f34eca9a0f5c81b9f3db2afb")
	addr, err := bridge.SwapAddress("targ", bridge.SwapDest{ChainID: 11155111, Symbol: "usdc", To: to})
	if err != nil {
		t.Fatal(err)
	}
	got := swapOf("targ", addr)
	if got == nil || got.Symbol != "USDC" || got.Chain != "Ethereum Sepolia" || got.To != "0x198c2D42C71E8046f34ECA9a0f5C81B9f3db2afB" {
		t.Fatalf("label: %+v", got)
	}
	if odd := swapOf("targ", mustSwap(t, 777)); odd == nil || odd.Chain != "chain 777" {
		t.Fatalf("unknown chain: %+v", odd)
	}
	if swapOf("arg", addr) != nil || swapOf("targ", "targ1qyllnnmn60w4hjgtkrlfmqmwqc4lzs99vz6nh6r34cy6eyg26awaqprsxa9") != nil {
		t.Fatal("labelled an address that is not a swap address of this network")
	}
}

func mustSwap(t *testing.T, chain uint64) string {
	a, err := bridge.SwapAddress("targ", bridge.SwapDest{ChainID: chain, Symbol: "USDT", To: [20]byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
