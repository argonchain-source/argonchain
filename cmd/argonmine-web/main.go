// Command argonmine-web serves the browser app (WebAssembly solo miner with
// in-browser wallet creation; work, balances and history come from the
// explorer) and the rendered documentation. There is no landing page: "/" is the app, or a
// redirect to it when -app puts the app on its own host such as
// app.argonmine.com. It can also export everything as static files.
package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"argonmine/ui"
)

//go:embed site/templates.html site/site.css site/static/*
var siteFS embed.FS

// Config is the site configuration (flags).
type Config struct {
	Listen   string
	Domain   string
	Explorer string
	Bridge   string
	App      string // public URL of the browser mining app; empty serves it at /mine
	Site     string // public URL of the website, used for links from the app host
	Docs     string // public URL of the documentation on its own host; empty serves it at /docs
	HRP      string
	RepoDir  string
	Export   string
	// SwapUpstream is the swap service on the private network, reached by
	// wallets at /swap/api/ on this site (swapproxy.go); empty: off.
	SwapUpstream string
	// HoldARG, when set, is the message the app shows instead of letting a
	// wallet send ARG or wARG or swap them (uARG keeps moving); empty: all on.
	HoldARG string
	// DownloadsUpstream is the file service holding the node release
	// archives, served at /downloads/ (downloads.go); empty: off.
	DownloadsUpstream string
}

type docEntry struct {
	Slug  string
	Title string
	Group string
	Path  string // relative to RepoDir
	HTML  template.HTML
	Prev  *docEntry
	Next  *docEntry
}

// docGroup is one heading of the documentation navigation.
type docGroup struct {
	Name string
	Docs []docEntry
}

type site struct {
	cfg      Config
	appHost  string // host[:port] of cfg.App, matched against the request Host
	staticV  string // content hash of the embedded miner, so browsers never keep a stale copy
	csp      string // Content-Security-Policy of every response
	tmpl     *template.Template
	md       goldmark.Markdown
	groups   []docGroup
	docs     []docEntry
	docsHost string // host of -docs, which gets the documentation
	docsBase string // where documentation links point
	siteHost string // host of -site; www.<it> redirects to it
}

