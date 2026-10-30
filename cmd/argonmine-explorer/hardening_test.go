package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"argonmine/consensus"
	"argonmine/miner"
	"argonmine/rpc"
	"argonmine/wallet"
	"argonmine/workclient"
)

// Regression tests for the work server and explorer hardening (review
// findings H3, H4, I2, I3, L4, M5, client addresses). The proof-of-concept
// numbers they replace: 2.43 MB and ~50 ms per job with a full template,
// every job wiped at 20,000, 499 MB heap from 64 bogus submissions.

// randomAddr is a valid address for params without deriving a key.
func randomAddr(t testing.TB, p *consensus.Params) string {
	b := make([]byte, 32)
	rand.Read(b)
	a, err := wallet.EncodeAddress(p.HRP, consensus.OutputTypeP2PK, b)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// fullTemplate fakes a node template carrying about size bytes of transactions.
func fullTemplate(p *consensus.Params, size int) *miner.TemplateJSON {
	t := &miner.TemplateJSON{Version: 1, PreviousBlockHash: p.GenesisHash.String(), Height: 1,
		Bits: fmt.Sprintf("%08x", p.PowLimitBits), CurTime: uint64(time.Now().Unix()), CoinbaseValue: consensus.Subsidy(1)}
	n := 0
	for i := 0; n < size; i++ {
		tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: consensus.OutPoint{Index: uint32(i)}}}, Witnesses: [][]byte{make([]byte, 64)}}
		for j := 0; j < 20; j++ {
			tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: 1000, Type: consensus.OutputTypeP2PK, Payload: make([]byte, 32)})
		}
		raw := tx.Serialize()
		n += len(raw)
		t.Transactions = append(t.Transactions, struct {
			Data string `json:"data"`
			TxID string `json:"txid"`
		}{hex.EncodeToString(raw), tx.TxID().String()})
	}
	return t
}

// fixedTemplate gives ws a template that never needs a node.
func fixedTemplate(t testing.TB, ws *workServer, tj *miner.TemplateJSON) *workTemplate {
	wt, err := newWorkTemplate(tj, time.Now().Add(time.Hour)) // "fetched" in the future: never refetched
	if err != nil {
		t.Fatal(err)
	}
	ws.tmpl = wt
	return wt
}

func heapNow() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// H3: a job keeps only its coinbase and header; the decoded template is
// shared. 200 jobs on a ~950 kB template must retain well under 100 kB each.
func TestWorkJobMemory(t *testing.T) {
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	tmpl := fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 950_000))
	if len(tmpl.body) < 900_000 || tmpl.ntx < 100 {
		t.Fatalf("template too small: %d bytes, %d txs", len(tmpl.body), tmpl.ntx)
	}
	before := heapNow()
	start := time.Now()
	const n = 200
	for i := 0; i < n; i++ {
		j, err := ws.Job(randomAddr(t, consensus.RegTest), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(j.MerkleBranch) == 0 || len(j.MerkleBranch) > 20 {
			t.Fatalf("branch length %d", len(j.MerkleBranch))
		}
	}
	per := time.Since(start) / n
	after := heapNow()
	ws.mu.Lock()
	for _, j := range ws.jobs {
		if j.tmpl != tmpl {
			t.Fatal("a job holds its own template")
		}
	}
	nj := len(ws.jobs)
	ws.mu.Unlock()
	retained := int64(after) - int64(before)
	t.Logf("%d jobs on a %d-byte template: %d bytes retained (%d per job), %s per job", nj, len(tmpl.body), retained, retained/n, per)
	if nj != n {
		t.Fatalf("%d jobs stored, want %d", nj, n)
	}
	if retained/n > 100_000 {
		t.Fatalf("each job retains %d bytes; the template is not shared", retained/n)
	}
	if per > 20*time.Millisecond {
		t.Errorf("a job takes %s to build; it should not depend on the template size", per)
	}
}

