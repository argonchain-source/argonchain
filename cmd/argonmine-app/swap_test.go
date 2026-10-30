package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"argonmine/bridge"
	"argonmine/consensus"
	"argonmine/wallet"
)

var (
	testUSDC  = [20]byte{0x9f, 0xfb, 0x48, 0x1e}
	testPayee = [20]byte{0x5a, 0xae, 0xb6, 0x05, 0x3f}
)

// testBridgeView is a bridge that pays USDC on Ethereum Sepolia: 100 USDC in
// reserve, payouts up to 1,000 uARG.
func testBridgeView(asset string) bridgeChainsView {
	return bridgeChainsView{Asset: asset, L1Confirmations: 6,
		Tiers: []bridgeTier{{UpTo: 50_00000000, Confirmations: 6}, {UpTo: 1000_00000000, Confirmations: 30}},
		Chains: []bridgeChain{{Name: "Ethereum Sepolia", ChainID: 11155111, ReleaseFeeBps: 100,
			Tokens: []bridgeToken{{Symbol: "USDC", Address: bridge.HexAddress(testUSDC), Decimals: 6, ReserveUnits: 100_00000000}}}}}
}

// swapApp is a test app with an unlocked wallet and a bridge that answered.
func swapApp(t *testing.T) (*app, string) {
	t.Helper()
	a := withWalletApp(t)
	genuine := strings.Repeat("ab", 32)
	a.bridgeMu.Lock()
	a.bridgeAsset = genuine
	a.bridgeView, a.bridgeAt = testBridgeView(genuine), time.Now()
	a.bridgeMu.Unlock()
	return a, genuine
}

