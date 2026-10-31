// Command argonmine-explorer is a block explorer. It follows a node over RPC,
// keeps its own address index, and serves HTML pages and chart data.
package main

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"argonmine/bridge"
	"argonmine/consensus"
	"argonmine/ui"
	"argonmine/wallet"
)

//go:embed ui/templates.html
var templateFS embed.FS

type server struct {
	params   *consensus.Params
	client   *wallet.Client
	ix       *Indexer
	work     *workServer
	tmpl     *template.Template
	start    time.Time
	clientIP func(*http.Request) string // the key of the per-client limits (-client-ip-header)
	miners   *minerBook                 // the website's miner count (miners.go); nil: off
	origins  originSet                  // browser origins allowed to submit work (-allow-origin)
	limits   *limiters

	blockCache *byteLRU // getblock (verbosity 2) of confirmed blocks, by hash
	txCache    *byteLRU // getrawtransaction of confirmed transactions, by txid
	infoCache  timedCache
	mpCache    mempoolCache
	chartsMu   sync.Mutex
	charts     chartsEntry

	txMu   sync.Mutex
	txAt   time.Time
	txList []recentTx

	nodeErrMu sync.Mutex
	nodeErrAt time.Time
}

// recentTx is one row of the home page's latest-transactions list.
type recentTx struct {
	TxID     string
	Height   uint32
	Time     uint64
	Coinbase bool
	Inputs   int
	Outputs  int
	Out      consensus.Amount
	Fee      consensus.Amount
}

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Argonmine")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".argonmine")
}