// H3: at the cap the oldest job goes, nothing is wiped; one address keeps at
// most workJobsPerAddr jobs, so it cannot push anyone else's out.
func TestWorkJobCaps(t *testing.T) {
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 0))
	victim := randomAddr(t, consensus.RegTest)
	vj, err := ws.Job(victim, "")
	if err != nil {
		t.Fatal(err)
	}
	// One address asking without end keeps only its newest jobs.
	attacker := randomAddr(t, consensus.RegTest)
	var first string
	for i := 0; i < 3*workJobsPerAddr; i++ {
		j, err := ws.Job(attacker, "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = j.ID
		}
	}
	ws.mu.Lock()
	kept, firstKept := len(ws.byAddr[attacker]), ws.jobs[first] != nil
	ws.mu.Unlock()
	if kept != workJobsPerAddr || firstKept {
		t.Fatalf("per-address cap: %d jobs kept (want %d), oldest kept %v", kept, workJobsPerAddr, firstKept)
	}
	// Many addresses filling the table up to the cap evict nobody.
	ws.mu.Lock()
	have := len(ws.jobs)
	ws.mu.Unlock()
	addrs := make([]string, (workMaxJobs-have)/workJobsPerAddr+1)
	for i := range addrs {
		addrs[i] = randomAddr(t, consensus.RegTest)
	}
	for i := 0; i < workMaxJobs-have; i++ {
		if _, err := ws.Job(addrs[i/workJobsPerAddr], ""); err != nil {
			t.Fatal(err)
		}
	}
	ws.mu.Lock()
	full, victimKept := len(ws.jobs), ws.jobs[vj.ID] != nil
	ws.mu.Unlock()
	if full != workMaxJobs || !victimKept {
		t.Fatalf("at the cap: %d jobs, victim kept %v", full, victimKept)
	}
	// One more evicts exactly the oldest job (the victim's), not everything.
	last, _ := ws.Job(randomAddr(t, consensus.RegTest), "")
	ws.mu.Lock()
	n, victimKept, lastKept := len(ws.jobs), ws.jobs[vj.ID] != nil, ws.jobs[last.ID] != nil
	ws.mu.Unlock()
	if n != workMaxJobs || victimKept || !lastKept {
		t.Fatalf("past the cap: %d jobs (want %d), oldest kept %v, newest kept %v", n, workMaxJobs, victimKept, lastKept)
	}
	// Expiry drops old jobs, oldest first, and keeps the indexes consistent.
	ws.mu.Lock()
	ws.expireLocked(time.Now().Add(workJobMaxAge + time.Second))
	n, na := len(ws.jobs), len(ws.byAddr)
	ws.mu.Unlock()
	if n != 0 || na != 0 || ws.order.Len() != 0 {
		t.Fatalf("after expiry: %d jobs, %d addresses, %d in order", n, na, ws.order.Len())
	}
}

// H4: job ids are random 128-bit values, and a submission must name the
// address its job pays.
func TestWorkJobIDs(t *testing.T) {
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 0))
	addr := randomAddr(t, consensus.RegTest)
	seen := map[string]bool{}
	var prev []byte
	for i := 0; i < 500; i++ {
		j, err := ws.Job(addr, "")
		if err != nil {
			t.Fatal(err)
		}
		b, err := hex.DecodeString(j.ID)
		if err != nil || len(b) != 16 || seen[j.ID] {
			t.Fatalf("job id %q: not 32 hex digits or repeated", j.ID)
		}
		if prev != nil && b[0] == prev[0] && b[1] == prev[1] && b[2] == prev[2] && b[3] == prev[3] {
			t.Fatalf("consecutive job ids share a prefix: %x %x", prev, b)
		}
		seen[j.ID], prev = true, b
	}
	j, _ := ws.Job(addr, "")
	other := randomAddr(t, consensus.RegTest)
	if r := ws.Submit(workclient.Submit{JobID: j.ID, Nonce: 1, Address: other}); r.Accepted || r.Stale || !strings.Contains(r.Reason, "another address") {
		t.Fatalf("submission for someone else's job: %+v", r)
	}
	ws.mu.Lock()
	used := len(ws.jobs[j.ID].nonces)
	ws.mu.Unlock()
	if used != 0 {
		t.Fatal("a refused submission used up the job's budget")
	}
}

