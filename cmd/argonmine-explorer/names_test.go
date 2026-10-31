package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"argonmine/consensus"
	"argonmine/names"
	"argonmine/wallet"
)

// A network with a treasury, so names cost what docs/NAMES.md says.
func namesParams(t *testing.T) (*consensus.Params, string) {
	t.Helper()
	p := *consensus.RegTest
	p.TreasuryHeight = 1
	p.TreasuryKey = make([]byte, consensus.P2PKPayloadLen)
	treasury := names.TreasuryAddress(&p)
	if treasury == "" {
		t.Fatal("no treasury address")
	}
	return &p, treasury
}

func dataOut(memo string) wallet.TxOutJSON {
	return wallet.TxOutJSON{Type: consensus.OutputTypeNullData, Payload: hex.EncodeToString([]byte(memo))}
}

// nameTx is a transaction signed by signer (its input 0 spends a coin of
// signer's) with the given outputs and a memo.
func nameTx(txid, signer, memo string, outs ...wallet.TxOutJSON) txJSON {
	return txJSON{TxJSON: wallet.TxJSON{TxID: txid,
		Inputs:  []wallet.TxInJSON{testIn("prev-"+txid, 0, signer, 1000*coin, "")},
		Outputs: append(outs, dataOut(memo))}}
}

func namesIndexer(t *testing.T, p *consensus.Params) *Indexer {
	t.Helper()
	ix, err := newIndexer(t.TempDir(), nil, p, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ix.close)
	return ix
}

