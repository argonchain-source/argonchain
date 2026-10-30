package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte("{}"))
	}))
	for _, tc := range []struct {
		name  string
		proto string
		hsts  bool
	}{{"plain http", "", false}, {"behind an https proxy", "https", true}, {"proxy chain", "https, http", true}, {"proxy over http", "http", false}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/stats", nil)
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			got := rec.Header()
			for k, want := range map[string]string{
				"Content-Security-Policy":     explorerCSP,
				"X-Frame-Options":             "DENY",
				"X-Content-Type-Options":      "nosniff",
				"Referrer-Policy":             "strict-origin-when-cross-origin",
				"Access-Control-Allow-Origin": "*",
			} {
				if got.Get(k) != want {
					t.Errorf("%s = %q, want %q", k, got.Get(k), want)
				}
			}
			if hsts := got.Get("Strict-Transport-Security"); (hsts != "") != tc.hsts || (tc.hsts && hsts != "max-age=31536000") {
				t.Errorf("Strict-Transport-Security = %q, want it only over https", hsts)
			}
		})
	}
	for _, d := range []string{"frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'", "connect-src 'self'", "img-src 'self' data:"} {
		if !strings.Contains(explorerCSP, d) {
			t.Errorf("CSP lacks %q", d)
		}
	}
}
