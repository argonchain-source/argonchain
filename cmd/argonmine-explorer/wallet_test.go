package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"argonmine/consensus"
	"argonmine/litewallet"
	"argonmine/wallet"
)

func coinKeys(t *testing.T, ix *Indexer, addr string) []string {
	t.Helper()
	cs, all := ix.coins(addr, 100)
	if !all {
		t.Fatalf("%s: coin list cut short", addr)
	}
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s:%d=%d%s", c.TxID, c.Index, c.Amount, c.Asset))
	}
	sort.Strings(out)
	return out
}

func wantCoins(t *testing.T, ix *Indexer, addr string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := coinKeys(t, ix, addr); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%s coins %v, want %v", addr, got, want)
	}
}

// The coin set follows blocks and rewinds: a coin created and spent in the
// same block never appears, and rewinding puts back exactly what was there.
func TestCoinIndexAndReorg(t *testing.T) {
	ix := testIndexer(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ix.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin))))
	t1 := txJSON{TxJSON: wallet.TxJSON{TxID: "t1",
		Inputs:  []wallet.TxInJSON{testIn("cb0", 0, minerAddr, 50*coin, "")},
		Outputs: []wallet.TxOutJSON{testOut(aliceAddr, 25*coin, testAsset), testOut(minerAddr, 49*coin, "")}}}
	must(ix.apply(testBlock(1, "hash0", coinbaseTx("cb1", 51*coin), t1)))
	wantCoins(t, ix, minerAddr, fmt.Sprintf("cb1:0=%d", 51*coin), fmt.Sprintf("t1:1=%d", 49*coin))
	wantCoins(t, ix, aliceAddr, fmt.Sprintf("t1:0=%d%s", 25*coin, testAsset))
	cs, _ := ix.coins(minerAddr, 10)
	for _, c := range cs {
		if c.TxID == "cb1" && (!c.Coinbase || c.Height != 1) {
			t.Fatalf("the mining reward is listed as %+v", c)
		}
	}

	// Block 2: t2 pays bob from the change; t3 spends bob's new coin at once.
	t2 := txJSON{TxJSON: wallet.TxJSON{TxID: "t2",
		Inputs:  []wallet.TxInJSON{testIn("t1", 1, minerAddr, 49*coin, "")},
		Outputs: []wallet.TxOutJSON{testOut(bobAddr, 10*coin, ""), testOut(minerAddr, 38*coin, "")}}}
	t3 := txJSON{TxJSON: wallet.TxJSON{TxID: "t3",
		Inputs:  []wallet.TxInJSON{testIn("t2", 0, bobAddr, 10*coin, "")},
		Outputs: []wallet.TxOutJSON{testOut(aliceAddr, 9*coin, "")}}}
	must(ix.apply(testBlock(2, "hash1", coinbaseTx("cb2", 52*coin), t2, t3)))
	wantCoins(t, ix, minerAddr, fmt.Sprintf("cb1:0=%d", 51*coin), fmt.Sprintf("cb2:0=%d", 52*coin), fmt.Sprintf("t2:1=%d", 38*coin))
	wantCoins(t, ix, bobAddr)
	wantCoins(t, ix, aliceAddr, fmt.Sprintf("t1:0=%d%s", 25*coin, testAsset), fmt.Sprintf("t3:0=%d", 9*coin))

	must(ix.rewind(2))
	wantCoins(t, ix, minerAddr, fmt.Sprintf("cb1:0=%d", 51*coin), fmt.Sprintf("t1:1=%d", 49*coin))
	wantCoins(t, ix, bobAddr)
	wantCoins(t, ix, aliceAddr, fmt.Sprintf("t1:0=%d%s", 25*coin, testAsset))
	must(ix.rewind(1))
	wantCoins(t, ix, minerAddr, fmt.Sprintf("cb0:0=%d", 50*coin))
	wantCoins(t, ix, aliceAddr)
}

type utxosJSON struct {
	Coins   []Coin        `json:"coins"`
	Pending []pendingCoin `json:"pending"`
	Mempool []string      `json:"mempool"`
	Height  int64         `json:"indexedHeight"`
}

