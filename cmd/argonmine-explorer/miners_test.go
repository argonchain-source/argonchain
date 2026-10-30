package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeoCountry(t *testing.T) {
	g := newGeoDB()
	g.load(t.Logf)
	for ip, want := range map[string]string{
		"1.0.1.1":        "CN", // the second range of the file
		"8.8.8.8":        "US",
		"81.2.69.160":    "GB",
		"2001:4860::1":   "US",
		"2a00:1450::5":   "IE",
		"::ffff:8.8.8.8": "US", // IPv4 in IPv6 form, as some proxies send it
		"10.1.2.3":       "",   // private
		"127.0.0.1":      "",
		"not an ip":      "",
	} {
		if got := g.country(ip); got != want {
			t.Errorf("country(%s) = %q, want %q", ip, got, want)
		}
	}
}

func TestMinerBook(t *testing.T) {
	g := newGeoDB()
	g.load(t.Logf)
	path := filepath.Join(t.TempDir(), "miners.json")
	b := newMinerBook(path, g)
	now := time.Now()
	b.note("arg1alice", "8.8.8.8", now.Add(-5*time.Minute)) // online, US
	b.note("arg1bob", "1.0.1.1", now.Add(-3*time.Hour))     // today, CN
	b.note("arg1carol", "8.8.4.4", now.Add(-2*time.Minute)) // online, US
	b.note("arg1dave", "10.0.0.7", now.Add(-time.Minute))   // online, no country
	b.note("arg1old", "8.8.8.8", now.Add(-30*time.Hour))    // older than a day
	found := []foundBlock{
		{"arg1node", now.Add(-20 * time.Minute).Unix()},  // a node miner: online, no country
		{"arg1alice", now.Add(-40 * time.Minute).Unix()}, // already counted
		{"arg1ancient", now.Add(-48 * time.Hour).Unix()}, // too old
	}
	v := b.compute(now, found)
	if v.Online != 4 || v.Day != 5 || v.Unlocated != 2 {
		t.Fatalf("online %d day %d unlocated %d, want 4 5 2", v.Online, v.Day, v.Unlocated)
	}
	if len(v.Countries) != 2 || v.Countries[0] != (countryCount{"US", 2, 2}) || v.Countries[1] != (countryCount{"CN", 1, 0}) {
		t.Fatalf("countries %+v", v.Countries)
	}

	// Saved without addresses or IP addresses; a restart keeps the counts.
	b.save(t.Logf)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"arg1", "8.8.", "1.0.1", "10.0.0"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the saved book contains %q: %s", leak, raw)
		}
	}
	b2 := newMinerBook(path, g)
	if v2 := b2.compute(now, found); v2.Day != v.Day || v2.Online != v.Online {
		t.Fatalf("after reload: %+v, want %+v", v2, v)
	}

	// The API answers with the view, readable from any site.
	s := &server{miners: b}
	rec := httptest.NewRecorder()
	s.apiMiners(rec, httptest.NewRequest("GET", "/api/miners", nil))
	var got minersView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("api: %v %s", err, rec.Body.String())
	}
	if got.Day != 4 || !strings.Contains(got.Source, "NRO") { // no index here: no block finders
		t.Fatalf("api view %+v", got)
	}
}
