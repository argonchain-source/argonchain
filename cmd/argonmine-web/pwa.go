package main

import (
	"encoding/json"
	"net/http"

	"argonmine/ui"
)

// The app installs on a phone or computer from the browser ("Install app"
// in Chrome, "Add to Home Screen" in Safari): /manifest.webmanifest names it
// and its icons and opens it full screen, and /sw.js is the service worker
// the browser asks for. The worker caches only /offline, the page shown when
// there is no network, and passes everything else straight through, so an
// installed app always loads the current version.

func serveManifest(w http.ResponseWriter, r *http.Request) {
	icon := func(src, size, purpose string) map[string]string {
		return map[string]string{"src": src, "sizes": size, "type": "image/png", "purpose": purpose}
	}
	m := map[string]any{
		"id":               "/",
		"name":             ui.Brand,
		"short_name":       ui.Brand,
		"description":      "Mine " + ui.Coin + " in your browser. Send, receive and swap.",
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"background_color": "#0d0d0d",
		"theme_color":      "#0d0d0d",
		"categories":       []string{"finance"},
		"icons": []map[string]string{
			icon("/static/icon-192.png", "192x192", "any"),
			icon("/static/icon-512.png", "512x512", "any"),
			icon("/static/icon-maskable-512.png", "512x512", "maskable"),
		},
		"shortcuts": []map[string]any{
			{"name": "Mine", "url": "/mine", "icons": []map[string]string{icon("/static/icon-192.png", "192x192", "any")}},
			{"name": "Wallet", "url": "/wallet", "icons": []map[string]string{icon("/static/icon-192.png", "192x192", "any")}},
		},
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(m)
}

const serviceWorker = `// Argonmine app: makes the app installable and shows a short offline page
// when there is no network. Nothing else is cached.
const CACHE = 'argonmine-offline-v1';
self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.add(new Request('/offline', { cache: 'reload' }))).then(() => self.skipWaiting()));
});
self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((ks) => Promise.all(ks.filter((k) => k !== CACHE).map((k) => caches.delete(k)))).then(() => self.clients.claim()));
});
self.addEventListener('fetch', (e) => {
  if (e.request.mode !== 'navigate') return;
  e.respondWith(fetch(e.request).catch(() => caches.match('/offline')));
});
`

func serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(serviceWorker))
}

func serveOffline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(`<!doctype html><html lang="en"><head>` + ui.Head(ui.Brand) + `
<style>
  body { min-height: 100vh; min-height: 100dvh; display: grid; place-items: center; padding: 24px; margin: 0; }
  .off { max-width: 320px; text-align: center; display: flex; flex-direction: column; align-items: center; gap: 12px; }
  .off svg.mark { width: 40px; height: 40px; color: var(--accent); }
  .off h1 { font-size: 20px; margin: 4px 0 0; }
  .off p { margin: 0; color: var(--ink-2); line-height: 1.5; }
</style></head><body>
<main class="off">` + ui.Mark + `
  <h1>You're offline</h1>
  <p>Connect to the internet to see your wallet and mine. Your coins are safe on the chain.</p>
  <button class="btn btn-primary" type="button" onclick="location.reload()">Try again</button>
</main></body></html>`))
}
