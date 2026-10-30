package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Security review G (2026-10-28): ModifyResponse makes the swap service's
// final answer inert, but httputil.ReverseProxy forwards 1xx interim
// responses itself (ClientTrace.Got1xxResponse), with the service's own
// headers and without ModifyResponse, and then clears the header map, so
// the final answer also loses the site's security headers (X-Frame-Options,
// HSTS, Referrer-Policy) that securityHeaders set. Trailers are forwarded
// as the service sent them too. This reads the raw bytes the browser gets.
func TestReviewG_SwapProxyInterimAndTrailers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Link", "</static/x.js>; rel=preload; as=script")
		h.Set("Set-Cookie", "evil=1; Path=/")
		h.Set("Clear-Site-Data", `"storage"`)
		w.WriteHeader(http.StatusEarlyHints)
		for k := range h {
			delete(h, k)
		}
		h.Set("Content-Type", "application/json")
		h.Set("Trailer", "Set-Cookie2")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"pairs":[]}`)
		w.(http.Flusher).Flush()
		h.Set("Set-Cookie2", "evil=2")
	}))
	defer up.Close()
	s := testSite(t, Config{Explorer: "https://explorer.example.com", SwapUpstream: up.URL})
	front := httptest.NewServer(s.handler())
	defer front.Close()

	c, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "GET /swap/api/info HTTP/1.1\r\nHost: app.example\r\nX-Forwarded-Proto: https\r\nConnection: close\r\n\r\n")
	raw, _ := io.ReadAll(bufio.NewReader(c))
	text := string(raw)
	t.Logf("what the browser gets:\n%s", text)

	interim, final, found := strings.Cut(text, "\r\n\r\nHTTP/1.1 200")
	if !found {
		interim, final = "", text // no interim answer reached the browser at all
	}
	var bad []string
	if strings.HasPrefix(interim, "HTTP/1.1 103") {
		for _, k := range []string{"Set-Cookie: evil=1", "Clear-Site-Data", "Link: </static/x.js>"} {
			if strings.Contains(interim, k) {
				bad = append(bad, "103 carries the service's "+k)
			}
		}
	}
	if !strings.Contains(final, "X-Frame-Options: DENY") {
		bad = append(bad, "the final answer lost X-Frame-Options")
	}
	if !strings.Contains(final, "Strict-Transport-Security") {
		bad = append(bad, "the final answer lost HSTS")
	}
	if strings.Contains(final, "evil=2") {
		bad = append(bad, "the service's trailer reached the browser")
	}
	if len(bad) > 0 {
		t.Errorf("upstream headers bypass ModifyResponse: %s", strings.Join(bad, "; "))
	}
}

// Security review G (2026-10-28): a redirect from the swap service never
// reaches the browser as one (Location is not kept), a compressed answer
// loses its Content-Encoding (the bytes stay as sent, so the page fails to
// parse them rather than reading anything else), and a large answer is cut.
func TestReviewG_SwapProxyRedirectsAndEncodings(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info":
			w.Header().Set("Location", "javascript:alert(document.domain)")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusFound)
			io.WriteString(w, `{}`)
		case "/api/quote":
			w.Header().Set("Location", "https://evil.example/")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusMovedPermanently)
		case "/api/swap":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			io.WriteString(w, strings.Repeat(" ", swapMaxBody+10)+`{"txid":"x"}`)
		}
	}))
	defer up.Close()
	s := testSite(t, Config{Explorer: "https://explorer.example.com", SwapUpstream: up.URL})
	h := s.handler()
	for _, path := range []string{"/swap/api/info", "/swap/api/quote", "/swap/api/swap"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s: the service's redirect reached the browser: %d %s", path, rec.Code, loc)
		}
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("%s: Content-Encoding %q kept", path, ce)
		}
		if rec.Body.Len() > swapMaxBody+100 {
			t.Errorf("%s: %d bytes relayed", path, rec.Body.Len())
		}
		t.Logf("%s: %d, %d bytes, %s", path, rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
}
