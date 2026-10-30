package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadsProxy(t *testing.T) {
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" cookie="+r.Header.Get("Cookie"))
		w.Header().Set("Set-Cookie", "x=1")
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path != "/argonmine-1.0.0-linux-amd64.zip" && r.URL.Path != "/SHA256SUMS" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "PK-bytes")
	}))
	defer up.Close()
	h, err := downloadsProxy(up.URL)
	if err != nil || h == nil {
		t.Fatal(err)
	}
	do := func(method, path string) *http.Response {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Cookie", "session=secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Result()
	}

	resp := do("GET", "/downloads/argonmine-1.0.0-linux-amd64.zip")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "PK-bytes" {
		t.Fatalf("zip: %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Fatalf("content type %q", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="argonmine-1.0.0-linux-amd64.zip"` {
		t.Fatalf("disposition %q", got)
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", resp.Header)
	}
	if !strings.HasSuffix(seen[0], "cookie=") {
		t.Fatalf("the cookie reached the file service: %s", seen[0])
	}

	if resp := do("GET", "/downloads/SHA256SUMS"); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("sums: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp := do("GET", "/downloads/missing.zip"); resp.StatusCode != 404 || resp.Header.Get("Content-Disposition") != "" {
		t.Fatalf("missing: %d %v", resp.StatusCode, resp.Header)
	}
	n := len(seen)
	for _, p := range []string{"/downloads/", "/downloads/../main.go", "/downloads/a/b.zip", "/downloads/.env", "/downloads/x%2F..%2Fy"} {
		if resp := do("GET", p); resp.StatusCode != 404 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp := do("POST", "/downloads/SHA256SUMS"); resp.StatusCode != 405 {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
	if len(seen) != n {
		t.Fatalf("refused requests reached the file service: %v", seen[n:])
	}
}
