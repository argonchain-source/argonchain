package bridge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

func testKey(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Vectors read from the deployed routers with cast (2026-10-26): the local
// anvil router and the Ethereum Sepolia one, both with the forwarder code
// hash below.
var routerVectors = []struct {
	router, to, want string
}{
	{"0x5f3f1dBD7B74C6B46e8c44f98792A1dAf8d69154", "targ1q9yf56tjyyyqh85ulcd7l4uh6238wedugj7zqwuthqxs86rqwe6ssech8ss", "0xC01DDdA0Eee5e4f259b4510B4bA2A673a514c0F1"},
	{"0x5f3f1dBD7B74C6B46e8c44f98792A1dAf8d69154", "targ1qcdymka08cd8jeeem5nreyng6fl6qh99jtljphsfwsrapj434n65q49v8s5", "0x6C2b0B201F349E92566B734468d2dd204f4A969d"},
	{"0x97C759384D62b16Ca79Ffdf857436C4C58cCD9c1", "targ1qcdymka08cd8jeeem5nreyng6fl6qh99jtljphsfwsrapj434n65q49v8s5", "0x63D0f3190eC6F4b4929FEAEcEFa4978bE255e8c3"},
}

const testForwarderCodeHash = "accea86f8d17654ed61b24007ca6544da7f6451c1050a1afd7685449af1193b8"

func codeHash(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 32 {
		t.Fatalf("bad hash %q", s)
	}
	var h [32]byte
	copy(h[:], b)
	return h
}

func TestDepositAddressFormula(t *testing.T) {
	h := codeHash(t, testForwarderCodeHash)
	for _, v := range routerVectors {
		r, _ := ParseAddress(v.router)
		got := DepositAddress(r, h, v.to)
		// The EIP-55 spelling is exactly what cast prints.
		if ChecksumAddress(got) != v.want {
			t.Errorf("depositAddress(%s) on %s = %s, want %s", v.to, v.router, ChecksumAddress(got), v.want)
		}
	}
	// The salt is the exact bytes: another spelling is another address.
	r, _ := ParseAddress(routerVectors[0].router)
	if DepositAddress(r, h, strings.ToUpper(routerVectors[0].to)) == DepositAddress(r, h, routerVectors[0].to) {
		t.Fatal("uppercase spelling gave the same address")
	}
}

func TestRouterABI(t *testing.T) {
	for name, c := range map[string]struct {
		got  []byte
		want string
	}{
		"sweep":             {selSweep[:], "7e456405"},
		"depositAddress":    {selDepositAddress[:], "e2e944b8"},
		"forwarderCodeHash": {selForwarderCodeHash[:], "96beb9d7"},
		"vault":             {selRouterVault[:], "fbfa77cf"},
		"Swept":             {topicSwept[:], "22b568cad982e95de408058a83fdb34a9128f86950928c6a47b0e73d057e1b33"},
		"Transfer":          {topicTransfer[:], "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
		"AmountTooSmall":    {errAmountTooSmall[:], "c2f5625a"},
		"NothingToSweep":    {errNothingToSweep[:], "351261fc"},
	} {
		if hex.EncodeToString(c.got) != c.want {
			t.Errorf("%s: %x, want %s", name, c.got, c.want)
		}
	}
	// Calldata as cast calldata builds it.
	to := "targ1qcdymka08cd8jeeem5nreyng6fl6qh99jtljphsfwsrapj434n65q49v8s5"
	tok, _ := ParseAddress("0x0E801D84Fa97b50751Dbf25036d067dCf18858bF")
	if got := hex.EncodeToString(EncodeSweepCall(tok, to)); got != "7e4564050000000000000000000000000e801d84fa97b50751dbf25036d067dcf18858bf000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000000407461726731716364796d6b6130386364386a6565656d356e7265796e6736666c36716839396a746c6a7068736677737261706a3433346e363571343976387335" {
		t.Errorf("sweep calldata %s", got)
	}
	if got := hex.EncodeToString(EncodeDepositAddressCall(to)); got != "e2e944b8000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000000407461726731716364796d6b6130386364386a6565656d356e7265796e6736666c36716839396a746c6a7068736677737261706a3433346e363571343976387335" {
		t.Errorf("depositAddress calldata %s", got)
	}
}

func TestDecodeRouterLogs(t *testing.T) {
	var from, to, token, fwd [20]byte
	from[19], to[19], token[19], fwd[19] = 1, 2, 3, 4
	topic := func(a [20]byte) string { return "0x" + hex.EncodeToString(word(a[:])) }
	ev, err := DecodeTransferLog([]string{"0x" + hex.EncodeToString(topicTransfer[:]), topic(from), topic(to)}, "0x"+hex.EncodeToString(wordUint(big.NewInt(1_500_000))))
	if err != nil || ev.From != from || ev.To != to || ev.Amount.Int64() != 1_500_000 {
		t.Fatalf("transfer: %+v %v", ev, err)
	}
	if _, err := DecodeTransferLog([]string{"0x" + hex.EncodeToString(topicSwept[:]), topic(from), topic(to)}, "0x"); err == nil {
		t.Fatal("decoded a non-Transfer log")
	}
	dest := "targ1q9yf56tjyyyqh85ulcd7l4uh6238wedugj7zqwuthqxs86rqwe6ssech8ss"
	data := append(append(append(wordUint(big.NewInt(2_000_000)), wordUint(big.NewInt(7))...), wordUint(big.NewInt(96))...), abiString(dest)...)
	sw, err := DecodeSweptLog([]string{"0x" + hex.EncodeToString(topicSwept[:]), topic(token), topic(fwd)}, "0x"+hex.EncodeToString(data))
	if err != nil || sw.Token != token || sw.Forwarder != fwd || sw.Amount.Int64() != 2_000_000 || sw.DepositID != 7 || sw.To != dest {
		t.Fatalf("swept: %+v %v", sw, err)
	}
}

func TestSweepRefusal(t *testing.T) {
	// What anvil answers to eth_estimateGas of a sweep the router refuses.
	nothing := &RPCError{Method: "eth_estimateGas", Code: 3, Message: "execution reverted: custom error 0x351261fc", Data: "0x351261fc"}
	if !errors.Is(sweepRefusal(nothing), ErrNothingToSweep) {
		t.Fatal("NothingToSweep")
	}
	small := &RPCError{Method: "eth_estimateGas", Code: 3, Message: "execution reverted", Data: "0xc2f5625a"}
	if !errors.Is(sweepRefusal(small), ErrSweepBelowMinimum) {
		t.Fatal("AmountTooSmall from data")
	}
	msgOnly := &RPCError{Method: "eth_estimateGas", Code: -32000, Message: "execution reverted: custom error 0xc2f5625a"}
	if !errors.Is(sweepRefusal(msgOnly), ErrSweepBelowMinimum) {
		t.Fatal("AmountTooSmall from the message")
	}
	paused := &RPCError{Method: "eth_estimateGas", Code: 3, Message: "execution reverted", Data: "0x1309a563"}
	if err := sweepRefusal(paused); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused: %v", err)
	}
	other := errors.New("dial tcp: connection refused")
	if err := sweepRefusal(other); err == nil || errors.Is(err, ErrSweepBelowMinimum) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("other: %v", err)
	}
	// The wrapper send() returns keeps both the marker and the node's error.
	wf := error(&wouldFail{small})
	var re *RPCError
	if !errors.Is(wf, errWouldFail) || !errors.As(wf, &re) || re.Data != "0xc2f5625a" {
		t.Fatal("wouldFail wrapping")
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(1, 3)
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !l.allow("a", now) {
			t.Fatalf("request %d refused", i)
		}
	}
	if l.allow("a", now) {
		t.Fatal("burst exceeded")
	}
	if !l.allow("b", now) {
		t.Fatal("another client was refused")
	}
	if !l.allow("a", now.Add(1100*time.Millisecond)) {
		t.Fatal("no refill")
	}
}

