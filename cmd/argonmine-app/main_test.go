package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"argonmine/bridge"
	"argonmine/consensus"
	"argonmine/wallet"
)

const (
	testPort     = "8379"
	testToken    = "0123456789abcdef0123456789abcdef"
	testPassword = "correct horse battery"
)

func newTestApp() *app {
	return &app{params: consensus.RegTest, uiPort: testPort, token: testToken, start: time.Now(), syncReq: make(chan struct{}, 1)}
}

// withWalletApp is a test app with a real (encrypted) wallet, unlocked.
func withWalletApp(t *testing.T) *app {
	t.Helper()
	a := newTestApp()
	a.dir = t.TempDir()
	w, _, err := wallet.Create(filepath.Join(a.dir, "wallet.json"), a.params, testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	_, err = a.adoptWallet(w, "")
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// call sends one request through the app's whole handler.
func call(a *app, method, path, host string, headers map[string]string, body any) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.handler().ServeHTTP(rec, req)
	return rec
}

func apiCall(a *app, endpoint string, headers map[string]string, body map[string]any) *httptest.ResponseRecorder {
	h := map[string]string{"X-Token": testToken, "Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	return call(a, http.MethodPost, "/api/"+endpoint, "127.0.0.1:"+testPort, h, body)
}

func errOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error
}

func checkSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	for _, kv := range securityHeaders {
		if got := rec.Header().Get(kv[0]); got != kv[1] {
			t.Errorf("%s: header %s = %q, want %q", what, kv[0], got, kv[1])
		}
	}
}

func TestHostCheck(t *testing.T) {
	a := newTestApp()
	for _, host := range []string{"127.0.0.1:8379", "localhost:8379", "[::1]:8379", "LOCALHOST:8379"} {
		rec := call(a, http.MethodGet, "/", host, nil, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testToken) {
			t.Errorf("Host %q: status %d, want 200 with the page", host, rec.Code)
		}
		checkSecurityHeaders(t, rec, "page for "+host)
	}
	for _, host := range []string{"evil.example:8379", "evil.example", "127.0.0.1:8371", "127.0.0.1", "localhost", "127.0.0.1.nip.io:8379", "192.168.1.5:8379", "0.0.0.0:8379", ""} {
		for _, path := range []string{"/", "/api/status", "/assets/x"} {
			rec := call(a, http.MethodGet, path, host, map[string]string{"X-Token": testToken}, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("Host %q %s: status %d, want 403", host, path, rec.Code)
			}
			if strings.Contains(rec.Body.String(), testToken) {
				t.Errorf("Host %q %s: the token leaked", host, path)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Host %q %s: Content-Type %q, want text/plain", host, path, ct)
			}
			checkSecurityHeaders(t, rec, "403 for "+host)
		}
	}
	// -ui-allow-remote accepts IP literals with the port, never other names.
	a.allowRemote = true
	if rec := call(a, http.MethodGet, "/", "192.168.1.5:8379", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("allow-remote, IP host: status %d, want 200", rec.Code)
	}
	for _, host := range []string{"evil.example:8379", "192.168.1.5:9999"} {
		if rec := call(a, http.MethodGet, "/", host, nil, nil); rec.Code != http.StatusForbidden {
			t.Errorf("allow-remote, Host %q: status %d, want 403", host, rec.Code)
		}
	}
	// Port 80 may be left out of the Host header.
	b := newTestApp()
	b.uiPort = "80"
	if rec := call(b, http.MethodGet, "/", "127.0.0.1", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("port 80 without port in Host: status %d, want 200", rec.Code)
	}
	// Before the listener is up nothing is served.
	c := newTestApp()
	c.uiPort = ""
	if rec := call(c, http.MethodGet, "/", "127.0.0.1:", nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("no port yet: status %d, want 403", rec.Code)
	}
}