// H4: at most workMaxSubmits solutions are checked per job, and when every
// hashing slot is taken a submission is answered 429 without using up the
// job (the miner can send the same nonce again).
func TestWorkSubmitLimits(t *testing.T) {
	ws := newWorkServer(nil, consensus.MainNet, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.MainNet, 0))
	addr := randomAddr(t, consensus.MainNet)
	j, err := ws.Job(addr, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < workMaxSubmits; i++ {
		if r := ws.Submit(workclient.Submit{JobID: j.ID, Nonce: uint64(100 + i), Address: addr}); r.Reason != "hash above target" {
			t.Fatalf("submission %d: %+v", i, r)
		}
	}
	if r := ws.Submit(workclient.Submit{JobID: j.ID, Nonce: 999, Address: addr}); !r.Stale || !strings.Contains(r.Reason, "too many") {
		t.Fatalf("submission past the per-job budget: %+v", r)
	}

	// Every slot busy: the HTTP answer is 429 and the nonce stays unused.
	s := newServer(consensus.MainNet, nil, nil, ws, nil, "", "https://app.example")
	h := s.routes()
	old := workBusyWait
	workBusyWait = 100 * time.Millisecond
	defer func() { workBusyWait = old }()
	for i := 0; i < cap(ws.sem); i++ {
		ws.sem <- struct{}{}
	}
	j2, _ := ws.Job(addr, "")
	body := fmt.Sprintf(`{"job_id":%q,"nonce":7,"address":%q}`, j2.ID, addr)
	req := httptest.NewRequest("POST", "/api/work/submit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res workclient.Result
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusTooManyRequests || res.Reason != workReasonBusy || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("busy submission: HTTP %d %+v", rec.Code, res)
	}
	ws.mu.Lock()
	used := len(ws.jobs[j2.ID].nonces)
	ws.mu.Unlock()
	if used != 0 {
		t.Fatal("a busy answer used up the job's budget")
	}
	for i := 0; i < cap(ws.sem); i++ {
		<-ws.sem
	}
	if r := ws.Submit(workclient.Submit{JobID: j2.ID, Nonce: 7, Address: addr}); r.Reason != "hash above target" {
		t.Fatalf("the same nonce after the busy answer: %+v", r)
	}
}

// H4: 64 concurrent bogus submissions (64 jobs, so the per-job budget does
// not hide the cost) run at most NumCPU Argon2id evaluations at a time.
func TestWorkSubmitConcurrencyBounded(t *testing.T) {
	ws := newWorkServer(nil, consensus.MainNet, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.MainNet, 0))
	type sub struct{ id, addr string }
	var subs []sub
	for len(subs) < 64 {
		a := randomAddr(t, consensus.MainNet)
		for i := 0; i < workJobsPerAddr && len(subs) < 64; i++ {
			j, err := ws.Job(a, "")
			if err != nil {
				t.Fatal(err)
			}
			subs = append(subs, sub{j.ID, a})
		}
	}
	var running, most atomic.Int32
	orig := powHash
	powHash = func(b []byte) consensus.Hash {
		n := running.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		defer running.Add(-1)
		return orig(b)
	}
	defer func() { powHash = orig }()
	base := heapNow()
	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak {
				peak = m.HeapInuse
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	var mu sync.Mutex
	verdicts := map[string]int{}
	start := time.Now()
	for i, x := range subs {
		wg.Add(1)
		go func(i int, x sub) {
			defer wg.Done()
			r := ws.Submit(workclient.Submit{JobID: x.id, Nonce: uint64(i), Address: x.addr})
			mu.Lock()
			verdicts[r.Reason]++
			mu.Unlock()
		}(i, x)
	}
	wg.Wait()
	close(stop)
	<-done
	slots := cap(ws.sem)
	t.Logf("64 concurrent bogus submissions on %d CPUs: %s, verdicts %v, at most %d Argon2id at once, peak heap in use %.0f MB (base %.0f MB)",
		runtime.NumCPU(), time.Since(start).Round(time.Millisecond), verdicts, most.Load(), float64(peak)/1e6, float64(base)/1e6)
	if verdicts["hash above target"]+verdicts[workReasonBusy] != 64 {
		t.Fatalf("unexpected verdicts %v", verdicts)
	}
	if int(most.Load()) > slots {
		t.Fatalf("%d Argon2id evaluations ran at once; the limit is %d", most.Load(), slots)
	}
	// Each evaluation holds 8 MiB; the collector lets the heap reach about
	// twice what is live before it runs.
	if limit := base + 2*uint64(slots+2)*9<<20 + 32<<20; peak > limit {
		t.Fatalf("peak heap %d MB above the bound %d MB for %d slots", peak>>20, limit>>20, slots)
	}
}

