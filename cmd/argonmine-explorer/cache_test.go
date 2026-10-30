package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
)

// fakeNode answers the RPC methods the explorer uses and counts the calls.
type fakeNode struct {
	mu      sync.Mutex
	calls   map[string]int
	answers map[string]any // method -> result (or func(params []any) any)
	delay   time.Duration
}

func newFakeNode(t *testing.T) (*fakeNode, *wallet.Client) {
	f := &fakeNode{calls: map[string]int{}, answers: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		f.mu.Lock()
		f.calls[req.Method]++
		a, ok := f.answers[req.Method]
		d := f.delay
		f.mu.Unlock()
		time.Sleep(d)
		if fn, isFn := a.(func([]any) any); isFn {
			a = fn(req.Params)
		}
		if !ok || a == nil {
			json.NewEncoder(w).Encode(map[string]any{"result": nil, "error": map[string]any{"code": -5, "message": "not found"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"result": a})
	}))
	t.Cleanup(srv.Close)
	return f, wallet.NewClient(srv.URL, "")
}

func (f *fakeNode) count(m string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[m]
}

// H3: concurrent job requests share one template fetch.
func TestWorkTemplateSingleFlight(t *testing.T) {
	f, client := newFakeNode(t)
	f.answers["getblocktemplate"] = fullTemplate(consensus.RegTest, 5000)
	f.delay = 200 * time.Millisecond
	ws := newWorkServer(client, consensus.RegTest, 5, t.Logf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ws.Job(randomAddr(t, consensus.RegTest), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count("getblocktemplate"); n != 1 {
		t.Fatalf("50 concurrent jobs fetched the template %d times", n)
	}
	// A refetch that finds the same template keeps the shared object.
	first := ws.tmpl
	ws.mu.Lock()
	ws.tmpl.fetched = time.Now().Add(-time.Minute)
	ws.mu.Unlock()
	if _, err := ws.Job(randomAddr(t, consensus.RegTest), ""); err != nil {
		t.Fatal(err)
	}
	if f.count("getblocktemplate") != 2 || ws.tmpl != first {
		t.Fatalf("unchanged template: %d fetches, same object %v", f.count("getblocktemplate"), ws.tmpl == first)
	}
}

// M6: the mempool page costs one getrawmempool per rebuild plus one
// getrawtransaction per new transaction; confirmed blocks and transactions
// are fetched once.
func TestReadCaches(t *testing.T) {
	f, client := newFakeNode(t)
	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	ix := testIndexer(t)
	if err := ix.apply(testBlock(0, "", coinbaseTx("cb0", 50*coin))); err != nil {
		t.Fatal(err)
	}
	ix.tip = 10
	s := newServer(consensus.RegTest, client, ix, newWorkServer(client, consensus.RegTest, 5, t.Logf), tmpl, "", "")
	h := s.routes()
	get := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	mtx := func(id string) map[string]any {
		return map[string]any{"txid": id, "size": 200, "inputs": []any{map[string]any{"txid": "aa", "index": 0, "prevout": map[string]any{"amount": 1000, "address": "rarg1x"}}},
			"outputs": []any{map[string]any{"n": 0, "amount": 900, "address": "rarg1y"}}}
	}
	ids := []string{"t1", "t2", "t3"}
	f.answers["getrawmempool"] = func([]any) any { return ids }
	f.answers["getrawtransaction"] = func(p []any) any {
		id, _ := p[0].(string)
		if strings.HasPrefix(id, "c0") {
			return map[string]any{"txid": id, "size": 100, "blockhash": strings.Repeat("b", 64), "height": 3, "confirmations": 1, "inputs": []any{}, "outputs": []any{}}
		}
		return mtx(id)
	}
	for i := 0; i < 5; i++ {
		get("/mempool")
	}
	if f.count("getrawmempool") != 1 || f.count("getrawtransaction") != 3 {
		t.Fatalf("5 mempool views: %d getrawmempool, %d getrawtransaction", f.count("getrawmempool"), f.count("getrawtransaction"))
	}
	ids = []string{"t1", "t2", "t3", "t4"}
	s.mpCache.mu.Lock()
	s.mpCache.at = time.Time{} // expire the view
	s.mpCache.mu.Unlock()
	if body := get("/mempool"); !strings.Contains(body, "4 waiting") {
		t.Errorf("rebuilt mempool page lacks the new count")
	}
	if f.count("getrawmempool") != 2 || f.count("getrawtransaction") != 4 {
		t.Fatalf("after one new tx: %d getrawmempool, %d getrawtransaction (rows are not reused)", f.count("getrawmempool"), f.count("getrawtransaction"))
	}

	// Blocks: a buried one is cached, the tip (no successor yet) is not.
	buried, tip := strings.Repeat("1", 64), strings.Repeat("2", 64)
	f.answers["getblock"] = func(p []any) any {
		id, _ := p[0].(string)
		b := map[string]any{"hash": id, "height": 3, "onmainchain": true, "confirmations": 1, "time": 1700000000, "bits": "207fffff", "tx": []any{}}
		if id == buried {
			b["nextblockhash"] = strings.Repeat("3", 64)
		}
		return b
	}
	for i := 0; i < 3; i++ {
		if body := get("/block/" + buried); !strings.Contains(body, "8 confirmations") {
			t.Fatalf("cached block page: confirmations not recomputed")
		}
		get("/block/" + tip)
	}
	if n := f.count("getblock"); n != 1+3 {
		t.Fatalf("3 views each of a buried block and the tip: %d getblock calls, want 4", n)
	}
	for i := 0; i < 3; i++ {
		get("/tx/c0" + strings.Repeat("0", 62))
	}
	if n := f.count("getrawtransaction"); n != 4+1 {
		t.Fatalf("3 views of a confirmed tx: %d getrawtransaction calls in total, want 5", n)
	}
	// A reorg empties the caches.
	ix.onRewind()
	get("/block/" + buried)
	if n := f.count("getblock"); n != 5 {
		t.Fatalf("after a rewind the block was not refetched (%d calls)", n)
	}
	// Malformed ids never reach the node.
	before := f.count("getblock") + f.count("getrawtransaction")
	for _, p := range []string{"/block/xyz", "/tx/1234", "/block/" + strings.Repeat("g", 64)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if after := f.count("getblock") + f.count("getrawtransaction"); after != before {
		t.Errorf("malformed ids caused %d node calls", after-before)
	}
}
