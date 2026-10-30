package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"argonmine/consensus"
	"argonmine/wallet"
)

// /api/health is what an uptime monitor polls: with no node to follow (or
// nothing indexed, or no recent block) it answers 503 with the reasons.
func TestHealthReportsDown(t *testing.T) {
	tmpl, err := newTemplates()
	if err != nil {
		t.Fatal(err)
	}
	dead := wallet.NewClient("http://127.0.0.1:1", "x")
	ix := testIndexer(t)
	ix.client = dead
	ix.syncOnce()
	s := newServer(consensus.RegTest, dead, ix, newWorkServer(dead, consensus.RegTest, 5, t.Logf), tmpl, "", "")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	var out struct {
		Status   string   `json:"status"`
		Problems []string `json:"problems"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable || out.Status != "down" || len(out.Problems) == 0 {
		t.Fatalf("health without a node: %d %+v", rec.Code, out)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health may be cached: %q", rec.Header().Get("Cache-Control"))
	}
}
