package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"argonmine/consensus"
	"argonmine/ui"
	"argonmine/wallet"
)

// Synthetic chain for the indexer: an issuance, a transfer, a burn in the
// same block as the transfer, then a reorg that undoes both blocks.
const (
	testAsset  = "df6f883c6ab23bfdc80e672b2c8e95bd0ab249962446d4ce3aaa1b9509b761cc"
	testIssuer = "1111111111111111111111111111111111111111111111111111111111111111"
	minerAddr  = "rarg1miner"
	aliceAddr  = "rarg1alice"
	bobAddr    = "rarg1bob"
	coin       = consensus.UnitsPerCoin
)

func testOut(addr string, amt consensus.Amount, asset string) wallet.TxOutJSON {
	return wallet.TxOutJSON{Address: addr, Amount: amt, Asset: asset}
}

func testIn(txid string, idx uint32, addr string, amt consensus.Amount, asset string) wallet.TxInJSON {
	return wallet.TxInJSON{TxID: txid, Index: idx, Prevout: &wallet.PrevOutJSON{Address: addr, Amount: amt, Asset: asset}}
}

func coinbaseTx(txid string, amt consensus.Amount) txJSON {
	return txJSON{TxJSON: wallet.TxJSON{TxID: txid, Coinbase: true, Inputs: []wallet.TxInJSON{{Coinbase: true}}, Outputs: []wallet.TxOutJSON{testOut(minerAddr, amt, "")}}}
}

func testBlock(h uint32, prev string, txs ...txJSON) *blockJSON {
	return &blockJSON{Hash: "hash" + string(rune('0'+h)), Height: h, PrevHash: prev, Time: 1_700_000_000 + uint64(h)*600, Bits: "207fffff", Size: 300, Tx: txs}
}

func testIndexer(t *testing.T) *Indexer {
	t.Helper()
	ix, err := newIndexer(t.TempDir(), nil, consensus.RegTest, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ix.close)
	return ix
}

func wantAmount(t *testing.T, what string, got, want consensus.Amount) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", what, got, want)
	}
}

// genuineUARG registers testAsset as the bridge's uARG for one test.
func genuineUARG(t *testing.T) {
	ui.SetCanonicalAsset("UARG", testAsset)
	t.Cleanup(func() { ui.SetCanonicalAsset("UARG", "") })
}