func main() {
	network := flag.String("network", "mainnet", "mainnet, testnet or regtest")
	dataDir := flag.String("datadir", defaultDataDir(), "node data directory (for rpc.cookie); the explorer index lives beside it")
	rpcURL := flag.String("rpc", "", "node RPC URL (default http://127.0.0.1:<port>)")
	token := flag.String("token", "", "RPC token (default: read <datadir>/<network>/rpc.cookie)")
	listen := flag.String("listen", "127.0.0.1:8380", "HTTP listen address")
	referralPct := flag.Int("referral-share", 5, "percent of a block reward that a browser miner who names a referrer gives to that referrer (0 disables)")
	ipHeader := flag.String("client-ip-header", "", "request header naming the client address when a proxy is in front, e.g. X-Forwarded-For (its right-most entry, the one the proxy appended, is used); empty: the TCP peer address")
	allowOrigin := flag.String("allow-origin", "http://127.0.0.1:8370,http://localhost:8370,http://127.0.0.1:8372,http://localhost:8372", "comma-separated web app origins whose pages may submit work (POST /api/work/submit) from a browser; * allows any")
	bridgeURL := flag.String("bridge-url", "", "bridge URL whose /api/status names the genuine uARG asset id (read at start and every 10 minutes)")
	uargAsset := flag.String("uarg-asset", "", "the genuine uARG asset id (64 hex); overrides -bridge-url")
	swapURL := flag.String("swap-url", "", "swap service whose /api/info names the wARG issuer (read at start and every 10 minutes)")
	poolWallet := flag.String("pool-wallet", "", "run the mining pool, paying out from this wallet file; created from POOL_MNEMONIC when missing, unlocked with POOL_WALLET_PASSWORD (empty: no pool)")
	poolFee := flag.Float64("pool-fee", 1, "pool fee in percent of each block the pool finds")
	poolMin := flag.String("pool-min-payout", "0.1", "smallest pool balance that is paid out, in ARG")
	flag.Parse()

	var params *consensus.Params
	switch *network {
	case "mainnet":
		params = consensus.MainNet
	case "testnet":
		params = consensus.TestNet
	case "regtest":
		params = consensus.RegTest
	default:
		log.Fatalf("unknown network %q", *network)
	}
	netDir := *dataDir
	if params != consensus.MainNet {
		netDir = filepath.Join(netDir, params.Name)
	}
	if *rpcURL == "" {
		*rpcURL = fmt.Sprintf("http://127.0.0.1:%d", params.RPCPort)
	}
	if *token == "" {
		if b, err := os.ReadFile(filepath.Join(netDir, "rpc.cookie")); err == nil {
			*token = strings.TrimSpace(string(b))
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	client := wallet.NewClient(*rpcURL, *token)
	ix, err := newIndexer(filepath.Join(netDir, "explorer-index"), client, params, log.Printf)
	if err != nil {
		log.Fatalf("index: %v", err)
	}
	defer ix.close()
	go ix.run()

	tmpl, err := newTemplates()
	if err != nil {
		log.Fatalf("templates: %v", err)
	}
	switch id := strings.ToLower(strings.TrimSpace(*uargAsset)); {
	case id != "":
		if !isAssetID(id) {
			log.Fatalf("-uarg-asset must be a 64-character hex asset id")
		}
		ui.SetCanonicalAsset("UARG", id)
		log.Printf("uarg: the genuine uARG is %s (-uarg-asset)", id)
	case *bridgeURL != "":
		go followBridgeAsset(*bridgeURL, log.Printf)
	default:
		log.Printf("uarg: neither -uarg-asset nor -bridge-url is set; every asset called UARG is shown as unverified")
	}
	if *swapURL != "" {
		go followSwapAsset(*swapURL, log.Printf)
	}
	s := newServer(params, client, ix, newWorkServer(client, params, *referralPct, log.Printf), tmpl, *ipHeader, *allowOrigin)
	if *poolWallet != "" {
		if err := startPool(s.work, params, client, *poolWallet, *poolFee, *poolMin, filepath.Join(netDir, "pool-ledger.json")); err != nil {
			log.Fatalf("pool: %v", err)
		}
	}
	geo := newGeoDB()
	go geo.load(log.Printf)
	s.miners = newMinerBook(filepath.Join(netDir, "miners.json"), geo)
	go s.miners.saveLoop(log.Printf)
	go s.work.run(nil)
	go s.limits.sweepEvery(time.Minute)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           securityHeaders(s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	log.Printf("explorer for %s on http://%s (node %s; client address from %s; work submissions from %s)", params.Name, *listen, *rpcURL, orDefault(*ipHeader, "the TCP peer"), s.origins)
	// On the platform's stop signal the pool ledger is written before the
	// process ends (it is saved within seconds anyway; this closes the gap).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		s.work.pool.save()
		s.miners.save(log.Printf)
		log.Print("explorer: stopped")
		os.Exit(0)
	}()
	log.Fatal(srv.ListenAndServe())
}

// startPool opens (or creates from POOL_MNEMONIC) the pool wallet and
// enables the pool on ws. Secrets come from the environment only; nothing
// of them is logged.
func startPool(ws *workServer, params *consensus.Params, client *wallet.Client, path string, feePct float64, minPayout, ledger string) error {
	pw := os.Getenv("POOL_WALLET_PASSWORD")
	if pw == "" {
		return fmt.Errorf("POOL_WALLET_PASSWORD is not set")
	}
	minPay, err := wallet.ParseAmount(minPayout)
	if err != nil {
		return fmt.Errorf("-pool-min-payout: %w", err)
	}
	var w *wallet.Wallet
	if _, err := os.Stat(path); err == nil {
		w, err = wallet.Open(path, params, pw)
		if err != nil {
			return fmt.Errorf("wallet %s: %w", path, err)
		}
	} else {
		m := strings.TrimSpace(os.Getenv("POOL_MNEMONIC"))
		if m == "" {
			return fmt.Errorf("wallet %s does not exist and POOL_MNEMONIC is not set", path)
		}
		w, err = wallet.Restore(path, params, m, "", pw)
		if err != nil {
			return fmt.Errorf("restore wallet %s: %w", path, err)
		}
		log.Printf("pool: wallet created at %s", path)
	}
	w.SetClient(client)
	addr, err := w.MiningAddress()
	if err != nil {
		return err
	}
	if err := ws.pool.enable(poolConfig{Address: addr, FeeBps: int(feePct*100 + 0.5), MinPayout: minPay, StatePath: ledger, Wallet: w}); err != nil {
		return err
	}
	go ws.pool.run(nil)
	log.Printf("pool: on, rewards to %s, fee %.2f%%, minimum payout %s ARG", addr, feePct, wallet.FormatAmount(minPay))
	return nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// newServer wires the explorer's handlers and caches.
func newServer(params *consensus.Params, client *wallet.Client, ix *Indexer, work *workServer, tmpl *template.Template, ipHeader, allowOrigin string) *server {
	s := &server{params: params, client: client, ix: ix, work: work, tmpl: tmpl, start: time.Now(),
		clientIP: clientIPFrom(ipHeader), origins: parseOrigins(allowOrigin), limits: newLimiters(),
		blockCache: newByteLRU(blockCacheBytes), txCache: newByteLRU(txCacheBytes)}
	if ix != nil {
		ix.onRewind = func() { s.blockCache.purge(); s.txCache.purge() }
	}
	return s
}

// routes is the explorer's HTTP surface. Every route except the static
// assets has a per-client rate limit.
func (s *server) routes() http.Handler {
	l := s.limits
	mux := http.NewServeMux()
	mux.Handle("/assets/", ui.Handler())
	page := func(path string, h http.HandlerFunc) { mux.HandleFunc(path, s.limit(l.pages, false, h)) }
	heavy := func(path string, h http.HandlerFunc) { mux.HandleFunc(path, s.limit(l.heavy, false, h)) }
	page("/", s.home)
	page("/blocks", s.blocks)
	heavy("/block/", s.block)
	heavy("/tx/", s.tx)
	page("/address/", s.address)
	// No asset pages (the owner's decision of 2026-10-28): old links go home.
	mux.HandleFunc("/asset/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusFound) })
	heavy("/search", s.search)
	heavy("/mempool", s.mempool)
	page("/api/stats", s.apiStats)
	page("/api/health", s.apiHealth)
	page("/api/charts", s.apiCharts)
	page("/api/address/", s.apiAddress)
	page("/api/asset/", s.apiAsset)
	page("/api/assets", s.apiAssets)
	page("/api/utxos/", s.apiUTXOs)
	page("/api/miners", s.apiMiners)
	page("/api/mempool", s.apiMempool)
	mux.HandleFunc("/api/tx", s.apiTx) // rate-limited inside, with CORS on every answer
	mux.HandleFunc("/api/work", s.limit(l.work, true, s.apiWork))
	mux.HandleFunc("/api/work/submit", s.limit(l.submit, true, s.apiWorkSubmit))
	// The pool speaks the same protocol under /pool (pool.go).
	mux.HandleFunc("/pool/api/work", s.limit(l.work, true, s.apiPoolWork))
	mux.HandleFunc("/pool/api/work/submit", s.limit(l.share, true, s.apiWorkSubmit))
	page("/pool", s.poolPage)
	page("/api/pool/stats", s.apiPoolStats)
	page("/api/pool/miner/", s.apiPoolMiner)
	heavy("/api/pool/ledger", s.apiPoolLedger)
	page("/qr.svg", func(w http.ResponseWriter, r *http.Request) { ui.ServeQR(w, r.URL.Query().Get("d")) })
	return mux
}

// logNode logs a failed node call for a page, at most once a minute.
func (s *server) logNode(what string, err error) {
	s.nodeErrMu.Lock()
	quiet := time.Since(s.nodeErrAt) < time.Minute
	if !quiet {
		s.nodeErrAt = time.Now()
	}
	s.nodeErrMu.Unlock()
	if !quiet {
		log.Printf("node: %s: %v", what, err)
	}
}

// explorerCSP is the content security policy of every explorer response:
// scripts, styles and images from the explorer itself (inline ones too, the
// pages are built with them; data: images for the favicon), requests to its
// own API only, no framing, no <base>, no plugins, forms (the search box)
// submitted to itself.
const explorerCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets the browser protections on every response: the
// content security policy, no framing, no MIME sniffing, only the origin as
// the referrer of cross-site requests, and HSTS when the request came over
// HTTPS (directly or, on Railway, through the TLS-terminating proxy). The
// JSON API keeps its own CORS headers; these do not restrict other sites
// reading it.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", explorerCSP)
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

// newTemplates parses the embedded page templates with their helpers.
func newTemplates() (*template.Template, error) {
	funcs := template.FuncMap{
		"coin":     func(a consensus.Amount) string { return wallet.FormatAmount(a) },
		"coinf":    func(a float64) string { return wallet.FormatAmount(consensus.Amount(a)) },
		"when":     func(t uint64) string { return time.Unix(int64(t), 0).UTC().Format("2006-01-02 15:04:05") },
		"whenf":    func(t float64) string { return time.Unix(int64(t), 0).UTC().Format("2006-01-02 15:04:05") },
		"ago":      ago,
		"agof":     func(f float64) string { return ago(uint64(f)) },
		"short":    func(s string) string { return shorten(s, 10, 6) },
		"hashrate": fmtHashrate,
		"addf":     func(a, b float64) float64 { return a + b },
		"sub64":    func(a, b consensus.Amount) consensus.Amount { return a - b },
		"int":      func(f float64) int64 { return int64(f) },
		"num":      func(v any) string { return numFmt(v) },
		"dur":      fmtDuration,
		"pct":      func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) + "%" },
		// sym names an asset: its display symbol, or a shortened id when the
		// index has not seen its issuance. Both arguments may be nil (map data).
		"sym": func(display, id any) string {
			if d, _ := display.(string); d != "" {
				return d
			}
			s, _ := id.(string)
			return shorten(s, 8, 4)
		},
	}
	return template.New("").Funcs(funcs).ParseFS(templateFS, "ui/templates.html")
}

type page struct {
	Title   string
	Head    template.HTML
	Mark    template.HTML
	Brand   string
	Coin    string
	UIJS    string
	Network string
	Height  int64
	Tip     uint32
	Err     string
	Data    any
}

func (s *server) render(w http.ResponseWriter, name, title string, data any) {
	h, tip, publicErr := s.ix.status()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	p := page{
		Title: title, Head: template.HTML(ui.Head(template.HTMLEscapeString(title + " · " + ui.Brand + " explorer"))), Mark: template.HTML(ui.Mark),
		Brand: ui.Brand, Coin: ui.Coin, UIJS: ui.AssetPath("ui.js"),
		Network: s.params.Name, Height: h, Tip: tip, Err: publicErr, Data: data,
	}
	if err := s.tmpl.ExecuteTemplate(w, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *server) notFound(w http.ResponseWriter, what string) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, "notfound", "Not found", what)
}

type homeData struct {
	Blocks     []BlockSummary
	Txs        []recentTx
	Hashrate   float64
	Difficulty float64
	Supply     consensus.Amount
	SupplyPct  float64
	Mempool    int
	MempoolB   float64
	Subsidy    consensus.Amount
	Peers      int
	Interval   float64 // mean seconds per block over the last 120
	Halving    uint32  // next halving height
	ToHalving  uint32  // blocks until it
	HalvingETA string  // estimated date
}

// recentTxs lists the transactions of the newest blocks (cached briefly).
func (s *server) recentTxs(h int64, n int) []recentTx {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	if time.Since(s.txAt) < 5*time.Second && s.txList != nil {
		return s.txList
	}
	var out []recentTx
	for _, b := range s.ix.latestBlocks(h, 4) {
		raw, err := s.cachedCall(s.blockCache, confirmedBlock, "getblock", b.Hash, 2)
		if err != nil {
			s.logNode("getblock", err)
			break
		}
		var blk blockJSON
		if json.Unmarshal(raw, &blk) != nil {
			break
		}
		for i := len(blk.Tx) - 1; i >= 0 && len(out) < n; i-- {
			tx := blk.Tx[i]
			rt := recentTx{TxID: tx.TxID, Height: blk.Height, Time: blk.Time, Coinbase: i == 0, Inputs: len(tx.Inputs), Outputs: len(tx.Outputs)}
			var in consensus.Amount
			for _, o := range tx.Outputs {
				if o.Asset == "" {
					rt.Out += o.Amount
				}
			}
			for _, inp := range tx.Inputs {
				if inp.Prevout != nil && inp.Prevout.Asset == "" {
					in += inp.Prevout.Amount
				}
			}
			if !rt.Coinbase && in >= rt.Out {
				rt.Fee = in - rt.Out
			}
			out = append(out, rt)
		}
		if len(out) >= n {
			break
		}
	}
	s.txAt, s.txList = time.Now(), out
	return out
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.notFound(w, r.URL.Path)
		return
	}
	h, _, _ := s.ix.status()
	d := homeData{Blocks: s.ix.latestBlocks(h, 10), Hashrate: s.ix.hashrate(120), Supply: supply(h), Subsidy: consensus.Subsidy(uint32(h + 1))}
	d.Txs = s.recentTxs(h, 10)
	d.SupplyPct = float64(d.Supply) / float64(consensus.MaxMoney) * 100
	if recent := s.ix.latestBlocks(h, 121); len(recent) > 1 {
		d.Interval = float64(int64(recent[0].Time)-int64(recent[len(recent)-1].Time)) / float64(len(recent)-1)
	}
	if h >= 0 {
		d.Halving = (uint32(h)/consensus.HalvingInterval + 1) * consensus.HalvingInterval
		d.ToHalving = d.Halving - uint32(h)
		per := d.Interval
		if per <= 0 {
			per = 600
		}
		d.HalvingETA = time.Now().Add(time.Duration(float64(d.ToHalving)*per) * time.Second).UTC().Format("2 Jan 2006")
	}
	ni := s.nodeInfo()
	d.Difficulty, d.Mempool, d.MempoolB, d.Peers = ni.Difficulty, ni.Mempool, ni.MempoolB, ni.Peers
	s.render(w, "home", "Explorer", d)
}

