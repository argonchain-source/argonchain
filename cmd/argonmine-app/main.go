// Command argonmine-app is the mining app: a full node, a wallet and a miner in
// one process, controlled from a local web interface. It mines solo on its
// own node. No cgo, no external runtime; it opens
// http://127.0.0.1:8371 in the default browser.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"argonmine/bridge"
	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/miner"
	"argonmine/p2p"
	"argonmine/rpc"
	"argonmine/ui"
	"argonmine/wallet"
)

//go:embed ui/index.html
var indexHTML string

// settings are the user's choices, persisted in the data directory.
type settings struct {
	Threads   int  `json:"threads"`
	AutoStart bool `json:"autostart"`
}

type app struct {
	params *consensus.Params
	dir    string
	start  time.Time

	ch     *chain.Chain
	pool   *mempool.Pool
	net    *p2p.Server
	rpcSrv *rpc.Server
	client *wallet.Client

	mu         sync.Mutex
	store      *wallet.Store  // nil: no wallet file yet
	w          *wallet.Wallet // nil: locked
	solo       *miner.Miner
	miningAddr string
	set        settings
	lastErr    string
	syncing    bool
	syncReq    chan struct{}
	token      string
	rateHist   []float64 // recent hashrate samples for the sparkline

	// The web interface: the port it listens on (the Host and Origin checks
	// accept only loopback names with this port) and whether -ui-allow-remote
	// was given (then IP-literal hosts are accepted too).
	uiPort      string
	allowRemote bool

	// Idle auto-lock: the wallet locks after autolock without an API call that
	// a person caused (background polls do not count); 0 disables it.
	autolock   time.Duration
	lastActive time.Time
	idleLocked bool // the idle timer locked the wallet since it was last unlocked

	bridgeFetch sync.Mutex       // one fetch of the bridge status at a time; never held with a.mu
	bridgeMu    sync.Mutex       // guards the three fields below; held only briefly
	bridgeView  bridgeChainsView // last answer from the bridge daemon
	bridgeAt    time.Time        // when bridgeView was fetched; zero: never
	bridgeAsset string           // the bridge's uARG asset id from its last good answer; "": not known yet
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

// Default seed peers per network, the same list as cmd/argonmined. Mainnet is
// filled in at launch (docs/LAUNCH.md step 7); the testnet seed is the public
// node described in deploy/railway/README.md.
var defaultSeeds = map[string][]string{
	"mainnet": {"altaria.proxy.rlwy.net:12347"}, // the Railway node (P2P on container port 19333 behind the TCP proxy)
	"testnet": {},                               // retired on 2026-10-28: no public nodes
	"regtest": {},
}

func main() {
	network := flag.String("network", "mainnet", "mainnet, testnet or regtest")
	dataDir := flag.String("datadir", defaultDataDir(), "data directory")
	uiAddr := flag.String("ui", "127.0.0.1:8371", "web interface address (a loopback address; see -ui-allow-remote)")
	// The interface has no TLS and no login: whoever can load the page gets
	// its API token, so on a non-loopback address anyone on that network can
	// read the wallet, create one and show its recovery phrase, change mining
	// and, with the password, spend. Only for a reachable address you protect
	// yourself (an authenticated tunnel, a firewalled host-only network).
	allowRemote := flag.Bool("ui-allow-remote", false, "DANGEROUS: allow -ui on a non-loopback address; anyone who can reach it controls the app (no TLS, no login)")
	autolock := flag.Duration("autolock", 15*time.Minute, "lock the wallet after this long without activity in the page (0 disables); mining continues")
	connect := flag.String("connect", "", "comma-separated peers to connect to")
	listen := flag.String("listen", "", "P2P listen address (default :<network port>; 'none' to disable)")
	threads := flag.Int("threads", 0, "miner threads (default: CPUs-1)")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser")
	flag.StringVar(&siteLinks.Docs, "docs-url", "https://docs.argonmine.com", "documentation link shown in the sidebar")
	flag.StringVar(&siteLinks.Explorer, "explorer-url", "https://explorer.argonmine.com", "explorer link shown in the sidebar")
	flag.StringVar(&siteLinks.Bridge, "bridge-url", "https://bridge.argonmine.com", "bridge service: swaps between uARG and USDC or USDT")
	flag.Parse()

	if err := checkUIAddr(*uiAddr, *allowRemote); err != nil {
		log.Fatal(err)
	}
	if *autolock < 0 {
		log.Fatal("-autolock must be 0 (off) or a positive duration such as 15m")
	}
	for name, u := range map[string]string{"docs-url": siteLinks.Docs, "explorer-url": siteLinks.Explorer, "bridge-url": siteLinks.Bridge} {
		if u != "" && !isWebURL(u) {
			log.Fatalf("-%s must be an http:// or https:// URL, got %q", name, u)
		}
	}

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
	dir := *dataDir
	if params != consensus.MainNet {
		dir = filepath.Join(dir, params.Name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	a := &app{params: params, dir: dir, start: time.Now(), syncReq: make(chan struct{}, 1), allowRemote: *allowRemote, autolock: *autolock}
	a.loadSettings()
	if *threads > 0 {
		a.set.Threads = *threads
	}
	if a.set.Threads <= 0 {
		a.set.Threads = runtime.NumCPU() - 1
		if a.set.Threads < 1 {
			a.set.Threads = 1
		}
	}
	var tb [16]byte
	rand.Read(tb[:])
	a.token = hex.EncodeToString(tb[:])

	var err error
	a.ch, err = chain.Open(params, filepath.Join(dir, "chain"))
	if err != nil {
		log.Fatalf("chain: %v", err)
	}
	a.pool = mempool.New(a.ch, mempool.DefaultConfig())

	p2pCfg := p2p.Config{Params: params, DataDir: dir, UserAgent: "/argonmine-app:" + ui.Version + "/"}
	p2pCfg.Seeds = append(p2pCfg.Seeds, defaultSeeds[params.Name]...)
	switch *listen {
	case "none":
	case "":
		p2pCfg.Listen = fmt.Sprintf(":%d", params.P2PPort)
	default:
		p2pCfg.Listen = *listen
	}
	for _, c := range strings.Split(*connect, ",") {
		if c = strings.TrimSpace(c); c != "" {
			p2pCfg.Connect = append(p2pCfg.Connect, c)
		}
	}
	a.net = p2p.New(p2pCfg, a.ch, a.pool)
	if err := a.net.Start(); err != nil {
		log.Fatalf("p2p: %v", err)
	}

	var rb [24]byte
	rand.Read(rb[:])
	rpcToken := hex.EncodeToString(rb[:])
	cookie := filepath.Join(dir, "rpc.cookie")
	_ = os.WriteFile(cookie, []byte(rpcToken), 0o600)
	deps := rpc.Deps{Params: params, Chain: a.ch, Pool: a.pool, P2P: a.net, Version: ui.Version, Started: a.start, Logf: log.Printf}
	a.rpcSrv = rpc.New(rpc.Config{Listen: fmt.Sprintf("127.0.0.1:%d", params.RPCPort), Token: rpcToken}, deps)
	if err := a.rpcSrv.Start(); err != nil {
		a.rpcSrv = rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: rpcToken}, deps)
		if err := a.rpcSrv.Start(); err != nil {
			log.Fatalf("rpc: %v", err)
		}
	}
	a.client = wallet.NewClient("http://"+a.rpcSrv.Addr(), rpcToken)

	walletPath := filepath.Join(dir, "wallet.json")
	if st, err := wallet.OpenStore(walletPath); err == nil {
		a.store = st
		a.miningAddr = wallet.FirstReceiveAddress(st)
	}

	a.ch.Subscribe(func(*chain.Node) { a.requestSync() })
	go a.syncLoop()
	go a.sampleLoop()
	go a.autolockLoop()
	if a.set.AutoStart && a.miningAddr != "" {
		if err := a.miningStart(0); err != nil {
			log.Printf("autostart: %v", err)
		}
	}

	ln, err := net.Listen("tcp", *uiAddr)
	if err != nil {
		log.Fatalf("ui: %v", err)
	}
	tcp, _ := ln.Addr().(*net.TCPAddr)
	if tcp == nil || (!tcp.IP.IsLoopback() && !*allowRemote) {
		log.Fatalf("ui: %s is not a loopback address; refusing to serve the wallet there (see -ui-allow-remote)", ln.Addr())
	}
	a.uiPort = strconv.Itoa(tcp.Port)
	if *allowRemote && !tcp.IP.IsLoopback() {
		log.Printf("WARNING: the interface listens on %s: anyone who can reach it controls this app", ln.Addr())
	}
	srv := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	url := "http://" + ln.Addr().String()
	log.Printf("%s app %s on %s — open %s", ui.Brand, ui.Version, params.Name, url)
	if !*noBrowser {
		openBrowser(url)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")
	a.stopMining()
	srv.Close()
	a.rpcSrv.Stop()
	a.net.Stop()
	a.ch.Close()
	os.Remove(cookie)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func (a *app) settingsPath() string { return filepath.Join(a.dir, "app.json") }

func (a *app) loadSettings() {
	if b, err := os.ReadFile(a.settingsPath()); err == nil {
		json.Unmarshal(b, &a.set)
	}
}

func (a *app) saveSettings() {
	b, _ := json.MarshalIndent(a.set, "", "  ")
	_ = os.WriteFile(a.settingsPath(), b, 0o600)
}

func (a *app) requestSync() {
	select {
	case a.syncReq <- struct{}{}:
	default:
	}
}

func (a *app) syncLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.syncReq:
		case <-t.C:
		}
		a.mu.Lock()
		w := a.w
		a.syncing = w != nil
		a.mu.Unlock()
		if w == nil {
			continue
		}
		_, err := w.Sync()
		a.mu.Lock()
		a.syncing = false
		if err != nil {
			a.lastErr = "wallet sync: " + err.Error()
		}
		a.mu.Unlock()
	}
}