// I2: a valid block on a side branch is reported stale, not found, and jobs
// whose parent is no longer the tip are dropped.
func TestWorkSideBranchStale(t *testing.T) {
	node := newTestNode(t)
	ws := newWorkServer(node.client, consensus.RegTest, 5, t.Logf)
	addr := randomAddr(t, consensus.RegTest)
	j, err := ws.Job(addr, "")
	if err != nil {
		t.Fatal(err)
	}
	j2, _ := ws.Job(addr, "")

	// Someone else's block at the same height arrives first.
	var tj miner.TemplateJSON
	raw, err := node.client.Call("getblocktemplate")
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &tj)
	other, err := miner.AssembleFromTemplate(consensus.RegTest, &tj, consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: make([]byte, 32)}, []byte("other"), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	for n := uint64(0); consensus.CheckProofOfWork(&other.Header, consensus.RegTest.PowLimit) != nil; n++ {
		other.Header.Nonce = n
	}
	if changed, err := node.ch.ProcessBlock(other, time.Now().Unix()); err != nil || !changed {
		t.Fatalf("competing block: changed %v err %v", changed, err)
	}

	// Our miner then solves its job: valid, but on the side branch.
	prefix, _ := hex.DecodeString(j.HeaderPrefix)
	hdr := make([]byte, consensus.HeaderSize)
	copy(hdr, prefix)
	target, _ := consensus.CompactToTarget(consensus.RegTest.PowLimitBits)
	var nonce uint64
	for ; ; nonce++ {
		for i := 0; i < 8; i++ {
			hdr[workclient.HeaderPrefixLen+i] = byte(nonce >> (8 * i))
		}
		if consensus.HashToBig(consensus.PowHash(hdr)).Cmp(target) <= 0 {
			break
		}
	}
	r := ws.Submit(workclient.Submit{JobID: j.ID, Nonce: nonce, Address: addr})
	if r.Accepted || !r.Stale || !strings.Contains(r.Reason, "stale") {
		t.Fatalf("side-branch block: %+v, want stale", r)
	}
	if found, _ := ws.status(); found != 0 {
		t.Fatalf("side-branch block counted as found (%d)", found)
	}
	if node.ch.Tip().Hash != other.Header.Hash() {
		t.Fatal("the tip moved to the side branch")
	}
	if verdictSideChain != rpc.SubmitSideChain {
		t.Fatalf("verdict constant %q differs from rpc's %q", verdictSideChain, rpc.SubmitSideChain)
	}

	// The tip poll drops the remaining job on the old parent; its miner is
	// told to fetch new work.
	stop := make(chan struct{})
	go ws.run(stop)
	defer close(stop)
	waitFor(t, "jobs on the old parent dropped", 3*workTick, func() bool {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		return ws.jobs[j2.ID] == nil
	})
	if r := ws.Submit(workclient.Submit{JobID: j2.ID, Nonce: 1, Address: addr}); !r.Stale {
		t.Fatalf("job on the old parent: %+v, want stale", r)
	}
	// New work builds on the new tip.
	j3, err := ws.Job(addr, "")
	if err != nil {
		t.Fatal(err)
	}
	p3, _ := hex.DecodeString(j3.HeaderPrefix)
	if h, _ := consensus.DecodeHeader(append(p3, make([]byte, 8)...)); h.PrevHash != other.Header.Hash() || j3.Height != 2 {
		t.Fatalf("new job builds on %s at %d", h.PrevHash, j3.Height)
	}
}