type blockRow struct {
	BlockSummary
	Interval int64 // seconds since the previous block (0 for the first)
}

type blocksData struct {
	Blocks []blockRow
	Before int64
	Next   int64
	Prev   int64
}

func (s *server) blocks(w http.ResponseWriter, r *http.Request) {
	h, _, _ := s.ix.status()
	before := h
	if v, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil && v >= 0 && v <= h {
		before = v
	}
	const per = 50
	raw := s.ix.latestBlocks(before, per+1) // one extra to compute the last interval
	rows := make([]blockRow, 0, per)
	for i := 0; i < len(raw) && i < per; i++ {
		row := blockRow{BlockSummary: raw[i]}
		if i+1 < len(raw) {
			row.Interval = int64(raw[i].Time) - int64(raw[i+1].Time)
		}
		rows = append(rows, row)
	}
	d := blocksData{Blocks: rows, Before: before, Next: before - per, Prev: before + per}
	if d.Prev > h {
		d.Prev = h
	}
	s.render(w, "blocks", "Blocks", d)
}

func (s *server) block(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/block/"))
	hash := id
	if height, err := strconv.ParseUint(id, 10, 32); err == nil && len(id) < 64 {
		if sum, ok := s.ix.block(uint32(height)); ok {
			hash = sum.Hash
		} else if hash, err = s.client.BlockHash(uint32(height)); err != nil {
			s.notFound(w, "block "+id)
			return
		}
	} else if !isAssetID(id) {
		s.notFound(w, "block "+id)
		return
	}
	raw, err := s.cachedCall(s.blockCache, confirmedBlock, "getblock", hash, 2)
	if err != nil {
		s.notFound(w, "block "+id)
		return
	}
	var blk map[string]any
	json.Unmarshal(raw, &blk)
	if hf, ok := blk["height"].(float64); ok && blk["onmainchain"] == true {
		blk["confirmations"] = float64(s.confirmations(hf))
	}
	var fees consensus.Amount
	if txs, ok := blk["tx"].([]any); ok {
		for i, t := range txs {
			tx, _ := t.(map[string]any)
			if tx == nil {
				continue
			}
			var in, out float64
			if ins, ok := tx["inputs"].([]any); ok {
				for _, x := range ins {
					if po, ok := x.(map[string]any)["prevout"].(map[string]any); ok && po["asset"] == nil {
						in += po["amount"].(float64)
					}
				}
			}
			if outs, ok := tx["outputs"].([]any); ok {
				for _, o := range outs {
					if om, _ := o.(map[string]any); om != nil && om["asset"] == nil {
						out += om["amount"].(float64)
					}
				}
			}
			tx["valueOut"] = consensus.Amount(out)
			if i > 0 && in >= out {
				tx["fee"] = consensus.Amount(in - out)
				fees += consensus.Amount(in - out)
			}
		}
	}
	blk["fees"] = fees
	if hf, ok := blk["height"].(float64); ok {
		if sum, ok := s.ix.block(uint32(hf)); ok {
			blk["miner"] = sum.Miner
			blk["reward"] = sum.Reward
			blk["work"] = sum.Work
		}
		if hf > 0 {
			blk["prevheight"] = uint32(hf) - 1
		}
		blk["nextheight"] = uint32(hf) + 1
	}
	if bits, ok := blk["bits"].(string); ok {
		if b, err := parseBits(bits); err == nil {
			if t, err := consensus.CompactToTarget(b); err == nil {
				blk["target"] = fmt.Sprintf("%064x", t)
			}
		}
	}
	s.render(w, "block", "Block "+fmt.Sprint(blk["height"]), blk)
}