func TestIndexerNames(t *testing.T) {
	p, treasury := namesParams(t)
	ix := namesIndexer(t, p)
	const year = names.BlocksPerYear
	pay := func(addr string, arg consensus.Amount) wallet.TxOutJSON { return testOut(addr, arg*coin, "") }

	if err := ix.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin))); err != nil {
		t.Fatal(err)
	}
	// Block 1: alice registers alice for 2 years (20 ARG); bob's register of
	// the same name is ignored (taken), his underpaid "bobby" is ignored,
	// his reserved "pool" is ignored, his paid "bob" (250 ARG, 3 letters)
	// goes through.
	b1 := testBlock(1, "hash0", coinbaseTx("cb1", 50*coin),
		nameTx("t1", aliceAddr, "NMS1 register alice 2", pay(aliceAddr, 1), pay(treasury, 20)),
		nameTx("t2", bobAddr, "NMS1 register alice 5", pay(bobAddr, 1), pay(treasury, 100)),
		nameTx("t3", bobAddr, "NMS1 register bobby 1", pay(bobAddr, 1), pay(treasury, 5)),
		nameTx("t4", bobAddr, "NMS1 register pool 1", pay(bobAddr, 1), pay(treasury, 50)),
		nameTx("t5", bobAddr, "NMS1 register bob 1", pay(bobAddr, 1), pay(treasury, 250)),
	)
	if err := ix.apply(b1); err != nil {
		t.Fatal(err)
	}
	r, ok := ix.nameRecord("alice")
	if !ok || r.Owner != aliceAddr || r.Registered != 1 || r.Expires != 1+2*year || r.RegTxID != "t1" || r.Primary {
		t.Fatalf("alice after block 1: %+v %v", r, ok)
	}
	for _, l := range []string{"bobby", "pool"} {
		if _, ok := ix.nameRecord(l); ok {
			t.Errorf("%s was registered", l)
		}
	}
	if r, ok := ix.nameRecord("bob"); !ok || r.Owner != bobAddr || r.Expires != 1+year {
		t.Fatalf("bob: %+v %v", r, ok)
	}
	if n := ix.nameCount(); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	if n := ix.activeNames(1); n != 2 {
		t.Errorf("active at 1 = %d, want 2", n)
	}
	if n := ix.activeNames(1 + year); n != 1 {
		t.Errorf("active at bob's expiry = %d, want 1", n)
	}
	if n := ix.activeNames(1 + 2*year); n != 0 {
		t.Errorf("active at alice's expiry = %d, want 0", n)
	}
	if d := ix.displayName(aliceAddr); d != "alice" {
		t.Errorf("alice's display name = %q", d)
	}
	if d := ix.displayName(minerAddr); d != "" {
		t.Errorf("miner's display name = %q", d)
	}
	if rec := ix.recentNames(10); len(rec) != 2 || rec[0].Label != "bob" || rec[1].Label != "alice" || rec[1].TxID != "t1" || rec[1].Expires != 1+2*year {
		t.Errorf("recent = %+v", rec)
	}
	if n := ix.namesSince(b1.Time); n != 2 {
		t.Errorf("since block 1 = %d", n)
	}
	if n := ix.namesSince(b1.Time + 1); n != 0 {
		t.Errorf("since after block 1 = %d", n)
	}
	if h := ix.nameHistory("alice", 10); len(h) != 1 || h[0].Op != "register" || h[0].Years != 2 || h[0].From != aliceAddr || h[0].To != aliceAddr || h[0].TxID != "t1" {
		t.Errorf("alice history = %+v", h)
	}
	if h := ix.nameHistory("bobby", 10); len(h) != 0 {
		t.Errorf("bobby has history: %+v", h)
	}

	// Block 2: alice renews (from the expiry), registers carol and makes it
	// primary in the same block, then makes alice primary: carol's flag
	// comes off.
	b2 := testBlock(2, "hash1", coinbaseTx("cb2", 50*coin),
		nameTx("t6", aliceAddr, "NMS1 renew alice 1", pay(aliceAddr, 1), pay(treasury, 10)),
		nameTx("t7", aliceAddr, "NMS1 register carol 1", pay(aliceAddr, 1), pay(treasury, 10)),
		nameTx("t8", aliceAddr, "NMS1 primary carol", pay(aliceAddr, 1)),
		nameTx("t9", bobAddr, "NMS1 renew alice 1", pay(bobAddr, 1), pay(treasury, 10)), // a stranger: ignored
		nameTx("t10", aliceAddr, "NMS1 primary alice", pay(aliceAddr, 1)),
	)
	if err := ix.apply(b2); err != nil {
		t.Fatal(err)
	}
	r, _ = ix.nameRecord("alice")
	if r.Expires != 1+3*year || !r.Primary {
		t.Fatalf("alice after block 2: %+v", r)
	}
	if c, ok := ix.nameRecord("carol"); !ok || c.Primary || c.Owner != aliceAddr {
		t.Fatalf("carol after block 2: %+v %v", c, ok)
	}
	if d := ix.displayName(aliceAddr); d != "alice" {
		t.Errorf("alice's display name = %q", d)
	}
	if h := ix.nameHistory("alice", 10); len(h) != 3 || h[0].Op != "primary" || h[1].Op != "renew" || h[1].Years != 1 || h[2].Op != "register" {
		t.Errorf("alice history = %+v", h)
	}
	if ls := ix.ownerLabels(aliceAddr); strings.Join(ls, ",") != "alice,carol" {
		t.Errorf("alice's labels = %v", ls)
	}

	// Block 3: alice transfers alice to bob (output 0). Bob then holds two
	// names registered at the same height: the label order decides.
	b3 := testBlock(3, "hash2", coinbaseTx("cb3", 50*coin),
		nameTx("t11", aliceAddr, "NMS1 transfer alice", pay(bobAddr, 1)),
		nameTx("t12", aliceAddr, "NMS1 primary alice", pay(aliceAddr, 1)), // no longer the owner: ignored
	)
	if err := ix.apply(b3); err != nil {
		t.Fatal(err)
	}
	r, _ = ix.nameRecord("alice")
	if r.Owner != bobAddr || r.Primary || r.Expires != 1+3*year {
		t.Fatalf("alice after transfer: %+v", r)
	}
	if ls := ix.ownerLabels(aliceAddr); strings.Join(ls, ",") != "carol" {
		t.Errorf("alice's labels = %v", ls)
	}
	if ls := ix.ownerLabels(bobAddr); strings.Join(ls, ",") != "alice,bob" {
		t.Errorf("bob's labels = %v", ls)
	}
	if d := ix.displayName(aliceAddr); d != "carol" {
		t.Errorf("alice's display name = %q", d)
	}
	if d := ix.displayName(bobAddr); d != "alice" {
		t.Errorf("bob's display name = %q", d)
	}
	if h := ix.nameHistory("alice", 10); len(h) != 4 || h[0].Op != "transfer" || h[0].From != aliceAddr || h[0].To != bobAddr {
		t.Errorf("alice history = %+v", h)
	}
	if e, ok := ix.nameEventOf("alice", 3, "t11"); !ok || e.Op != "transfer" {
		t.Errorf("event of t11: %+v %v", e, ok)
	}
	if _, ok := ix.nameEventOf("alice", 3, "t12"); ok {
		t.Error("the ignored t12 has an event")
	}
	bobRec, _ := ix.nameRecord("bob")

	// Grace: at bob's expiry height a stranger cannot take "bob"; the
	// owner renews from that block. Then the block is rewound.
	exp := bobRec.Expires
	if names.Status(&bobRec, exp-1) != names.StatusRegistered || names.Status(&bobRec, exp) != names.StatusGrace || names.Status(&bobRec, exp+names.GraceBlocks) != names.StatusAvailable {
		t.Fatal("status by height")
	}
	// (A rewind needs the block below it in the index, so a plain block
	// precedes each far-off block; apply does not check continuity.)
	if err := ix.apply(testBlock(exp+4, "hash3", coinbaseTx("cb4a", 50*coin))); err != nil {
		t.Fatal(err)
	}
	b4 := testBlock(exp+5, "hash3", coinbaseTx("cb4", 50*coin),
		nameTx("t13", aliceAddr, "NMS1 register bob 1", pay(aliceAddr, 1), pay(treasury, 250)),
		nameTx("t14", bobAddr, "NMS1 renew bob 1", pay(bobAddr, 1), pay(treasury, 250)),
	)
	if err := ix.apply(b4); err != nil {
		t.Fatal(err)
	}
	if r, _ := ix.nameRecord("bob"); r.Owner != bobAddr || r.Expires != exp+5+year {
		t.Fatalf("bob after a renewal in grace: %+v", r)
	}
	// Alice and bob are active there; carol (one year from block 2) is not.
	if n := ix.activeNames(exp + 5); n != 2 {
		t.Errorf("active after the grace renewal = %d, want 2", n)
	}
	if d := ix.displayName(aliceAddr); d != "" {
		t.Errorf("alice's display name with carol expired = %q", d)
	}
	if err := ix.rewind(exp + 5); err != nil {
		t.Fatal(err)
	}
	if r, _ := ix.nameRecord("bob"); r != bobRec {
		t.Fatalf("bob after the rewind: %+v, want %+v", r, bobRec)
	}
	if h := ix.nameHistory("bob", 10); len(h) != 1 {
		t.Errorf("bob history after the rewind = %+v", h)
	}
	if n := ix.activeNames(exp + 5); n != 1 {
		t.Errorf("active after the rewind = %d, want 1", n)
	}

	// After grace anyone registers it, with a fresh registration; the
	// record is replaced, the count does not grow.
	if err := ix.apply(testBlock(exp+names.GraceBlocks-1, "hash3", coinbaseTx("cb5a", 50*coin))); err != nil {
		t.Fatal(err)
	}
	b5 := testBlock(exp+names.GraceBlocks, "hash3", coinbaseTx("cb5", 50*coin),
		nameTx("t15", aliceAddr, "NMS1 register bob 1", pay(aliceAddr, 1), pay(treasury, 250)))
	if err := ix.apply(b5); err != nil {
		t.Fatal(err)
	}
	if r, _ := ix.nameRecord("bob"); r.Owner != aliceAddr || r.Registered != exp+names.GraceBlocks || r.RegTxID != "t15" {
		t.Fatalf("bob re-registered: %+v", r)
	}
	if n := ix.nameCount(); n != 3 {
		t.Errorf("count after re-registration = %d, want 3", n)
	}
	if ls := ix.ownerLabels(bobAddr); strings.Join(ls, ",") != "alice" {
		t.Errorf("bob's labels after losing bob = %v", ls)
	}
	if err := ix.rewind(exp + names.GraceBlocks); err != nil {
		t.Fatal(err)
	}
	if r, _ := ix.nameRecord("bob"); r != bobRec {
		t.Fatalf("bob after rewinding the re-registration: %+v", r)
	}
	if ls := ix.ownerLabels(bobAddr); strings.Join(ls, ",") != "alice,bob" {
		t.Errorf("bob's labels after the rewind = %v", ls)
	}

	// Reorg: the transfer block goes, then the renew/carol block. Every
	// lookup is back to what it was.
	if err := ix.rewind(3); err != nil {
		t.Fatal(err)
	}
	r, _ = ix.nameRecord("alice")
	if r.Owner != aliceAddr || !r.Primary || r.Expires != 1+3*year {
		t.Fatalf("alice after rewinding the transfer: %+v", r)
	}
	if d := ix.displayName(aliceAddr); d != "alice" {
		t.Errorf("alice's display name after rewind = %q", d)
	}
	if d := ix.displayName(bobAddr); d != "bob" {
		t.Errorf("bob's display name after rewind = %q", d)
	}
	if err := ix.rewind(2); err != nil {
		t.Fatal(err)
	}
	r, _ = ix.nameRecord("alice")
	if r.Owner != aliceAddr || r.Primary || r.Expires != 1+2*year {
		t.Fatalf("alice after rewinding block 2: %+v", r)
	}
	if _, ok := ix.nameRecord("carol"); ok {
		t.Error("carol survived the rewind of her registration")
	}
	if n := ix.nameCount(); n != 2 {
		t.Errorf("count after rewinds = %d, want 2", n)
	}
	if ls := ix.ownerLabels(aliceAddr); strings.Join(ls, ",") != "alice" {
		t.Errorf("alice's labels after rewinds = %v", ls)
	}
	if h := ix.nameHistory("alice", 10); len(h) != 1 {
		t.Errorf("alice history after rewinds = %+v", h)
	}
	if rec := ix.recentNames(10); len(rec) != 2 {
		t.Errorf("recent after rewinds = %+v", rec)
	}
	if err := ix.rewind(1); err != nil {
		t.Fatal(err)
	}
	if n := ix.nameCount(); n != 0 || len(ix.recentNames(10)) != 0 || len(ix.ownerLabels(aliceAddr)) != 0 {
		t.Errorf("names left after rewinding block 1: count %d", n)
	}
	if _, ok := ix.nameRecord("alice"); ok {
		t.Error("alice survived the rewind of block 1")
	}
	if _, err := ix.db.Get(keyNameExpiry(1+2*year, "alice"), nil); err == nil {
		t.Error("alice's expiry key survived the rewind")
	}

	// Free on a network without a treasury: a register that pays nothing.
	free := namesIndexer(t, consensus.RegTest)
	if err := free.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin))); err != nil {
		t.Fatal(err)
	}
	if err := free.apply(testBlock(1, "hash0", coinbaseTx("cb1", 50*coin), nameTx("f1", aliceAddr, "NMS1 register alice 1", pay(aliceAddr, 1)))); err != nil {
		t.Fatal(err)
	}
	if d := free.displayName(aliceAddr); d != "alice" {
		t.Errorf("free register: display name %q", d)
	}
}

