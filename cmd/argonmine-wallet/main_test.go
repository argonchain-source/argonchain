package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"argonmine/ui"
	"argonmine/wallet"
)

// The CLI picks uARG by the bridge's asset id only; look-alikes called UARG
// (or WARG, ARG) are never chosen by symbol.
func TestPickAssetUARG(t *testing.T) {
	const real = "aa00000000000000000000000000000000000000000000000000000000000001"
	const fake = "bb00000000000000000000000000000000000000000000000000000000000002"
	const gold = "cc00000000000000000000000000000000000000000000000000000000000003"
	held := []wallet.AssetBalance{{Asset: fake, Symbol: "UARG"}, {Asset: real, Symbol: "UARG"}, {Asset: gold, Symbol: "GOLD"}, {Asset: "dd" + real[2:], Symbol: "WARG"}}
	for _, tc := range []struct {
		q, genuine, want, err string
	}{
		{"UARG", real, real, ""},
		{"uarg", real, real, ""},
		{"UARG", "", "", "Pass -uarg-asset"},
		{"UARG", "ee" + real[2:], "", "look-alikes"},
		{"GOLD", real, gold, ""},
		{"WARG", real, "", "not picked by symbol"},
		{"ARG", real, "", "not picked by symbol"},
		{fake, real, fake, ""}, // an explicit id is the user's choice
		{"SILVER", real, "", "no asset"},
	} {
		b, err := pickAsset(held, tc.q, tc.genuine)
		switch {
		case tc.err == "" && (err != nil || b.Asset != tc.want):
			t.Errorf("pickAsset(%q, %.4s): %s %v, want %s", tc.q, tc.genuine, b.Asset, err, tc.want)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("pickAsset(%q, %.4s): %v, want an error with %q", tc.q, tc.genuine, err, tc.err)
		}
	}
	ui.SetCanonicalAsset("UARG", real)
	defer ui.SetCanonicalAsset("UARG", "")
	if got := assetLabel(held[0]); got != "UARG (unverified)" {
		t.Errorf("look-alike label %q", got)
	}
	if got := assetLabel(held[1]); got != "uARG" {
		t.Errorf("genuine label %q", got)
	}
}

func TestFetchBridgeAsset(t *testing.T) {
	const id = "aa00000000000000000000000000000000000000000000000000000000000001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"asset":"` + strings.ToUpper(id) + `","asset_symbol":"UARG"}`))
	}))
	defer srv.Close()
	if got, err := fetchBridgeAsset(srv.URL + "/"); err != nil || got != id {
		t.Fatalf("fetchBridgeAsset: %q %v", got, err)
	}
	if _, err := fetchBridgeAsset(srv.URL + "/nothing"); err == nil {
		t.Fatal("a bridge without a status answered")
	}
}