func (s *server) tx(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/tx/"))
	if !isAssetID(id) {
		s.notFound(w, "transaction "+id)
		return
	}
	raw, err := s.cachedCall(s.txCache, confirmedTx, "getrawtransaction", id)
	if err != nil {
		s.notFound(w, "transaction "+id)
		return
	}
	tx := s.decorateTx(raw)
	if hf, ok := tx["height"].(float64); ok && tx["blockhash"] != nil {
		tx["confirmations"] = float64(s.confirmations(hf))
	}
	s.render(w, "tx", "Transaction", tx)
}

// decorateTx turns the node's transaction JSON into the tx page's data:
// ARG totals and fee, memo text, asset labels and flows, block time.
func (s *server) decorateTx(raw []byte) map[string]any {
	var tx map[string]any
	json.Unmarshal(raw, &tx)
	if tx == nil {
		tx = map[string]any{}
	}
	var typed txJSON
	json.Unmarshal(raw, &typed)
	var in, out float64
	ins, _ := tx["inputs"].([]any)
	outs, _ := tx["outputs"].([]any)
	tx["inputs"], tx["outputs"] = ins, outs
	for _, i := range ins {
		if po, ok := i.(map[string]any)["prevout"].(map[string]any); ok && po["asset"] == nil {
			in += po["amount"].(float64)
		}
	}
	for _, o := range outs {
		if om, _ := o.(map[string]any); om != nil && om["asset"] == nil {
			out += om["amount"].(float64)
		}
	}
	tx["totalIn"] = consensus.Amount(in)
	tx["totalOut"] = consensus.Amount(out)
	tx["assets"] = s.annotateAssets(tx, &typed)
	if tx["coinbase"] != true && in >= out {
		tx["fee"] = consensus.Amount(in - out)
		if size, ok := tx["size"].(float64); ok && size > 0 {
			tx["feeRate"] = (in - out) / size
		}
	}
	// Decode data outputs that are printable text (memos).
	for _, o := range outs {
		om, _ := o.(map[string]any)
		if om == nil || om["address"] != nil && om["address"] != "" {
			continue
		}
		if ph, ok := om["payload"].(string); ok {
			if raw, err := hex.DecodeString(ph); err == nil && len(raw) > 0 {
				printable := true
				for _, c := range raw {
					if c < 0x20 || c > 0x7e {
						printable = false
						break
					}
				}
				if printable {
					om["text"] = string(raw)
				}
			}
		}
	}
	if bh, ok := tx["blockhash"].(string); ok && bh != "" {
		if hf, ok := tx["height"].(float64); ok {
			if sum, ok := s.ix.block(uint32(hf)); ok {
				tx["time"] = sum.Time
			}
		}
	}
	return tx
}