func swapAddr(t *testing.T, hrp string, chain uint64, sym string) string {
	t.Helper()
	s, err := bridge.SwapAddress(hrp, bridge.SwapDest{ChainID: chain, Symbol: sym, To: testPayee})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// notSwap returns an address that starts like a swap address but is not a
// canonical one: byte 3 changed (or a layout version this app does not know).
func notSwap(t *testing.T, hrp string, version bool) string {
	t.Helper()
	p, err := bridge.SwapPayload(bridge.SwapDest{ChainID: 11155111, Symbol: "USDC", To: testPayee})
	if err != nil {
		t.Fatal(err)
	}
	if version {
		p[2] |= 0x01
	} else {
		p[3]++
	}
	s, err := wallet.EncodeAddress(hrp, consensus.OutputTypeP2PK, p[:])
	if err != nil {
		t.Fatal(err)
	}
	if bridge.IsSwapAddress(hrp, s) {
		t.Fatalf("%s is a swap address", s)
	}
	return s
}

// Send is for transfers between Argonmine addresses: wallet/send and
// wallet/send-asset refuse every swap address, whatever the asset, amount or
// state of the bridge, and an address that only looks like one. They do so
// before the password. Only wallet/swap pays a swap address (TestWalletSwap).
func TestSendRefusesSwapAddresses(t *testing.T) {
	a, genuine := swapApp(t)
	hrp := a.params.HRP
	listed := swapAddr(t, hrp, 11155111, "USDC")
	type tc struct {
		ep   string
		body map[string]any
		want string
	}
	cases := []tc{
		{"wallet/send", map[string]any{"to": listed, "amount": "1"}, errSwapUseSwap.Error()},
		{"wallet/send", map[string]any{"to": strings.ToUpper(listed), "amount": "1"}, errSwapUseSwap.Error()},
		{"wallet/send-asset", map[string]any{"asset": "", "to": listed, "amount": "1"}, errSwapUseSwap.Error()},
		{"wallet/send-asset", map[string]any{"asset": genuine, "to": listed, "amount": "1"}, errSwapUseSwap.Error()},
		{"wallet/send-asset", map[string]any{"asset": "uarg", "to": listed, "amount": "1", "password": testPassword}, errSwapUseSwap.Error()},
		{"wallet/send-asset", map[string]any{"asset": strings.Repeat("cd", 32), "to": listed, "amount": "1"}, errSwapUseSwap.Error()},
		{"wallet/send-asset", map[string]any{"asset": "", "to": swapAddr(t, hrp, 5, "USDC"), "amount": "1"}, errSwapUseSwap.Error()},
		// An address that only looks like a swap address can never be paid out.
		{"wallet/send", map[string]any{"to": notSwap(t, hrp, false), "amount": "1"}, errSwapInvalid.Error()},
		{"wallet/send-asset", map[string]any{"asset": "", "to": notSwap(t, hrp, true), "amount": "1"}, errSwapInvalid.Error()},
		// Plain addresses are not affected: on to the password check.
		{"wallet/send", map[string]any{"to": a.miningAddr, "amount": "1"}, errPasswordMissing.Error()},
		{"wallet/send-asset", map[string]any{"asset": "", "to": a.miningAddr, "amount": "1"}, errPasswordMissing.Error()},
	}
	for _, c := range cases {
		c.body["feerate"] = "1"
		rec := apiCall(a, c.ep, nil, c.body)
		if msg := errOf(t, rec); rec.Code != http.StatusBadRequest || msg != c.want {
			t.Errorf("%s to %v: %d %q, want %q", c.ep, c.body["to"], rec.Code, msg, c.want)
		}
	}
	// The same with the bridge unreachable or unknown: still refused, never sent.
	a.bridgeMu.Lock()
	a.bridgeAsset, a.bridgeView, a.bridgeAt = "", bridgeChainsView{Error: "bridge unreachable", Chains: []bridgeChain{}}, time.Now()
	a.bridgeMu.Unlock()
	if msg := errOf(t, apiCall(a, "wallet/send-asset", nil, map[string]any{"asset": "", "to": listed, "amount": "1", "password": testPassword})); msg != errSwapUseSwap.Error() {
		t.Errorf("bridge down: %q", msg)
	}
	// wallet/swap needs the bridge's list and its uARG id.
	if msg := errOf(t, apiCall(a, "wallet/swap", nil, map[string]any{"amount": "1", "evm": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "chain_id": "11155111", "token": "USDC", "password": testPassword})); msg != errSwapNoList.Error() {
		t.Errorf("swap with no list: %q", msg)
	}
	a.bridgeMu.Lock()
	a.bridgeView = testBridgeView("")
	a.bridgeMu.Unlock()
	if msg := errOf(t, apiCall(a, "wallet/swap", nil, map[string]any{"amount": "1", "evm": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "chain_id": "11155111", "token": "USDC", "password": testPassword})); msg != errSwapNoUARG.Error() {
		t.Errorf("swap with no uARG id: %q", msg)
	}
}

func TestSwapPayoutLimits(t *testing.T) {
	v := testBridgeView("")
	d := bridge.SwapDest{ChainID: 11155111, Symbol: "USDC", To: testPayee}
	if _, _, err := swapPayout(v, d, 100_00000000); err != nil {
		t.Errorf("within the reserve: %v", err)
	}
	v.Chains[0].Tokens[0].ReserveUnits = 5000_00000000
	if _, _, err := swapPayout(v, d, 1000_00000001); err == nil || !strings.Contains(err.Error(), "at most 1000.00 uARG") {
		t.Errorf("above the largest tier: %v", err)
	}
	v.Chains[0].Tokens[0].AvailableNow = "2000000" // 2 USDC
	if _, _, err := swapPayout(v, d, 3_00000000); err == nil || !strings.Contains(err.Error(), "at most 2.00 USDC can be paid") {
		t.Errorf("above what the vault can release now: %v", err)
	}
	v.Chains[0].Unavailable = true
	if _, _, err := swapPayout(v, d, 1); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Errorf("unavailable chain: %v", err)
	}
	// The token's minimum withdrawal (min_release, gross token units),
	// converted as the bridge converts it: below it nothing is sent.
	v = testBridgeView("")
	v.Chains[0].Tokens[0].MinRelease = "2500000" // 2.5 USDC
	if _, _, err := swapPayout(v, d, 2_49999999); err == nil || err.Error() != "the smallest withdrawal of USDC on Ethereum Sepolia is 2.50 uARG; nothing was sent" {
		t.Errorf("below the minimum withdrawal: %v", err)
	}
	if _, _, err := swapPayout(v, d, 2_50000000); err != nil {
		t.Errorf("at the minimum withdrawal: %v", err)
	}
	if _, _, err := swapPayout(v, d, 0); err != nil {
		t.Errorf("the route alone (amount 0): %v", err)
	}
	v.Chains[0].Tokens[0].Decimals, v.Chains[0].Tokens[0].MinRelease = 18, "1000000000000000001" // one token and one wei
	if _, _, err := swapPayout(v, d, 1_00000000); err == nil || !strings.Contains(err.Error(), "is 1.00000001 uARG") {
		t.Errorf("an 18-decimal minimum: %v", err)
	}
	if _, _, err := swapPayout(v, d, 1_00000001); err != nil {
		t.Errorf("at an 18-decimal minimum: %v", err)
	}
	// Two tokens with one symbol on a chain: a swap address cannot tell them apart.
	v = testBridgeView("")
	v.Chains[0].Tokens = append(v.Chains[0].Tokens, bridgeToken{Symbol: "usdc", Address: bridge.HexAddress([20]byte{7}), Decimals: 6, ReserveUnits: 1e12})
	if _, _, err := swapPayout(v, d, 1); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Errorf("ambiguous symbol: %v", err)
	}
}