// I3: submissions only from the configured origins, only as JSON; the read
// API stays open to every origin.
func TestWorkSubmitCORS(t *testing.T) {
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 0))
	s := newServer(consensus.RegTest, nil, nil, ws, nil, "", "https://app.example, http://127.0.0.1:8370")
	h := s.routes()
	addr := randomAddr(t, consensus.RegTest)
	j, _ := ws.Job(addr, "")
	// Regtest's target is easy, so pick nonces whose hash misses it: a hit
	// would go on to submitblock, and this server has no node.
	miss := missNonces(t, ws, j.ID, 2)
	body := fmt.Sprintf(`{"job_id":%q,"nonce":%d,"address":%q}`, j.ID, miss[0], addr)
	do := func(method, path, origin, ctype, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if method == "OPTIONS" {
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Access-Control-Request-Headers", "content-type")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if r := do("OPTIONS", "/api/work/submit", "https://app.example", "", ""); r.Code != http.StatusNoContent || r.Header().Get("Access-Control-Allow-Origin") != "https://app.example" || !strings.Contains(r.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Fatalf("preflight from the app: %d %v", r.Code, r.Header())
	}
	if r := do("OPTIONS", "/api/work/submit", "https://evil.example", "", ""); r.Code != http.StatusForbidden || r.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight from another site: %d %v", r.Code, r.Header())
	}
	if r := do("POST", "/api/work/submit", "https://evil.example", "application/json", body); r.Code != http.StatusForbidden {
		t.Fatalf("submission from another site: %d", r.Code)
	}
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		if r := do("POST", "/api/work/submit", "https://app.example", ct, body); r.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("submission as %q: %d", ct, r.Code)
		}
	}
	if r := do("POST", "/api/work/submit", "http://127.0.0.1:8370", "application/json; charset=utf-8", body); r.Code != http.StatusOK || r.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:8370" || !strings.Contains(r.Header().Get("Vary"), "Origin") {
		t.Fatalf("submission from the app: %d %v %s", r.Code, r.Header(), r.Body)
	}
	// Non-browser miners send no Origin.
	if r := do("POST", "/api/work/submit", "", "application/json", fmt.Sprintf(`{"job_id":%q,"nonce":%d,"address":%q}`, j.ID, miss[1], addr)); r.Code != http.StatusOK {
		t.Fatalf("submission without Origin: %d", r.Code)
	}
	// Job fetching and the read API stay open to any page.
	if r := do("GET", "/api/work?address="+addr, "https://evil.example", "", ""); r.Code != http.StatusOK || r.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("GET /api/work from any origin: %d %v", r.Code, r.Header())
	}
}

// missNonces returns n nonces whose proof-of-work hash is above the job's
// target, so submitting them never reaches the node.
func missNonces(t testing.TB, ws *workServer, id string, n int) []uint64 {
	t.Helper()
	ws.mu.Lock()
	j := ws.jobs[id]
	ws.mu.Unlock()
	if j == nil {
		t.Fatalf("job %s not found", id)
	}
	var out []uint64
	for nonce := uint64(1); len(out) < n; nonce++ {
		if nonce > 10000 {
			t.Fatal("no nonce misses the regtest target")
		}
		hdr := j.header
		hdr.Nonce = nonce
		if consensus.HashToBig(powHash(hdr.Serialize())).Cmp(j.tmpl.target) > 0 {
			out = append(out, nonce)
		}
	}
	return out
}

