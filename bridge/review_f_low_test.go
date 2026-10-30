package bridge

// Review F (second pass, 2026-10-28): low findings.

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestWithdrawalsSameOriginOnly (fixed 2026-10-28): POST /api/withdrawals
// answered the CORS preflight with Access-Control-Allow-Origin: *, so any
// site could create withdrawals from its visitors' browsers. Only the bridge
// page itself creates them now: the preflight is refused and no answer of
// the endpoint carries Access-Control-Allow-Origin; the GET endpoints stay
// open to every origin.
func TestWithdrawalsSameOriginOnly(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	e.issueDeposit(e.chains[0].Tokens[0].Address, usd(100, 6))
	base := e.urls[0]
	do := func(method, path, body string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	pre := do(http.MethodOptions, "/api/withdrawals", "", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type"})
	if pre.StatusCode < 400 || pre.Header.Get("Access-Control-Allow-Origin") != "" || pre.Header.Get("Access-Control-Allow-Methods") != "" || pre.Header.Get("Access-Control-Allow-Headers") != "" {
		t.Fatalf("preflight: %d %v", pre.StatusCode, pre.Header)
	}
	body := fmt.Sprintf(`{"chain_id":%d,"token":"usdc","to":"0x00000000000000000000000000000000000000b7","amount":"10"}`, e.chains[0].ID)
	ok := do(http.MethodPost, "/api/withdrawals", body, map[string]string{"Content-Type": "application/json"})
	if ok.StatusCode != 200 || ok.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("POST: %d %v", ok.StatusCode, ok.Header)
	}
	for _, b := range []string{`{"chain_id":999,"token":"usdc","to":"0x00000000000000000000000000000000000000b7","amount":"10"}`, `{`} {
		if r := do(http.MethodPost, "/api/withdrawals", b, map[string]string{"Content-Type": "application/json"}); r.StatusCode != 400 || r.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("POST refused: %d %v", r.StatusCode, r.Header)
		}
	}
	if r := do(http.MethodPost, "/api/withdrawals", body, map[string]string{"Content-Type": "text/plain"}); r.StatusCode != http.StatusUnsupportedMediaType || r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("POST as text/plain: %d %v", r.StatusCode, r.Header)
	}
	for _, path := range []string{"/api/status", "/api/withdrawal/ZZZZ-ZZZZ", "/api/deposits"} {
		if r := do(http.MethodGet, path, "", nil); r.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("GET %s: %v", path, r.Header)
		}
	}
}

// TestMaxDepositIDReverseSeek: the highest deposit id on record, read with
// one reverse seek per prefix, is what a walk of every key gives, for every
// chain, whatever else is on record.
func TestMaxDepositIDReverseSeek(t *testing.T) {
	t.Parallel()
	st, err := openState(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rng := rand.New(rand.NewSource(3))
	chains := []uint64{1, 97, 31337, 11155111}
	prefixes := []string{"i", pfxSettled, pfxSignedDep, pfxReplaced, pfxArchive + "2/i", pfxArchive + "3/i"}
	var ops []kvOp
	for i := 0; i < 4000; i++ {
		c := chains[rng.Intn(len(chains))]
		id := uint64(rng.Int63n(1 << 40))
		if rng.Intn(50) == 0 {
			id = ^uint64(0) >> uint(rng.Intn(8)) // large ids too
		}
		p := prefixes[rng.Intn(len(prefixes))]
		k := append(append([]byte(p), be64(c)...), be64(id)...)
		if p == pfxSignedDep || p == pfxReplaced {
			k = append(k, []byte("tx"+strconv.Itoa(i))...)
		}
		ops = append(ops, kvOp{key: k, value: []byte("{}")})
	}
	// Neighbours that must not count: other prefixes, chain 0.
	ops = append(ops, kvOp{key: append(append([]byte(pfxCheckpoint), be64(31337)...), be64(^uint64(0))...), value: []byte("x")})
	if err := st.write(ops, false); err != nil {
		t.Fatal(err)
	}
	walk := func(chain uint64) uint64 {
		var max uint64
		for _, p := range prefixes {
			prefix := append([]byte(p), be64(chain)...)
			st.eachKey(prefix, func(k []byte) {
				if id := binary.BigEndian.Uint64(k[len(prefix) : len(prefix)+8]); id > max {
					max = id
				}
			})
		}
		return max
	}
	for _, c := range append(chains, 5) {
		if got, want := st.MaxDepositID(c), walk(c); got != want {
			t.Fatalf("chain %d: %d, a walk of every key gives %d", c, got, want)
		}
	}
}