type page struct {
	Title     string
	Head      template.HTML
	Mark      template.HTML
	Brand     string
	Coin      string
	Tagline   string
	Version   string
	UIJS      string
	EVMJS     string // EVM wallet helpers for Swap in
	Cfg       Config
	Docs      []docEntry
	DocGroups []docGroup
	Active    string
	AppMode   bool   // rendering on the app host (no website chrome in the header)
	MineURL   string // where "start mining" links go
	DocsURL   string // the documentation's base: https://docs.example.com, or <site>/docs
	SiteURL   string // the website's front page
	Assets    string // /assets/<build>/: token logos and shared files
	StaticV   string // cache-busting version for the miner files
	Data      any
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8370", "HTTP listen address")
	flag.StringVar(&cfg.Domain, "domain", "argonmine.com", "canonical domain shown on the site")
	flag.StringVar(&cfg.Explorer, "explorer", "http://127.0.0.1:8380", "explorer URL")
	flag.StringVar(&cfg.Bridge, "bridge", "", "bridge URL (empty hides the link)")
	flag.StringVar(&cfg.App, "app", "", "public URL of the browser mining app, e.g. https://app.argonmine.com; requests for that host get the app, and the website links to it (empty: app at /mine)")
	flag.StringVar(&cfg.Site, "site", "", "public URL of this website, e.g. https://argonmine.com; required with -app so the app can link back")
	flag.StringVar(&cfg.Docs, "docs", "", "public URL of the documentation on its own host, e.g. https://docs.argonmine.com; requests for that host get the docs at /<page>, and /docs/... elsewhere redirects there (empty: docs at /docs)")
	flag.StringVar(&cfg.HRP, "hrp", "arg", "address prefix of the network the site is for")
	flag.StringVar(&cfg.RepoDir, "repo", ".", "repository root (for README.md, docs/, SECURITY.md)")
	flag.StringVar(&cfg.Export, "export", "", "write the site as static files into this directory and exit")
	flag.StringVar(&cfg.SwapUpstream, "swap-upstream", "", "swap service URL on the private network, served at /swap/api/ (e.g. http://swap.railway.internal:8410); empty: off")
	flag.StringVar(&cfg.HoldARG, "hold-arg", "", "message shown by the app instead of sending or swapping ARG and wARG (uARG stays); empty: everything on")
	flag.StringVar(&cfg.DownloadsUpstream, "downloads-upstream", "", "file service with the node release archives on the private network, served at /downloads/ (e.g. http://downloads.railway.internal:8080); empty: off")
	flag.Parse()

	s, err := newSite(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.Export != "" {
		if err := s.export(cfg.Export); err != nil {
			log.Fatal(err)
		}
		log.Printf("site exported to %s", cfg.Export)
		return
	}
	if s.cfg.App != "" {
		log.Printf("%s website on http://%s (site %s, app %s)", ui.Brand, cfg.Listen, s.cfg.Site, s.cfg.App)
	} else {
		log.Printf("%s website on http://%s", ui.Brand, cfg.Listen)
	}
	log.Fatal(http.ListenAndServe(cfg.Listen, s.handler()))
}

// newSite checks the configuration, parses the templates and renders the
// documentation.
func newSite(cfg Config) (*site, error) {
	s := &site{cfg: cfg}
	// One version for every file under /static/ (miner, worker, stylesheet):
	// a change to any of them gives all of them a new URL.
	{
		h := sha256.New()
		for _, name := range []string{"site/static/miner.wasm", "site/static/miner-worker.js", "site/site.css"} {
			if b, err := siteFS.ReadFile(name); err == nil {
				h.Write(b)
			}
		}
		s.staticV = fmt.Sprintf("%x", h.Sum(nil)[:6])
	}
	if cfg.App != "" {
		u, err := url.Parse(cfg.App)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("-app must be an http(s) URL with a host, got %q", cfg.App)
		}
		if cfg.Site == "" {
			return nil, errors.New("-site is required when -app is set")
		}
		cfg.App = strings.TrimRight(cfg.App, "/")
		cfg.Site = strings.TrimRight(cfg.Site, "/")
		if su, err := url.Parse(cfg.Site); err == nil && strings.EqualFold(su.Host, u.Host) {
			// App and website on one host: serve both there, as without
			// -app (otherwise each would redirect to the other forever).
			cfg.App = ""
		} else {
			s.appHost = strings.ToLower(u.Host)
		}
		s.cfg = cfg
	}
	if cfg.Docs != "" {
		u, err := url.Parse(cfg.Docs)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("-docs must be an http(s) URL with a host, got %q", cfg.Docs)
		}
		s.cfg.Docs = strings.TrimRight(cfg.Docs, "/")
		s.docsHost = strings.ToLower(u.Host)
		s.docsBase = s.cfg.Docs
	} else {
		s.docsBase = s.cfg.Site + "/docs"
	}
	if su, err := url.Parse(s.cfg.Site); err == nil && su.Host != "" {
		s.siteHost = strings.ToLower(su.Host)
	}
	s.md = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	funcs := template.FuncMap{
		"lower": strings.ToLower,
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(siteFS, "site/templates.html")
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	s.tmpl = tmpl
	csp, err := contentSecurityPolicy(cfg.Explorer, cfg.Bridge)
	if err != nil {
		return nil, err
	}
	s.csp = csp
	s.loadDocs()
	return s, nil
}