// sampleLoop records the hashrate every 5 s for the sparkline (last hour).
func (a *app) sampleLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		a.mu.Lock()
		rate := a.currentRateLocked()
		a.rateHist = append(a.rateHist, rate)
		if len(a.rateHist) > 720 {
			a.rateHist = a.rateHist[len(a.rateHist)-720:]
		}
		a.mu.Unlock()
	}
}

func (a *app) currentRateLocked() float64 {
	if a.solo != nil && a.solo.Running() {
		return a.solo.HashRate()
	}
	return 0
}

// siteLinks are the public services linked from the sidebar.
var siteLinks struct{ Docs, Explorer, Bridge string }

// securityHeaders go on every response of the interface. The page has inline
// scripts and styles (hence 'unsafe-inline'); everything it loads or fetches
// is its own origin. Links to the explorer and the bridge are navigations,
// which the policy does not restrict.
var securityHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"},
	{"X-Frame-Options", "DENY"},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
}

// handler is the whole web interface. Every request must name this app in
// its Host header: a page on another site that rebinds its DNS name to
// 127.0.0.1 still sends its own name, so it never gets the page (and with it
// the API token) nor an API answer.
func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/assets/", ui.Handler())
	mux.HandleFunc("/", a.serveIndex)
	mux.HandleFunc("/api/", a.serveAPI)
	// QR codes of the wallet's address and of deposit addresses (0x…).
	mux.HandleFunc("/qr.svg", func(w http.ResponseWriter, r *http.Request) { ui.ServeQR(w, r.URL.Query().Get("d")) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for _, kv := range securityHeaders {
			h.Set(kv[0], kv[1])
		}
		if !a.hostAllowed(r.Host) {
			http.Error(w, "forbidden: this interface answers only to http://127.0.0.1:"+a.uiPort+", http://localhost:"+a.uiPort+" or http://[::1]:"+a.uiPort, http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether a Host header (or the host:port of an Origin)
// names this interface: 127.0.0.1, localhost or [::1] with its port (the port
// may be left out only when it is 80). With -ui-allow-remote any IP literal
// with the port is accepted too; a DNS name never is, other than localhost.
func (a *app) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	if a.uiPort == "" {
		return false
	}
	for _, name := range []string{"127.0.0.1", "localhost", "[::1]"} {
		if host == name+":"+a.uiPort || (a.uiPort == "80" && host == name) {
			return true
		}
	}
	if a.allowRemote {
		if h, p, err := net.SplitHostPort(host); err == nil && p == a.uiPort && net.ParseIP(h) != nil {
			return true
		}
	}
	return false
}

// apiRequestAllowed rejects API calls made from another site: a browser
// names the calling page in Origin (on every POST) and says in Sec-Fetch-Site
// whether the call is same-origin. Either header, when present, must point at
// this interface. Requests without them (curl, older browsers) still need the
// Host check and the token.
func (a *app) apiRequestAllowed(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		if !strings.HasPrefix(strings.ToLower(o), "http://") || !a.hostAllowed(o[len("http://"):]) {
			return false
		}
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return false
	}
	return true
}

// tokenOK compares the API token in constant time.
func (a *app) tokenOK(got string) bool {
	return a.token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}

// checkUIAddr refuses a non-loopback interface address unless the user
// accepted the risk with -ui-allow-remote. A host name other than localhost
// is refused too: it could resolve to anything.
func checkUIAddr(addr string, allowRemote bool) error {
	if allowRemote {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-ui %q: %v", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-ui %q is not a loopback address: anyone who can reach it would control the wallet (no TLS, no login). Use 127.0.0.1, or add -ui-allow-remote if you protect that address yourself", addr)
}

// isWebURL reports whether s is an absolute http(s) URL with a host.
func isWebURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// safeExplorerURL keeps a block-explorer base URL that came from the bridge
// only when it is https (or http on this computer, for local test chains);
// anything else (javascript:, data:, a plain-http site) becomes "" and the
// interface shows no link.
func safeExplorerURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.User != nil || u.Host == "" || u.Opaque != "" {
		return ""
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := strings.ToLower(u.Hostname()); h != "127.0.0.1" && h != "localhost" {
			return ""
		}
	default:
		return ""
	}
	if u.RawQuery != "" || u.Fragment != "" || !plainHost(u.Hostname()) {
		return ""
	}
	if p := u.Port(); p != "" && strings.Trim(p, "0123456789") != "" {
		return ""
	}
	out := strings.TrimRight(u.String(), "/")
	if strings.ContainsAny(out, "\"'<>`\\ \t\r\n") {
		return ""
	}
	return out
}

// plainHost reports whether h is a DNS name or an IP address made of the
// usual characters only (url.Parse lets through quotes and angle brackets).
func plainHost(h string) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// jsString renders s as a JavaScript string literal that is safe inside a
// <script> element (json escapes <, > and & as < and so on).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (a *app) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	page := strings.NewReplacer(
		"{{TOKEN}}", a.token,
		"{{HEAD}}", ui.Head(ui.Brand+" · "+a.params.Name),
		"{{BRAND}}", ui.Brand,
		"{{ARG}}", ui.Coin,
		"{{MARK}}", ui.Mark,
		"{{UIJS}}", ui.AssetPath("ui.js"),
		"{{EVMJS}}", ui.AssetPath("evm.js"),
		"{{VERSION}}", ui.Version,
		"{{DOCS_URL}}", html.EscapeString(siteLinks.Docs),
		"{{EXPLORER_URL}}", html.EscapeString(siteLinks.Explorer),
		"{{EXPLORER_JS}}", jsString(strings.TrimRight(siteLinks.Explorer, "/")),
		"{{BRIDGE_URL}}", html.EscapeString(siteLinks.Bridge),
	).Replace(indexHTML)
	fmt.Fprint(w, page)
}

type apiError struct {
	Error string `json:"error"`
}

// passiveEndpoints are what the page asks for on its own, on timers and when
// a section opens. They do not count as activity for the idle auto-lock;
// every other call does (the page also reports keys and clicks via
// "activity").
var passiveEndpoints = map[string]bool{
	"status":                true,
	"peers":                 true,
	"wallet/history":        true,
	"wallet/addresses":      true,
	"wallet/utxos":          true,
	"wallet/assets":         true,
	"bridge/chains":         true,
	"bridge/activity":       true,
	"bridge/deposit-status": true,
	"bridge/deposit":        true,
	"bridge/redemption":     true,
	"swap/native-info":      true,
	"swap/native-status":    true,
}

