package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"argonmine/consensus"
	"argonmine/wallet"
)

// A network with a mint window over blocks 1 to 3: 500 ARG a block, 20 ARG
// per unit of testAsset paid to a fixed key.
func mintParams(t *testing.T) (*consensus.Params, string) {
	t.Helper()
	p := *consensus.RegTest
	asset, err := consensus.HashFromHex(testAsset)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, consensus.P2PKPayloadLen)
	for i := range key {
		key[i] = 7
	}
	if err := consensus.SetMint(&p, &consensus.MintRule{Start: 1, End: 3, PerBlock: 500 * coin, Rate: 20, Asset: asset, Key: key}); err != nil {
		t.Fatal(err)
	}
	addr := mintAddress(&p)
	if addr == "" {
		t.Fatal("no mint address")
	}
	return &p, addr
}

// mintTx is a request signed by buyer paying the mint address units of
// asset, with the consensus.MintMemo memo and the ARG change that pays the fee.
func mintTx(txid, buyer, mintAddr string, paid consensus.Amount, asset string, fee consensus.Amount) txJSON {
	return txJSON{TxJSON: wallet.TxJSON{TxID: txid,
		Inputs:  []wallet.TxInJSON{testIn("p-"+txid, 0, buyer, 1000*coin, ""), testIn("p-"+txid, 1, buyer, 100*coin, testAsset)},
		Outputs: []wallet.TxOutJSON{testOut(buyer, 1000*coin-fee, ""), testOut(mintAddr, paid, asset), testOut(buyer, 100*coin-paid, testAsset), dataOut(consensus.MintMemo)}}}
}

func cbOuts(txid string, outs ...wallet.TxOutJSON) txJSON {
	return txJSON{TxJSON: wallet.TxJSON{TxID: txid, Coinbase: true, Inputs: []wallet.TxInJSON{{Coinbase: true}}, Outputs: outs}}
}

// mintChain indexes a small chain with requests: alice mints 40 ARG in
// block 1; bob 20 and alice 10 in block 2 (with a 1 ARG fee); block 3 is
// plain; block 4, after the window, carries a request that is ignored.
func mintChain(t *testing.T, ix *Indexer, p *consensus.Params, mintAddr, alice, bob string) {
	t.Helper()
	sub := func(h uint32) consensus.Amount { return consensus.Subsidy(h) }
	if err := ix.apply(testBlock(0, "", coinbaseTx("cb0", sub(0)))); err != nil {
		t.Fatal(err)
	}
	b1 := testBlock(1, "hash0", cbOuts("cb1", testOut(minerAddr, sub(1), ""), testOut(alice, 40*coin, "")),
		mintTx("t1", alice, mintAddr, 2*coin, testAsset, 0),
		mintTx("t2", bob, mintAddr, 30*coin, testAsset, 0), // 600 ARG: over the per-block cap, ignored
		mintTx("t3", bob, mintAddr, 0, testAsset, 0),       // pays nothing: ignored
		txJSON{TxJSON: wallet.TxJSON{TxID: "t3b", Inputs: []wallet.TxInJSON{testIn("p-t3b", 0, bob, 10*coin, "")},
			Outputs: []wallet.TxOutJSON{testOut(mintAddr, 5*coin, ""), dataOut(consensus.MintMemo)}}}, // pays ARG, not the asset: ignored
	)
	if err := ix.apply(b1); err != nil {
		t.Fatal(err)
	}
	b2 := testBlock(2, "hash1", cbOuts("cb2", testOut(minerAddr, sub(2)+coin, ""), testOut(bob, 20*coin, ""), testOut(alice, 10*coin, "")),
		mintTx("t4", bob, mintAddr, coin, testAsset, coin),
		mintTx("t5", alice, mintAddr, coin/2, testAsset, 0),
	)
	if err := ix.apply(b2); err != nil {
		t.Fatal(err)
	}
	if err := ix.apply(testBlock(3, "hash2", coinbaseTx("cb3", sub(3)))); err != nil {
		t.Fatal(err)
	}
	b4 := testBlock(4, "hash3", cbOuts("cb4", testOut(minerAddr, sub(4)+coin, "")), mintTx("t6", alice, mintAddr, coin, testAsset, coin))
	if err := ix.apply(b4); err != nil {
		t.Fatal(err)
	}
}

