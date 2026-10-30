package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstallableApp(t *testing.T) {
	get := func(h http.HandlerFunc, path string) (*http.Response, string) {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", path, nil))
		resp := w.Result()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, body := get(serveManifest, "/manifest.webmanifest")
	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Fatalf("manifest type %q", ct)
	}
	var m struct {
		Name, ShortName, StartURL, Display, Scope string
		Icons                                     []struct{ Src, Sizes, Type, Purpose string }
	}
	if err := json.Unmarshal([]byte(strings.NewReplacer(`"short_name"`, `"ShortName"`, `"start_url"`, `"StartURL"`).Replace(body)), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name == "" || m.ShortName == "" || m.StartURL != "/" || m.Display != "standalone" || m.Scope != "/" {
		t.Fatalf("manifest %+v", m)
	}
	// Chrome installs an app with a 192 and a 512 icon; Android masks a maskable one.
	have := map[string]bool{}
	for _, i := range m.Icons {
		have[i.Sizes+" "+i.Purpose] = true
		f := strings.TrimPrefix(i.Src, "/static/")
		if _, err := siteFS.ReadFile("site/static/" + f); err != nil {
			t.Fatalf("icon %s: %v", i.Src, err)
		}
	}
	for _, k := range []string{"192x192 any", "512x512 any", "512x512 maskable"} {
		if !have[k] {
			t.Fatalf("no %s icon: %v", k, have)
		}
	}

	resp, body = get(serveServiceWorker, "/sw.js")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(body, "addEventListener('fetch'") {
		t.Fatalf("service worker: %s %q", resp.Header.Get("Content-Type"), body[:60])
	}
	// It must only fall back to the offline page for page loads, never cache API answers.
	if !strings.Contains(body, "e.request.mode !== 'navigate'") || strings.Contains(body, "cache.put") {
		t.Fatal("the service worker caches more than the offline page")
	}

	resp, body = get(serveOffline, "/offline")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, "offline") {
		t.Fatalf("offline page: %q", body)
	}
}