func (a *app) serveAPI(w http.ResponseWriter, r *http.Request) {
	if !a.apiRequestAllowed(r) {
		http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !a.tokenOK(r.Header.Get("X-Token")) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(apiError{"unauthorized"})
		return
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/api/")
	if !passiveEndpoints[endpoint] {
		a.touch()
	}
	body := map[string]any{}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			body = map[string]any{}
		}
	}
	str := func(k string) string {
		if v, ok := body[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	num := func(k string) int {
		if v, ok := body[k].(float64); ok {
			return int(v)
		}
		return 0
	}
	boolean := func(k string) (bool, bool) {
		v, ok := body[k].(bool)
		return v, ok
	}

	var result any
	var err error
	switch endpoint {
	case "activity":
		// The page reports that someone is using it (keys, clicks); touch
		// above already pushed the idle lock back.
	case "status":
		result = a.status()
	case "peers":
		result = a.net.Peers()
	case "wallet/create":
		result, err = a.walletCreate(str("password"))
	case "wallet/restore":
		err = a.walletRestore(str("mnemonic"), str("password"))
	case "wallet/unlock":
		err = a.walletUnlock(str("password"))
	case "wallet/newaddress":
		result, err = a.withWallet(func(wl *wallet.Wallet) (any, error) { return wl.NewAddress() })
	case "wallet/addresses":
		result, err = a.withWallet(func(wl *wallet.Wallet) (any, error) { return wl.Addresses(), nil })
	case "wallet/history":
		result, err = a.withWallet(func(wl *wallet.Wallet) (any, error) { return wl.History(), nil })
	case "wallet/utxos":
		result, err = a.withWallet(func(wl *wallet.Wallet) (any, error) { return wl.UTXOs(), nil })
	case "wallet/mnemonic":
		result, err = a.walletMnemonic(str("password"))
	case "wallet/send":
		result, err = a.walletSend(str("to"), str("amount"), str("feerate"), str("memo"), str("password"))
	case "wallet/assets":
		genuine, live := a.uargStatus()
		result, err = a.withWallet(func(wl *wallet.Wallet) (any, error) {
			return assetView(wl.AssetBalances(), genuine, live, wargID()), nil
		})
	case "wallet/send-asset":
		result, err = a.walletSendAsset(str("asset"), str("to"), str("amount"), str("feerate"), str("password"))
	case "wallet/swap":
		result, err = a.walletSwap(str("amount"), str("evm"), str("chain_id"), str("token"), str("feerate"), str("password"))
	case "bridge/chains":
		result = a.bridgeChains()
	case "bridge/checkaddress":
		// The Swap screen checks the payout address before the review step.
		if verr := checkEVMAddress(str("address")); verr != nil {
			result = map[string]any{"valid": false, "error": verr.Error()}
		} else {
			result = map[string]any{"valid": true, "error": ""}
		}
	case "bridge/handoff":
		result, err = fetchBridgeHandoff(siteLinks.Bridge, str("code"))
	case "bridge/activity":
		result, err = a.bridgeActivity()
	case "bridge/deposit-address":
		result, err = a.bridgeDepositAddress(str("chain_id"), false)
	case "bridge/deposit-status":
		result, err = a.bridgeDepositAddress(str("chain_id"), true)
	case "bridge/deposit":
		result, err = a.bridgeDeposit(str("tx"))
	case "swap/native-info":
		result, err = a.swapNativeInfo()
	case "swap/native-prepare":
		result, err = a.swapNativePrepare(str("from"), str("to"), str("amount"))
	case "swap/native-execute":
		result, err = a.swapNativeExecute(str("quote_id"), str("password"))
	case "swap/native-status":
		result, err = a.swapNativeStatus(str("txid"))
	case "bridge/redemption":
		result, err = a.bridgeRedemption(str("txid"), str("address"), str("to"), str("chain_id"), str("token"), int64(num("since")))
	case "wallet/validate":
		result = a.checkAddress(str("address"))
	case "mining/start":
		err = a.miningStart(num("threads"))
	case "mining/stop":
		a.stopMining()
	case "settings":
		a.mu.Lock()
		if v, ok := boolean("autostart"); ok {
			a.set.AutoStart = v
		}
		if t := num("threads"); t > 0 {
			a.set.Threads = t
		}
		a.saveSettings()
		result = a.set
		a.mu.Unlock()
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(apiError{"unknown endpoint"})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(apiError{err.Error()})
		return
	}
	if result == nil {
		result = map[string]bool{"ok": true}
	}
	json.NewEncoder(w).Encode(result)
}

func difficulty(limit, target *big.Int) float64 {
	if target == nil || target.Sign() <= 0 {
		return 0
	}
	f := new(big.Float).SetInt(limit)
	f.Quo(f, new(big.Float).SetInt(target))
	d, _ := f.Float64()
	return d
}

func (a *app) status() map[string]any {
	genuine, live := a.uargStatus() // before a.mu: the bridge cache has its own lock
	warg := wargID()
	a.mu.Lock()
	defer a.mu.Unlock()
	tip := a.ch.Tip()
	target, _ := consensus.CompactToTarget(a.ch.NextBits())
	s := map[string]any{
		"brand":         ui.Brand,
		"coin":          ui.Coin,
		"version":       ui.Version,
		"network":       a.params.Name,
		"hrp":           a.params.HRP,
		"height":        tip.Height,
		"bestHash":      tip.Hash.String(),
		"tipTime":       tip.Header.Time,
		"peers":         a.net.PeerCount(),
		"ibd":           a.net.Syncing(),
		"difficulty":    difficulty(a.params.PowLimit, target),
		"nextSubsidy":   consensus.Subsidy(tip.Height + 1),
		"mempool":       a.pool.Count(),
		"walletExists":  a.store != nil,
		"unlocked":      a.w != nil,
		"miningAddress": a.miningAddr,
		"autostart":     a.set.AutoStart,
		"mining":        false,
		"hashrate":      a.currentRateLocked(),
		"rateHistory":   a.rateHist,
		"threads":       a.set.Threads,
		"cpus":          runtime.NumCPU(),
		"blocksFound":   uint64(0),
		"hashes":        uint64(0),
		"walletSyncing": a.syncing,
		"syncedHeight":  int64(-1),
		"uptime":        int64(time.Since(a.start).Seconds()),
		"lastError":     a.lastErr,
		"autolock":      int64(a.autolock / time.Second),
		"idleLocked":    a.idleLocked && a.w == nil,
		"uargAsset":     genuine, // the bridge's uARG asset id; "": the bridge has not answered yet
	}
	if a.solo != nil && a.solo.Running() {
		s["mining"] = true
		s["blocksFound"] = a.solo.BlocksFound()
		s["hashes"] = a.solo.Hashes()
	}
	if a.w != nil {
		sp, im, lk := a.w.Balance(tip.Height)
		s["balance"] = map[string]any{"spendable": sp, "immature": im, "pending": lk}
		s["syncedHeight"] = a.w.SyncedHeight()
		s["assets"] = assetView(a.w.AssetBalances(), genuine, live, warg)
		names := map[string]string{}
		for id, info := range a.w.Assets() {
			names[id], _, _, _ = assetDisplay(id, info.Symbol, genuine, warg)
		}
		s["assetNames"] = names
	}
	return s
}

func (a *app) withWallet(fn func(*wallet.Wallet) (any, error)) (any, error) {
	a.mu.Lock()
	w := a.w
	a.mu.Unlock()
	if w == nil {
		return nil, errors.New("the wallet is locked")
	}
	return fn(w)
}

func (a *app) walletCreate(password string) (any, error) {
	if len(password) < 8 {
		return nil, errors.New("the password must be at least 8 characters")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		return nil, errors.New("a wallet already exists")
	}
	w, mnemonic, err := wallet.Create(filepath.Join(a.dir, "wallet.json"), a.params, password, "")
	if err != nil {
		return nil, err
	}
	return a.adoptWallet(w, mnemonic)
}

func (a *app) walletRestore(mnemonic, password string) error {
	if len(password) < 8 {
		return errors.New("the password must be at least 8 characters")
	}
	mnemonic = strings.ToLower(strings.Join(strings.Fields(mnemonic), " "))
	if !wallet.ValidateMnemonic(mnemonic) {
		return errors.New("that is not a valid 24-word recovery phrase")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		return errors.New("a wallet already exists")
	}
	w, err := wallet.Restore(filepath.Join(a.dir, "wallet.json"), a.params, mnemonic, "", password)
	if err != nil {
		return err
	}
	_, err = a.adoptWallet(w, "")
	return err
}

// adoptWallet wires a freshly created/restored/unlocked wallet. Caller holds a.mu.
func (a *app) adoptWallet(w *wallet.Wallet, mnemonic string) (any, error) {
	w.SetClient(a.client)
	addr, err := w.MiningAddress()
	if err != nil {
		return nil, err
	}
	a.w = w
	a.miningAddr = addr
	st, _ := wallet.OpenStore(filepath.Join(a.dir, "wallet.json"))
	a.store = st
	a.lastActive, a.idleLocked = time.Now(), false
	a.requestSync()
	go a.bridgeChains() // learn which asset is the bridge's uARG before balances are shown
	return map[string]any{"mnemonic": mnemonic, "miningAddress": addr}, nil
}

// touch records activity in the page, which pushes the idle lock back.
func (a *app) touch() {
	a.mu.Lock()
	a.lastActive = time.Now()
	a.mu.Unlock()
}

// autolockLoop locks the wallet once it has been idle for a.autolock.
func (a *app) autolockLoop() {
	if a.autolock <= 0 {
		return
	}
	every := a.autolock / 10
	if every > 15*time.Second {
		every = 15 * time.Second
	}
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for now := range t.C {
		a.checkAutolock(now)
	}
}

// checkAutolock locks an unlocked wallet that has seen no activity for
// a.autolock: the keys are dropped, mining goes on to the same address, and
// the page asks for the password again. It reports whether it locked.
func (a *app) checkAutolock(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.autolock <= 0 || a.w == nil || now.Sub(a.lastActive) < a.autolock {
		return false
	}
	a.w = nil
	a.idleLocked = true
	log.Printf("wallet locked after %s without activity; mining continues", a.autolock)
	return true
}

// Errors of the password check before a spend; the page shows them inside
// the confirm dialog, so their wording is matched there.
var (
	errPasswordMissing = errors.New("enter your wallet password to confirm")
	errWrongPassword   = errors.New("wrong password")
)

// verifyPassword checks the wallet password against the wallet file. It
// decrypts a copy of the store, so the secret does not stay in a.store.
func (a *app) verifyPassword(password string) error {
	a.mu.Lock()
	store := a.store
	a.mu.Unlock()
	if store == nil {
		return errors.New("no wallet yet")
	}
	if password == "" {
		return errPasswordMissing
	}
	probe := *store
	if err := probe.Unlock(password); err != nil {
		if errors.Is(err, wallet.ErrWrongPassword) {
			return errWrongPassword
		}
		return err
	}
	return nil
}

func (a *app) walletUnlock(password string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return errors.New("no wallet yet")
	}
	if a.w != nil {
		return nil
	}
	w, err := wallet.Open(filepath.Join(a.dir, "wallet.json"), a.params, password)
	if err != nil {
		if errors.Is(err, wallet.ErrWrongPassword) {
			return errors.New("wrong password")
		}
		return err
	}
	_, err = a.adoptWallet(w, "")
	return err
}

// assetRow is one non-ARG holding as the interface shows it.
type assetRow struct {
	Asset      string           `json:"asset"`
	Symbol     string           `json:"symbol"`
	Display    string           `json:"display"`
	Confirmed  consensus.Amount `json:"confirmed"`
	Locked     consensus.Amount `json:"locked"`
	UARG       bool             `json:"uarg"`       // the bridge's own uARG: swappable, paid out one for one
	WARG       bool             `json:"warg"`       // the swap service's wARG (its id checked against the issuer)
	USD        bool             `json:"usd"`        // show a dollar value: uARG, and the bridge answered last time
	Unverified bool             `json:"unverified"` // calls itself UARG, WARG or ARG but is not that asset (or its id is not known yet)
}

// Anyone can issue an asset with the symbol UARG. Only the asset id the
// bridge names in its status is the real uARG; the app takes that id from the
// bridge and never goes by the symbol.