func TestIndexerAssets(t *testing.T) {
	genuineUARG(t)
	ix := testIndexer(t)

	// Block 0: the miner earns 50 ARG.
	if err := ix.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin))); err != nil {
		t.Fatal(err)
	}
	// Block 1: t1 spends that coinbase, issues 25 UARG to alice, returns change.
	t1 := txJSON{TxJSON: wallet.TxJSON{TxID: "t1",
		Inputs:  []wallet.TxInJSON{testIn("cb0", 0, minerAddr, 50*coin, "")},
		Outputs: []wallet.TxOutJSON{testOut(aliceAddr, 25*coin, testAsset), testOut(minerAddr, 49*coin, "")}},
		Issuances: []issuanceJSON{{Asset: testAsset, Issuer: testIssuer, Symbol: "UARG", Amount: 25 * coin}}}
	if err := ix.apply(testBlock(1, "hash0", coinbaseTx("cb1", 51*coin), t1)); err != nil {
		t.Fatal(err)
	}

	at := ix.totals(aliceAddr)
	wantAmount(t, "aliceAddr ARG received", at.Received, 0)
	wantAmount(t, "aliceAddr ARG sent", at.Sent, 0)
	if at.TxCount != 1 {
		t.Errorf("aliceAddr txcount = %d, want 1", at.TxCount)
	}
	wantAmount(t, "aliceAddr UARG received", at.Assets[testAsset].Received, 25*coin)
	wantAmount(t, "aliceAddr UARG sent", at.Assets[testAsset].Sent, 0)
	if at.Assets[testAsset].Symbol != "UARG" {
		t.Errorf("aliceAddr asset symbol = %q", at.Assets[testAsset].Symbol)
	}
	mt := ix.totals(minerAddr)
	wantAmount(t, "minerAddr ARG received", mt.Received, 150*coin)
	wantAmount(t, "minerAddr ARG sent", mt.Sent, 50*coin)
	if len(mt.Assets) != 0 {
		t.Errorf("minerAddr has assets: %+v", mt.Assets)
	}
	info, ok := ix.asset(testAsset)
	if !ok {
		t.Fatal("asset not registered")
	}
	if info.Symbol != "UARG" || info.Display != "uARG" || info.Issuer != testIssuer || info.FirstHeight != 1 || info.FirstTxID != "t1" || info.TxCount != 1 {
		t.Errorf("registry entry = %+v", info)
	}
	wantAmount(t, "issued", info.Issued, 25*coin)
	wantAmount(t, "burned", info.Burned, 0)
	wantAmount(t, "supply", info.Supply, 25*coin)
	hist := ix.addressHistory(aliceAddr, 10)
	if len(hist) != 1 || len(hist[0].Assets) != 1 || hist[0].Assets[0].Display != "uARG" || hist[0].Assets[0].Received != 25*coin || hist[0].MovedARG() || !hist[0].Incoming() {
		t.Errorf("aliceAddr history = %+v", hist)
	}
	h := ix.holdings(at)
	if len(h) != 1 || h[0].Display != "uARG" || h[0].Balance != 25*coin || h[0].Asset != testAsset {
		t.Errorf("holdings = %+v", h)
	}

	// Block 2: t2 moves 10 to bob and 15 back to alice; t3 (same block)
	// spends bob's 10 and keeps 4: 6 burned. Bob and the registry are
	// touched twice in one block, which must compose.
	t2 := txJSON{TxJSON: wallet.TxJSON{TxID: "t2",
		Inputs:  []wallet.TxInJSON{testIn("t1", 0, aliceAddr, 25*coin, testAsset), testIn("t1", 1, minerAddr, 49*coin, "")},
		Outputs: []wallet.TxOutJSON{testOut(bobAddr, 10*coin, testAsset), testOut(aliceAddr, 15*coin, testAsset), testOut(minerAddr, 48*coin, "")}}}
	t3 := txJSON{TxJSON: wallet.TxJSON{TxID: "t3",
		Inputs:  []wallet.TxInJSON{testIn("t2", 0, bobAddr, 10*coin, testAsset)},
		Outputs: []wallet.TxOutJSON{testOut(bobAddr, 4*coin, testAsset)}}}
	if err := ix.apply(testBlock(2, "hash1", coinbaseTx("cb2", 52*coin), t2, t3)); err != nil {
		t.Fatal(err)
	}
	at = ix.totals(aliceAddr)
	wantAmount(t, "aliceAddr UARG balance", at.Assets[testAsset].Received-at.Assets[testAsset].Sent, 15*coin)
	bt := ix.totals(bobAddr)
	wantAmount(t, "bobAddr UARG received", bt.Assets[testAsset].Received, 14*coin)
	wantAmount(t, "bobAddr UARG sent", bt.Assets[testAsset].Sent, 10*coin)
	if bt.TxCount != 2 {
		t.Errorf("bobAddr txcount = %d, want 2", bt.TxCount)
	}
	mt = ix.totals(minerAddr)
	wantAmount(t, "minerAddr ARG received after block 2", mt.Received, 250*coin)
	wantAmount(t, "minerAddr ARG sent after block 2", mt.Sent, 99*coin)
	info, _ = ix.asset(testAsset)
	wantAmount(t, "burned after t3", info.Burned, 6*coin)
	wantAmount(t, "supply after t3", info.Supply, 19*coin)
	if info.TxCount != 3 {
		t.Errorf("asset txcount = %d, want 3", info.TxCount)
	}
	ah := ix.assetHistory(testAsset, 0, 10)
	if len(ah) != 3 || ah[0].TxID != "t3" || ah[0].Burned != 6*coin || ah[0].Out != 4*coin || ah[2].TxID != "t1" || ah[2].Issued != 25*coin {
		t.Errorf("asset history = %+v", ah)
	}
	if older := ix.assetHistory(testAsset, 2, 10); len(older) != 1 || older[0].TxID != "t1" {
		t.Errorf("asset history before 2 = %+v", older)
	}
	bh := ix.addressHistory(bobAddr, 10)
	if len(bh) != 2 || bh[0].TxID != "t3" || bh[0].Incoming() || bh[1].TxID != "t2" || !bh[1].Incoming() {
		t.Errorf("bobAddr history = %+v", bh)
	}
	if all := ix.assets(); len(all) != 1 || all[0].Asset != testAsset {
		t.Errorf("registry list = %+v", all)
	}

	// Reorg: block 2 goes away.
	if err := ix.rewind(2); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Get(keyTotals(bobAddr), nil); err == nil {
		t.Error("bobAddr still has totals after rewind")
	}
	if len(ix.addressHistory(bobAddr, 10)) != 0 {
		t.Error("bobAddr still has history after rewind")
	}
	at = ix.totals(aliceAddr)
	wantAmount(t, "aliceAddr UARG balance after rewind", at.Assets[testAsset].Received-at.Assets[testAsset].Sent, 25*coin)
	if at.TxCount != 1 {
		t.Errorf("aliceAddr txcount after rewind = %d, want 1", at.TxCount)
	}
	mt = ix.totals(minerAddr)
	wantAmount(t, "minerAddr ARG received after rewind", mt.Received, 150*coin)
	wantAmount(t, "minerAddr ARG sent after rewind", mt.Sent, 50*coin)
	info, _ = ix.asset(testAsset)
	wantAmount(t, "burned after rewind", info.Burned, 0)
	wantAmount(t, "supply after rewind", info.Supply, 25*coin)
	if info.TxCount != 1 || len(ix.assetHistory(testAsset, 0, 10)) != 1 {
		t.Errorf("registry after rewind = %+v", info)
	}
	if h, _, _ := ix.status(); h != 1 {
		t.Errorf("height after rewind = %d", h)
	}

	// And the issuance block: the asset is gone entirely.
	if err := ix.rewind(1); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.asset(testAsset); ok {
		t.Error("asset still registered after its issuance was rewound")
	}
	if len(ix.assets()) != 0 || len(ix.assetHistory(testAsset, 0, 10)) != 0 {
		t.Error("registry not empty after rewind")
	}
	if _, err := ix.db.Get(keyTotals(aliceAddr), nil); err == nil {
		t.Error("aliceAddr still has totals after rewind")
	}
	mt = ix.totals(minerAddr)
	wantAmount(t, "minerAddr ARG received at genesis", mt.Received, 50*coin)

	// A competing block 1 then indexes cleanly on the rewound state.
	if err := ix.apply(testBlock(1, "hash0", coinbaseTx("cb1b", 50*coin), t1)); err != nil {
		t.Fatal(err)
	}
	info, _ = ix.asset(testAsset)
	wantAmount(t, "supply after re-apply", info.Supply, 25*coin)
	if info.TxCount != 1 {
		t.Errorf("asset txcount after re-apply = %d", info.TxCount)
	}
}

