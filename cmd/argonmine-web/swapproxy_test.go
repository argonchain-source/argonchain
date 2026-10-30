package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The swap service answers at /swap/api/ on this site: three paths only,
// with the client's address as the last X-Forwarded-For entry.
func TestSwapProxy(t *testing.T) {
	var gotPath, gotXFF, gotCookie string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotXFF, gotCookie = r.URL.Path, r.Header.Get("X-Forwarded-For"), r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	h := testSite(t, Config{Explorer: "http://127.0.0.1:8380", SwapUpstream: up.URL}).handler()

	req := httptest.NewRequest(http.MethodGet, "/swap/api/info", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	req.Header.Set("Cookie", "a=b")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) || gotPath != "/api/info" || gotXFF != "203.0.113.9" || gotCookie != "" {
		t.Fatalf("info: %d %q path %q xff %q cookie %q", rec.Code, rec.Body.String(), gotPath, gotXFF, gotCookie)
	}
	req = httptest.NewRequest(http.MethodPost, "/swap/api/quote", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || gotPath != "/api/quote" || gotXFF != "192.0.2.1" {
		t.Fatalf("quote: %d path %q xff %q", rec.Code, gotPath, gotXFF)
	}
	for _, p := range []string{"/swap/api/other", "/swap/api/../admin", "/swap/api/info/x"} {
		gotPath = ""
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == 200 || gotPath != "" {
			t.Fatalf("%s: %d, forwarded to %q", p, rec.Code, gotPath)
		}
	}
	// Off without -swap-upstream.
	off := testSite(t, Config{Explorer: "http://127.0.0.1:8380"}).handler()
	rec = httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/swap/api/info", nil))
	if strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatal("forwarded without -swap-upstream")
	}
}