// uargStatus returns the bridge's uARG asset id as last announced in this run
// ("" when the bridge has not answered yet) and whether the latest answer
// from the bridge confirmed it, which is when a dollar value is shown.
func (a *app) uargStatus() (genuine string, live bool) {
	a.bridgeMu.Lock()
	defer a.bridgeMu.Unlock()
	return a.bridgeAsset, a.bridgeAsset != "" && a.bridgeView.Asset == a.bridgeAsset
}

// assetDisplay names an asset for the interface: "uARG" only for the
// bridge's asset id, "wARG" only for the swap service's wARG id (warg), and
// "UARG (unverified)", "WARG (unverified)" or "ARG (unverified)" for any
// other asset with one of the house symbols; the rest show their on-chain
// symbol (or a short id).
func assetDisplay(id, symbol, genuine, warg string) (display string, uarg, isWARG, unverified bool) {
	if genuine != "" && strings.EqualFold(id, genuine) {
		if symbol == "" || symbol == "UARG" {
			return "uARG", true, false, false
		}
		return ui.DisplaySymbol(symbol), true, false, false
	}
	if warg != "" && strings.EqualFold(id, warg) {
		return "wARG", false, true, false
	}
	switch s := strings.ToUpper(symbol); s {
	case "UARG", "WARG", "ARG":
		return s + " (unverified)", false, false, true
	}
	if display = symbol; display == "" {
		display = id
		if len(display) > 8 {
			display = display[:8] + "…"
		}
	}
	return display, false, false, false
}

func assetView(balances []wallet.AssetBalance, genuine string, live bool, warg string) []assetRow {
	out := []assetRow{}
	for _, b := range balances {
		display, uarg, isWARG, unverified := assetDisplay(b.Asset, b.Symbol, genuine, warg)
		out = append(out, assetRow{Asset: b.Asset, Symbol: b.Symbol, Display: display, Confirmed: b.Confirmed, Locked: b.Locked,
			UARG: uarg, WARG: isWARG, USD: uarg && live, Unverified: unverified})
	}
	return out
}

// walletMnemonic reveals the recovery phrase only after the password is
// confirmed again, so an unlocked but unattended app does not give it away.
func (a *app) walletMnemonic(password string) (any, error) {
	a.mu.Lock()
	store, w := a.store, a.w
	a.mu.Unlock()
	if store == nil || w == nil {
		return nil, errors.New("unlock the wallet first")
	}
	if password == "" {
		return nil, errors.New("enter your password to reveal the phrase")
	}
	if err := a.verifyPassword(password); err != nil {
		return nil, err
	}
	return w.Mnemonic(), nil
}

func parseFeeRate(feerate string) (consensus.Amount, error) {
	if feerate == "" {
		return 1, nil
	}
	v, err := strconv.ParseInt(feerate, 10, 64)
	if err != nil || v < 1 || v > 1_000_000 {
		return 0, errors.New("fee rate must be a whole number of units per byte, from 1 to 1000000")
	}
	return consensus.Amount(v), nil
}

// pickAsset picks the wallet holding named by id or symbol. An empty query,
// or "uARG" in any case, means the bridge's uARG and only that id (genuine):
// an asset that merely calls itself UARG is reachable by its id alone.
func pickAsset(held []wallet.AssetBalance, q, genuine string) (wallet.AssetBalance, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" || q == "uarg" {
		if genuine == "" {
			return wallet.AssetBalance{}, errors.New("the bridge has not confirmed which asset is its uARG yet (is it reachable?); try again shortly")
		}
		for _, b := range held {
			if strings.EqualFold(b.Asset, genuine) {
				return b, nil
			}
		}
		return wallet.AssetBalance{}, errors.New("this wallet holds no uARG")
	}
	for _, b := range held {
		if strings.EqualFold(b.Asset, q) {
			return b, nil
		}
	}
	var hits []wallet.AssetBalance
	for _, b := range held {
		if strings.EqualFold(b.Symbol, "UARG") {
			continue // never by symbol: see above
		}
		if strings.EqualFold(b.Symbol, q) || strings.EqualFold(ui.DisplaySymbol(b.Symbol), q) {
			hits = append(hits, b)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return wallet.AssetBalance{}, errors.New("this wallet does not hold that asset")
	default:
		return wallet.AssetBalance{}, errors.New("several assets share that symbol here; pick one by id")
	}
}

func (a *app) walletSendAsset(asset, to, amount, feerate, password string) (any, error) {
	amt, err := wallet.ParseAmount(amount)
	if err != nil {
		return nil, errors.New("enter a valid amount, for example 1.25")
	}
	fr, err := parseFeeRate(feerate)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(asset) == "" {
		asset = "uarg" // an empty asset means uARG (see pickAsset), never ARG
	}
	if err := a.checkSendTo(to); err != nil {
		return nil, err
	}
	if err := a.verifyPassword(password); err != nil {
		return nil, err
	}
	genuine, _ := a.uargStatus()
	return a.withWallet(func(wl *wallet.Wallet) (any, error) {
		if _, err := wl.Sync(); err != nil {
			return nil, err
		}
		b, err := pickAsset(wl.AssetBalances(), asset, genuine)
		if err != nil {
			return nil, err
		}
		txid, err := wl.SendAsset(b.Asset, strings.ToLower(to), amt, fr)
		if err != nil {
			return nil, friendlySendError(err)
		}
		return map[string]string{"txid": txid}, nil
	})
}

// ---- swap addresses ----
//
// A swap address (bridge/swapaddr.go) names a payout: uARG sent to it is paid
// out by the bridge as the named token on the named chain to the named EVM
// address, with no memo. Nobody holds its key, so anything else sent there
// is lost for good. Send is for transfers between Argonmine addresses only:
// wallet/send and wallet/send-asset refuse every swap address, and an address
// that merely starts like one. Only wallet/swap pays a swap address, and
// only with the bridge's own uARG (by asset id), for a chain and token the
// bridge lists right now, within what it can pay.

var (
	errSwapUseSwap  = errors.New("this is a swap address; use Swap")
	errSwapOnlyUARG = errors.New("only the bridge's uARG can be swapped")
	errSwapInvalid  = errors.New("this address starts like a swap address but is not a valid one; anything sent there would be lost, so nothing was sent")
	errSwapNoList   = errors.New("the bridge's list of payout chains is not available right now, so the swap cannot be checked; nothing was sent, try again shortly")
	errSwapNoUARG   = errors.New("the bridge has not confirmed which asset is its uARG (is it reachable?), so nothing was sent; try again shortly")
)

// swapPrefix is the start every swap address payload shares: three magic
// bytes, the last one's low four bits being the layout version. It is read
// off a real swap payload rather than copied from the bridge package.
var swapPrefix = func() [3]byte {
	p, err := bridge.SwapPayload(bridge.SwapDest{ChainID: 1, Symbol: "USDC", To: [20]byte{1}})
	if err != nil {
		panic(err)
	}
	return [3]byte{p[0], p[1], p[2] & 0xf0}
}()

// swapRecipient reports whether to is a swap address and, if so, the payout
// it names. An address that carries the swap prefix but is not a canonical
// swap address of a known layout is an error: it can never be paid out.
func swapRecipient(hrp, to string) (bridge.SwapDest, bool, error) {
	to = strings.ToLower(strings.TrimSpace(to))
	if d, ok := bridge.ParseSwapAddress(hrp, to); ok {
		return d, true, nil
	}
	if out, err := wallet.DecodeAddress(hrp, to); err == nil && len(out.Payload) == 32 &&
		out.Payload[0] == swapPrefix[0] && out.Payload[1] == swapPrefix[1] && out.Payload[2]&0xf0 == swapPrefix[2] {
		return bridge.SwapDest{}, false, errSwapInvalid
	}
	return bridge.SwapDest{}, false, nil
}

// swapPayout finds the chain and token a swap payout names on the bridge's
// list and checks that the bridge can pay amt (uARG units) there now: at
// least the token's minimum withdrawal, within the token's reserve, what the
// vault can release right now, and the largest payout tier. (amt 0 checks
// the route alone.)
func swapPayout(view bridgeChainsView, d bridge.SwapDest, amt consensus.Amount) (bridgeChain, bridgeToken, error) {
	if len(view.Chains) == 0 {
		return bridgeChain{}, bridgeToken{}, errSwapNoList
	}
	for _, c := range view.Chains {
		if c.ChainID != d.ChainID {
			continue
		}
		var hits []bridgeToken
		for _, t := range c.Tokens {
			if strings.EqualFold(t.Symbol, d.Symbol) {
				hits = append(hits, t)
			}
		}
		switch len(hits) {
		case 0:
			return c, bridgeToken{}, fmt.Errorf("the bridge does not pay %s on %s; nothing was sent", d.Symbol, c.Name)
		case 1:
		default:
			return c, bridgeToken{}, fmt.Errorf("the bridge lists more than one %s on %s, so a swap address cannot name one; nothing was sent", d.Symbol, c.Name)
		}
		t := hits[0]
		if c.Unavailable {
			return c, t, fmt.Errorf("payouts on %s are paused right now; nothing was sent, try again later", c.Name)
		}
		if min := belowMinRelease(t, amt); min != 0 {
			return c, t, fmt.Errorf("the smallest withdrawal of %s on %s is %s uARG; nothing was sent", t.Symbol, c.Name, formatReserve(min))
		}
		if uint64(amt) > t.ReserveUnits {
			return c, t, fmt.Errorf("the bridge holds only %s %s on %s; nothing was sent", formatReserve(t.ReserveUnits), t.Symbol, c.Name)
		}
		if t.AvailableNow != "" {
			if now := tokenUnitsToArg(t.AvailableNow, t.Decimals); uint64(amt) > now {
				return c, t, fmt.Errorf("at most %s %s can be paid on %s right now; nothing was sent", formatReserve(now), t.Symbol, c.Name)
			}
		}
		if n := len(view.Tiers); n > 0 && uint64(amt) > view.Tiers[n-1].UpTo {
			return c, t, fmt.Errorf("at most %s uARG can be swapped at once; nothing was sent", formatReserve(view.Tiers[n-1].UpTo))
		}
		return c, t, nil
	}
	return bridgeChain{}, bridgeToken{}, fmt.Errorf("the bridge does not pay %s on chain %d; nothing was sent", d.Symbol, d.ChainID)
}