// serveDocsHost answers the documentation's own host: the pages at
// /<slug>, the first one at /, and the old /docs/<slug> addresses
// redirected to the new ones.
func (s *site) serveDocsHost(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/docs" || strings.HasPrefix(path, "/docs/") {
		http.Redirect(w, r, "/"+strings.Trim(strings.TrimPrefix(path, "/docs"), "/"), http.StatusMovedPermanently)
		return
	}
	slug := strings.Trim(path, "/")
	if slug == "" {
		if len(s.docs) == 0 {
			s.notFound(w)
			return
		}
		http.Redirect(w, r, "/"+s.docs[0].Slug, http.StatusFound)
		return
	}
	d := s.doc(slug)
	if d == nil {
		s.notFound(w)
		return
	}
	s.render(w, "doc", "docs", d.Title, d)
}

// handler routes every request of the website and the app, with the
// security headers on each response.
func (s *site) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/assets/", ui.Handler())
	static, _ := fs.Sub(siteFS, "site/static")
	mux.Handle("/static/", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))
	mux.HandleFunc("/static/site.css", func(w http.ResponseWriter, r *http.Request) {
		b, _ := siteFS.ReadFile("site/site.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/qr.svg", serveQR)
	mux.HandleFunc("/manifest.webmanifest", serveManifest)
	mux.HandleFunc("/sw.js", serveServiceWorker)
	mux.HandleFunc("/offline", serveOffline)
	if s.cfg.SwapUpstream != "" {
		if h, err := swapProxy(s.cfg.SwapUpstream); err == nil && h != nil {
			mux.Handle("/swap/api/", h)
		} else {
			log.Printf("swap: -swap-upstream %q is not a URL; /swap/api/ is off", s.cfg.SwapUpstream)
		}
	}
	if s.cfg.DownloadsUpstream != "" {
		if h, err := downloadsProxy(s.cfg.DownloadsUpstream); err == nil && h != nil {
			mux.Handle("/downloads/", h)
		} else {
			log.Printf("downloads: -downloads-upstream %q is not a URL; /downloads/ is off", s.cfg.DownloadsUpstream)
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		// www.<site> is the site.
		if s.siteHost != "" && host == "www."+s.siteHost {
			http.Redirect(w, r, s.cfg.Site+r.URL.RequestURI(), http.StatusMovedPermanently)
			return
		}
		if s.docsHost != "" && host == s.docsHost {
			s.serveDocsHost(w, r)
			return
		}
		// The docs on their own host: old /docs links follow them there.
		if s.docsHost != "" && (r.URL.Path == "/docs" || strings.HasPrefix(r.URL.Path, "/docs/") || r.URL.Path == "/security") {
			slug := strings.Trim(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/docs"), "/"), "/")
			if r.URL.Path == "/security" {
				slug = "security"
			}
			http.Redirect(w, r, s.cfg.Docs+"/"+slug, http.StatusMovedPermanently)
			return
		}
		if s.appHost != "" && strings.EqualFold(r.Host, s.appHost) {
			// The app host serves only the mining app; everything else goes
			// back to the website.
			if isAppPath(r.URL.Path) {
				s.render(w, "app", "app", "App", nil)
			} else {
				http.Redirect(w, r, s.cfg.Site+r.URL.Path, http.StatusFound)
			}
			return
		}
		switch {
		case r.URL.Path == "/" && s.cfg.App != "":
			// The website's front page; the app has its own host.
			s.render(w, "landing", "landing", "The coin anyone can mine", nil)
		case isAppPath(r.URL.Path):
			// Without an app host, the app is the front page.
			if s.cfg.App != "" {
				http.Redirect(w, r, s.cfg.App+strings.TrimSuffix(r.URL.Path, "/"), http.StatusFound)
				return
			}
			s.render(w, "app", "app", "App", nil)
		case r.URL.Path == "/docs" || r.URL.Path == "/docs/":
			if len(s.docs) > 0 {
				http.Redirect(w, r, "/docs/"+s.docs[0].Slug, http.StatusFound)
				return
			}
			s.notFound(w)
		case strings.HasPrefix(r.URL.Path, "/docs/"):
			slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/docs/"), "/")
			d := s.doc(slug)
			if d == nil {
				s.notFound(w)
				return
			}
			s.render(w, "doc", "docs", d.Title, d)
		case r.URL.Path == "/security":
			if d := s.doc("security"); d != nil {
				s.render(w, "doc", "security", d.Title, d)
				return
			}
			s.notFound(w)
		default:
			s.notFound(w)
		}
	})
	return securityHeaders(s.csp, mux)
}

// originRe is what a CSP source may be built from: scheme, host and port,
// nothing that could end the directive or add another source.
var originRe = regexp.MustCompile(`^https?://([a-z0-9.-]+|\[[0-9a-f:.]+\])(:[0-9]{1,5})?$`)

// origin returns scheme://host[:port] of an http(s) URL.
func origin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("%q is not an http(s) URL with a host", raw)
	}
	o := strings.ToLower(u.Scheme + "://" + u.Host)
	if !originRe.MatchString(o) {
		return "", fmt.Errorf("%q has a host that cannot be named in a content security policy", raw)
	}
	return o, nil
}