func TestIndexerMint(t *testing.T) {
	p, mintAddr := mintParams(t)
	alice, bob := randomAddr(t, p), randomAddr(t, p)
	ix := namesIndexer(t, p)
	mintChain(t, ix, p, mintAddr, alice, bob)

	b1, _ := ix.block(1)
	wantAmount(t, "block 1 minted", b1.Minted, 40*coin)
	wantAmount(t, "block 1 fees", b1.Fees, 0)
	wantAmount(t, "block 1 reward", b1.Reward, consensus.Subsidy(1)+40*coin)
	wantAmount(t, "block 1 minted total", b1.MintedTotal, 40*coin)
	b2, _ := ix.block(2)
	wantAmount(t, "block 2 minted", b2.Minted, 30*coin)
	wantAmount(t, "block 2 fees", b2.Fees, coin)
	wantAmount(t, "block 2 minted total", b2.MintedTotal, 70*coin)
	b4, _ := ix.block(4)
	wantAmount(t, "block 4 minted", b4.Minted, 0)
	wantAmount(t, "block 4 fees", b4.Fees, coin)
	wantAmount(t, "block 4 minted total", b4.MintedTotal, 70*coin)

	if tot := ix.mintTotals(); tot.Minted != 70*coin || tot.Requests != 3 {
		t.Errorf("totals = %+v", tot)
	}
	wantAmount(t, "supply at 2", ix.supply(2), subsidySupply(2)+70*coin)
	wantAmount(t, "supply at 4", ix.supply(4), subsidySupply(4)+70*coin)
	wantAmount(t, "supply at 0", ix.supply(0), 0)
	rec := ix.recentMints(20)
	if len(rec) != 3 || rec[0].TxID != "t5" || rec[1].TxID != "t4" || rec[2].TxID != "t1" {
		t.Fatalf("recent = %+v", rec)
	}
	if rec[0].Buyer != alice || rec[0].Paid != coin/2 || rec[0].Minted != 10*coin || rec[0].Height != 2 || rec[0].Time == 0 {
		t.Errorf("recent[0] = %+v", rec[0])
	}
	if r := ix.recentMints(1); len(r) != 1 || r[0].TxID != "t5" {
		t.Errorf("recent(1) = %+v", r)
	}
	if m, ok := ix.mintRecordOf(1, "t1"); !ok || m.Minted != 40*coin || m.Buyer != alice {
		t.Errorf("record of t1 = %+v %v", m, ok)
	}
	if _, ok := ix.mintRecordOf(1, "t2"); ok {
		t.Error("the oversized t2 was indexed")
	}
	if _, ok := ix.mintRecordOf(4, "t6"); ok {
		t.Error("a request after the window was indexed")
	}
	if buyers := ix.mintBuyersAt(2); len(buyers) != 2 || buyers[alice] != 10*coin || buyers[bob] != 20*coin {
		t.Errorf("buyers at 2 = %v", buyers)
	}
	ar, paid, minted, n := ix.mintsOf(alice, 20)
	if n != 2 || paid != 2*coin+coin/2 || minted != 50*coin || len(ar) != 2 || ar[0].TxID != "t5" || ar[1].TxID != "t1" {
		t.Errorf("alice's mints = %+v %d %d %d", ar, paid, minted, n)
	}
	if ar, _, _, n := ix.mintsOf(alice, 1); n != 2 || len(ar) != 1 {
		t.Errorf("alice's mints capped = %+v %d", ar, n)
	}
	if _, _, _, n := ix.mintsOf(minerAddr, 20); n != 0 {
		t.Errorf("the miner has mints: %d", n)
	}

	// The buyer's share of the coinbase is "minted" in the history, not a
	// mined block; the miner's is mined.
	at := ix.totals(alice)
	if at.Mined != 0 || at.MinedTotal != 0 {
		t.Errorf("alice counts as a miner: %+v", at)
	}
	wantAmount(t, "alice received", at.Received, 3*(1000*coin)-coin+50*coin) // t1, t5, t6 change and the two coinbase shares
	hist := ix.addressHistory(alice, 10)
	var minted2, mined int
	for _, e := range hist {
		if e.Mint {
			minted2++
		}
		if e.Coinbase && !e.Mint {
			mined++
		}
	}
	if minted2 != 2 || mined != 0 {
		t.Errorf("alice history: %d minted, %d mined entries: %+v", minted2, mined, hist)
	}
	mt := ix.totals(minerAddr)
	if mt.Mined != 5 {
		t.Errorf("miner mined = %d, want 5", mt.Mined)
	}
	// Alice's minted coins mature like a reward.
	wantAmount(t, "alice maturing", ix.maturing(alice, 4), 50*coin)

	// Reorg: block by block, every figure goes back.
	for _, h := range []uint32{4, 3} {
		if err := ix.rewind(h); err != nil {
			t.Fatal(err)
		}
	}
	if tot := ix.mintTotals(); tot.Minted != 70*coin || tot.Requests != 3 {
		t.Errorf("totals after rewinding plain blocks = %+v", tot)
	}
	if err := ix.rewind(2); err != nil {
		t.Fatal(err)
	}
	if tot := ix.mintTotals(); tot.Minted != 40*coin || tot.Requests != 1 {
		t.Errorf("totals after rewinding block 2 = %+v", tot)
	}
	wantAmount(t, "supply after rewind", ix.supply(1), subsidySupply(1)+40*coin)
	if r := ix.recentMints(20); len(r) != 1 || r[0].TxID != "t1" {
		t.Errorf("recent after rewind = %+v", r)
	}
	if _, _, _, n := ix.mintsOf(bob, 20); n != 0 {
		t.Errorf("bob still has mints after the rewind: %d", n)
	}
	if _, _, minted, n := ix.mintsOf(alice, 20); n != 1 || minted != 40*coin {
		t.Errorf("alice's mints after rewind: %d %d", n, minted)
	}
	if err := ix.rewind(1); err != nil {
		t.Fatal(err)
	}
	if tot := ix.mintTotals(); tot.Minted != 0 || tot.Requests != 0 {
		t.Errorf("totals after rewinding block 1 = %+v", tot)
	}
	if _, err := ix.db.Get(keyMintTotals, nil); err == nil {
		t.Error("the totals key survived the rewind of every request")
	}
	if len(ix.recentMints(20)) != 0 {
		t.Error("records survived the rewind")
	}
	wantAmount(t, "supply at genesis", ix.supply(0), 0)
	if _, err := ix.db.Get(keyTotals(alice), nil); err == nil {
		t.Error("alice still has totals after the rewind")
	}

	// A network without a window indexes the same block with nothing minted:
	// the buyer's coinbase output reads as fees.
	off := namesIndexer(t, consensus.RegTest)
	if err := off.apply(testBlock(0, "", coinbaseTx("cb0", consensus.Subsidy(0)))); err != nil {
		t.Fatal(err)
	}
	if err := off.apply(testBlock(1, "hash0", cbOuts("cb1", testOut(minerAddr, consensus.Subsidy(1), ""), testOut(alice, 40*coin, "")), mintTx("t1", alice, mintAddr, 2*coin, testAsset, 0))); err != nil {
		t.Fatal(err)
	}
	if b, _ := off.block(1); b.Minted != 0 || b.Fees != 40*coin {
		t.Errorf("block without a window = %+v", b)
	}
	if off.supply(1) != subsidySupply(1) {
		t.Error("supply counts a mint on a network without a window")
	}
}