// namesFixture is a server over a small chain with names: alice holds
// alice (2 years) and zed (primary); bob holds nothing.
type namesFixture struct {
	s          *server
	p          *consensus.Params
	treasury   string
	alice, bob string
}

func namesServer(t *testing.T) namesFixture {
	t.Helper()
	p, treasury := namesParams(t)
	f := namesFixture{p: p, treasury: treasury, alice: randomAddr(t, p), bob: randomAddr(t, p)}
	ix := namesIndexer(t, p)
	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	pay := func(addr string, arg consensus.Amount) wallet.TxOutJSON { return testOut(addr, arg*coin, "") }
	if err := ix.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin))); err != nil {
		t.Fatal(err)
	}
	if err := ix.apply(testBlock(1, "hash0", coinbaseTx("cb1", 50*coin),
		nameTx("t1", f.alice, "NMS1 register alice 2", pay(f.alice, 1), pay(treasury, 20)),
		nameTx("t2", f.alice, "NMS1 register zed 1", pay(f.alice, 1), pay(treasury, 250)),
	)); err != nil {
		t.Fatal(err)
	}
	if err := ix.apply(testBlock(2, "hash1", coinbaseTx("cb2", 50*coin),
		nameTx("t3", f.alice, "NMS1 primary zed", pay(f.alice, 1)),
	)); err != nil {
		t.Fatal(err)
	}
	f.s = newServer(p, nil, ix, newWorkServer(nil, p, 5, t.Logf), tmpl, "", "")
	return f
}