// belowMinRelease is, when amt uARG units pay less than t's minimum
// withdrawal (tokens[].min_release of /api/status, gross token units), the
// smallest amount that pays it; 0 when amt is enough or the bridge names no
// minimum. The payout is converted exactly as the bridge converts it
// (bridge.TokenFromUnits): below the minimum the bridge never sends it, and
// the uARG paid stays with the guardians.
func belowMinRelease(t bridgeToken, amt consensus.Amount) uint64 {
	min, ok := new(big.Int).SetString(t.MinRelease, 10)
	if amt <= 0 || !ok || min.Sign() <= 0 || t.Decimals < 0 || t.Decimals > 36 {
		return 0
	}
	if bridge.TokenFromUnits(amt, t.Decimals).Cmp(min) >= 0 {
		return 0
	}
	u := new(big.Int)
	if t.Decimals >= 8 {
		// the smallest u with u * 10^(D-8) >= min
		step := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(t.Decimals-8)), nil)
		u.Add(min, step).Sub(u, big.NewInt(1)).Quo(u, step)
	} else {
		u.Mul(min, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(8-t.Decimals)), nil))
	}
	if !u.IsUint64() {
		return math.MaxUint64
	}
	return u.Uint64()
}

// checkSendTo is the Send rule: a transfer goes to an Argonmine address of a
// person, never to a swap address (that is Swap) nor to an address that only
// looks like one.
func (a *app) checkSendTo(to string) error {
	_, isSwap, err := swapRecipient(a.params.HRP, to)
	if err != nil {
		return err
	}
	if isSwap {
		return errSwapUseSwap
	}
	return nil
}

// swapRoute picks the chain and token of a swap out from the bridge's list:
// the token by its contract address (0x…) or by its symbol.
func swapRoute(view bridgeChainsView, chainID uint64, token string) (bridgeChain, bridgeToken, error) {
	if len(view.Chains) == 0 {
		return bridgeChain{}, bridgeToken{}, errSwapNoList
	}
	token = strings.TrimSpace(token)
	for _, c := range view.Chains {
		if c.ChainID != chainID {
			continue
		}
		var hits []bridgeToken
		if strings.HasPrefix(strings.ToLower(token), "0x") {
			addr, err := bridge.ParseAddress(token)
			if err != nil {
				return c, bridgeToken{}, errors.New("the token must be a contract address (0x followed by 40 hex characters) or a symbol")
			}
			for _, t := range c.Tokens {
				if t.Address == bridge.HexAddress(addr) {
					hits = append(hits, t)
				}
			}
		} else {
			for _, t := range c.Tokens {
				if strings.EqualFold(t.Symbol, token) {
					hits = append(hits, t)
				}
			}
		}
		if len(hits) != 1 {
			return c, bridgeToken{}, fmt.Errorf("the bridge does not pay that token on %s; choose another", c.Name)
		}
		return c, hits[0], nil
	}
	return bridgeChain{}, bridgeToken{}, fmt.Errorf("the bridge does not pay out on chain %d; choose another", chainID)
}

// swapTarget is the swap address that pays t on c to the EVM address to.
// It is decoded back before use: a swap address that does not name exactly
// this payout is never paid.
func swapTarget(hrp string, c bridgeChain, t bridgeToken, to [20]byte) (string, bridge.SwapDest, error) {
	dest := bridge.SwapDest{ChainID: c.ChainID, Symbol: strings.ToUpper(strings.TrimSpace(t.Symbol)), To: to}
	addr, err := bridge.SwapAddress(hrp, dest)
	if err != nil {
		return "", dest, fmt.Errorf("%s on %s cannot be paid through a swap address; nothing was sent", t.Symbol, c.Name)
	}
	if back, ok := bridge.ParseSwapAddress(hrp, addr); !ok || back != dest {
		return "", dest, errors.New("the swap address for this payout did not decode back to it; nothing was sent")
	}
	return addr, dest, nil
}

// walletSwap swaps uARG out: it sends exactly amount of the bridge's own
// uARG (by asset id) to the swap address that names the chain, token and
// EVM recipient, and the bridge pays the token out from there. The chain and
// token must be on the bridge's current list and the amount within what it
// can pay; the payout address is checked (EIP-55 when mixed case) and the
// wallet password is required.
func (a *app) walletSwap(amount, evm, chainID, token, feerate, password string) (any, error) {
	amt, err := wallet.ParseAmount(amount)
	if err != nil || amt <= 0 {
		return nil, errors.New("enter a valid amount, for example 1.25")
	}
	evm = strings.TrimSpace(evm)
	if err := checkEVMAddress(evm); err != nil {
		return nil, err
	}
	to, _ := bridge.ParseAddress(evm)
	fr, err := parseFeeRate(feerate)
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseUint(strings.TrimSpace(chainID), 10, 64)
	if err != nil || id == 0 {
		return nil, errors.New("choose the chain to be paid on")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("choose the token to be paid")
	}
	view := a.bridgeChains()
	c, t, err := swapRoute(view, id, token)
	if err != nil {
		return nil, err
	}
	addr, dest, err := swapTarget(a.params.HRP, c, t, to)
	if err != nil {
		return nil, err
	}
	if _, _, err := swapPayout(view, dest, amt); err != nil {
		return nil, err
	}
	genuine, _ := a.uargStatus()
	if genuine == "" {
		return nil, errSwapNoUARG
	}
	if err := a.verifyPassword(password); err != nil {
		return nil, err
	}
	return a.withWallet(func(wl *wallet.Wallet) (any, error) {
		if _, err := wl.Sync(); err != nil {
			return nil, err
		}
		b, err := pickAsset(wl.AssetBalances(), "", genuine)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(b.Asset, genuine) {
			return nil, errSwapOnlyUARG
		}
		// The payout is checked again against the bridge's list as it is now.
		if _, _, err := swapPayout(a.bridgeChains(), dest, amt); err != nil {
			return nil, err
		}
		txid, err := wl.SendAsset(b.Asset, addr, amt, fr)
		if err != nil {
			return nil, friendlySendError(err)
		}
		return map[string]any{"txid": txid, "address": addr, "chain_id": c.ChainID, "chain": c.Name, "token": t.Address,
			"symbol": dest.Symbol, "to": eip55(to), "amount": wallet.FormatAmount(amt)}, nil
	})
}

// addressCheck answers wallet/validate: whether an address is valid on this
// network and, for a swap address, the payout it names and whether the
// bridge pays it.
type addressCheck struct {
	Valid bool       `json:"valid"`
	Swap  *swapCheck `json:"swap,omitempty"`
	Error string     `json:"error,omitempty"` // why nothing can be sent to it
}

type swapCheck struct {
	ChainID       uint64 `json:"chain_id"`
	Chain         string `json:"chain"`  // the bridge's name for the chain; "" when it does not list it
	Symbol        string `json:"symbol"` // USDC, USDT
	To            string `json:"to"`     // EVM recipient, EIP-55
	Token         string `json:"token"`  // the listed token's contract address; "" when not listed
	Decimals      int    `json:"decimals"`
	ReleaseFeeBps uint16 `json:"release_fee_bps"`
	Listed        bool   `json:"listed"`          // the bridge pays this token on this chain
	Error         string `json:"error,omitempty"` // why the bridge would not pay it
}

func (a *app) checkAddress(addr string) addressCheck {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if _, err := wallet.DecodeAddress(a.params.HRP, addr); err != nil {
		return addressCheck{}
	}
	d, isSwap, err := swapRecipient(a.params.HRP, addr)
	if err != nil {
		return addressCheck{Valid: true, Error: err.Error()}
	}
	if !isSwap {
		return addressCheck{Valid: true}
	}
	sw := &swapCheck{ChainID: d.ChainID, Symbol: d.Symbol, To: eip55(d.To)}
	c, t, perr := swapPayout(a.bridgeChains(), d, 0)
	sw.Chain = c.Name
	if t.Address != "" {
		sw.Token, sw.Decimals, sw.ReleaseFeeBps = t.Address, t.Decimals, c.ReleaseFeeBps
	}
	if perr != nil {
		sw.Error = perr.Error()
	} else {
		sw.Listed = true
	}
	return addressCheck{Valid: true, Swap: sw}
}

// checkEVMAddress accepts a 0x address of 40 hex characters in lower case,
// upper case or EIP-55 mixed case. Mixed case carries a checksum, which must
// be right: a typo in a payout address cannot be undone.
func checkEVMAddress(s string) error {
	s = strings.TrimSpace(s)
	if len(s) != 42 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return errors.New("the payout address must be an EVM address: 0x followed by 40 hex characters")
	}
	a, err := bridge.ParseAddress(s)
	if err != nil {
		return errors.New("the payout address must be an EVM address: 0x followed by 40 hex characters")
	}
	digits := s[2:]
	if strings.ToLower(digits) == digits || strings.ToUpper(digits) == digits {
		return nil // no checksum to check
	}
	if eip55(a)[2:] != digits {
		return errors.New("the payout address has a wrong checksum (its upper and lower case letters do not match, EIP-55): check it for a typo, or paste it again from your wallet")
	}
	return nil
}