// assetFlowRow is one non-ARG asset of a transaction page: what came in,
// was issued, went out and was burned, with the registry's name for it.
type assetFlowRow struct {
	Asset      string
	Symbol     string
	Display    string
	Unverified bool
	Issuer     string
	In         consensus.Amount
	Issued     consensus.Amount
	Out        consensus.Amount
	Burned     consensus.Amount
}

// Name is the display symbol, or a shortened id for an asset the index
// has not named yet.
func (r assetFlowRow) Name() string {
	if r.Display != "" {
		return r.Display
	}
	return shorten(r.Asset, 8, 4)
}

// annotateAssets labels the asset inputs, outputs and issuances of a node
// transaction map with display symbols and returns the per-asset flows.
// typed is the same transaction decoded with exact amounts.
func (s *server) annotateAssets(tx map[string]any, typed *txJSON) []assetFlowRow {
	rows := map[string]*assetFlowRow{}
	var order []string
	row := func(id string) *assetFlowRow {
		r, ok := rows[id]
		if !ok {
			r = &assetFlowRow{Asset: id}
			if info, ok := s.ix.asset(id); ok {
				r.Symbol, r.Display, r.Unverified, r.Issuer = info.Symbol, info.Display, info.Unverified, info.Issuer
			}
			rows[id] = r
			order = append(order, id)
		}
		return r
	}
	label := func(m map[string]any, r *assetFlowRow, amount consensus.Amount) {
		if m == nil {
			return
		}
		m["assetDisplay"] = r.Display
		m["coin"] = wallet.FormatAmount(amount)
	}
	iss, _ := tx["issuances"].([]any)
	for i, is := range typed.Issuances {
		r := row(is.Asset)
		if is.Symbol != "" && r.Symbol == "" {
			r.Symbol, r.Issuer = is.Symbol, is.Issuer
			r.Display, r.Unverified, _ = assetLabel(is.Symbol, is.Asset)
		}
		r.Issued = satAdd(r.Issued, is.Amount)
		if i < len(iss) {
			m, _ := iss[i].(map[string]any)
			label(m, r, is.Amount)
		}
	}
	outs, _ := tx["outputs"].([]any)
	for i, o := range typed.Outputs {
		if o.Asset == "" {
			continue
		}
		r := row(o.Asset)
		r.Out = satAdd(r.Out, o.Amount)
		if i < len(outs) {
			m, _ := outs[i].(map[string]any)
			label(m, r, o.Amount)
		}
	}
	ins, _ := tx["inputs"].([]any)
	for i, in := range typed.Inputs {
		if in.Prevout == nil || in.Prevout.Asset == "" {
			continue
		}
		r := row(in.Prevout.Asset)
		r.In = satAdd(r.In, in.Prevout.Amount)
		if i < len(ins) {
			if m, _ := ins[i].(map[string]any); m != nil {
				po, _ := m["prevout"].(map[string]any)
				label(po, r, in.Prevout.Amount)
			}
		}
	}
	out := make([]assetFlowRow, 0, len(order))
	for _, id := range order {
		r := rows[id]
		if r.Burned = satSub(satAdd(r.In, r.Issued), r.Out); r.Burned < 0 {
			r.Burned = 0 // prevouts missing (unconfirmed spend of an unconfirmed parent)
		}
		out = append(out, *r)
	}
	return out
}