// The referral test sees the coinbase without the buyers' outputs.
func TestWithoutMintOutputs(t *testing.T) {
	outs := []wallet.TxOutJSON{testOut(minerAddr, 95*coin, ""), testOut(aliceAddr, 5*coin, ""), testOut(bobAddr, 40*coin, ""), testOut(aliceAddr, 20*coin, "")}
	mints := []mintRecord{{Buyer: bobAddr, Minted: 40 * coin}, {Buyer: aliceAddr, Minted: 10 * coin}, {Buyer: aliceAddr, Minted: 10 * coin}}
	got := withoutMintOutputs(outs, mints)
	if len(got) != 2 || got[0].Address != minerAddr || got[1].Address != aliceAddr || got[1].Amount != 5*coin {
		t.Errorf("without mint outputs = %+v", got)
	}
	if got := withoutMintOutputs(outs, nil); len(got) != 4 {
		t.Errorf("without mints = %+v", got)
	}
}

func mintServer(t *testing.T) (*server, *consensus.Params, string, string, string) {
	t.Helper()
	p, mintAddr := mintParams(t)
	alice, bob := randomAddr(t, p), randomAddr(t, p)
	ix := namesIndexer(t, p)
	mintChain(t, ix, p, mintAddr, alice, bob)
	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(p, nil, ix, newWorkServer(nil, p, 5, t.Logf), tmpl, "", "")
	return s, p, mintAddr, alice, bob
}