// eip55 renders an address with its EIP-55 checksum: a hex letter is upper
// case when the matching nibble of keccak256(lower-case hex) is 8 or more.
func eip55(a [20]byte) string {
	lower := hex.EncodeToString(a[:])
	h := bridge.Keccak256([]byte(lower))
	out := []byte(lower)
	for i, c := range out {
		if c < 'a' || c > 'f' {
			continue
		}
		nib := h[i/2] >> 4
		if i%2 == 1 {
			nib = h[i/2] & 0x0f
		}
		if nib >= 8 {
			out[i] = c - 'a' + 'A'
		}
	}
	return "0x" + string(out)
}

// formatReserve renders a reserve in uARG units the way the form does: at
// least two decimals, no trailing zeros beyond that ("5.00", "12.5" -> "12.50").
func formatReserve(units uint64) string {
	s := wallet.FormatAmount(consensus.Amount(units))
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return s + ".00"
	}
	s = strings.TrimRight(s, "0")
	for len(s)-i-1 < 2 {
		s += "0"
	}
	return s
}

// bridgeToken is one stablecoin the bridge pays out and takes in, as the Swap
// screen shows it.
type bridgeToken struct {
	Symbol       string `json:"symbol"`
	Address      string `json:"address"`
	Decimals     int    `json:"decimals"`
	ReserveUnits uint64 `json:"reserve_units"` // the bridge's reserve in uARG units (1e8 per whole token)
	MinDeposit   string `json:"min_deposit"`   // smallest deposit, in token units (decimal string); "" when not said
	AvailableNow string `json:"available_now"` // token units the vault can pay out right now (decimal string); "" when not said
	MinRelease   string `json:"min_release"`   // smallest payout the bridge sends, gross token units (decimal string); "" when not said
}

// bridgeChain is one EVM chain the bridge pays out on.
type bridgeChain struct {
	Name          string        `json:"name"`
	ChainID       uint64        `json:"chain_id"`
	Explorer      string        `json:"explorer"`
	ReleaseFeeBps uint16        `json:"release_fee_bps"` // kept by the vault on every payout, in basis points
	DepositFeeBps uint16        `json:"deposit_fee_bps"` // kept by the vault on every deposit, in basis points
	Confirmations uint64        `json:"confirmations"`   // blocks the guardians wait before issuing a deposit
	Finality      string        `json:"finality"`        // "finalized" when deposits wait for the chain's finality
	Head          uint64        `json:"head"`            // last block the guardians scanned on this chain
	Router        bool          `json:"router"`          // deposit addresses are available on this chain
	Unavailable   bool          `json:"unavailable"`     // the guardians cannot reach the chain, or its vault is paused
	Vault         string        `json:"vault"`           // the vault the user's own EVM wallet deposits into (Swap in); "" when unknown
	RPC           string        `json:"rpc"`             // the chain's public RPC, offered to the user's wallet when it lacks the chain; "" when none
	NativeSymbol  string        `json:"native_symbol"`   // the chain's gas coin, for that same offer
	Tokens        []bridgeToken `json:"tokens"`
}

// bridgeTier is one step of the payout depth: a swap out of up to UpTo
// uARG units waits Confirmations blocks on this chain.
type bridgeTier struct {
	UpTo          uint64 `json:"up_to"`
	Confirmations uint32 `json:"confirmations"`
}

// bridgeChainsView is the answer to bridge/chains. Error is set when the list
// could not be fetched (then Chains is empty) and the payout has to go to the
// bridge's default chain and token.
type bridgeChainsView struct {
	Chains          []bridgeChain `json:"chains"`
	L1Confirmations uint32        `json:"l1_confirmations"`      // blocks on this chain before a swap out is paid
	Tiers           []bridgeTier  `json:"l1_confirmation_tiers"` // payout depth by amount, smallest first
	Asset           string        `json:"asset"`                 // the bridge's uARG asset id (64 hex); "" when the bridge did not answer
	Error           string        `json:"error"`
}

// Freshness of the cached bridge status: a good answer is reused for 30 s, a
// failure for 10 s so a bridge that comes back is noticed quickly. Nothing is
// ever served older than 60 s.
const (
	bridgeCacheOK   = 30 * time.Second
	bridgeCacheFail = 10 * time.Second
)

// bridgeChains fetches the bridge daemon's status (its chains and tokens with
// their reserves, and its uARG asset id) through a short cache, so the form
// can ask on every open. The cache lock is not held during the fetch, so the
// status poll never waits on the network.
func (a *app) bridgeChains() bridgeChainsView {
	fresh := func() (bridgeChainsView, bool) {
		a.bridgeMu.Lock()
		defer a.bridgeMu.Unlock()
		ttl := bridgeCacheOK
		if a.bridgeView.Error != "" {
			ttl = bridgeCacheFail
		}
		return a.bridgeView, !a.bridgeAt.IsZero() && time.Since(a.bridgeAt) < ttl
	}
	if v, ok := fresh(); ok {
		return v
	}
	a.bridgeFetch.Lock()
	defer a.bridgeFetch.Unlock()
	if v, ok := fresh(); ok { // fetched by another caller meanwhile
		return v
	}
	v := fetchBridgeChains(siteLinks.Bridge)
	a.bridgeMu.Lock()
	a.bridgeView, a.bridgeAt = v, time.Now()
	if v.Asset != "" {
		a.bridgeAsset = v.Asset
	}
	a.bridgeMu.Unlock()
	return v
}