// wallet/swap computes the swap address for the chosen payout in Go, checks
// that it decodes back to exactly that payout, and sends uARG there.
func TestWalletSwap(t *testing.T) {
	a, _ := swapApp(t)
	hrp := a.params.HRP
	v := testBridgeView("")
	c, tok, err := swapRoute(v, 11155111, "usdc")
	if err != nil {
		t.Fatal(err)
	}
	if c2, tok2, err := swapRoute(v, 11155111, strings.ToUpper(bridge.HexAddress(testUSDC)[:2])+bridge.HexAddress(testUSDC)[2:]); err != nil || c2.ChainID != c.ChainID || tok2.Address != tok.Address {
		t.Errorf("route by contract address: %v %v %v", c2, tok2, err)
	}
	addr, dest, err := swapTarget(hrp, c, tok, testPayee)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := bridge.ParseSwapAddress(hrp, addr); !ok || got != dest || got.Symbol != "USDC" || got.ChainID != 11155111 || got.To != testPayee {
		t.Errorf("swap address %s decodes to %+v %v", addr, got, ok)
	}
	if !strings.HasPrefix(addr, hrp+"1qswap") {
		t.Errorf("swap address %s does not read %s1qswap…", addr, hrp)
	}
	if _, _, err := swapTarget(hrp, c, bridgeToken{Symbol: "USDC.e"}, testPayee); err == nil {
		t.Error("a symbol a swap address cannot carry was accepted")
	}

	base := func() map[string]any {
		return map[string]any{"amount": "2.5", "evm": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "chain_id": "11155111", "token": "USDC", "feerate": "1"}
	}
	for _, c := range []struct {
		edit func(map[string]any)
		want string
	}{
		{func(b map[string]any) { b["chain_id"] = "5" }, "does not pay out on chain 5"},
		{func(b map[string]any) { b["token"] = "USDT" }, "does not pay that token on Ethereum Sepolia"},
		{func(b map[string]any) { b["token"] = "0x1234" }, "contract address"},
		{func(b map[string]any) { b["chain_id"] = "" }, "choose the chain"},
		{func(b map[string]any) { b["evm"] = "0x123" }, "EVM address"},
		{func(b map[string]any) { b["amount"] = "0" }, "valid amount"},
		{func(b map[string]any) { b["amount"] = "101" }, "holds only 100.00 USDC"},
		{func(b map[string]any) {}, errPasswordMissing.Error()},
		{func(b map[string]any) { b["token"] = bridge.HexAddress(testUSDC) }, errPasswordMissing.Error()},
		{func(b map[string]any) { b["password"] = "nope" }, errWrongPassword.Error()},
		{func(b map[string]any) { b["password"] = testPassword }, "node connection"},
	} {
		b := base()
		c.edit(b)
		rec := apiCall(a, "wallet/swap", nil, b)
		if msg := errOf(t, rec); rec.Code != http.StatusBadRequest || !strings.Contains(msg, c.want) {
			t.Errorf("wallet/swap %v: %d %q, want %q", b, rec.Code, msg, c.want)
		}
	}
}

// wallet/validate says what a swap address pays, so the Send screen can show it.
func TestValidateSwapAddress(t *testing.T) {
	a, _ := swapApp(t)
	hrp := a.params.HRP
	check := func(addr string) addressCheck {
		t.Helper()
		var v addressCheck
		if err := json.Unmarshal(apiCall(a, "wallet/validate", nil, map[string]any{"address": addr}).Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := check(a.miningAddr); !v.Valid || v.Swap != nil || v.Error != "" {
		t.Errorf("plain address: %+v", v)
	}
	if v := check("nonsense"); v.Valid {
		t.Errorf("nonsense: %+v", v)
	}
	v := check(swapAddr(t, hrp, 11155111, "USDC"))
	if !v.Valid || v.Swap == nil || !v.Swap.Listed || v.Swap.Chain != "Ethereum Sepolia" || v.Swap.Symbol != "USDC" || v.Swap.Token != bridge.HexAddress(testUSDC) ||
		v.Swap.To != eip55(testPayee) || v.Swap.Decimals != 6 || v.Swap.ReleaseFeeBps != 100 {
		t.Errorf("listed swap address: %+v %+v", v, v.Swap)
	}
	if v := check(swapAddr(t, hrp, 5, "USDC")); v.Swap == nil || v.Swap.Listed || !strings.Contains(v.Swap.Error, "chain 5") {
		t.Errorf("unlisted swap address: %+v", v)
	}
	if v := check(notSwap(t, hrp, false)); !v.Valid || v.Swap != nil || v.Error != errSwapInvalid.Error() {
		t.Errorf("look-alike: %+v", v)
	}
}

// The QR endpoint draws this chain's addresses and EVM deposit addresses.
func TestQREndpoint(t *testing.T) {
	a := newTestApp()
	for d, want := range map[string]int{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed": http.StatusOK,
		"targ1qcdymka0xyz":                           http.StatusOK,
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAe":  http.StatusBadRequest,
		"<script>": http.StatusBadRequest,
	} {
		rec := call(a, http.MethodGet, "/qr.svg?d="+d, "127.0.0.1:"+testPort, nil, nil)
		if rec.Code != want {
			t.Errorf("/qr.svg?d=%s: %d, want %d", d, rec.Code, want)
		}
		if want == http.StatusOK && !strings.HasPrefix(rec.Body.String(), "<svg") {
			t.Errorf("/qr.svg?d=%s: not an svg", d)
		}
	}
	if rec := call(a, http.MethodGet, "/qr.svg?d=0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "evil.example:"+testPort, nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("QR for another host: %d", rec.Code)
	}
}
