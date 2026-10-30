// Package ui is the shared design system for every web interface in the
// project (mining app, explorer, pool page): one stylesheet, one script with
// formatting, icons and chart helpers, and the brand constants. Each server
// mounts Handler at /assets/.
package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

//go:embed theme.css
var CSS string

//go:embed ui.js
var JS string

// EVMJS connects an EVM wallet for the apps' Swap in (evm.js): wallet
// discovery, chain switching, token reads, approve and the vault deposit.
//
//go:embed evm.js
var EVMJS string

// Tokens holds the coin logos (ARG, uARG, wARG, and the bridge's USDC and
// USDT) and the EVM chain logos (chain-<name>.svg), served at
// /assets/<BuildID>/tokens/; UI.token(symbol) and UI.chain(chainID) in ui.js
// draw them. The USDC, USDT and chain logos are their owners' marks, used to
// name the coin or chain.
//
//go:embed tokens/*.svg
var Tokens embed.FS

// Brand constants. These are the only places the product name appears;
// change them once when the project is named (docs/BRAND.md).
const (
	Brand   = "Argonmine"
	Tagline = "Proof-of-work money anyone can mine"
	Coin    = "ARG"
	Version = "1.1.0"
)

// Mark is the logotype symbol: a faceted ore nugget, 24×24 viewBox,
// stroke-based so it inherits the text colour.
const Mark = `<svg class="mark" viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" aria-hidden="true"><path d="M12 2.5 20.5 8v8L12 21.5 3.5 16V8L12 2.5Z"/><path d="M12 2.5v19M3.5 8l8.5 4.5L20.5 8M12 12.5 20.5 16M12 12.5 3.5 16"/></svg>`

// BuildID names the asset folder after the assets' content, so a changed
// stylesheet, script or logo gets a new URL. The files are cached for a year
// as immutable, so a folder named only after the release would keep returning
// visitors on old copies of ui.js and theme.css after every change.
var BuildID = func() string {
	h := sha256.New()
	h.Write([]byte(CSS))
	h.Write([]byte(JS))
	h.Write([]byte(EVMJS))
	if entries, err := Tokens.ReadDir("tokens"); err == nil {
		for _, e := range entries {
			b, _ := Tokens.ReadFile("tokens/" + e.Name())
			h.Write([]byte(e.Name()))
			h.Write(b)
		}
	}
	return Version + "-" + hex.EncodeToString(h.Sum(nil))[:10]
}()

// Handler serves the shared assets: under /assets/<BuildID>/ with long-lived
// caching, and under the old /assets/<Version>/ path without it, so pages
// cached before content hashing still load the current files.
func Handler() http.Handler {
	mux := http.NewServeMux()
	serve := func(name, ctype, body string) {
		for _, dir := range []string{BuildID, Version} {
			cache := "public, max-age=31536000, immutable"
			if dir == Version {
				cache = "no-cache"
			}
			mux.HandleFunc("/assets/"+dir+"/"+name, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ctype)
				w.Header().Set("Cache-Control", cache)
				w.Write([]byte(body))
			})
		}
	}
	serve("theme.css", "text/css; charset=utf-8", CSS)
	serve("ui.js", "application/javascript; charset=utf-8", JS)
	serve("evm.js", "application/javascript; charset=utf-8", EVMJS)
	if entries, err := Tokens.ReadDir("tokens"); err == nil {
		for _, e := range entries {
			if b, err := Tokens.ReadFile("tokens/" + e.Name()); err == nil {
				serve("tokens/"+e.Name(), "image/svg+xml", string(b))
			}
		}
	}
	return mux
}

// AssetPath returns the versioned URL of a shared asset.
func AssetPath(name string) string { return "/assets/" + BuildID + "/" + name }

// Head returns the common <head> fragment: charset, viewport, theme-colour,
// the stylesheet and the inline theme bootstrap (applies the saved theme
// before first paint so there is no flash).
func Head(title string) string {
	var b strings.Builder
	b.WriteString(`<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<meta name="color-scheme" content="light dark"><meta name="theme-color" content="#0d0d0d">`)
	b.WriteString(`<title>` + title + `</title>`)
	b.WriteString(`<link rel="icon" href="data:image/svg+xml,` + faviconData + `">`)
	b.WriteString(`<link rel="stylesheet" href="` + AssetPath("theme.css") + `">`)
	b.WriteString(`<script>try{var t=localStorage.getItem('theme');if(t)document.documentElement.dataset.theme=t}catch(e){}</script>`)
	return b.String()
}