func TestAPIOriginAndFetchSite(t *testing.T) {
	a := newTestApp()
	allowed := []map[string]string{
		nil,
		{"Origin": "http://127.0.0.1:8379"},
		{"Origin": "http://localhost:8379"},
		{"Origin": "http://[::1]:8379"},
		{"Sec-Fetch-Site": "same-origin"},
		{"Sec-Fetch-Site": "none"},
		{"Origin": "http://127.0.0.1:8379", "Sec-Fetch-Site": "same-origin"},
	}
	for _, h := range allowed {
		rec := apiCall(a, "activity", h, map[string]any{})
		if rec.Code != http.StatusOK {
			t.Errorf("headers %v: status %d (%s), want 200", h, rec.Code, rec.Body.String())
		}
		checkSecurityHeaders(t, rec, fmt.Sprint("api ", h))
	}
	rejected := []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "http://evil.example:8379"},
		{"Origin": "null"},
		{"Origin": "http://127.0.0.1:9999"},
		{"Origin": "https://127.0.0.1:8379"},
		{"Origin": "http://127.0.0.1:8379.evil.example"},
		{"Origin": "http://127.0.0.1:8379/"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "http://127.0.0.1:8379", "Sec-Fetch-Site": "cross-site"},
	}
	for _, h := range rejected {
		rec := apiCall(a, "activity", h, map[string]any{})
		if rec.Code != http.StatusForbidden {
			t.Errorf("headers %v: status %d, want 403", h, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("headers %v: Content-Type %q, want text/plain", h, ct)
		}
	}
	// wallet/create must be unreachable cross-origin (it would hand out a recovery phrase).
	rec := apiCall(a, "wallet/create", map[string]string{"Origin": "https://evil.example"}, map[string]any{"password": testPassword})
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin wallet/create: status %d, want 403", rec.Code)
	}
}

func TestToken(t *testing.T) {
	a := newTestApp()
	for _, tok := range []string{"", "x", testToken[:len(testToken)-1], testToken + "0", strings.ToUpper(testToken), " " + testToken} {
		if a.tokenOK(tok) {
			t.Errorf("tokenOK(%q) = true", tok)
		}
		rec := call(a, http.MethodPost, "/api/activity", "127.0.0.1:"+testPort, map[string]string{"X-Token": tok}, map[string]any{})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, rec.Code)
		}
	}
	if !a.tokenOK(testToken) {
		t.Error("tokenOK(right token) = false")
	}
	empty := newTestApp()
	empty.token = ""
	if empty.tokenOK("") {
		t.Error("an app without a token accepted an empty one")
	}
}

func TestSpendsNeedPassword(t *testing.T) {
	a := withWalletApp(t)
	a.bridgeMu.Lock()
	a.bridgeAsset = strings.Repeat("ab", 32) // pretend the bridge answered, so a swap reaches the password check
	a.bridgeView, a.bridgeAt = testBridgeView(a.bridgeAsset), time.Now()
	a.bridgeMu.Unlock()
	to := a.miningAddr
	cases := map[string]map[string]any{
		"wallet/send":       {"to": to, "amount": "1", "feerate": "1"},
		"wallet/send-asset": {"asset": strings.Repeat("cd", 32), "to": to, "amount": "1", "feerate": "1"},
		"wallet/swap":       {"amount": "1", "evm": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "chain_id": "11155111", "token": "USDC", "feerate": "1"},
	}
	for ep, body := range cases {
		if rec := apiCall(a, ep, nil, body); rec.Code != http.StatusBadRequest || errOf(t, rec) != errPasswordMissing.Error() {
			t.Errorf("%s without password: %d %q, want 400 %q", ep, rec.Code, errOf(t, rec), errPasswordMissing)
		}
		body["password"] = "not the password"
		if rec := apiCall(a, ep, nil, body); rec.Code != http.StatusBadRequest || errOf(t, rec) != errWrongPassword.Error() {
			t.Errorf("%s with a wrong password: %d %q, want 400 %q", ep, rec.Code, errOf(t, rec), errWrongPassword)
		}
		// With the right password the request gets past the check and fails
		// only later, at the node (this test wallet has no node client).
		body["password"] = testPassword
		rec := apiCall(a, ep, nil, body)
		if msg := errOf(t, rec); strings.Contains(msg, "password") || !strings.Contains(msg, "node connection") {
			t.Errorf("%s with the right password: %d %q, want the node-client error past the check", ep, rec.Code, msg)
		}
	}
	// The password check never leaves the secret in the app's store copy.
	if !a.store.Locked() {
		t.Error("verifyPassword left a.store unlocked")
	}
}