// MovedARG reports whether the entry has an ARG line to show: ARG moved,
// or nothing else did either.
func (e AddrEntry) MovedARG() bool { return e.Received != 0 || e.Sent != 0 || len(e.Assets) == 0 }

// Incoming reports whether the entry reads as value coming in: by ARG when
// it moved, otherwise by the first listed asset.
func (e AddrEntry) Incoming() bool {
	if e.MovedARG() {
		return e.Received >= e.Sent
	}
	return e.Assets[0].Received >= e.Assets[0].Sent
}

type addressData struct {
	Address  string
	Totals   AddrTotals
	Balance  consensus.Amount
	Maturing consensus.Amount
	Stats    AddrStats
	Holdings []AssetHolding // non-ARG assets, sorted by symbol
	History  []AddrEntry
	Tip      int64
	Before   uint32 // paging cursor in effect (0 = newest)
	Older    uint32 // cursor for the next page (0 = none)
	Swap     *swapInfo
	Treasury bool // the network's treasury (consensus/treasury.go)
	Pool     bool // the mining pool's payout address (pool.go)
}

// isTreasury reports whether addr is the network's treasury.
func isTreasury(p *consensus.Params, addr string) bool {
	out, err := wallet.DecodeAddress(p.HRP, addr)
	return err == nil && len(p.TreasuryKey) == consensus.P2PKPayloadLen && out.Type == consensus.OutputTypeP2PK && string(out.Payload) == string(p.TreasuryKey)
}

// swapInfo is the payout a swap address names (bridge/swapaddr.go).
type swapInfo struct {
	Symbol string
	Chain  string
	To     string // EIP-55
}

// evmChainNames names the EVM chains a swap address can point at; others
// show by id.
var evmChainNames = map[uint64]string{
	1: "Ethereum", 11155111: "Ethereum Sepolia", 8453: "Base", 84532: "Base Sepolia", 56: "BNB Chain", 97: "BNB Chain Testnet",
	42161: "Arbitrum One", 421614: "Arbitrum Sepolia", 137: "Polygon", 80002: "Polygon Amoy", 10: "OP Mainnet", 11155420: "OP Sepolia",
}

func swapOf(hrp, addr string) *swapInfo {
	d, ok := bridge.ParseSwapAddress(hrp, addr)
	if !ok {
		return nil
	}
	name := evmChainNames[d.ChainID]
	if name == "" {
		name = "chain " + strconv.FormatUint(d.ChainID, 10)
	}
	return &swapInfo{Symbol: d.Symbol, Chain: name, To: bridge.ChecksumAddress(d.To)}
}

func (s *server) address(w http.ResponseWriter, r *http.Request) {
	addr := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/address/"))
	if _, err := wallet.DecodeAddress(s.params.HRP, addr); err != nil {
		s.notFound(w, "address "+addr)
		return
	}
	h, _, _ := s.ix.status()
	const per = 100
	t := s.ix.totals(addr)
	d := addressData{Address: addr, Totals: t, Balance: satSub(t.Received, t.Sent), Stats: addressStats(t), Holdings: s.ix.holdings(t), Tip: h, Maturing: s.ix.maturing(addr, h), Swap: swapOf(s.params.HRP, addr), Treasury: isTreasury(s.params, addr), Pool: s.isPoolAddress(addr)}
	if v, err := strconv.ParseUint(r.URL.Query().Get("before"), 10, 32); err == nil && v > 0 {
		d.Before = uint32(v)
		d.History = s.ix.addressHistoryBefore(addr, uint32(v), per)
	} else {
		d.History = s.ix.addressHistory(addr, per)
	}
	if len(d.History) == per {
		d.Older = d.History[len(d.History)-1].Height
	}
	s.render(w, "address", "Address", d)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("q")))
	switch {
	case q == "":
		http.Redirect(w, r, "/", http.StatusFound)
	case strings.HasPrefix(q, s.params.HRP+"1"):
		http.Redirect(w, r, "/address/"+q, http.StatusFound)
	case isAssetID(q):
		if _, err := s.client.Call("getblockheader", q); err == nil {
			http.Redirect(w, r, "/block/"+q, http.StatusFound)
			return
		}
		http.Redirect(w, r, "/tx/"+q, http.StatusFound)
	default:
		if _, err := strconv.ParseUint(q, 10, 32); err == nil {
			http.Redirect(w, r, "/block/"+q, http.StatusFound)
			return
		}
		s.notFound(w, q)
	}
}