// contentSecurityPolicy is the policy of every page of the website and the
// app: scripts, styles and images from this server only (plus data: images
// for the favicon); the WebAssembly miner compiled in a worker from this
// server; network requests to this server, the explorer (work, balances,
// history) and the bridge (status, hand-off codes, transfer progress).
// Inline scripts and styles stay allowed because the pages are built with
// them; nothing the explorer or the bridge returns is put in markup
// unescaped. No framing, no <base>, no plugins.
func contentSecurityPolicy(explorer, bridge string) (string, error) {
	connect := []string{"'self'"}
	for _, f := range []struct{ flag, url string }{{"-explorer", explorer}, {"-bridge", bridge}} {
		if f.url == "" {
			continue
		}
		o, err := origin(f.url)
		if err != nil {
			return "", fmt.Errorf("%s: %w", f.flag, err)
		}
		if !slices.Contains(connect, o) {
			connect = append(connect, o)
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'",
		"worker-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"connect-src " + strings.Join(connect, " "),
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; "), nil
}

// securityHeaders sets the browser protections on every response: the
// content security policy, no framing, no MIME sniffing, only the origin as
// the referrer of cross-site requests, and HSTS when the request came over
// HTTPS (directly or, on Railway, through the TLS-terminating proxy).
func securityHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if isHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// isHTTPS reports whether the browser reached us over HTTPS: a TLS
// connection, or X-Forwarded-Proto: https from the proxy in front (its
// first value is the hop the browser used).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	p, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(p), "https")
}

// serveQR renders an address as an SVG QR code for the Receive panel.
func serveQR(w http.ResponseWriter, r *http.Request) {
	ui.ServeQR(w, r.URL.Query().Get("d"))
}

// isAppPath reports whether path is one of the app's screens. The app uses
// real paths (/mine, /wallet, ...) and switches screens client-side.
func isAppPath(path string) bool {
	switch strings.TrimSuffix(path, "/") {
	case "", "/mine", "/wallet", "/dashboard", "/history", "/settings":
		return true
	}
	return false
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if strings.HasSuffix(r.URL.Path, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		}
		h.ServeHTTP(w, r)
	})
}

