package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"argonmine/wallet"
)

// Only the swap service's wARG id is shown as wARG. Before the service has
// named it, and for any other asset with that symbol (or with ARG's), the
// row says "(unverified)".
func TestWARGByServiceID(t *testing.T) {
	genuine, warg, fake := strings.Repeat("aa", 32), strings.Repeat("dd", 32), strings.Repeat("ee", 32)
	if name, uarg, isW, unv := assetDisplay(warg, "WARG", genuine, warg); name != "wARG" || uarg || !isW || unv {
		t.Errorf("genuine wARG = %q uarg=%v warg=%v unverified=%v", name, uarg, isW, unv)
	}
	if name, _, isW, unv := assetDisplay(warg, "WARG", genuine, ""); name != "WARG (unverified)" || isW || !unv {
		t.Errorf("wARG before the service answered = %q warg=%v unverified=%v", name, isW, unv)
	}
	if name, _, isW, unv := assetDisplay(fake, "WARG", genuine, warg); name != "WARG (unverified)" || isW || !unv {
		t.Errorf("look-alike wARG = %q warg=%v unverified=%v", name, isW, unv)
	}
	if name, _, _, unv := assetDisplay(fake, "arg", genuine, warg); name != "ARG (unverified)" || !unv {
		t.Errorf("an asset calling itself ARG = %q unverified=%v", name, unv)
	}
	// The id decides, whatever symbol the asset was issued with.
	if name, _, isW, _ := assetDisplay(strings.ToUpper(warg), "", genuine, warg); name != "wARG" || !isW {
		t.Errorf("genuine wARG without a symbol = %q", name)
	}
	rows := assetView([]wallet.AssetBalance{{Asset: warg, Symbol: "WARG", Confirmed: 5}, {Asset: fake, Symbol: "WARG", Confirmed: 9}}, genuine, true, warg)
	if !rows[0].WARG || rows[0].Display != "wARG" || rows[0].USD || rows[1].WARG || !rows[1].Unverified {
		t.Errorf("rows = %+v", rows)
	}
}

// bridge/deposit follows a swap in paid from the user's own EVM wallet. Only
// a 0x transaction hash is asked for, and only the bridge's record of that
// transaction paying this wallet is passed on.
func TestBridgeDeposit(t *testing.T) {
	tx := "0x" + strings.Repeat("ab", 32)
	to := "rarg1qexample"
	var answer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/deposit/"+tx {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"unknown deposit"}`)
			return
		}
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()
	old := siteLinks
	defer func() { siteLinks = old }()
	siteLinks.Bridge = srv.URL
	a := newTestApp()
	a.miningAddr = to

	for _, bad := range []string{"", "ab", strings.Repeat("ab", 32), "0x" + strings.Repeat("zz", 32), tx + "/../x"} {
		if _, err := a.bridgeDeposit(bad); err == nil || err.Error() != "not a transaction hash" {
			t.Errorf("bridgeDeposit(%q) = %v", bad, err)
		}
	}
	if _, err := a.bridgeDeposit("0x" + strings.Repeat("cd", 32)); err == nil || !strings.Contains(err.Error(), "unknown deposit") {
		t.Errorf("unknown deposit: %v", err)
	}
	answer = fmt.Sprintf(`{"chain_id":11155111,"evm_tx":%q,"to":%q,"status":"issued","amount":2475000000,"issue_tx":"%s"}`, tx, to, strings.Repeat("12", 32))
	got, err := a.bridgeDeposit(strings.ToUpper(tx))
	if err != nil {
		t.Fatalf("bridgeDeposit = %v", err)
	}
	if !strings.Contains(fmt.Sprintf("%+v", got), "issued") {
		t.Errorf("record = %+v", got)
	}
	answer = fmt.Sprintf(`{"evm_tx":%q,"to":"rarg1qsomeoneelse","status":"issued"}`, tx)
	if _, err := a.bridgeDeposit(tx); err == nil {
		t.Error("a record for another address was passed on")
	}
	answer = fmt.Sprintf(`{"evm_tx":"0x%s","to":%q,"status":"issued"}`, strings.Repeat("99", 32), to)
	if _, err := a.bridgeDeposit(tx); err == nil {
		t.Error("a record for another transaction was passed on")
	}
	// The page polls it: it does not keep the wallet from locking.
	if !passiveEndpoints["bridge/deposit"] || !passiveEndpoints["swap/native-info"] || !passiveEndpoints["swap/native-status"] || passiveEndpoints["swap/native-execute"] {
		t.Error("passive endpoints wrong for the swap routes")
	}
}

// The chain list carries what the user's EVM wallet needs for a deposit:
// the vault, and the public RPC (https only) and gas coin to offer when the
// wallet lacks the chain.
func TestBridgeChainsForWalletDeposits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"chains":[
			{"name":"Good","chain_id":1,"vault":"0xE77DBB4D59C67E85A7D8497C59D6FE0D919FBE3F","rpc":"https://rpc.example/","native_symbol":"ETH","tokens":[{"symbol":"USDC","address":"0x9ffb481eaae54296b9b4ec1ae58564584e42b6f6","decimals":6}]},
			{"name":"Odd","chain_id":2,"vault":"not an address","rpc":"javascript:alert(1)","native_symbol":"<b>","tokens":[{"symbol":"USDT","address":"0xb5a8458072817cb2fa7add0f8c1f5e0a7c1f2b33","decimals":6}]}]}`)
	}))
	defer srv.Close()
	v := fetchBridgeChains(srv.URL)
	if len(v.Chains) != 2 {
		t.Fatalf("view = %+v", v)
	}
	g, o := v.Chains[0], v.Chains[1]
	if g.Vault != "0xe77dbb4d59c67e85a7d8497c59d6fe0d919fbe3f" || g.RPC != "https://rpc.example" || g.NativeSymbol != "ETH" {
		t.Errorf("good chain = %+v", g)
	}
	if o.Vault != "" || o.RPC != "" || o.NativeSymbol != "" {
		t.Errorf("odd chain kept unsafe fields: %+v", o)
	}
}

// The page loads the shared EVM wallet script and links to no bridge site.
func TestIndexWalletPage(t *testing.T) {
	old := siteLinks
	defer func() { siteLinks = old }()
	siteLinks.Bridge = "https://bridge.example.com"
	a := newTestApp()
	body := call(a, http.MethodGet, "/", "127.0.0.1:"+testPort, nil, nil).Body.String()
	if !strings.Contains(body, `<script src="/assets/`) || !strings.Contains(body, `/evm.js"></script>`) {
		t.Error("evm.js not loaded")
	}
	if strings.Contains(body, "{{") || strings.Contains(body, "bridge.example.com") {
		t.Error("placeholder left, or a link to the bridge site")
	}
	for _, id := range []string{`id="actReceive"`, `id="actSend"`, `id="actSwap"`, `id="actCode"`, `id="flSend"`, `id="flSwap"`, `id="flRecv"`, `id="swDepConn"`, `nQuoteT`} {
		if !strings.Contains(body, id) {
			t.Errorf("%s missing", id)
		}
	}
	for _, word := range []string{"Redeem", "redeem it", "Burn", "Mint", "Bridge out"} {
		if strings.Contains(body, ">"+word) {
			t.Errorf("%q shown in the interface", word)
		}
	}
}
