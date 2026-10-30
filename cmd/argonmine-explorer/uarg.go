package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/ui"
)

// Which asset is uARG is the bridge's answer, never the symbol: anyone can
// issue an asset called UARG. The explorer takes the id from -uarg-asset, or
// from the bridge's /api/status (-bridge-url) at start and every 10 minutes,
// keeping the last good answer when the bridge does not respond. Only that
// id is named "uARG" and shown with the logo; other assets with a reserved
// symbol are "UARG (unverified)" with their issuer key.

const bridgeAssetEvery = 10 * time.Minute

// isHexID reports whether s is a 64-character lower-case hex id.
func isHexID(s string) bool { return isAssetID(s) }

// fetchBridgeAsset reads the uARG asset id from a bridge's /api/status.
func fetchBridgeAsset(hc *http.Client, bridgeURL string) (string, error) {
	resp, err := hc.Get(strings.TrimRight(bridgeURL, "/") + "/api/status")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var st struct {
		Asset string `json:"asset"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return "", err
	}
	id := strings.ToLower(strings.TrimSpace(st.Asset))
	if !isHexID(id) {
		return "", fmt.Errorf("no asset id in the bridge status")
	}
	return id, nil
}

// followBridgeAsset keeps ui's canonical UARG id current from the bridge.
func followBridgeAsset(bridgeURL string, logf func(string, ...any)) {
	hc := &http.Client{Timeout: 15 * time.Second}
	last := ""
	for {
		id, err := fetchBridgeAsset(hc, bridgeURL)
		switch {
		case err != nil:
			logf("uarg: bridge %s: %v (keeping %q)", bridgeURL, err, last)
		case id != last:
			ui.SetCanonicalAsset("UARG", id)
			logf("uarg: the bridge's uARG is %s", id)
			last = id
		}
		wait := bridgeAssetEvery
		if last == "" {
			wait = time.Minute // not known yet: ask again sooner
		}
		time.Sleep(wait)
	}
}

// followSwapAsset keeps ui's canonical WARG id current from the swap
// service's /api/info (docs/SWAP.md): wARG is the asset its issuer key
// issues, recomputed here from that key rather than taken as given.
func followSwapAsset(swapURL string, logf func(string, ...any)) {
	hc := &http.Client{Timeout: 15 * time.Second}
	last := ""
	for {
		id, err := fetchSwapAsset(hc, swapURL)
		switch {
		case err != nil:
			logf("warg: swap service %s: %v (keeping %q)", swapURL, err, last)
		case id != last:
			ui.SetCanonicalAsset("WARG", id)
			logf("warg: the swap service's wARG is %s", id)
			last = id
		}
		wait := bridgeAssetEvery
		if last == "" {
			wait = time.Minute
		}
		time.Sleep(wait)
	}
}

func fetchSwapAsset(hc *http.Client, swapURL string) (string, error) {
	resp, err := hc.Get(strings.TrimRight(swapURL, "/") + "/api/info")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var info struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return "", err
	}
	b, err := hex.DecodeString(strings.TrimSpace(info.Issuer))
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("no issuer key in the swap service's info")
	}
	var issuer [32]byte
	copy(issuer[:], b)
	return consensus.AssetID(issuer, "WARG").String(), nil
}

// uargLogo is the logo URL of the genuine uARG, served by ui.Handler.
var uargLogo = ui.AssetPath("tokens/uarg.svg")

// wargLogo is the logo URL of the genuine wARG.
var wargLogo = ui.AssetPath("tokens/warg.svg")

// assetLabel names an asset for the pages: the display symbol (see
// ui.DisplaySymbol), whether it is an unverified look-alike, and the logo
// URL, which only the canonical uARG has.
func assetLabel(symbol, id string) (display string, unverified bool, logo string) {
	display = ui.DisplaySymbol(symbol, id)
	unverified = ui.Unverified(symbol, id)
	if symbol == "UARG" && !unverified {
		logo = uargLogo
	}
	if symbol == "WARG" && !unverified && ui.CanonicalAsset("WARG") != "" {
		logo = wargLogo
	}
	return display, unverified, logo
}