const faviconData = `%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Cpath fill='%231a1a19' d='M20.547 0c4.009 0 6.014 0 7.545 0.78a7.158 7.158 0 0 1 3.128 3.128c0.78 1.531 0.78 3.536 0.78 7.545L32 20.547c0 4.009 0 6.014 -0.78 7.545a7.158 7.158 0 0 1 -3.128 3.128c-1.531 0.78 -3.536 0.78 -7.545 0.78L11.453 32c-4.009 0 -6.014 0 -7.545 -0.78a7.158 7.158 0 0 1 -3.128 -3.128c-0.78 -1.531 -0.78 -3.536 -0.78 -7.545L0 11.453c0 -4.009 0 -6.014 0.78 -7.545a7.158 7.158 0 0 1 3.128 -3.128c1.531 -0.78 3.536 -0.78 7.545 -0.78Z'/%3E%3Cpath fill='%23e0a63a' fill-rule='evenodd' d='M15.455 4.162A1 1 0 0 1 16.545 4.162L26.545 10.662A1 1 0 0 1 27 11.5L27 20.5A1 1 0 0 1 26.545 21.338L16.545 27.838A1 1 0 0 1 15.455 27.838L5.455 21.338A1 1 0 0 1 5 20.5L5 11.5A1 1 0 0 1 5.455 10.662ZM17 14.882L23.991 11.387L17 6.843ZM18.439 16.399L25 19.023L25 13.118ZM17 17.977L17 25.157L23.838 20.712ZM15 17.977L8.162 20.712L15 25.157ZM13.561 16.399L7 13.118L7 19.023ZM15 14.882L15 6.843L8.009 11.387Z'/%3E%3C/svg%3E`

// Any key can issue an asset with any symbol, so a symbol proves nothing:
// "UARG" on chain may be the bridge's uARG or a look-alike. The house
// spellings (uARG, wARG) and the coin's own ticker therefore belong to one
// asset id each, registered with SetCanonicalAsset by the program that knows
// it (from the bridge's status or a flag).
var canonical struct {
	sync.RWMutex
	ids map[string]string // symbol -> asset id (64 lower-case hex)
}

// reservedSymbols imitate the chain's own coins. An asset using one is shown
// with its house spelling only when it is the registered asset for it.
var reservedSymbols = map[string]string{"UARG": "uARG", "WARG": "wARG", "ARG": "ARG"}

// SetCanonicalAsset records id as the one genuine asset for symbol (the
// bridge's uARG for "UARG"); an empty id forgets it.
func SetCanonicalAsset(symbol, id string) {
	symbol, id = strings.ToUpper(strings.TrimSpace(symbol)), strings.ToLower(strings.TrimSpace(id))
	canonical.Lock()
	defer canonical.Unlock()
	if canonical.ids == nil {
		canonical.ids = map[string]string{}
	}
	if id == "" {
		delete(canonical.ids, symbol)
		return
	}
	canonical.ids[symbol] = id
}

// CanonicalAsset returns the asset id registered for symbol, or "".
func CanonicalAsset(symbol string) string {
	canonical.RLock()
	defer canonical.RUnlock()
	return canonical.ids[strings.ToUpper(symbol)]
}

// Unverified reports whether the asset id uses a symbol that imitates the
// chain's coins (UARG, WARG, ARG) without being the registered asset for it.
func Unverified(symbol, id string) bool {
	if _, ok := reservedSymbols[symbol]; !ok {
		return false
	}
	c := CanonicalAsset(symbol)
	return c == "" || !strings.EqualFold(c, id)
}

// DisplaySymbol names an asset for the interface from its consensus symbol
// (1-8 characters of A-Z and 0-9) and its asset id:
//
//   - the registered asset of a reserved symbol gets the house spelling
//     ("uARG" for UARG);
//   - any other asset with a reserved symbol is "UARG (unverified)": callers
//     add the issuer key and show no logo;
//   - every other symbol is shown as it is on chain.
//
// Called with the symbol alone, it names an asset the caller configured
// itself (the bridge naming the asset it issues) and applies the house
// spelling without a check. Never pass a symbol read from the chain that way.
func DisplaySymbol(sym string, id ...string) string {
	house, reserved := reservedSymbols[sym]
	if !reserved {
		return sym
	}
	if len(id) == 0 {
		return house
	}
	if Unverified(sym, id[0]) {
		return sym + " (unverified)"
	}
	return house
}