func (s *site) render(w http.ResponseWriter, name, active, title string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	if err := s.renderTo(&buf, name, active, title, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Write(buf.Bytes())
}

func (s *site) renderTo(buf *bytes.Buffer, name, active, title string, data any) error {
	full := title + " · " + ui.Brand
	if name == "app" {
		full = ui.Brand + " app"
	}
	mineURL := "/mine"
	if s.cfg.App != "" {
		mineURL = s.cfg.App
	}
	return s.tmpl.ExecuteTemplate(buf, name, page{
		Title: title, Head: template.HTML(ui.Head(template.HTMLEscapeString(full))), Mark: template.HTML(ui.Mark), Brand: ui.Brand, Coin: ui.Coin,
		Tagline: ui.Tagline, Version: ui.Version, UIJS: ui.AssetPath("ui.js"), EVMJS: ui.AssetPath("evm.js"), Cfg: s.cfg, Docs: s.docs, DocGroups: s.groups, Active: active,
		AppMode: name == "app", MineURL: mineURL, StaticV: s.staticV, Data: data,
		DocsURL: s.docsBase, SiteURL: s.siteURL(), Assets: ui.AssetPath(""),
	})
}

func (s *site) notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, "notfound", "", "Not found", nil)
}

// ---- docs ----

// docSources lists the documentation in reading order. The title is what the
// navigation shows; the page's own first heading is its headline.
var docSources = []struct{ slug, path, group, title string }{
	{"overview", "docs/OVERVIEW.md", "Start here", "Overview"},
	{"getting-started", "docs/GETTING-STARTED.md", "Start here", "Getting started"},
	{"mining", "docs/MINING.md", "Start here", "Mining"},
	{"ways-to-mine", "docs/WAYS-TO-MINE.md", "Start here", "Ways to mine"},
	{"fees", "docs/FEES.md", "Start here", "Fees"},
	{"faq", "docs/FAQ.md", "Start here", "FAQ"},
	{"spec", "docs/SPEC.md", "Protocol", "Consensus specification"},
	{"tokens", "docs/TOKENS.md", "Protocol", "Assets: ARG, wARG, uARG"},
	{"bridge", "docs/BRIDGE.md", "Protocol", "Bridge"},
	{"swaps", "docs/SWAP.md", "Protocol", "Swaps"},
	{"node", "docs/NODE.md", "Operate", "Run a node"},
	{"seed-nodes", "deploy/README.md", "Operate", "Seed nodes"},
	{"launch", "docs/LAUNCH.md", "Operate", "Launch runbook"},
	{"roadmap", "docs/ROADMAP.md", "Project", "Roadmap"},
	{"security-review", "docs/SECURITY-REVIEW.md", "Project", "Security review"},
	{"security", "SECURITY.md", "Project", "Security policy"},
}

// linkRewrites maps the repository paths the documents link to onto the
// page that renders them (its slug, under the docs base).
var linkRewrites = map[string]string{
	"docs/OVERVIEW.md": "overview", "docs/GETTING-STARTED.md": "getting-started", "docs/MINING.md": "mining", "docs/WAYS-TO-MINE.md": "ways-to-mine",
	"docs/NODE.md": "node", "docs/FAQ.md": "faq", "docs/FEES.md": "fees", "docs/SWAP.md": "swaps",
	"docs/SPEC.md": "spec", "docs/ROADMAP.md": "roadmap", "docs/BRIDGE.md": "bridge", "docs/LAUNCH.md": "launch",
	"docs/SECURITY-REVIEW.md": "security-review", "docs/TOKENS.md": "tokens", "docs/BRAND.md": "overview", "deploy/README.md": "seed-nodes",
	"SECURITY.md": "security", "README.md": "overview",
}

var headingRe = regexp.MustCompile(`(?m)^#\s+(.+)$`)