// TestPagesRender executes the templates that gained asset data with
// realistic values, so a broken pipeline fails here rather than at runtime.
func TestPagesRender(t *testing.T) {
	genuineUARG(t)
	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	ix := testIndexer(t)
	t1 := txJSON{TxJSON: wallet.TxJSON{TxID: "t1",
		Inputs:  []wallet.TxInJSON{testIn("cb0", 0, minerAddr, 50*coin, "")},
		Outputs: []wallet.TxOutJSON{testOut(aliceAddr, 25*coin, testAsset), testOut(minerAddr, 49*coin, "")}},
		Issuances: []issuanceJSON{{Asset: testAsset, Issuer: testIssuer, Symbol: "UARG", Amount: 25 * coin}}}
	t2 := txJSON{TxJSON: wallet.TxJSON{TxID: "t2",
		Inputs:  []wallet.TxInJSON{testIn("t1", 0, aliceAddr, 25*coin, testAsset)},
		Outputs: []wallet.TxOutJSON{testOut(bobAddr, 4*coin, testAsset)}}}
	if err := ix.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin))); err != nil {
		t.Fatal(err)
	}
	if err := ix.apply(testBlock(1, "hash0", coinbaseTx("cb1", 50*coin), t1, t2)); err != nil {
		t.Fatal(err)
	}
	s := &server{params: consensus.RegTest, ix: ix, tmpl: tmpl}
	render := func(name string, data any) string {
		t.Helper()
		var buf bytes.Buffer
		p := page{Title: name, Brand: "Argonmine", Coin: "ARG", Network: "regtest", Data: data}
		if err := tmpl.ExecuteTemplate(&buf, name, p); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		return buf.String()
	}

	// Transaction page: the node's JSON shape for t2 (a burn of 21 UARG).
	raw, _ := json.Marshal(map[string]any{
		"txid": "t2", "wtxid": "w2", "version": 2, "lock_height": 0, "size": 200, "coinbase": false, "blockhash": "hash1", "height": 1, "confirmations": 1,
		"inputs":  []any{map[string]any{"txid": "t1", "index": 0, "prevout": map[string]any{"amount": 25 * coin, "coin": "25.00000000", "type": 0, "address": aliceAddr, "asset": testAsset}}},
		"outputs": []any{map[string]any{"n": 0, "amount": 4 * coin, "coin": "4.00000000", "type": 0, "address": bobAddr, "payload": "", "asset": testAsset}},
	})
	tx := s.decorateTx(raw)
	flows, _ := tx["assets"].([]assetFlowRow)
	if len(flows) != 1 || flows[0].Burned != 21*coin || flows[0].Display != "uARG" {
		t.Fatalf("tx flows = %+v", flows)
	}
	out := render("tx", tx)
	if strings.Contains(out, "/asset/") {
		t.Error("the tx page links to an asset page")
	}
	for _, want := range []string{"Burned", "21.00000000", "uARG"} {
		if !strings.Contains(out, want) {
			t.Errorf("tx page lacks %q", want)
		}
	}
	// Issuance page for t1.
	raw, _ = json.Marshal(map[string]any{
		"txid": "t1", "wtxid": "w1", "version": 2, "lock_height": 0, "size": 300, "coinbase": false, "blockhash": "hash1", "height": 1, "confirmations": 1,
		"inputs":    []any{map[string]any{"txid": "cb0", "index": 0, "prevout": map[string]any{"amount": 50 * coin, "coin": "50.00000000", "type": 0, "address": minerAddr}}},
		"outputs":   []any{map[string]any{"n": 0, "amount": 25 * coin, "coin": "25.00000000", "type": 0, "address": aliceAddr, "payload": "", "asset": testAsset}, map[string]any{"n": 1, "amount": 49 * coin, "coin": "49.00000000", "type": 0, "address": minerAddr, "payload": ""}},
		"issuances": []any{map[string]any{"asset": testAsset, "issuer": testIssuer, "symbol": "UARG", "amount": 25 * coin}},
	})
	out = render("tx", s.decorateTx(raw))
	for _, want := range []string{"Issuance", "25.00000000", "uARG", testIssuer} {
		if !strings.Contains(out, want) {
			t.Errorf("issuance tx page lacks %q", want)
		}
	}
	if strings.Contains(out, "Burned") {
		t.Error("issuance tx page shows a Burned row")
	}

	// Address page for bob: one asset card, one history row marked uARG.
	bt := ix.totals(bobAddr)
	out = render("address", addressData{Address: bobAddr, Totals: bt, Holdings: ix.holdings(bt), History: ix.addressHistory(bobAddr, 100)})
	for _, want := range []string{"uARG balance", "4.00000000", "+4.00000000"} {
		if !strings.Contains(out, want) {
			t.Errorf("address page lacks %q", want)
		}
	}
	// Asset page and registry list.
	info, _ := ix.asset(testAsset)
	out = render("asset", assetData{Info: info, History: ix.assetHistory(testAsset, 0, 100)})
	for _, want := range []string{"uARG", testIssuer, "4.00000000", "21.00000000", "issuance"} {
		if !strings.Contains(out, want) {
			t.Errorf("asset page lacks %q", want)
		}
	}
	out = render("assets", ix.assets())
	if !strings.Contains(out, "uARG") {
		t.Error("asset list lacks uARG")
	}
	out = render("assets", []AssetInfo{})
	if !strings.Contains(out, "No assets yet") {
		t.Error("empty asset list lacks its empty state")
	}
	// Names pages with nothing registered yet (names_test.go has the rest).
	out = render("names", namesPageData{Free: true, Suffix: "arg", HRP: "rarg", Prices: []priceRow{{Length: "3 characters"}}, Recent: []nameRow{}})
	if !strings.Contains(out, "No names yet") || !strings.Contains(out, "Not available") {
		t.Error("empty names page lacks its empty state")
	}
	out = render("name", s.nameData("nobody"))
	if !strings.Contains(out, "nobody.arg") || !strings.Contains(out, "Available") {
		t.Error("available name page lacks its empty state")
	}
	out = render("address", addressData{Address: bobAddr, Totals: bt, Name: "bob", Names: s.ownedNames(bobAddr)})
	if !strings.Contains(out, "bob.arg") {
		t.Error("address page lacks the display name")
	}
}