// Client addresses for the per-IP limits.
func TestClientIPHeader(t *testing.T) {
	req := func(remote string, hdr map[string][]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for k, vs := range hdr {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
		return r
	}
	direct, xff, real := clientIPFrom(""), clientIPFrom("X-Forwarded-For"), clientIPFrom("x-real-ip")
	for _, tc := range []struct {
		name string
		f    func(*http.Request) string
		r    *http.Request
		want string
	}{
		{"peer address", direct, req("203.0.113.7:4444", nil), "203.0.113.7"},
		{"no header configured ignores a forged one", direct, req("203.0.113.7:4444", map[string][]string{"X-Forwarded-For": {"1.2.3.4"}}), "203.0.113.7"},
		{"proxy appended entry", xff, req("10.0.0.2:5000", map[string][]string{"X-Forwarded-For": {"1.2.3.4, 198.51.100.9"}}), "198.51.100.9"},
		{"several header lines", xff, req("10.0.0.2:5000", map[string][]string{"X-Forwarded-For": {"1.2.3.4", "5.6.7.8, 198.51.100.10"}}), "198.51.100.10"},
		{"entry with a port", xff, req("10.0.0.2:5000", map[string][]string{"X-Forwarded-For": {"198.51.100.11:7000"}}), "198.51.100.11"},
		{"malformed entry falls back to the peer", xff, req("10.0.0.2:5000", map[string][]string{"X-Forwarded-For": {"1.2.3.4, garbage"}}), "10.0.0.2"},
		{"missing header falls back to the peer", xff, req("10.0.0.2:5000", nil), "10.0.0.2"},
		{"IPv6 grouped by /64", xff, req("10.0.0.2:5000", map[string][]string{"X-Forwarded-For": {"2001:db8:1:2:aaaa::1"}}), "2001:db8:1:2::/64"},
		{"IPv4-mapped IPv6", direct, req("[::ffff:203.0.113.8]:1", nil), "203.0.113.8"},
		{"other header name", real, req("10.0.0.2:5000", map[string][]string{"X-Real-Ip": {"198.51.100.12"}}), "198.51.100.12"},
	} {
		if got := tc.f(tc.r); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	// The limit follows the header: one client is refused, another is not.
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 0))
	s := newServer(consensus.RegTest, nil, nil, ws, nil, "X-Forwarded-For", "")
	h := s.routes()
	addr := randomAddr(t, consensus.RegTest)
	get := func(client string) int {
		r := httptest.NewRequest("GET", "/api/work?address="+addr, nil)
		r.RemoteAddr = "10.0.0.2:5000"
		r.Header.Set("X-Forwarded-For", "6.6.6.6, "+client)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	codes := map[int]int{}
	for i := 0; i < int(limitWork[1])+5; i++ {
		codes[get("198.51.100.20")]++
	}
	if codes[http.StatusTooManyRequests] == 0 || codes[http.StatusOK] < int(limitWork[1])-1 {
		t.Fatalf("one client past its burst: %v", codes)
	}
	if c := get("198.51.100.21"); c != http.StatusOK {
		t.Fatalf("another client behind the same proxy: %d", c)
	}
}

// L4: node errors (host, port, DNS) never reach public pages or the API.
func TestErrorSanitising(t *testing.T) {
	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	dead := wallet.NewClient("http://127.0.0.1:1", "x") // nothing listens there
	ix := testIndexer(t)
	ix.client = dead
	if err := ix.syncOnce(); err == nil {
		t.Fatal("sync against a dead node succeeded")
	} else {
		ix.lastErr = "rpc: dial tcp node.railway.internal:19332: lookup node.railway.internal: no such host; " + err.Error()
	}
	ws := newWorkServer(dead, consensus.RegTest, 5, t.Logf)
	if _, err := ws.Job(randomAddr(t, consensus.RegTest), ""); err == nil || err.Error() != workNodeDownText {
		t.Fatalf("job without a node: %v", err)
	}
	s := newServer(consensus.RegTest, dead, ix, ws, tmpl, "", "")
	h := s.routes()
	leaks := []string{"railway", "19332", "127.0.0.1:1", "dial", "lookup", "refused", "connectex"}
	for _, path := range []string{"/api/stats", "/", "/mempool", "/api/assets", "/api/address/" + randomAddr(t, consensus.RegTest), "/api/work?address=" + randomAddr(t, consensus.RegTest)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		body, _ := io.ReadAll(rec.Body)
		for _, l := range leaks {
			if strings.Contains(strings.ToLower(string(body)), l) {
				t.Errorf("%s leaks %q: %.300s", path, l, body)
			}
		}
		if path == "/api/stats" {
			var st map[string]any
			json.Unmarshal(body, &st)
			if st["error"] != nodeUnavailable || st["workError"] != nodeUnavailable {
				t.Errorf("/api/stats errors: %v / %v", st["error"], st["workError"])
			}
		}
		if (path == "/" || path == "/mempool") && !strings.Contains(string(body), nodeUnavailable) {
			t.Errorf("%s does not say %q", path, nodeUnavailable)
		}
	}
}