func (s *site) loadDocs() {
	s.docs = nil
	s.groups = nil
	for _, src := range docSources {
		raw, err := os.ReadFile(filepath.Join(s.cfg.RepoDir, filepath.FromSlash(src.path)))
		if err != nil {
			continue
		}
		title := src.title
		if title == "" {
			title = src.slug
			if m := headingRe.FindSubmatch(raw); m != nil {
				title = strings.TrimSpace(string(m[1]))
			}
		}
		text := string(raw)
		for from, slug := range linkRewrites {
			to := s.docsBase + "/" + slug
			for _, open := range []string{"](", "](../"} {
				text = strings.ReplaceAll(text, open+from+")", "]("+to+")")
				text = strings.ReplaceAll(text, open+from+"#", "]("+to+"#")
			}
		}
		var out bytes.Buffer
		if err := s.md.Convert([]byte(text), &out); err != nil {
			continue
		}
		s.docs = append(s.docs, docEntry{Slug: src.slug, Title: title, Group: src.group, Path: src.path, HTML: template.HTML(out.String())})
	}
	for i := range s.docs {
		if i > 0 {
			s.docs[i].Prev = &s.docs[i-1]
		}
		if i+1 < len(s.docs) {
			s.docs[i].Next = &s.docs[i+1]
		}
	}
	for _, d := range s.docs {
		if n := len(s.groups); n > 0 && s.groups[n-1].Name == d.Group {
			s.groups[n-1].Docs = append(s.groups[n-1].Docs, d)
		} else {
			s.groups = append(s.groups, docGroup{Name: d.Group, Docs: []docEntry{d}})
		}
	}
}

// siteURL is the website's front page: the site's own URL when it has one
// apart from the app, else the app.
func (s *site) siteURL() string {
	if s.cfg.App != "" && s.cfg.Site != "" {
		return s.cfg.Site + "/"
	}
	if s.cfg.App != "" {
		return s.cfg.App
	}
	return "/mine"
}

func (s *site) doc(slug string) *docEntry {
	for i := range s.docs {
		if s.docs[i].Slug == slug {
			return &s.docs[i]
		}
	}
	return nil
}

// ---- static export ----

func (s *site) export(dir string) error {
	write := func(rel string, b []byte) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, b, 0o644)
	}
	pages := []struct {
		rel, name, active, title string
		data                     any
	}{
		{"index.html", "app", "app", "App", nil},
		{"mine/index.html", "app", "app", "App", nil},
		{"wallet/index.html", "app", "app", "App", nil},
		{"history/index.html", "app", "app", "App", nil},
		{"settings/index.html", "app", "app", "App", nil},
		{"404.html", "notfound", "", "Not found", nil},
	}
	for _, p := range pages {
		var buf bytes.Buffer
		if err := s.renderTo(&buf, p.name, p.active, p.title, p.data); err != nil {
			return fmt.Errorf("%s: %w", p.rel, err)
		}
		if err := write(p.rel, buf.Bytes()); err != nil {
			return err
		}
	}
	for i := range s.docs {
		d := &s.docs[i]
		var buf bytes.Buffer
		active := "docs"
		if d.Slug == "security" {
			active = "security"
		}
		if err := s.renderTo(&buf, "doc", active, d.Title, d); err != nil {
			return err
		}
		if err := write("docs/"+d.Slug+"/index.html", buf.Bytes()); err != nil {
			return err
		}
		if d.Slug == "security" {
			write("security/index.html", buf.Bytes())
		}
	}
	if len(s.docs) > 0 {
		write("docs/index.html", []byte(`<!doctype html><meta http-equiv="refresh" content="0; url=/docs/`+s.docs[0].Slug+`/">`))
	}
	// Assets.
	write("assets/"+ui.BuildID+"/theme.css", []byte(ui.CSS))
	write("assets/"+ui.BuildID+"/ui.js", []byte(ui.JS))
	if entries, err := ui.Tokens.ReadDir("tokens"); err == nil {
		for _, e := range entries {
			if b, err := ui.Tokens.ReadFile("tokens/" + e.Name()); err == nil {
				write("assets/"+ui.BuildID+"/tokens/"+e.Name(), b)
			}
		}
	}
	css, _ := siteFS.ReadFile("site/site.css")
	write("static/site.css", css)
	entries, _ := fs.ReadDir(siteFS, "site/static")
	for _, e := range entries {
		b, err := siteFS.ReadFile(path.Join("site/static", e.Name()))
		if err == nil {
			write("static/"+e.Name(), b)
		}
	}
	return nil
}