func TestMintAPI(t *testing.T) {
	s, p, mintAddr, alice, bob := mintServer(t)
	code, m := getJSON(t, s, "/api/early")
	keys := []string{"window", "start", "end", "height", "status", "perBlock", "perBlockCoin", "rate", "maxPaid", "maxPaidCoin", "asset", "address",
		"minted", "mintedCoin", "requests", "cap", "capCoin", "remaining", "pending", "pendingCoin", "maturity", "recent"}
	wantKeys(t, "mint", m, keys...)
	if len(m) != len(keys) {
		t.Errorf("mint has %d fields, want %d: %v", len(m), len(keys), m)
	}
	if code != 200 || m["window"] != true || m["start"] != 1.0 || m["end"] != 3.0 || m["height"] != 4.0 || m["status"] != "closed" {
		t.Errorf("mint = %v", m)
	}
	if m["perBlock"] != float64(500*coin) || m["perBlockCoin"] != "500.00000000" || m["rate"] != 20.0 || m["maxPaid"] != float64(25*coin) || m["maxPaidCoin"] != "25.00000000" {
		t.Errorf("rule = %v", m)
	}
	if m["asset"] != testAsset || m["address"] != mintAddr {
		t.Errorf("asset/address = %v %v", m["asset"], m["address"])
	}
	if m["minted"] != float64(70*coin) || m["mintedCoin"] != "70.00000000" || m["requests"] != 3.0 || m["cap"] != float64(1500*coin) || m["capCoin"] != "1500.00000000" {
		t.Errorf("totals = %v", m)
	}
	if m["remaining"] != 0.0 || m["pending"] != 0.0 || m["pendingCoin"] != "0.00000000" || m["maturity"] != float64(consensus.CoinbaseMaturity) {
		t.Errorf("remaining/pending = %v", m)
	}
	recent, _ := m["recent"].([]any)
	if len(recent) != 3 {
		t.Fatalf("recent = %v", m["recent"])
	}
	first, _ := recent[0].(map[string]any)
	wantKeys(t, "recent entry", first, "height", "txid", "buyer", "paid", "paidCoin", "minted", "mintedCoin")
	if first["txid"] != "t5" || first["buyer"] != alice || first["paid"] != float64(coin/2) || first["paidCoin"] != "0.50000000" || first["minted"] != float64(10*coin) || first["mintedCoin"] != "10.00000000" {
		t.Errorf("recent[0] = %v", first)
	}

	// The status follows the index height.
	for _, c := range []struct {
		h         int64
		status    string
		remaining int64
	}{{-1, mintUpcoming, 2}, {0, mintUpcoming, 1}, {1, mintOpen, 3}, {3, mintOpen, 1}, {4, mintClosed, 0}} {
		if st, rem := mintStatus(p, c.h); st != c.status || rem != c.remaining {
			t.Errorf("status at %d = %s %d, want %s %d", c.h, st, rem, c.status, c.remaining)
		}
	}
	if st, rem := mintStatus(consensus.RegTest, 1); st != mintNone || rem != 0 {
		t.Errorf("status without a window = %s %d", st, rem)
	}

	// An address's requests.
	code, a := getJSON(t, s, "/api/early/"+alice)
	wantKeys(t, "address", a, "address", "count", "paid", "paidCoin", "minted", "mintedCoin", "requests")
	if code != 200 || a["address"] != alice || a["count"] != 2.0 || a["paid"] != float64(2*coin+coin/2) || a["minted"] != float64(50*coin) || a["mintedCoin"] != "50.00000000" {
		t.Errorf("alice = %v", a)
	}
	if rs, _ := a["requests"].([]any); len(rs) != 2 || rs[0].(map[string]any)["txid"] != "t5" {
		t.Errorf("alice's requests = %v", a["requests"])
	}
	if code, b := getJSON(t, s, "/api/early/"+bob); code != 200 || b["count"] != 1.0 || b["minted"] != float64(20*coin) {
		t.Errorf("bob = %d %v", code, b)
	}
	if code, _ := getJSON(t, s, "/api/early/nope"); code != 404 {
		t.Errorf("invalid address = %d", code)
	}
	if code, _ := getJSON(t, s, "/api/early/"); code != 404 {
		t.Errorf("empty address = %d", code)
	}

	// Pending: a request in the mempool counts toward the next block.
	s.client = wallet.NewClient("http://127.0.0.1:1", "x") // never reached: the view below is planted
	s.mpCache.get(mempoolTTL, func() any {
		return mempoolView{All: []mempoolRow{{TxID: "m1", Mint: 60 * coin}, {TxID: "m2"}, {TxID: "m3", Mint: 20 * coin}}}
	})
	if _, m := getJSON(t, s, "/api/early"); m["pending"] != float64(80*coin) || m["pendingCoin"] != "80.00000000" {
		t.Errorf("pending = %v %v", m["pending"], m["pendingCoin"])
	}
	raw, _ := json.Marshal(map[string]any{"txid": "m9", "size": 300,
		"inputs":  []any{map[string]any{"txid": "p", "index": 0, "prevout": map[string]any{"amount": 10 * coin, "type": 0, "address": bob}}},
		"outputs": []any{map[string]any{"n": 0, "amount": 3 * coin, "type": 0, "address": mintAddr, "payload": "", "asset": testAsset}, map[string]any{"n": 1, "amount": 0, "type": 1, "payload": hex.EncodeToString([]byte(consensus.MintMemo))}},
	})
	if rw := mempoolRowOf(p, "m9", raw); rw.Mint != 60*coin {
		t.Errorf("mempool row mint = %d", rw.Mint)
	}
	if rw := mempoolRowOf(consensus.RegTest, "m9", raw); rw.Mint != 0 {
		t.Errorf("mempool row mint without a window = %d", rw.Mint)
	}

	// Without a window the API says so.
	off := newServer(consensus.RegTest, nil, namesIndexer(t, consensus.RegTest), newWorkServer(nil, consensus.RegTest, 5, t.Logf), s.tmpl, "", "")
	code, m = getJSON(t, off, "/api/early")
	wantKeys(t, "mint off", m, keys...)
	if code != 200 || m["window"] != false || m["status"] != "none" || m["address"] != "" || m["cap"] != 0.0 || m["rate"] != 0.0 {
		t.Errorf("mint without a window = %v", m)
	}
}