func (s *server) apiStats(w http.ResponseWriter, r *http.Request) {
	h, tip, publicErr := s.ix.status()
	found, workDown := s.work.status()
	workErr := ""
	if workDown {
		workErr = nodeUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]any{
		"network": s.params.Name, "indexedHeight": h, "nodeHeight": tip, "hashrate": s.ix.hashrate(120),
		"supply": supply(h), "supplyCoin": wallet.FormatAmount(supply(h)), "error": publicErr,
		"blocksSubmitted": found, "workError": workErr,
	})
}

// healthMaxBlockAge is how long the chain may go without a block before
// /api/health reports it down: six times the ten-minute target.
const healthMaxBlockAge = time.Hour

// apiHealth answers GET /api/health for uptime monitors: 200 and "ok" while
// the explorer follows its node and blocks keep coming, 503 with the reasons
// otherwise (the node unreachable, the index behind it, no block for an
// hour). A free monitor polling it every few minutes is the alarm for a
// stopped chain.
func (s *server) apiHealth(w http.ResponseWriter, r *http.Request) {
	h, tip, publicErr := s.ix.status()
	out := map[string]any{"network": s.params.Name, "indexedHeight": h, "nodeHeight": tip}
	var problems []string
	if publicErr != "" {
		problems = append(problems, "node: "+publicErr)
	}
	if int64(tip) > h+3 {
		problems = append(problems, fmt.Sprintf("the index is %d blocks behind the node", int64(tip)-h))
	}
	if h >= 0 {
		if b, ok := s.ix.block(uint32(h)); ok {
			age := time.Now().Unix() - int64(b.Time)
			out["lastBlockAge"] = age
			if age > int64(healthMaxBlockAge/time.Second) {
				problems = append(problems, fmt.Sprintf("no new block for %d minutes", age/60))
			}
		}
	} else {
		problems = append(problems, "nothing indexed yet")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if len(problems) > 0 {
		out["status"], out["problems"] = "down", problems
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		out["status"] = "ok"
	}
	json.NewEncoder(w).Encode(out)
}

// apiCharts returns per-block series for the last n blocks (default 288, max
// 2016): height, time, difficulty, estimated hashrate (24-block window),
// block interval and cumulative supply.
func (s *server) apiCharts(w http.ResponseWriter, r *http.Request) {
	n := 288
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v >= 24 && v <= 2016 {
		n = v
	}
	h, _, _ := s.ix.status()
	// The series only change with the height: one computation per (n, height).
	s.chartsMu.Lock()
	if s.charts.n != n || s.charts.h != h || s.charts.body == nil {
		s.charts = chartsEntry{n: n, h: h, body: s.chartsBody(n, h)}
	}
	body := s.charts.body
	s.chartsMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Write(body)
}

// chartsBody computes the chart series for the last n blocks below height h.
func (s *server) chartsBody(n int, h int64) []byte {
	const window = 24
	blocks := s.ix.latestBlocks(h, n+window) // newest first
	for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
		blocks[i], blocks[j] = blocks[j], blocks[i]
	}
	type point struct {
		Height     uint32  `json:"h"`
		Time       uint64  `json:"t"`
		Difficulty float64 `json:"difficulty"`
		Hashrate   float64 `json:"hashrate"`
		Interval   int64   `json:"interval"`
		Supply     int64   `json:"supply"`
	}
	var pts []point
	for i := range blocks {
		if i < window {
			continue
		}
		b := blocks[i]
		p := point{Height: b.Height, Time: b.Time, Supply: int64(supply(int64(b.Height)))}
		if bits, err := parseBits(b.Bits); err == nil {
			if t, err := consensus.CompactToTarget(bits); err == nil {
				f := new(big.Float).SetInt(s.params.PowLimit)
				f.Quo(f, new(big.Float).SetInt(t))
				p.Difficulty, _ = f.Float64()
			}
		}
		work := new(big.Int)
		for _, wb := range blocks[i-window+1 : i+1] {
			if wv, ok := new(big.Int).SetString(wb.Work, 16); ok {
				work.Add(work, wv)
			}
		}
		if span := int64(b.Time) - int64(blocks[i-window].Time); span > 0 {
			wf, _ := new(big.Float).SetInt(work).Float64()
			p.Hashrate = wf / float64(span)
		}
		p.Interval = int64(b.Time) - int64(blocks[i-1].Time)
		pts = append(pts, p)
	}
	b, _ := json.Marshal(map[string]any{"points": pts})
	return append(b, '\n')
}