// Since the treasury height a coinbase ends with the treasury's output. It
// is not a referral, and a referrer's output before it still is one.
func TestReferralOutputWithTreasury(t *testing.T) {
	p := *consensus.RegTest
	p.TreasuryHeight = 1
	p.TreasuryKey = make([]byte, consensus.P2PKPayloadLen)
	treasuryAddr, err := wallet.EncodeAddress(p.HRP, consensus.OutputTypeP2PK, p.TreasuryKey)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := newIndexer(t.TempDir(), nil, &p, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.close()
	sub := consensus.Subsidy(1)
	tre := consensus.TreasuryAmount(&p, 1)
	if tre == 0 {
		t.Fatal("no treasury amount at height 1")
	}
	rest := sub - tre
	share := rest / 100 * 5
	cb := func(outs ...wallet.TxOutJSON) txJSON {
		return txJSON{TxJSON: wallet.TxJSON{TxID: "cb", Coinbase: true, Inputs: []wallet.TxInJSON{{Coinbase: true}}, Outputs: outs}}
	}
	cases := []struct {
		name   string
		height uint32
		tx     txJSON
		want   int
	}{
		{"miner and treasury", 1, cb(testOut(minerAddr, rest, ""), testOut(treasuryAddr, tre, "")), -1},
		{"miner, referrer and treasury", 1, cb(testOut(minerAddr, rest-share, ""), testOut(aliceAddr, share, ""), testOut(treasuryAddr, tre, "")), 1},
		{"miner alone with treasury", 1, cb(testOut(minerAddr, sub, ""), testOut(treasuryAddr, tre, "")), -1},
		{"before the treasury: miner and referrer", 0, cb(testOut(minerAddr, sub-sub/100*5, ""), testOut(aliceAddr, sub/100*5, "")), 1},
		{"before the treasury: miner alone", 0, cb(testOut(minerAddr, sub, "")), -1},
	}
	for _, c := range cases {
		if got := ix.referralOutput(c.tx, c.height); got != c.want {
			t.Errorf("%s: referral output %d, want %d", c.name, got, c.want)
		}
	}
}