func TestAutolock(t *testing.T) {
	a := withWalletApp(t)
	a.autolock = 15 * time.Minute
	t0 := a.lastActive
	if t0.IsZero() {
		t.Fatal("unlocking did not record activity")
	}
	if a.checkAutolock(t0.Add(14 * time.Minute)) {
		t.Fatal("locked before the idle time was up")
	}
	// Background polls do not count as activity; anything a person does does.
	a.lastActive = t0.Add(-time.Hour)
	if rec := apiCall(a, "wallet/history", nil, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("wallet/history: %d %s", rec.Code, rec.Body.String())
	}
	if !a.lastActive.Equal(t0.Add(-time.Hour)) {
		t.Error("a passive poll counted as activity")
	}
	if rec := apiCall(a, "activity", nil, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("activity: %d", rec.Code)
	}
	if time.Since(a.lastActive) > time.Minute {
		t.Error("the activity call did not count")
	}
	now := a.lastActive
	if a.checkAutolock(now.Add(15*time.Minute - time.Second)) {
		t.Error("locked a second early")
	}
	if !a.checkAutolock(now.Add(15 * time.Minute)) {
		t.Fatal("did not lock after 15 minutes idle")
	}
	if a.w != nil || !a.idleLocked {
		t.Error("the wallet is not locked after autolock")
	}
	if rec := apiCall(a, "wallet/history", nil, map[string]any{}); errOf(t, rec) != "the wallet is locked" {
		t.Errorf("after autolock wallet/history = %d %q, want locked", rec.Code, errOf(t, rec))
	}
	if a.checkAutolock(now.Add(time.Hour)) {
		t.Error("locked twice")
	}
	// Unlocking again starts a fresh idle period.
	if err := a.walletUnlock(testPassword); err != nil {
		t.Fatal(err)
	}
	if a.w == nil || a.idleLocked {
		t.Fatal("unlock did not reset the lock state")
	}
	// 0 disables the lock.
	a.autolock = 0
	if a.checkAutolock(time.Now().Add(1000 * time.Hour)) {
		t.Error("autolock 0 still locked")
	}
}

func TestSafeExplorerURL(t *testing.T) {
	cases := map[string]string{
		"https://sepolia.etherscan.io":    "https://sepolia.etherscan.io",
		"https://sepolia.etherscan.io/":   "https://sepolia.etherscan.io",
		" https://bscscan.com/ ":          "https://bscscan.com",
		"https://explorer.example/base":   "https://explorer.example/base",
		"http://127.0.0.1:8545":           "http://127.0.0.1:8545",
		"http://localhost:4000/":          "http://localhost:4000",
		"http://etherscan.io":             "",
		"javascript:alert(1)":             "",
		"JavaScript://x/%0aalert(1)":      "",
		"data:text/html,<script>":         "",
		"//evil.example":                  "",
		"https://":                        "",
		"https://user:pw@evil.example":    "",
		"https://x.example/?q=1":          "",
		"https://x.example/#frag":         "",
		"ftp://x.example":                 "",
		"":                                "",
		"https://x.example\"onmouseover=": "",
	}
	for in, want := range cases {
		if got := safeExplorerURL(in); got != want {
			t.Errorf("safeExplorerURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchBridgeChainsFiltersAndAsset(t *testing.T) {
	asset := strings.Repeat("Ab", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"asset":%q,"l1_confirmations":6,"chains":[
			{"name":"Good","chain_id":1,"explorer":"https://etherscan.io/","tokens":[{"symbol":"USDC","address":"0x9ffb481eaae54296b9b4ec1ae58564584e42b6f6","decimals":6,"reserve_units":100}]},
			{"name":"Bad","chain_id":2,"explorer":"javascript:alert(document.domain)","tokens":[{"symbol":"USDT","address":"0xb5a8458072817cb2fa7add0f8c1f5e0a7c1f2b33","decimals":6,"reserve_units":100}]}]}`, asset)
	}))
	defer srv.Close()
	v := fetchBridgeChains(srv.URL)
	if v.Error != "" || len(v.Chains) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if v.Chains[0].Explorer != "https://etherscan.io" || v.Chains[1].Explorer != "" {
		t.Errorf("explorers = %q, %q", v.Chains[0].Explorer, v.Chains[1].Explorer)
	}
	if v.Asset != strings.ToLower(asset) {
		t.Errorf("asset = %q", v.Asset)
	}
	// A malformed asset id is ignored.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"asset":"not-an-id","chains":[]}`)
	}))
	defer bad.Close()
	if v := fetchBridgeChains(bad.URL); v.Asset != "" {
		t.Errorf("malformed asset kept: %q", v.Asset)
	}
	// The app remembers the id from the last good answer, and shows a dollar
	// value only while the latest answer confirms it.
	a := newTestApp()
	siteLinks.Bridge = srv.URL
	defer func() { siteLinks.Bridge = "" }()
	a.bridgeChains()
	if g, live := a.uargStatus(); g != strings.ToLower(asset) || !live {
		t.Errorf("uargStatus = %q %v after a good answer", g, live)
	}
	siteLinks.Bridge = "http://127.0.0.1:1" // unreachable
	a.bridgeMu.Lock()
	a.bridgeAt = time.Time{}
	a.bridgeMu.Unlock()
	a.bridgeChains()
	if g, live := a.uargStatus(); g != strings.ToLower(asset) || live {
		t.Errorf("uargStatus = %q %v with the bridge down, want the remembered id and no dollar value", g, live)
	}
}