// TestRateLimiterFull: a table full of clients forgets the refilled ones,
// then the ones seen longest ago; a client being throttled right now stays
// throttled (the table used to be wiped past 10 000 clients).
func TestRateLimiterFull(t *testing.T) {
	l := newRateLimiter(1.0/60, 2)
	now := time.Unix(1000, 0)
	for i := 0; i < limiterMaxClients; i++ {
		k := fmt.Sprintf("old%d", i)
		l.allow(k, now)
		l.allow(k, now) // each one has used its burst
	}
	later := now.Add(time.Minute)
	if !l.allow("abuser", later) || !l.allow("abuser", later) || l.allow("abuser", later) {
		t.Fatal("the abuser's burst")
	}
	// A flood of new addresses while the abuser keeps asking: it stays
	// throttled throughout.
	for i := 0; i < 2*limiterMaxClients; i++ {
		at := later.Add(time.Duration(i) * time.Millisecond)
		l.allow(fmt.Sprintf("new%d", i), at)
		if i%100 == 0 && l.allow("abuser", at) {
			t.Fatalf("after %d new addresses the throttled client got a fresh bucket", i)
		}
		if n := len(l.m); n > limiterMaxClients {
			t.Fatalf("%d clients kept", n)
		}
	}
}

func TestClientIP(t *testing.T) {
	req := func(peer string, hdr ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Add(hdr[i], hdr[i+1])
		}
		return r
	}
	for _, c := range []struct {
		r      *http.Request
		header string
		want   string
	}{
		{req("203.0.113.5:4000"), "", "203.0.113.5"},
		// A client on the internet cannot pick its key.
		{req("203.0.113.5:4000", "X-Forwarded-For", "198.51.100.1"), "", "203.0.113.5"},
		// Behind a proxy on loopback, a private or a carrier-grade NAT address
		// (Railway's edge): the right-most entry, the one the proxy appended.
		{req("127.0.0.1:4000", "X-Forwarded-For", "1.1.1.1, 198.51.100.1"), "", "198.51.100.1"},
		{req("10.1.2.3:4000", "X-Forwarded-For", "198.51.100.2"), "", "198.51.100.2"},
		{req("100.64.0.7:4000", "X-Forwarded-For", "6.6.6.6", "X-Forwarded-For", "198.51.100.3"), "", "198.51.100.3"},
		{req("100.127.255.254:4000", "X-Forwarded-For", "198.51.100.4"), "", "198.51.100.4"},
		{req("100.128.0.1:4000", "X-Forwarded-For", "198.51.100.4"), "", "100.128.0.1"}, // outside 100.64.0.0/10
		// A malformed entry falls back to the peer.
		{req("10.1.2.3:4000", "X-Forwarded-For", "nonsense"), "", "10.1.2.3"},
		// -client-ip-header: that header's right-most entry, whatever the peer.
		{req("203.0.113.5:4000", "X-Forwarded-For", "6.6.6.6, 198.51.100.9"), "X-Forwarded-For", "198.51.100.9"},
		{req("203.0.113.5:4000", "X-Real-Ip", "198.51.100.10"), "x-real-ip", "198.51.100.10"},
		{req("203.0.113.5:4000"), "X-Forwarded-For", "203.0.113.5"},
		// IPv6 by /64; IPv4-mapped as IPv4.
		{req("[2001:db8:1:2:aaaa::1]:4000"), "", "2001:db8:1:2::/64"},
		{req("10.0.0.1:1", "X-Forwarded-For", "2001:db8:1:2:bbbb::9"), "", "2001:db8:1:2::/64"},
		{req("[::ffff:203.0.113.7]:4000"), "", "203.0.113.7"},
	} {
		if got := clientIPFrom(c.r, c.header); got != c.want {
			t.Errorf("peer %s, %v, header %q: %q, want %q", c.r.RemoteAddr, c.r.Header, c.header, got, c.want)
		}
	}
}