// A browser wallet pays through the explorer: its coins, a payment, the
// coin marked as being spent, its unconfirmed change spent again at once,
// a second payment of the same coin refused, and the confirmed result.
func TestWalletAPIEndToEnd(t *testing.T) {
	n := newTestNode(t)
	ix, err := newIndexer(t.TempDir(), n.client, consensus.RegTest, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ix.close)
	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(consensus.RegTest, n.client, ix, newWorkServer(n.client, consensus.RegTest, 5, t.Logf), tmpl, "", "")
	h := s.routes()

	m, _ := wallet.NewMnemonic()
	acct, err := litewallet.FromMnemonic(consensus.RegTest, m)
	if err != nil {
		t.Fatal(err)
	}
	friend := testKeyAddr(t, 0)
	mine := func(k int, addr string) {
		t.Helper()
		if _, err := n.client.Call("generatetoaddress", k, addr); err != nil {
			t.Fatal(err)
		}
		if err := ix.syncOnce(); err != nil {
			t.Fatal(err)
		}
	}
	mine(2, acct.Address())
	mine(100, friend)

	utxos := func() utxosJSON {
		t.Helper()
		s.mpCache.expire()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/utxos/"+acct.Address(), nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("GET /api/utxos: %d %s", rec.Code, rec.Body.String())
		}
		var u utxosJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	coins := func(u utxosJSON) []litewallet.Coin {
		var out []litewallet.Coin
		for _, c := range u.Coins {
			if c.SpentBy == "" {
				out = append(out, litewallet.Coin{TxID: c.TxID, Index: c.Index, Amount: c.Amount, Asset: c.Asset, Height: c.Height, Coinbase: c.Coinbase})
			}
		}
		for _, p := range u.Pending {
			if p.Own {
				out = append(out, litewallet.Coin{TxID: p.TxID, Index: p.Index, Amount: p.Amount, Asset: p.Asset})
			}
		}
		return out
	}
	post := func(body string) (int, map[string]string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/tx", bytes.NewBufferString(body)))
		var out map[string]string
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("POST /api/tx answered without CORS")
		}
		return rec.Code, out
	}
	pay := func(cs []litewallet.Coin, tip uint32, amount consensus.Amount) *litewallet.Payment {
		t.Helper()
		p, err := acct.Pay(cs, tip, litewallet.PayRequest{To: friend, Amount: amount})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	u := utxos()
	if len(u.Coins) != 2 || !u.Coins[0].Coinbase || len(u.Pending) != 0 {
		t.Fatalf("coins %+v pending %+v", u.Coins, u.Pending)
	}
	tip := uint32(u.Height)
	p1 := pay(coins(u), tip, 7*coin)
	if code, out := post(`{"hex":"` + p1.Hex + `"}`); code != http.StatusOK || out["txid"] != p1.TxID {
		t.Fatalf("broadcast: %d %v", code, out)
	}
	u = utxos()
	spent := 0
	for _, c := range u.Coins {
		if c.SpentBy == p1.TxID {
			spent++
		}
	}
	if spent != 1 || len(u.Pending) != 1 || !u.Pending[0].Own || u.Pending[0].TxID != p1.TxID || len(u.Mempool) != 1 {
		t.Fatalf("after paying: coins %+v pending %+v mempool %v", u.Coins, u.Pending, u.Mempool)
	}

	// The same coin again, from a stale list: refused, the first payment stays.
	var stale []litewallet.Coin
	for _, c := range p1.Spent {
		stale = append(stale, c)
	}
	p2 := pay(stale, tip, 3*coin)
	if code, out := post(`{"hex":"` + p2.Hex + `"}`); code != http.StatusConflict {
		t.Fatalf("a payment spending a waiting payment's coin: %d %v", code, out)
	}
	// The unconfirmed change, at once.
	var change []litewallet.Coin
	for _, c := range coins(u) {
		if c.TxID == p1.TxID {
			change = append(change, c)
		}
	}
	p3 := pay(change, tip, 5*coin)
	if code, out := post(`{"hex":"` + p3.Hex + `"}`); code != http.StatusOK {
		t.Fatalf("spending unconfirmed change: %d %v", code, out)
	}

	for body, want := range map[string]int{`{"hex":"zz"}`: 400, `{"hex":""}`: 400, `not json`: 400, `{"hex":"00"}`: 400} {
		if code, _ := post(body); code != want {
			t.Fatalf("POST %s: %d, want %d", body, code, want)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/api/tx", nil))
	if rec.Code != http.StatusNoContent || !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Fatalf("preflight: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/tx", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/tx: %d", rec.Code)
	}

	mine(1, friend)
	u = utxos()
	if len(u.Pending) != 0 || len(u.Mempool) != 0 {
		t.Fatalf("after the block: pending %+v mempool %v", u.Pending, u.Mempool)
	}
	var total consensus.Amount
	for _, c := range u.Coins {
		total += c.Amount
	}
	if want := 100*coin - 12*coin - p1.Fee - p3.Fee; total != want {
		t.Fatalf("after the block the account has %d, want %d", total, want)
	}
}