// fetchBridgeChains reads <bridgeURL>/api/status and keeps what the Swap
// screen needs. Fields the daemon does not send are tolerated: a token without
// reserve_units takes its reserve from the decimal "reserve" string, a chain
// without a usable id or token is left out.
func fetchBridgeChains(bridgeURL string) bridgeChainsView {
	view := bridgeChainsView{Chains: []bridgeChain{}}
	bridgeURL = strings.TrimRight(strings.TrimSpace(bridgeURL), "/")
	if bridgeURL == "" {
		view.Error = "no bridge configured"
		return view
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(bridgeURL + "/api/status")
	if err != nil {
		view.Error = "bridge unreachable: " + err.Error()
		return view
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		view.Error = fmt.Sprintf("bridge unreachable: HTTP %d", resp.StatusCode)
		return view
	}
	var raw struct {
		Asset           string       `json:"asset"`
		L1Confirmations uint32       `json:"l1_confirmations"`
		Tiers           []bridgeTier `json:"l1_confirmation_tiers"`
		Chains          []struct {
			Name          string `json:"name"`
			ChainID       uint64 `json:"chain_id"`
			Explorer      string `json:"explorer"`
			ReleaseFeeBps uint16 `json:"release_fee_bps"`
			DepositFeeBps uint16 `json:"deposit_fee_bps"`
			Confirmations uint64 `json:"confirmations"`
			Finality      string `json:"finality"`
			Head          uint64 `json:"head"`
			Router        string `json:"router"`
			Vault         string `json:"vault"`
			RPC           string `json:"rpc"`
			NativeSymbol  string `json:"native_symbol"`
			Error         string `json:"error"`
			VaultPaused   bool   `json:"vault_paused"`
			Tokens        []struct {
				Symbol       string `json:"symbol"`
				Address      string `json:"address"`
				Decimals     int    `json:"decimals"`
				Reserve      string `json:"reserve"`
				ReserveUnits uint64 `json:"reserve_units"`
				MinDeposit   string `json:"min_deposit"`
				AvailableNow string `json:"available_now"`
				MinRelease   string `json:"min_release"`
			} `json:"tokens"`
		} `json:"chains"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		view.Error = "bridge status unreadable: " + err.Error()
		return view
	}
	view.L1Confirmations = raw.L1Confirmations
	for _, t := range raw.Tiers {
		if t.UpTo > 0 && t.Confirmations > 0 && len(view.Tiers) < 16 {
			view.Tiers = append(view.Tiers, t)
		}
	}
	if id := strings.ToLower(strings.TrimSpace(raw.Asset)); len(id) == 64 && strings.Trim(id, "0123456789abcdef") == "" {
		view.Asset = id
	}
	for _, c := range raw.Chains {
		if c.ChainID == 0 {
			continue
		}
		ch := bridgeChain{Name: c.Name, ChainID: c.ChainID, Explorer: safeExplorerURL(c.Explorer), ReleaseFeeBps: c.ReleaseFeeBps, DepositFeeBps: c.DepositFeeBps,
			Confirmations: c.Confirmations, Finality: c.Finality, Head: c.Head, Unavailable: c.Error != "" || c.VaultPaused, Tokens: []bridgeToken{}}
		if r, err := bridge.ParseAddress(c.Router); err == nil && r != [20]byte{} {
			ch.Router = true
		}
		if v, err := bridge.ParseAddress(c.Vault); err == nil && v != [20]byte{} {
			ch.Vault = bridge.HexAddress(v)
		}
		ch.RPC = safeExplorerURL(c.RPC)
		if sym := strings.TrimSpace(c.NativeSymbol); len(sym) <= 8 && strings.Trim(strings.ToUpper(sym), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			ch.NativeSymbol = sym
		}
		if ch.Name == "" {
			ch.Name = "chain " + strconv.FormatUint(c.ChainID, 10)
		}
		for _, t := range c.Tokens {
			addr, err := bridge.ParseAddress(t.Address)
			if err != nil || addr == [20]byte{} {
				continue
			}
			tok := bridgeToken{Symbol: t.Symbol, Address: bridge.HexAddress(addr), Decimals: t.Decimals, ReserveUnits: t.ReserveUnits,
				MinDeposit: decimalOrEmpty(t.MinDeposit), AvailableNow: decimalOrEmpty(t.AvailableNow), MinRelease: decimalOrEmpty(t.MinRelease)}
			if tok.Symbol == "" {
				tok.Symbol = tok.Address[:10] + "…"
			}
			if tok.ReserveUnits == 0 && t.Reserve != "" {
				tok.ReserveUnits = tokenUnitsToArg(t.Reserve, t.Decimals)
			}
			ch.Tokens = append(ch.Tokens, tok)
		}
		if len(ch.Tokens) == 0 {
			continue
		}
		view.Chains = append(view.Chains, ch)
	}
	if len(view.Chains) == 0 {
		view.Error = "the bridge lists no chains"
	}
	return view
}

// bridgeActivityView is what the Wallet page's "Bridge activity" card shows:
// deposits paid to any of this wallet's addresses and the payouts of its own
// burns, with what is needed to draw progress (each chain's scanned head and
// confirmation depth, this chain's height and the payout depth).
type bridgeActivityView struct {
	Deposits        json.RawMessage `json:"deposits"`
	Redemptions     json.RawMessage `json:"redemptions"`
	Chains          []bridgeChain   `json:"chains"`
	L1Confirmations uint32          `json:"l1_confirmations"`
	Tip             uint32          `json:"tip"`
	Error           string          `json:"error"`
}

// bridgeActivity asks the bridge for this wallet's transfers. Addresses come
// from the wallet (every one handed out, plus the mining address); burns are
// the wallet's own transactions that reduced an asset balance, newest first.
func (a *app) bridgeActivity() (any, error) {
	view := bridgeActivityView{Deposits: json.RawMessage("[]"), Redemptions: json.RawMessage("[]"), Chains: []bridgeChain{}}
	bridgeURL := strings.TrimRight(strings.TrimSpace(siteLinks.Bridge), "/")
	if bridgeURL == "" {
		view.Error = "no bridge configured"
		return view, nil
	}
	a.mu.Lock()
	mining := a.miningAddr
	view.Tip = a.ch.Tip().Height
	a.mu.Unlock()
	var addrs, burns []string
	if _, err := a.withWallet(func(wl *wallet.Wallet) (any, error) {
		seen := map[string]bool{}
		for _, ad := range append(wl.Addresses(), mining) {
			if ad != "" && !seen[ad] {
				seen[ad] = true
				addrs = append(addrs, ad)
			}
		}
		hist := wl.History()
		for i := len(hist) - 1; i >= 0 && len(burns) < 50; i-- {
			for _, n := range hist[i].Assets {
				if n < 0 {
					burns = append(burns, hist[i].TxID)
					break
				}
			}
		}
		return nil, nil
	}); err != nil {
		return nil, err
	}
	chains := a.bridgeChains()
	view.Chains, view.L1Confirmations = chains.Chains, chains.L1Confirmations
	if chains.Error != "" && len(chains.Chains) == 0 {
		view.Error = chains.Error
		return view, nil
	}
	get := func(path string) (json.RawMessage, error) {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(bridgeURL + path)
		if err != nil {
			return nil, errors.New("bridge unreachable: " + err.Error())
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil || resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bridge unreachable: HTTP %d", resp.StatusCode)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, errors.New("bridge answer unreadable")
		}
		return json.RawMessage(body), nil
	}
	if len(addrs) > 0 {
		d, err := get("/api/deposits?to=" + url.QueryEscape(strings.Join(addrs, ",")))
		if err != nil {
			view.Error = err.Error()
			return view, nil
		}
		view.Deposits = d
	}
	if len(burns) > 0 {
		r, err := get("/api/redemptions?txids=" + url.QueryEscape(strings.Join(burns, ",")))
		if err != nil {
			view.Error = err.Error()
			return view, nil
		}
		view.Redemptions = mergeSwapPayments(r, burns, get)
	}
	return view, nil
}

// mergeSwapPayments adds to the redemptions found by id the payments this
// wallet made to swap addresses: the bridge may file those under
// "txid:vout", so they are picked from its recent list by transaction id.
func mergeSwapPayments(byID json.RawMessage, txids []string, get func(string) (json.RawMessage, error)) json.RawMessage {
	all, err := get("/api/redemptions")
	if err != nil {
		return byID
	}
	want := map[string]bool{}
	for _, t := range txids {
		want[strings.ToLower(t)] = true
	}
	key := func(raw json.RawMessage) (string, string) {
		var k struct {
			TxID string  `json:"txid"`
			Vout *uint32 `json:"vout"`
		}
		json.Unmarshal(raw, &k)
		v := ""
		if k.Vout != nil {
			v = strconv.FormatUint(uint64(*k.Vout), 10)
		}
		return strings.ToLower(k.TxID), v
	}
	var have, more []json.RawMessage
	json.Unmarshal(byID, &have)
	json.Unmarshal(all, &more)
	if have == nil {
		have = []json.RawMessage{}
	}
	seen := map[string]bool{}
	for _, r := range have {
		t, v := key(r)
		seen[t+":"+v] = true
	}
	for _, r := range more {
		if t, v := key(r); want[t] && !seen[t+":"+v] {
			seen[t+":"+v] = true
			have = append(have, r)
		}
	}
	out, err := json.Marshal(have)
	if err != nil {
		return byID
	}
	return out
}

// decimalOrEmpty keeps a decimal string of digits (token units from the
// bridge) and drops anything else.
func decimalOrEmpty(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 78 || strings.Trim(s, "0123456789") != "" {
		return ""
	}
	return s
}

// bridgeGet fetches <bridge><path> and decodes its JSON answer into out. An
// answer other than 200 becomes an error with the bridge's own message.
func bridgeGet(path string, timeout time.Duration, out any) error {
	base := strings.TrimRight(strings.TrimSpace(siteLinks.Bridge), "/")
	if base == "" {
		return errors.New("no bridge configured")
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(base + path)
	if err != nil {
		return errors.New("bridge unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return errors.New("bridge unreachable: " + err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			if len(e.Error) > 200 {
				e.Error = e.Error[:200]
			}
			return errors.New("the bridge says: " + e.Error)
		}
		return fmt.Errorf("bridge unreachable: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("bridge answer unreadable")
	}
	return nil
}

// depositAddressView is the Swap screen's deposit address for this wallet
// on one chain: USDC or USDT sent there from any wallet or exchange becomes
// uARG at the wallet's first address.
type depositAddressView struct {
	ChainID       uint64                       `json:"chain_id"`
	Chain         string                       `json:"chain"`
	To            string                       `json:"to"`
	Address       string                       `json:"address"`  // EIP-55
	Explorer      string                       `json:"explorer"` // the address on the chain's explorer, or ""
	Confirmations uint64                       `json:"confirmations"`
	DepositFeeBps uint16                       `json:"deposit_fee_bps"`
	Tokens        []bridge.DepositAddressToken `json:"tokens"`
}

// bridgeDepositAddress asks the bridge for this wallet's deposit address on
// a chain (status false) or for what has arrived there (status true). The
// address is the wallet's first one, where mining rewards go too. An answer
// for another chain or address is refused.
func (a *app) bridgeDepositAddress(chainID string, status bool) (any, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(chainID), 10, 64)
	if err != nil || id == 0 {
		return nil, errors.New("choose a chain")
	}
	a.mu.Lock()
	to := a.miningAddr
	a.mu.Unlock()
	if to == "" {
		return nil, errors.New("create or restore a wallet first")
	}
	q := "?chain=" + strconv.FormatUint(id, 10) + "&to=" + url.QueryEscape(to)
	if status {
		var st bridge.DepositAddressStatus
		if err := bridgeGet("/api/deposit-address/status"+q, 20*time.Second, &st); err != nil {
			return nil, err
		}
		if st.ChainID != id || !strings.EqualFold(st.To, to) {
			return nil, errors.New("the bridge answered for another chain or address")
		}
		if st.Balances == nil {
			st.Balances = []bridge.DepositAddressBalance{}
		}
		if st.Sweeps == nil {
			st.Sweeps = []bridge.Sweep{}
		}
		if st.Deposits == nil {
			st.Deposits = []bridge.Inbound{}
		}
		return st, nil
	}
	var info bridge.DepositAddressInfo
	if err := bridgeGet("/api/deposit-address"+q, 20*time.Second, &info); err != nil {
		return nil, err
	}
	addr, err := bridge.ParseAddress(info.Address)
	if info.ChainID != id || !strings.EqualFold(info.To, to) || err != nil || addr == [20]byte{} {
		return nil, errors.New("the bridge answered for another chain or address")
	}
	view := depositAddressView{ChainID: id, Chain: info.Chain, To: to, Address: eip55(addr), Explorer: safeExplorerURL(info.Explorer),
		Confirmations: info.Confirmations, DepositFeeBps: info.DepositFeeBps, Tokens: []bridge.DepositAddressToken{}}
	for _, t := range info.Tokens {
		if ta, err := bridge.ParseAddress(t.Address); err == nil && ta != [20]byte{} {
			t.Address, t.MinDeposit = bridge.HexAddress(ta), decimalOrEmpty(t.MinDeposit)
			view.Tokens = append(view.Tokens, t)
		}
	}
	return view, nil
}

// bridgeDeposit follows a swap in paid from the user's own EVM wallet: the
// bridge's record of that deposit transaction (an error until the bridge
// has seen it). A record for another transaction or for an address other
// than this wallet's first one is refused.
func (a *app) bridgeDeposit(tx string) (any, error) {
	tx = strings.ToLower(strings.TrimSpace(tx))
	if len(tx) != 66 || !strings.HasPrefix(tx, "0x") || strings.Trim(tx[2:], "0123456789abcdef") != "" {
		return nil, errors.New("not a transaction hash")
	}
	a.mu.Lock()
	to := a.miningAddr
	a.mu.Unlock()
	if to == "" {
		return nil, errors.New("create or restore a wallet first")
	}
	var d bridge.Inbound
	if err := bridgeGet("/api/deposit/"+tx, 10*time.Second, &d); err != nil {
		return nil, err
	}
	if !strings.EqualFold(d.EVMTx, tx) || !strings.EqualFold(d.To, to) {
		return nil, errors.New("the bridge answered for another deposit")
	}
	d.Error = clipText(d.Error, 200)
	return d, nil
}

// redemptionView is the progress of one swap out: the bridge's record of it
// (nil until the bridge has seen the payment), this chain's height and the
// height of the payment's block (0 while unconfirmed), from the wallet.
type redemptionView struct {
	Redemption *bridge.Redemption `json:"redemption"`
	Tip        uint32             `json:"tip"`
	Height     uint32             `json:"height"`
}

// bridgeRedemption follows a swap out: the payment txid to the swap address
// addr, for a payout to `to` of token on chain. The bridge is asked for the
// transaction by id first; a payment to a swap address may be filed under
// "txid:vout", so its recent list is searched too: by transaction id, by
// swap address, or by recipient, chain and token since `since`.
func (a *app) bridgeRedemption(txid, addr, to, chainID, token string, since int64) (any, error) {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if len(txid) != 64 || strings.Trim(txid, "0123456789abcdef") != "" {
		return nil, errors.New("not a transaction id")
	}
	a.mu.Lock()
	view := redemptionView{Tip: a.ch.Tip().Height}
	a.mu.Unlock()
	a.withWallet(func(wl *wallet.Wallet) (any, error) {
		for _, h := range wl.History() {
			if h.TxID == txid {
				view.Height = h.Height
			}
		}
		return nil, nil
	})
	addr, to, token = strings.ToLower(strings.TrimSpace(addr)), strings.ToLower(strings.TrimSpace(to)), strings.ToLower(strings.TrimSpace(token))
	chain, _ := strconv.ParseUint(strings.TrimSpace(chainID), 10, 64)
	var pick *bridge.Redemption
	better := func(r bridge.Redemption) {
		if pick == nil || r.Created > pick.Created {
			x := r
			pick = &x
		}
	}
	var byID []bridge.Redemption
	if err := bridgeGet("/api/redemptions?txids="+txid, 10*time.Second, &byID); err == nil {
		for _, r := range byID {
			if strings.EqualFold(r.TxID, txid) {
				better(r)
			}
		}
	}
	if pick == nil {
		var all []bridge.Redemption
		if err := bridgeGet("/api/redemptions", 10*time.Second, &all); err != nil {
			return nil, err
		}
		for _, r := range all {
			switch {
			case strings.EqualFold(r.TxID, txid):
			case addr != "" && strings.EqualFold(r.Address, addr) && r.Created >= since-600:
			case to != "" && strings.EqualFold(r.To, to) && r.Chain == chain && strings.EqualFold(r.Token, token) && since > 0 && r.Created >= since-600:
			default:
				continue
			}
			better(r)
		}
	}
	if pick != nil {
		pick.Send, pick.Error = nil, clipText(pick.Error, 200)
	}
	view.Redemption = pick
	return view, nil
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// fetchBridgeHandoff reads a redemption prepared on a phone: the browser app
// cannot sign, so it hands the amount, route and payout address over with a
// short code (10 characters such as K7M2Q-X9RT4) the bridge keeps for 15
// minutes under <bridgeURL>/api/handoff/<code> and deletes on first read. The bridge's object is passed through
// unchanged so the screen fills exactly what the phone showed; the payment
// itself still goes through wallet/swap or the asset send, and their checks.
// normalizeHandoffCode strips spaces and dashes and upper-cases the code; it
// returns "" unless 6 to 12 letters and digits remain. The bridge checks the
// exact alphabet.
func normalizeHandoffCode(code string) string {
	code = strings.ToUpper(strings.Join(strings.FieldsFunc(code, func(r rune) bool { return r == '-' || r == ' ' || r == '\t' }), ""))
	if len(code) < 6 || len(code) > 12 {
		return ""
	}
	for _, c := range code {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return ""
		}
	}
	return code
}

func fetchBridgeHandoff(bridgeURL, code string) (json.RawMessage, error) {
	code = normalizeHandoffCode(code)
	if code == "" {
		return nil, errors.New("enter the code shown on your phone")
	}
	bridgeURL = strings.TrimRight(strings.TrimSpace(bridgeURL), "/")
	if bridgeURL == "" {
		return nil, errors.New("no bridge configured")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(bridgeURL + "/api/handoff/" + code)
	if err != nil {
		return nil, errors.New("bridge unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, errors.New("bridge unreachable: " + err.Error())
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errors.New("unknown or expired code")
	default:
		return nil, fmt.Errorf("bridge unreachable: HTTP %d", resp.StatusCode)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, errors.New("bridge answer unreadable: not a JSON object")
	}
	return json.RawMessage(body), nil
}

// tokenUnitsToArg converts a token amount in its own base units (a decimal
// string, e.g. "5000000" with 6 decimals) to uARG units (8 decimals). It
// returns 0 for anything it cannot read.
func tokenUnitsToArg(units string, decimals int) uint64 {
	n, ok := new(big.Int).SetString(strings.TrimSpace(units), 10)
	if !ok || n.Sign() < 0 || decimals < 0 || decimals > 36 {
		return 0
	}
	if decimals <= 8 {
		n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(8-decimals)), nil))
	} else {
		n.Quo(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-8)), nil))
	}
	if !n.IsUint64() {
		return 0
	}
	return n.Uint64()
}

func friendlySendError(err error) error {
	switch {
	case errors.Is(err, wallet.ErrInsufficientFunds):
		return fmt.Errorf("not enough balance: %v", strings.TrimPrefix(err.Error(), "wallet: insufficient confirmed funds: "))
	case errors.Is(err, wallet.ErrAddressChecksum), errors.Is(err, wallet.ErrAddressFormat), errors.Is(err, wallet.ErrAddressHRP):
		return errors.New("that address is not valid on this network")
	case errors.Is(err, wallet.ErrAmount):
		return errors.New("enter a valid amount, for example 1.25")
	}
	return err
}

func (a *app) walletSend(to, amount, feerate, memoHex, password string) (any, error) {
	amt, err := wallet.ParseAmount(amount)
	if err != nil {
		return nil, errors.New("enter a valid amount, for example 1.25")
	}
	var memo []byte
	if memoHex != "" {
		memo, err = hex.DecodeString(memoHex)
		if err != nil || len(memo) == 0 || len(memo) > consensus.MaxNullDataLen {
			return nil, errors.New("the memo must be 1 to 80 bytes of hex")
		}
	}
	fr := consensus.Amount(1)
	if feerate != "" {
		v, err := strconv.ParseInt(feerate, 10, 64)
		if err != nil || v < 1 || v > 1_000_000 {
			return nil, errors.New("fee rate must be a whole number of units per byte, from 1 to 1000000")
		}
		fr = consensus.Amount(v)
	}
	if err := a.checkSendTo(to); err != nil {
		return nil, err
	}
	if err := a.verifyPassword(password); err != nil {
		return nil, err
	}
	return a.withWallet(func(wl *wallet.Wallet) (any, error) {
		if _, err := wl.Sync(); err != nil {
			return nil, err
		}
		txid, err := wl.SendManyMemo([]wallet.Recipient{{Address: strings.ToLower(to), Amount: amt}}, memo, fr)
		if err != nil {
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				return nil, errors.New("not enough spendable balance for that amount plus the fee")
			}
			if errors.Is(err, wallet.ErrAddressChecksum) || errors.Is(err, wallet.ErrAddressFormat) || errors.Is(err, wallet.ErrAddressHRP) {
				return nil, errors.New("that address is not valid on this network")
			}
			return nil, err
		}
		return map[string]string{"txid": txid}, nil
	})
}

// miningStart starts solo mining on this node with the given thread count
// (0 keeps the saved one). Caller must not hold a.mu.
func (a *app) miningStart(threads int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.miningAddr == "" {
		return errors.New("create or restore a wallet first, so rewards have somewhere to go")
	}
	if threads > 0 {
		a.set.Threads = threads
	}
	a.saveSettings()
	a.stopMiningLocked()
	a.lastErr = ""
	payout, err := wallet.DecodeAddress(a.params.HRP, a.miningAddr)
	if err != nil {
		return err
	}
	if a.solo == nil {
		a.solo = miner.New(a.ch, a.pool, payout, a.set.Threads, log.Printf)
		a.solo.SetTag([]byte(ui.Brand + "-app"))
		a.solo.SetBroadcast(func(*consensus.Block) {})
	} else {
		a.solo.SetThreads(a.set.Threads)
	}
	a.solo.Start()
	return nil
}

func (a *app) stopMining() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopMiningLocked()
}

func (a *app) stopMiningLocked() {
	if a.solo != nil {
		a.solo.Stop()
	}
}