func getJSON(t *testing.T, s *server, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("%s: no CORS header", path)
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rec.Code, out
}

func wantKeys(t *testing.T, what string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s lacks %q (has %v)", what, k, m)
		}
	}
}

// The API answers the JSON documented in docs/NAMES.md.
func TestNamesAPI(t *testing.T) {
	f := namesServer(t)
	s, treasury, aliceAddr, bobAddr := f.s, f.treasury, f.alice, f.bob
	const year = float64(names.BlocksPerYear)

	code, info := getJSON(t, s, "/api/names/info")
	wantKeys(t, "info", info, "suffix", "min_len", "max_len", "blocks_per_year", "grace_blocks", "years_max", "prices", "treasury", "reserved", "count", "height")
	if code != 200 || info["suffix"] != "arg" || info["treasury"] != treasury || info["count"] != 2.0 || info["height"] != 2.0 {
		t.Errorf("info = %v", info)
	}
	if pr, _ := info["prices"].(map[string]any); pr["3"] != float64(250*coin) || pr["4"] != float64(50*coin) || pr["5"] != float64(10*coin) {
		t.Errorf("prices = %v", info["prices"])
	}
	if rs, _ := info["reserved"].([]any); len(rs) != len(names.Reserved) {
		t.Errorf("reserved = %v", info["reserved"])
	}

	nameKeys := []string{"name", "status", "owner", "primary", "registered_height", "registered_time", "expires_height", "expires_time", "txid", "price", "history"}
	code, n := getJSON(t, s, "/api/name/Alice.ARG")
	wantKeys(t, "name", n, nameKeys...)
	if code != 200 || n["name"] != "alice" || n["status"] != "registered" || n["owner"] != aliceAddr || n["primary"] != false || n["registered_height"] != 1.0 || n["expires_height"] != 1+2*year || n["txid"] != "t1" || n["price"] != float64(10*coin) {
		t.Errorf("name alice = %v", n)
	}
	if n["registered_time"] == 0.0 || n["expires_time"].(float64) <= n["registered_time"].(float64) {
		t.Errorf("times: %v %v", n["registered_time"], n["expires_time"])
	}
	hist, _ := n["history"].([]any)
	if len(hist) != 1 {
		t.Fatalf("history = %v", n["history"])
	}
	wantKeys(t, "history entry", hist[0].(map[string]any), "op", "height", "time", "txid", "from", "to", "years")
	_, z := getJSON(t, s, "/api/name/zed")
	if z["primary"] != true || z["status"] != "registered" {
		t.Errorf("zed = %v", z)
	}
	code, n = getJSON(t, s, "/api/name/nobody")
	wantKeys(t, "available name", n, nameKeys...)
	if code != 200 || n["status"] != "available" || n["owner"] != "" || n["price"] != float64(10*coin) {
		t.Errorf("available = %v", n)
	}
	if _, n = getJSON(t, s, "/api/name/pool"); n["status"] != "reserved" {
		t.Errorf("reserved = %v", n)
	}
	code, n = getJSON(t, s, "/api/name/ab")
	if code != 200 || n["status"] != "invalid" || n["reason"] != names.ErrLength.Error() {
		t.Errorf("invalid = %v", n)
	}
	if _, n = getJSON(t, s, "/api/name/al_ice"); n["status"] != "invalid" || n["reason"] != names.ErrChars.Error() {
		t.Errorf("invalid chars = %v", n)
	}

	code, r := getJSON(t, s, "/api/resolve/alice.arg")
	if code != 200 || r["name"] != "alice" || r["address"] != aliceAddr {
		t.Errorf("resolve = %d %v", code, r)
	}
	code, r = getJSON(t, s, "/api/resolve/nobody")
	if code != 404 || r["error"] != "not found" {
		t.Errorf("resolve unknown = %d %v", code, r)
	}
	if code, _ := getJSON(t, s, "/api/resolve/ab"); code != 404 {
		t.Errorf("resolve invalid = %d", code)
	}

	code, o := getJSON(t, s, "/api/names?owner="+aliceAddr)
	wantKeys(t, "owner", o, "address", "primary", "names")
	if code != 200 || o["address"] != aliceAddr || o["primary"] != "zed" {
		t.Errorf("owner = %v", o)
	}
	ns, _ := o["names"].([]any)
	if len(ns) != 2 {
		t.Fatalf("owner names = %v", o["names"])
	}
	first := ns[0].(map[string]any)
	wantKeys(t, "owned name", first, "name", "expires_height", "expires_time", "primary", "grace")
	if first["name"] != "alice" || first["grace"] != false || first["primary"] != false || ns[1].(map[string]any)["primary"] != true {
		t.Errorf("owner names = %v", ns)
	}
	if code, o := getJSON(t, s, "/api/names?owner="+bobAddr); code != 200 || o["primary"] != "" || len(o["names"].([]any)) != 0 {
		t.Errorf("owner without names = %d %v", code, o)
	}
	if code, _ := getJSON(t, s, "/api/names?owner=nope"); code != 404 {
		t.Errorf("owner invalid = %d", code)
	}
	if code, _ := getJSON(t, s, "/api/names"); code != 400 {
		t.Errorf("owner missing = %d", code)
	}

	code, rc := getJSON(t, s, "/api/names/recent?limit=1")
	wantKeys(t, "recent", rc, "count", "names")
	rn, _ := rc["names"].([]any)
	if code != 200 || rc["count"] != 2.0 || len(rn) != 1 {
		t.Fatalf("recent = %v", rc)
	}
	wantKeys(t, "recent entry", rn[0].(map[string]any), "name", "owner", "height", "time", "txid", "expires_height")
	if rn[0].(map[string]any)["name"] != "zed" {
		t.Errorf("recent[0] = %v", rn[0])
	}
	if _, rc := getJSON(t, s, "/api/names/recent?limit=9999"); len(rc["names"].([]any)) != 2 {
		t.Errorf("recent capped = %v", rc)
	}
}

