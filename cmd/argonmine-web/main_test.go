package main

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"argonmine/bridge"
	"argonmine/ui"
)

func testSite(t *testing.T, cfg Config) *site {
	t.Helper()
	cfg.RepoDir = "../.."
	if cfg.HRP == "" {
		cfg.HRP = "targ"
	}
	s, err := newSite(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// directive returns the value of one CSP directive ("" when absent).
func directive(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if v, ok := strings.CutPrefix(d, name+" "); ok {
			return v
		}
	}
	return ""
}

func TestContentSecurityPolicy(t *testing.T) {
	csp, err := contentSecurityPolicy("https://Explorer.example.com/", "https://bridge.example.com:8443/api")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self' 'unsafe-inline' 'wasm-unsafe-eval'",
		"worker-src":      "'self'",
		"style-src":       "'self' 'unsafe-inline'",
		"img-src":         "'self' data:",
		"connect-src":     "'self' https://explorer.example.com https://bridge.example.com:8443",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
	} {
		if got := directive(csp, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// No bridge: only this server and the explorer.
	csp, err = contentSecurityPolicy("http://127.0.0.1:8380", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := directive(csp, "connect-src"); got != "'self' http://127.0.0.1:8380" {
		t.Errorf("connect-src without a bridge = %q", got)
	}
	// Flags that are not plain http(s) origins are refused rather than
	// written into the policy.
	for _, bad := range []string{"javascript:alert(1)", "ftp://x.example", "https://a.example; script-src *", "https://user:pw@a.example", "//a.example", "https://"} {
		if _, err := contentSecurityPolicy(bad, ""); err == nil {
			t.Errorf("explorer %q accepted", bad)
		}
		if _, err := contentSecurityPolicy("https://e.example", bad); err == nil {
			t.Errorf("bridge %q accepted", bad)
		}
	}
	if _, err := newSite(Config{Explorer: "not a url", HRP: "targ", RepoDir: "../.."}); err == nil {
		t.Error("newSite accepted an explorer that is not a URL")
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	cfgs := map[string]Config{
		"one host": {Explorer: "http://127.0.0.1:8380", Bridge: "https://bridge-production-0393.up.railway.app"},
		"app host": {Explorer: "https://explorer.example.com", Bridge: "https://bridge.example.com", App: "https://app.example.com", Site: "https://example.com"},
	}
	for cname, cfg := range cfgs {
		s := testSite(t, cfg)
		h := s.handler()
		paths := []string{"/", "/mine", "/wallet", "/settings", "/docs", "/docs/overview", "/security", "/no-such-page",
			"/static/miner-worker.js", "/static/wasm_exec.js", "/static/site.css", "/qr.svg?d=targ1qqq",
			ui.AssetPath("ui.js"), ui.AssetPath("tokens/uarg.svg")}
		for _, host := range []string{"example.com", "app.example.com"} {
			for _, p := range paths {
				for _, proto := range []string{"", "https"} {
					req := httptest.NewRequest("GET", p, nil)
					req.Host = host
					if proto != "" {
						req.Header.Set("X-Forwarded-Proto", proto)
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					got := rec.Header()
					where := cname + " " + host + p + " " + proto
					if got.Get("Content-Security-Policy") != s.csp {
						t.Errorf("%s: CSP = %q", where, got.Get("Content-Security-Policy"))
					}
					for k, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "strict-origin-when-cross-origin"} {
						if got.Get(k) != want {
							t.Errorf("%s: %s = %q, want %q", where, k, got.Get(k), want)
						}
					}
					hsts := got.Get("Strict-Transport-Security")
					if proto == "https" && hsts != "max-age=31536000" || proto == "" && hsts != "" {
						t.Errorf("%s: Strict-Transport-Security = %q", where, hsts)
					}
					if rec.Code >= 500 {
						t.Errorf("%s: status %d", where, rec.Code)
					}
				}
			}
		}
		for _, o := range []string{cfg.Explorer, cfg.Bridge} {
			if !strings.Contains(directive(s.csp, "connect-src"), o) {
				t.Errorf("%s: connect-src %q lacks %s", cname, directive(s.csp, "connect-src"), o)
			}
		}
	}
}

// -app and -site on the same host serve the app and the docs there instead
// of redirecting between themselves.
func TestAppAndSiteOnOneHost(t *testing.T) {
	h := testSite(t, Config{Explorer: "http://127.0.0.1:8380", App: "http://127.0.0.1:8373", Site: "http://127.0.0.1:8373/"}).handler()
	for _, p := range []string{"/", "/mine", "/wallet", "/docs/overview", "/security"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Host = "127.0.0.1:8373"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, Location %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// The worker that runs the miner must be served as JavaScript and the
// miner as WebAssembly, or nosniff and the CSP stop them.
func TestMinerContentTypes(t *testing.T) {
	h := testSite(t, Config{Explorer: "http://127.0.0.1:8380"}).handler()
	for p, want := range map[string]string{"/static/miner-worker.js": "javascript", "/static/wasm_exec.js": "javascript", "/static/miner.wasm": "application/wasm"} {
		if p != "/static/miner-worker.js" {
			// Built by deploy/railway/Dockerfile, not checked in.
			if _, err := os.Stat(filepath.Join("site", "static", path.Base(p))); err != nil {
				t.Logf("%s: not built here, skipped", p)
				continue
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), want) {
			t.Errorf("%s: status %d, Content-Type %q, want %s", p, rec.Code, rec.Header().Get("Content-Type"), want)
		}
	}
}

// The wallet's Swap screen shows the deposit address of another chain as a
// QR code from this server (the page may load images only from here).
func TestQRDrawsDepositAddresses(t *testing.T) {
	h := testSite(t, Config{Explorer: "http://127.0.0.1:8380", Bridge: "https://bridge.example.com"}).handler()
	for d, want := range map[string]int{
		"0x198c2D42C71E8046f34ECA9a0f5C81B9f3db2afB": http.StatusOK,
		"targ1qqq": http.StatusOK,
		"0x198c2D42C71E8046f34ECA9a0f5C81B9f3db2afBB": http.StatusBadRequest,
		"javascript:alert(1)":                         http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/qr.svg?d="+d, nil))
		if rec.Code != want {
			t.Errorf("/qr.svg?d=%s: %d, want %d", d, rec.Code, want)
		}
		if want == http.StatusOK && (!strings.HasPrefix(rec.Body.String(), "<svg") || rec.Header().Get("Content-Type") != "image/svg+xml") {
			t.Errorf("/qr.svg?d=%s: not an SVG", d)
		}
	}
}

// The wallet has Receive, Send, Swap and Mine; Send is a plain transfer, the
// interface never says burn, redeem or mint, and the bridge appears only as
// a navigation link (the sidebar and the phone's More sheet), never inside a
// wallet flow.
func TestWalletScreens(t *testing.T) {
	h := testSite(t, Config{Explorer: "http://127.0.0.1:8380", Bridge: "https://bridge.example.com"}).handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/wallet", nil))
	body := rec.Body.String()
	for _, id := range []string{`id="acctChip"`, `id="actReceive"`, `id="actSend"`, `id="actSwap"`, `id="actMine"`, `id="flSend"`, `id="flSwap"`, `id="flRecv"`, `id="swFlip"`, `id="sdTo"`} {
		if !strings.Contains(body, id) {
			t.Errorf("wallet page lacks %s", id)
		}
	}
	// The bridge's site is linked from the navigation only.
	if n := strings.Count(body, `href="https://bridge.example.com`); n != 2 {
		t.Errorf("the app links to the bridge site %d times, want 2 (sidebar and More)", n)
	}
	if i, j := strings.Index(body, `id="flSend"`), strings.Index(body, "<script src="); i > 0 && j > i && strings.Contains(body[i:j], "bridge.example.com") {
		t.Error("a wallet flow links to the bridge site")
	}
	// Visible words only: the markup between tags, not ids or script.
	text := body[:strings.Index(body, "<script src=")]
	for _, w := range []string{">Redeem", "Burn", "burn ", ">Mint", "Bridge out", "wARG</span>"} {
		if strings.Contains(text, w) {
			t.Errorf("wallet page says %q", w)
		}
	}
}

// ui/evm.js builds the vault and token calls by hand; its selectors must be
// the bridge's.
func TestEVMSelectors(t *testing.T) {
	for name, sel := range map[string][4]byte{
		"approve":   bridge.SelApprove,
		"balanceOf": bridge.Selector("balanceOf(address)"),
		"allowance": bridge.Selector("allowance(address,address)"),
		"deposit":   bridge.SelDeposit,
	} {
		if want := name + ": '" + hex.EncodeToString(sel[:]) + "'"; !strings.Contains(ui.EVMJS, want) {
			t.Errorf("evm.js lacks %s", want)
		}
	}
	h := testSite(t, Config{Explorer: "http://127.0.0.1:8380", Bridge: "https://bridge.example.com"}).handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", ui.AssetPath("evm.js"), nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "window.EVM") {
		t.Errorf("evm.js not served: %d", rec.Code)
	}
}

// One service, four names: the website's front page, the app, the docs on
// their own host (old /docs links follow them), and www.
func TestLandingAppDocsHosts(t *testing.T) {
	h := testSite(t, Config{Explorer: "https://explorer.example.com", App: "https://app.example.com", Site: "https://example.com", Docs: "https://docs.example.com"}).handler()
	get := func(host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, c := range []struct {
		host, path string
		code       int
		where      string // Location, or a string the page contains
	}{
		{"example.com", "/", 200, "anyone can mine"},
		{"example.com", "/mine", 302, "https://app.example.com/mine"},
		{"example.com", "/docs/faq", 301, "https://docs.example.com/faq"},
		{"example.com", "/security", 301, "https://docs.example.com/security"},
		{"www.example.com", "/docs/faq?x=1", 301, "https://example.com/docs/faq?x=1"},
		{"docs.example.com", "/", 302, "/overview"},
		{"docs.example.com", "/faq", 200, "https://docs.example.com/getting-started"},
		{"docs.example.com", "/docs/mining", 301, "/mining"},
		{"docs.example.com", "/nope", 404, ""},
		{"app.example.com", "/wallet", 200, "wallet.sealed"},
		{"app.example.com", "/docs", 301, "https://docs.example.com/"},
	} {
		rec := get(c.host, c.path)
		if rec.Code != c.code {
			t.Errorf("%s%s: status %d, want %d (Location %q)", c.host, c.path, rec.Code, c.code, rec.Header().Get("Location"))
			continue
		}
		if c.code >= 300 && c.code < 400 {
			if got := rec.Header().Get("Location"); got != c.where {
				t.Errorf("%s%s: Location %q, want %q", c.host, c.path, got, c.where)
			}
		} else if c.where != "" && !strings.Contains(rec.Body.String(), c.where) {
			t.Errorf("%s%s: page does not contain %q", c.host, c.path, c.where)
		}
	}
}