func TestMintPages(t *testing.T) {
	s, p, mintAddr, alice, bob := mintServer(t)
	get := func(s *server, path string) (int, string) {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}
	code, body := get(s, "/early")
	if code != 200 {
		t.Errorf("/early: %d", code)
	}
	asset := s.mintAssetName()
	for _, want := range []string{
		"<h1>Early blocks</h1>",
		"For 3 blocks the chain mints ARG for anyone who asks: 20 ARG for 1 " + asset + ", up to 500 ARG a block, 1,500 in all. Then ARG comes only from mining.",
		"<h2>How it works</h2>",
		"Send " + asset + " to the address below with the memo <span class=\"memo\">" + consensus.MintMemo + "</span>, or use the app.",
		"The next block that takes your request pays you 20 ARG per " + asset + " from its coinbase, spendable 100 blocks later.",
		"At most 25 " + asset + " a request and 500 ARG a block. The " + asset + " goes to the treasury.",
		"Closed", "ran from block 1 to block 3", "Minted so far", ">70<", "of 1,500 ARG", ">3<", "t5", "t1", mintAddr,
		`href="` + defaultAppURL + `/early" rel="noopener">Open in the app</a>`,
		`href="/early">Early blocks</a>`, `href="/api/early">/api/early</a> · /api/early/{address}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/early lacks %q", want)
		}
	}
	for _, bad := range []string{"!", "sale", "Sale", "buy", "Buy", "invest", "discount", "presale", "The rule", "How a request is made", "MINT1", "about the mint"} {
		if strings.Contains(stripMarkup(body), bad) {
			t.Errorf("/early says %q", bad)
		}
	}
	if code, _ := get(s, "/early/x"); code != 404 {
		t.Errorf("/early/x: %d", code)
	}

	// Upcoming and open, by the index height.
	d := s.mintData()
	if d.Status != mintClosed || d.Blocks != 3 || d.MaxPaid != 25*coin || d.Cap != 1500*coin || d.Pct < 4.6 || d.Pct > 4.7 || d.MaturityTime == "" {
		t.Errorf("mint data = %+v", d)
	}
	render := func(data mintPageData) string {
		var buf strings.Builder
		if err := s.tmpl.ExecuteTemplate(&buf, "mint", page{Title: "Early blocks", Brand: "Argonmine", Coin: "ARG", Network: "regtest", Mint: true, App: defaultAppURL, Data: data}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	up := d
	up.Status, up.Remaining, up.ETA, up.Recent = mintUpcoming, 2, "about 20 minutes", []mintRecord{}
	out := render(up)
	for _, want := range []string{"Opens at block 1", "2 blocks to go, about 20 minutes", "Requests can be made from block 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("upcoming page lacks %q", want)
		}
	}
	open := d
	open.Status, open.Remaining, open.ETA = mintOpen, 1, "about 10 minutes"
	out = render(open)
	for _, want := range []string{">Open<", "closes at block 3", "1 block remaining, about 10 minutes"} {
		if !strings.Contains(out, want) {
			t.Errorf("open page lacks %q", want)
		}
	}

	// Without a window: no nav entry, and the page says so.
	off := newServer(consensus.RegTest, nil, namesIndexer(t, consensus.RegTest), newWorkServer(nil, consensus.RegTest, 5, t.Logf), s.tmpl, "", "")
	if _, body := get(off, "/early"); !strings.Contains(body, "No early blocks") || strings.Contains(body, `href="/early">Early blocks</a>`) {
		t.Error("the page without a window is wrong")
	}
	if _, body := get(off, "/names"); strings.Contains(body, `href="/early"`) {
		t.Error("the nav shows Early blocks on a network without a window")
	}

	// The block page: the minted line and the request's badge.
	raw, _ := json.Marshal(map[string]any{
		"hash": "hash1", "height": 1, "prevhash": "hash0", "time": 1_700_000_600, "bits": "207fffff", "size": 300, "nonce": 1, "version": 1, "difficulty": 1.0, "onmainchain": true, "txcount": 2,
		"txroot": "r", "witnessroot": "w", "powhash": "p",
		"tx": []any{
			map[string]any{"txid": "cb1", "coinbase": true, "inputs": []any{map[string]any{"coinbase": true}},
				"outputs": []any{map[string]any{"n": 0, "amount": consensus.Subsidy(1), "type": 0, "address": minerAddr, "payload": ""}, map[string]any{"n": 1, "amount": 40 * coin, "type": 0, "address": alice, "payload": ""}}},
			map[string]any{"txid": "t1", "coinbase": false,
				"inputs":  []any{map[string]any{"txid": "p-t1", "index": 0, "prevout": map[string]any{"amount": 1000 * coin, "type": 0, "address": alice}}},
				"outputs": []any{map[string]any{"n": 0, "amount": 1000 * coin, "type": 0, "address": alice, "payload": ""}, map[string]any{"n": 1, "amount": 2 * coin, "type": 0, "address": mintAddr, "payload": "", "asset": testAsset}, map[string]any{"n": 2, "amount": 0, "type": 1, "payload": hex.EncodeToString([]byte(consensus.MintMemo))}}},
		},
	})
	blk := s.decorateBlock(raw)
	if blk["minted"] != consensus.Amount(40*coin) || blk["mintRequests"] != 1 {
		t.Errorf("block minted = %v for %v", blk["minted"], blk["mintRequests"])
	}
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, "block", page{Title: "Block", Brand: "Argonmine", Coin: "ARG", Network: "regtest", Mint: true, Data: blk}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<dt>Minted</dt><dd>40 ARG for 1 request", "and 40 ARG minted", `<span class="badge badge-accent">early</span>`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("block page lacks %q", want)
		}
	}

	// The transaction page: the request, confirmed and pending, and the
	// coinbase that pays it.
	txRaw := func(txid string, confirmed bool, addr string) []byte {
		m := map[string]any{"txid": txid, "wtxid": "w", "version": 2, "lock_height": 0, "size": 250, "coinbase": false,
			"inputs":  []any{map[string]any{"txid": "p-" + txid, "index": 0, "prevout": map[string]any{"amount": 1000 * coin, "coin": "1000.00000000", "type": 0, "address": addr}}},
			"outputs": []any{map[string]any{"n": 0, "amount": 1000 * coin, "coin": "1000.00000000", "type": 0, "address": addr, "payload": ""}, map[string]any{"n": 1, "amount": 2 * coin, "coin": "2.00000000", "type": 0, "address": mintAddr, "payload": "", "asset": testAsset}, map[string]any{"n": 2, "amount": 0, "coin": "0", "type": 1, "payload": hex.EncodeToString([]byte(consensus.MintMemo))}},
		}
		if confirmed {
			m["blockhash"], m["height"] = "hash1", 1
		}
		b, _ := json.Marshal(m)
		return b
	}
	tx := s.decorateTx(txRaw("t1", true, alice))
	v, _ := tx["mintReq"].(*mintReqView)
	if v == nil || !v.Confirmed || v.Height != 1 || v.Buyer != alice || v.Paid != 2*coin || v.Minted != 40*coin {
		t.Fatalf("mintReq = %+v", v)
	}
	renderTx := func(tx map[string]any) string {
		var buf strings.Builder
		if err := s.tmpl.ExecuteTemplate(&buf, "tx", page{Title: "tx", Brand: "Argonmine", Coin: "ARG", Network: "regtest", Mint: true, Data: tx}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	out = renderTx(tx)
	for _, want := range []string{"Early block request: paid 2.00000000 ", ", 40.00000000 ARG paid to", shorten(alice, 10, 6), "in block <a class=\"link\" href=\"/block/1\">1</a>'s coinbase", `href="/early">early</a>`} {
		if !strings.Contains(out, want) {
			t.Errorf("tx page lacks %q", want)
		}
	}
	tx = s.decorateTx(txRaw("m1", false, bob))
	if v, _ := tx["mintReq"].(*mintReqView); v == nil || v.Confirmed || v.Buyer != bob || v.Minted != 40*coin {
		t.Errorf("pending mintReq = %+v", v)
	}
	if out = renderTx(tx); !strings.Contains(out, "waiting for a block") {
		t.Error("the pending request does not say it is waiting")
	}
	if _, ok := s.decorateTx([]byte(`{"txid":"x","outputs":[{"n":0,"amount":0,"type":1,"payload":"` + hex.EncodeToString([]byte(consensus.MintMemo)) + `"}]}`))["mintReq"]; ok {
		t.Error("a memo without a payment reads as a request")
	}
	cbRaw, _ := json.Marshal(map[string]any{"txid": "cb1", "wtxid": "w", "version": 1, "lock_height": 0, "size": 150, "coinbase": true, "blockhash": "hash1", "height": 1,
		"inputs":  []any{map[string]any{"coinbase": true}},
		"outputs": []any{map[string]any{"n": 0, "amount": consensus.Subsidy(1), "coin": "1", "type": 0, "address": minerAddr, "payload": ""}, map[string]any{"n": 1, "amount": 40 * coin, "coin": "40.00000000", "type": 0, "address": alice, "payload": ""}},
	})
	tx = s.decorateTx(cbRaw)
	outs, _ := tx["outputs"].([]any)
	if len(outs) != 2 || outs[0].(map[string]any)["mint"] != nil || outs[1].(map[string]any)["mint"] != true {
		t.Errorf("coinbase outputs = %v", outs)
	}
	if out = renderTx(tx); strings.Count(out, `title="minted for a request in this block">early</span>`) != 1 {
		t.Error("the coinbase page does not label the buyer's output once")
	}

	// The address page: the Minted card and the history's type.
	_, body = get(s, "/address/"+alice)
	for _, want := range []string{"Minted", "50.00000000", "for 2 requests", "Early block requests", "t5", ">Minted</span>", "Blocks mined"} {
		if !strings.Contains(body, want) {
			t.Errorf("address page lacks %q", want)
		}
	}
	if strings.Contains(body, ">Mined</span>") {
		t.Error("alice's coinbase share reads as mined")
	}
	if _, body = get(s, "/address/"+minerAddr); strings.Contains(body, "Early block requests") {
		t.Error("the miner's page has an early blocks card")
	}
	_ = p
}

// stripMarkup drops tags so the wording tests read the text only.
func stripMarkup(s string) string {
	var b strings.Builder
	in := false
	for _, c := range s {
		switch {
		case c == '<':
			in = true
		case c == '>':
			in = false
		case !in:
			b.WriteRune(c)
		}
	}
	return b.String()
}
