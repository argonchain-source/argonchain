package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The swap service is served on the app's own origin (/swap/api/*). Its
// answers must stay inert whatever it sends (security review C, 2026-10-28):
// a compromised service could otherwise serve HTML with an inline script on
// app.argonmine.com, or headers that act on the whole origin
// (Clear-Site-Data wipes the sealed wallet; Service-Worker-Allowed lets a
// script it serves control every app page).
func TestSwapProxyAnswersAreInert(t *testing.T) {
	const evil = `<!doctype html><script>/* would run on the app origin */</script>`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Clear-Site-Data", `"storage"`)
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Set-Cookie", "x=1; Path=/")
		if r.URL.Path == "/api/quote" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"q1"}`)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, evil)
	}))
	defer up.Close()
	s := testSite(t, Config{Explorer: "https://explorer.example.com", SwapUpstream: up.URL})
	h := s.handler()

	check := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Sec-Fetch-Mode", "navigate") // a user who follows a link to it
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		got := rec.Header()
		if got.Get("Content-Type") != "application/json" || got.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: content type %q", path, got.Get("Content-Type"))
		}
		for _, k := range []string{"Clear-Site-Data", "Service-Worker-Allowed", "Set-Cookie"} {
			if v := got.Values(k); len(v) > 0 {
				t.Fatalf("%s: the service's %s reached the browser: %q", path, k, v)
			}
		}
		sandboxed := false
		for _, v := range got.Values("Content-Security-Policy") {
			if strings.Contains(v, "sandbox") && strings.Contains(v, "default-src 'none'") {
				sandboxed = true
			}
		}
		if !sandboxed {
			t.Fatalf("%s: no sandboxing policy: %q", path, got.Values("Content-Security-Policy"))
		}
		return rec
	}
	if rec := check("/swap/api/info"); rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "<script>") {
		t.Fatalf("HTML from the service was relayed: %d %q", rec.Code, rec.Body.String())
	}
	if rec := check("/swap/api/quote"); rec.Code != http.StatusOK || rec.Body.String() != `{"id":"q1"}` {
		t.Fatalf("a JSON answer was not relayed as sent: %d %q", rec.Code, rec.Body.String())
	}
}