// mempool lists unconfirmed transactions with their fees (at most 200),
// from a view rebuilt at most every few seconds (cache.go).
func (s *server) mempool(w http.ResponseWriter, r *http.Request) {
	s.render(w, "mempool", "Mempool", s.mempoolView())
}

// apiAddress returns balance and history for one address: what the browser
// app shows on its Wallet and History pages. "spendable" excludes mining
// rewards younger than CoinbaseMaturity blocks.
func (s *server) apiAddress(w http.ResponseWriter, r *http.Request) {
	addr := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/address/"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if _, err := wallet.DecodeAddress(s.params.HRP, addr); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": "not a valid address for this network"})
		return
	}
	h, tip, publicErr := s.ix.status()
	t := s.ix.totals(addr)
	hist := s.ix.addressHistory(addr, 500)
	maturing := s.ix.maturing(addr, h)
	balance := satSub(t.Received, t.Sent)
	// Referral figures are lifetime totals kept by the index, not a sum
	// over the history returned here.
	json.NewEncoder(w).Encode(map[string]any{
		"referralTotal": t.ReferralTotal, "referralCount": t.ReferralCount, "referralPct": s.work.referralPct,
		"minedCount": t.Mined, "minedTotal": t.MinedTotal,
		"address": addr, "received": t.Received, "sent": t.Sent, "balance": balance, "spendable": satSub(balance, maturing),
		"maturing": maturing, "txcount": t.TxCount, "indexedHeight": h, "nodeHeight": tip, "history": hist, "error": publicErr,
		"nextSubsidy": consensus.Subsidy(uint32(h) + 1), "assets": s.ix.holdings(t),
	})
}

// isAssetID reports whether s looks like an asset id (64 lower-case hex).
func isAssetID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type assetData struct {
	Info    AssetInfo
	History []AssetTx
	Before  uint32 // paging cursor in effect (0 = newest)
	Older   uint32 // cursor for the next page (0 = none)
}

// asset serves /asset/{id} (one asset) and /asset/ (the registry).
func (s *server) asset(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/asset/"))
	if id == "" {
		s.render(w, "assets", "Assets", s.ix.assets())
		return
	}
	info, ok := s.ix.asset(id)
	if !isAssetID(id) || !ok {
		s.notFound(w, "asset "+id)
		return
	}
	const per = 100
	d := assetData{Info: info}
	if v, err := strconv.ParseUint(r.URL.Query().Get("before"), 10, 32); err == nil && v > 0 {
		d.Before = uint32(v)
	}
	d.History = s.ix.assetHistory(id, d.Before, per)
	if len(d.History) == per {
		d.Older = d.History[len(d.History)-1].Height
	}
	title := "Asset"
	if info.Display != "" {
		title = info.Display
	}
	s.render(w, "asset", title, d)
}

// apiAsset returns one registry entry with its supply and newest
// transactions.
func (s *server) apiAsset(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/asset/"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	info, ok := s.ix.asset(id)
	if !isAssetID(id) || !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": "unknown asset"})
		return
	}
	h, tip, publicErr := s.ix.status()
	json.NewEncoder(w).Encode(map[string]any{
		"asset": info.Asset, "symbol": info.Symbol, "display": info.Display, "unverified": info.Unverified, "logo": info.Logo, "issuer": info.Issuer,
		"issued": info.Issued, "burned": info.Burned, "supply": info.Supply, "txcount": info.TxCount,
		"firstHeight": info.FirstHeight, "firstTime": info.FirstTime, "firstTxid": info.FirstTxID,
		"history": s.ix.assetHistory(id, 0, 100), "indexedHeight": h, "nodeHeight": tip, "error": publicErr,
	})
}

// apiAssets lists the asset registry.
func (s *server) apiAssets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	h, tip, publicErr := s.ix.status()
	json.NewEncoder(w).Encode(map[string]any{"assets": s.ix.assets(), "indexedHeight": h, "nodeHeight": tip, "error": publicErr})
}

func shorten(s string, head, tail int) string {
	if len(s) > head+tail+1 {
		return s[:head] + "…" + s[len(s)-tail:]
	}
	return s
}

func numFmt(v any) string {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	case uint32:
		n = int64(x)
	case uint64:
		n = int64(x)
	case float64:
		n = int64(x)
	default:
		return fmt.Sprint(v)
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func ago(t uint64) string {
	d := time.Since(time.Unix(int64(t), 0))
	switch {
	case d < 0:
		return "just now" // a timestamp ahead of this clock (allowed up to 2 hours)
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// fmtDuration prints seconds as "4m 12s" or "1h 03m".
func fmtDuration(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh %02dm", sec/3600, sec%3600/60)
}

func fmtHashrate(h float64) string {
	units := []string{"H/s", "kH/s", "MH/s", "GH/s", "TH/s"}
	i := 0
	for h >= 1000 && i < len(units)-1 {
		h /= 1000
		i++
	}
	return fmt.Sprintf("%.2f %s", h, units[i])
}