// The pages render with names, and the search finds them.
func TestNamesPages(t *testing.T) {
	f := namesServer(t)
	s, aliceAddr, bobAddr := f.s, f.alice, f.bob
	get := func(path string) (int, string, string) {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String(), rec.Header().Get("Location")
	}
	code, body, _ := get("/names")
	for _, want := range []string{"Names registered", "Price per year", "alice.arg", "zed.arg", "250.00000000", "Register in the app", defaultAppURL} {
		if !strings.Contains(body, want) {
			t.Errorf("/names lacks %q", want)
		}
	}
	if code != 200 {
		t.Errorf("/names: %d", code)
	}
	code, body, _ = get("/name/alice")
	for _, want := range []string{"alice.arg", "Registered", aliceAddr, "Years left", "register", "t1", "History"} {
		if !strings.Contains(body, want) {
			t.Errorf("/name/alice lacks %q", want)
		}
	}
	if code != 200 {
		t.Errorf("/name/alice: %d", code)
	}
	if _, body, _ = get("/name/nobody"); !strings.Contains(body, "Available") || !strings.Contains(body, defaultAppURL) {
		t.Error("/name/nobody is not offered for registration")
	}
	if _, body, _ = get("/name/pool"); !strings.Contains(body, "Reserved") {
		t.Error("/name/pool is not shown as reserved")
	}
	if _, body, _ = get("/name/ab"); !strings.Contains(body, "Not a valid name") || !strings.Contains(body, names.ErrLength.Error()) {
		t.Error("/name/ab does not say why it is invalid")
	}
	if code, _, loc := get("/name/Alice.arg"); code != 302 || loc != "/name/alice" {
		t.Errorf("/name/Alice.arg -> %d %s", code, loc)
	}
	if code, _, loc := get("/name/?q=Zed.ARG"); code != 302 || loc != "/name/zed" {
		t.Errorf("/name/?q= -> %d %s", code, loc)
	}
	if code, _, loc := get("/search?q=alice.arg"); code != 302 || loc != "/name/alice" {
		t.Errorf("search alice.arg -> %d %s", code, loc)
	}
	if code, _, loc := get("/search?q=zed"); code != 302 || loc != "/name/zed" {
		t.Errorf("search zed -> %d %s", code, loc)
	}
	if code, _, _ := get("/search?q=nobody"); code != 404 {
		t.Errorf("search of an unknown label -> %d", code)
	}
	if code, _, loc := get("/search?q=123"); code != 302 || loc != "/block/123" {
		t.Errorf("search 123 -> %d %s", code, loc)
	}

	// The address page shows the display name and the names card.
	_, body, _ = get("/address/" + aliceAddr)
	for _, want := range []string{"zed.arg", "alice.arg", ">primary<", "Names"} {
		if !strings.Contains(body, want) {
			t.Errorf("address page lacks %q", want)
		}
	}
	if _, body, _ = get("/address/" + bobAddr); strings.Contains(body, ".arg") {
		t.Error("bob's address page shows a name")
	}

	// The transaction page says what a name operation does, applied or not.
	raw, _ := json.Marshal(map[string]any{
		"txid": "t1", "wtxid": "w1", "version": 1, "lock_height": 0, "size": 200, "coinbase": false, "blockhash": "hash1", "height": 1,
		"inputs":  []any{map[string]any{"txid": "prev-t1", "index": 0, "prevout": map[string]any{"amount": 1000 * coin, "coin": "1000.00000000", "type": 0, "address": aliceAddr}}},
		"outputs": []any{map[string]any{"n": 0, "amount": coin, "coin": "1.00000000", "type": 0, "address": aliceAddr, "payload": ""}, map[string]any{"n": 1, "amount": 0, "coin": "0", "type": 1, "payload": hex.EncodeToString([]byte("NMS1 register alice 2"))}},
	})
	tx := s.decorateTx(raw)
	v, _ := tx["nameOp"].(*nameOpView)
	if v == nil || v.Label != "alice" || !v.Applied || !v.Confirmed || v.Text() != "register alice.arg for 2 years" {
		t.Fatalf("nameOp = %+v", v)
	}
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, "tx", page{Title: "tx", Brand: "Argonmine", Coin: "ARG", Network: "regtest", Data: tx}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"register <a class=\"link\" href=\"/name/alice\">alice.arg</a> for 2 years", "applied", "NMS1 register alice 2"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("tx page lacks %q", want)
		}
	}
	if strings.Count(buf.String(), "NMS1 register alice 2") != 1 {
		t.Error("the raw memo is shown more than once")
	}
	// The same operation by a stranger, unconfirmed: pending.
	raw, _ = json.Marshal(map[string]any{
		"txid": "t9", "wtxid": "w9", "version": 1, "lock_height": 0, "size": 200, "coinbase": false,
		"inputs":  []any{map[string]any{"txid": "prev-t9", "index": 0, "prevout": map[string]any{"amount": 1000 * coin, "coin": "1000.00000000", "type": 0, "address": bobAddr}}},
		"outputs": []any{map[string]any{"n": 0, "amount": coin, "coin": "1.00000000", "type": 0, "address": bobAddr, "payload": ""}, map[string]any{"n": 1, "amount": 0, "coin": "0", "type": 1, "payload": hex.EncodeToString([]byte("NMS1 transfer alice"))}},
	})
	tx = s.decorateTx(raw)
	if v, _ := tx["nameOp"].(*nameOpView); v == nil || v.Confirmed || v.Applied || v.Text() != "transfer alice.arg to output 0" {
		t.Errorf("unconfirmed nameOp = %+v", v)
	}
	if _, ok := s.decorateTx([]byte(`{"txid":"x","outputs":[{"n":0,"amount":0,"type":1,"payload":"68656c6c6f"}]}`))["nameOp"]; ok {
		t.Error("a plain memo reads as a name operation")
	}
}