// TestRouterAnvil reads the deployed router of a local anvil through
// RPCEVM. It runs only with BRIDGE_ANVIL_RPC (e.g. http://127.0.0.1:8545)
// and BRIDGE_ANVIL_ROUTER, BRIDGE_ANVIL_VAULT set (deploy/railway/chains/anvil.env).
func TestRouterAnvil(t *testing.T) {
	url, routerHex, vaultHex := os.Getenv("BRIDGE_ANVIL_RPC"), os.Getenv("BRIDGE_ANVIL_ROUTER"), os.Getenv("BRIDGE_ANVIL_VAULT")
	if url == "" || routerHex == "" || vaultHex == "" {
		t.Skip("set BRIDGE_ANVIL_RPC, BRIDGE_ANVIL_ROUTER and BRIDGE_ANVIL_VAULT to run against anvil")
	}
	router, _ := ParseAddress(routerHex)
	vault, _ := ParseAddress(vaultHex)
	e := NewRPCEVM(url, 31337, vault, nil)
	ctx := context.Background()
	h, err := e.ForwarderCodeHash(ctx, router)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := e.RouterVault(ctx, router); err != nil || v != vault {
		t.Fatalf("vault(): %x %v", v, err)
	}
	for _, to := range []string{"targ1q9yf56tjyyyqh85ulcd7l4uh6238wedugj7zqwuthqxs86rqwe6ssech8ss", "targ1qcdymka08cd8jeeem5nreyng6fl6qh99jtljphsfwsrapj434n65q49v8s5", "rarg1qtest"} {
		view, err := e.DepositAddressView(ctx, router, to)
		if err != nil {
			t.Fatal(err)
		}
		if mine := DepositAddress(router, h, to); mine != view {
			t.Fatalf("%s: formula %s, router %s", to, HexAddress(mine), HexAddress(view))
		}
	}
	// A sweep of an empty address is refused before anything is sent.
	key := testKey(t)
	es := NewRPCEVM(url, 31337, vault, key)
	tok, _ := ParseAddress(os.Getenv("BRIDGE_ANVIL_TOKEN"))
	if tok != ([20]byte{}) {
		if _, err := es.Sweep(ctx, router, tok, "targ1qemptyemptyempty"); !errors.Is(err, ErrNothingToSweep) {
			t.Fatalf("sweep of an empty address: %v", err)
		}
	}
}