func TestUARGOnlyByBridgeID(t *testing.T) {
	genuine := strings.Repeat("aa", 32)
	fake := strings.Repeat("bb", 32)
	gold := strings.Repeat("cc", 32)
	held := []wallet.AssetBalance{
		{Asset: fake, Symbol: "UARG", Confirmed: 500},
		{Asset: genuine, Symbol: "UARG", Confirmed: 100},
		{Asset: gold, Symbol: "GOLD", Confirmed: 7},
	}
	rows := assetView(held, genuine, true, "")
	by := map[string]assetRow{}
	for _, r := range rows {
		by[r.Asset] = r
	}
	if r := by[genuine]; r.Display != "uARG" || !r.UARG || !r.USD || r.Unverified {
		t.Errorf("genuine row = %+v", r)
	}
	if r := by[fake]; r.Display != "UARG (unverified)" || r.UARG || r.USD || !r.Unverified {
		t.Errorf("fake row = %+v", r)
	}
	if r := by[gold]; r.Display != "GOLD" || r.UARG || r.USD || r.Unverified {
		t.Errorf("other row = %+v", r)
	}
	// Bridge down this time: no dollar value. Never heard from it: nothing is uARG.
	if r := assetView(held[1:2], genuine, false, "")[0]; !r.UARG || r.USD {
		t.Errorf("bridge down: %+v", r)
	}
	if r := assetView(held[1:2], "", false, "")[0]; r.UARG || !r.Unverified || r.Display != "UARG (unverified)" {
		t.Errorf("bridge unknown: %+v", r)
	}

	for _, q := range []string{"", "uarg", "UARG", "uARG"} {
		if b, err := pickAsset(held, q, genuine); err != nil || b.Asset != genuine {
			t.Errorf("pickAsset(%q) = %v %v, want the genuine id", q, b.Asset, err)
		}
		if _, err := pickAsset(held, q, ""); err == nil {
			t.Errorf("pickAsset(%q) with no bridge id succeeded", q)
		}
	}
	if b, err := pickAsset(held, fake, genuine); err != nil || b.Asset != fake {
		t.Errorf("pickAsset(fake id) = %v %v", b.Asset, err)
	}
	if b, err := pickAsset(held, "gold", genuine); err != nil || b.Asset != gold {
		t.Errorf("pickAsset(gold) = %v %v", b.Asset, err)
	}
	if _, err := pickAsset(held[:1], "", genuine); err == nil {
		t.Error("a wallet with only the fake found uARG")
	}
	if name, _, _, _ := assetDisplay(fake, "UARG", genuine, ""); name != "UARG (unverified)" {
		t.Errorf("history name of the fake = %q", name)
	}
}

func TestSwapRefusesUncheckedAndUnknown(t *testing.T) {
	a := withWalletApp(t)
	body := map[string]any{"amount": "1", "evm": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "chain_id": "11155111", "token": "USDC", "feerate": "1", "password": testPassword}
	// No list from the bridge: the chain and token cannot be checked, so nothing is sent.
	if msg := errOf(t, apiCall(a, "wallet/swap", nil, body)); !strings.Contains(msg, "cannot be checked") {
		t.Errorf("swap with an unchecked route: %q", msg)
	}
	// A list, but the bridge never named its uARG: nothing to send.
	a.bridgeMu.Lock()
	a.bridgeView, a.bridgeAt = testBridgeView(""), time.Now()
	a.bridgeMu.Unlock()
	if msg := errOf(t, apiCall(a, "wallet/swap", nil, body)); !strings.Contains(msg, "has not confirmed") {
		t.Errorf("swap with no bridge id: %q", msg)
	}
	// The old burn-with-memo route is gone: one path.
	if rec := apiCall(a, "wallet/redeem", nil, body); rec.Code != http.StatusNotFound {
		t.Errorf("wallet/redeem still answers: %d %q", rec.Code, errOf(t, rec))
	}
}