// A mempool register makes a free name pending; a taken name stays
// registered.
func TestNamesPending(t *testing.T) {
	f := namesServer(t)
	s, treasury, bobAddr := f.s, f.treasury, f.bob
	s.client = wallet.NewClient("http://127.0.0.1:1", "x") // never reached: the view below is planted
	row := func(id, signer, memo string, paid consensus.Amount) mempoolRow {
		op, _ := names.Parse([]byte(memo))
		return mempoolRow{TxID: id, Name: &op, Spends: []coinRef{{Addr: signer, TxID: "p", Index: 0}}, Pays: []mpOut{{Index: 0, Addr: signer, Amount: coin}, {Index: 1, Addr: treasury, Amount: paid}}}
	}
	s.mpCache.get(mempoolTTL, func() any {
		return mempoolView{All: []mempoolRow{
			row("m1", bobAddr, "NMS1 register fresh 1", 10*coin),
			row("m2", bobAddr, "NMS1 register cheap 1", 1*coin),
			row("m3", bobAddr, "NMS1 register alice 1", 10*coin),
			row("m4", bobAddr, "NMS1 register pool 1", 10*coin),
		}}
	})
	for label, want := range map[string]string{"fresh": "pending", "cheap": "available", "alice": "registered", "pool": "reserved", "other": "available"} {
		if n := s.lookupName(label, false); n.Status != want {
			t.Errorf("%s: %s, want %s", label, n.Status, want)
		}
	}
	if _, n := getJSON(t, s, "/api/name/fresh"); n["status"] != "pending" {
		t.Errorf("api pending = %v", n)
	}
	if code, _ := getJSON(t, s, "/api/resolve/fresh"); code != http.StatusNotFound {
		t.Error("a pending name resolves")
	}
}