func TestEIP55(t *testing.T) {
	// The test vectors of EIP-55.
	good := []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	}
	for _, s := range good {
		if err := checkEVMAddress(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
		if err := checkEVMAddress(strings.ToLower(s)); err != nil {
			t.Errorf("lower %s: %v", s, err)
		}
		if err := checkEVMAddress("0x" + strings.ToUpper(s[2:])); err != nil {
			t.Errorf("upper %s: %v", s, err)
		}
		// One letter's case flipped breaks the checksum.
		b := []byte(s)
		for i := 2; i < len(b); i++ {
			if b[i] >= 'a' && b[i] <= 'f' {
				b[i] -= 'a' - 'A'
				break
			}
		}
		if err := checkEVMAddress(string(b)); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Errorf("flipped %s: %v, want a checksum error", string(b), err)
		}
		a, _ := bridge.ParseAddress(s)
		if got := eip55(a); got != s {
			t.Errorf("eip55 = %s, want %s", got, s)
		}
	}
	for _, s := range []string{"", "0x", "5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAe", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAedd", "0xZaAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"} {
		if err := checkEVMAddress(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	// The swap endpoint refuses a bad checksum before anything else.
	a := withWalletApp(t)
	rec := apiCall(a, "wallet/swap", nil, map[string]any{"amount": "1", "evm": "0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "chain_id": "11155111", "token": "USDC", "password": testPassword})
	if msg := errOf(t, rec); !strings.Contains(msg, "checksum") {
		t.Errorf("swap with a bad checksum: %q", msg)
	}
	rec = apiCall(a, "bridge/checkaddress", nil, map[string]any{"address": "0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"})
	var v struct {
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Valid || !strings.Contains(v.Error, "checksum") {
		t.Errorf("bridge/checkaddress bad checksum = %+v", v)
	}
}

func TestCheckUIAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8371", "localhost:8371", "[::1]:8371", "127.0.0.2:0"} {
		if err := checkUIAddr(addr, false); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":8371", "0.0.0.0:8371", "[::]:8371", "192.168.1.2:8371", "example.com:8371", "8371"} {
		if err := checkUIAddr(addr, false); err == nil {
			t.Errorf("%s accepted without -ui-allow-remote", addr)
		}
		if addr != "8371" {
			if err := checkUIAddr(addr, true); err != nil {
				t.Errorf("%s with -ui-allow-remote: %v", addr, err)
			}
		}
	}
}

func TestIndexEscapesLinks(t *testing.T) {
	old := siteLinks
	defer func() { siteLinks = old }()
	siteLinks.Explorer = `https://x.example/a"b<c>`
	siteLinks.Docs = `https://docs.example/?a=1&b=2`
	a := newTestApp()
	body := call(a, http.MethodGet, "/", "127.0.0.1:"+testPort, nil, nil).Body.String()
	if strings.Contains(body, `a"b<c>`) {
		t.Error("explorer URL inserted unescaped")
	}
	if !strings.Contains(body, "const EXPLORER = "+jsString(`https://x.example/a"b<c>`)) {
		t.Error("explorer URL not a safe JS string")
	}
	if !strings.Contains(jsString("<"), "\\u003c") {
		t.Error("jsString does not escape <")
	}
	if !strings.Contains(body, `href="https://docs.example/?a=1&amp;b=2"`) {
		t.Error("docs URL not escaped in the href")
	}
}

// Hand-off codes from the bridge are 10 characters, shown with a dash.
func TestNormalizeHandoffCode(t *testing.T) {
	for in, want := range map[string]string{
		"K7M2Q-X9RT4":     "K7M2QX9RT4",
		" k7m2q x9rt4 ":   "K7M2QX9RT4",
		"123456":          "123456",
		"12345":           "",
		"K7M2Q-X9RT4-ABC": "",
		"K7M2Q/X9RT4":     "",
		"../../api/x":     "",
	} {
		if got := normalizeHandoffCode(in); got != want {
			t.Errorf("normalizeHandoffCode(%q) = %q, want %q", in, got, want)
		}
	}
}
