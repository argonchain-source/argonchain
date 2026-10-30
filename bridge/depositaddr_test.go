package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/rpc"
	"argonmine/wallet"
)

// fakeRouter is a DepositRouter on a fakeEVM: deposit addresses by the
// CREATE2 formula, ERC-20 balances and Transfer logs for them, and sweep()
// that moves a forwarder's whole balance into the fake vault as a deposit
// from the router, as the contract does. With hold set, a sweep is only
// queued (its receipt pending) until mineSweeps.
type fakeRouter struct {
	router    [20]byte
	vault     [20]byte // what vault() answers
	codeHash  [32]byte
	wrongView bool // depositAddress() disagrees with the formula
	balances  map[[20]byte]map[[20]byte]*big.Int
	transfers []TransferEvent
	vaultMin  map[[20]byte]*big.Int // the vault's own minimum per token (AmountTooSmall below it)
	hold      bool
	queued    []fakeSweep
	sweeps    []fakeSweep // executed
	noGas     bool

	transferCalls, balanceCalls, sweepCalls, routerCalls int
}

type fakeSweep struct {
	token  [20]byte
	to     string
	tx     string
	amount *big.Int
	id     uint64
}

func (f *fakeEVM) withRouter(router [20]byte, codeHash [32]byte) *fakeEVM {
	f.rt = &fakeRouter{router: router, vault: f.vault, codeHash: codeHash, balances: map[[20]byte]map[[20]byte]*big.Int{}, vaultMin: map[[20]byte]*big.Int{}}
	return f
}

func (r *fakeRouter) balance(token, holder [20]byte) *big.Int {
	if r.balances[token] == nil {
		r.balances[token] = map[[20]byte]*big.Int{}
	}
	if r.balances[token][holder] == nil {
		r.balances[token][holder] = new(big.Int)
	}
	return r.balances[token][holder]
}

// transferIn is a plain ERC-20 transfer to a deposit address, in a new block.
func (f *fakeEVM) transferIn(token, to [20]byte, amount *big.Int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head++
	h := fmt.Sprintf("0x%064x", 0xa000+len(f.rt.transfers)+1)
	var from [20]byte
	from[0] = 0xEE
	f.rt.transfers = append(f.rt.transfers, TransferEvent{Token: token, From: from, To: to, Amount: new(big.Int).Set(amount), Block: f.head, TxHash: h})
	f.rt.balance(token, to).Add(f.rt.balance(token, to), amount)
	return h
}

func (f *fakeEVM) needRouter(router [20]byte) error {
	if f.rt == nil {
		return errors.New("this chain has no router")
	}
	if router != f.rt.router {
		return fmt.Errorf("no contract at %x", router)
	}
	return nil
}

func (f *fakeEVM) ForwarderCodeHash(_ context.Context, router [20]byte) ([32]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rt != nil {
		f.rt.routerCalls++
	}
	if f.fail {
		return [32]byte{}, errRPCDown
	}
	if err := f.needRouter(router); err != nil {
		return [32]byte{}, err
	}
	return f.rt.codeHash, nil
}

func (f *fakeEVM) RouterVault(_ context.Context, router [20]byte) ([20]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return [20]byte{}, errRPCDown
	}
	if err := f.needRouter(router); err != nil {
		return [20]byte{}, err
	}
	f.rt.routerCalls++
	return f.rt.vault, nil
}

func (f *fakeEVM) DepositAddressView(_ context.Context, router [20]byte, to string) ([20]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return [20]byte{}, errRPCDown
	}
	if err := f.needRouter(router); err != nil {
		return [20]byte{}, err
	}
	f.rt.routerCalls++
	a := DepositAddress(router, f.rt.codeHash, to)
	if f.rt.wrongView {
		a[0] ^= 0xff
	}
	return a, nil
}

func (f *fakeEVM) Transfers(_ context.Context, token [20]byte, recipients [][20]byte, from, to uint64) ([]TransferEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	if f.rt == nil {
		return nil, errors.New("this chain has no router")
	}
	f.rt.transferCalls++
	if len(recipients) > watchChunk {
		return nil, fmt.Errorf("%d recipients in one query", len(recipients))
	}
	want := map[[20]byte]bool{}
	for _, r := range recipients {
		want[r] = true
	}
	var out []TransferEvent
	for _, ev := range f.rt.transfers {
		if ev.Token == token && want[ev.To] && ev.Block >= from && ev.Block <= to {
			e := ev
			e.Amount = new(big.Int).Set(ev.Amount)
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeEVM) BalanceOf(_ context.Context, token, holder [20]byte, _ uint64) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	if f.rt == nil {
		return nil, errors.New("this chain has no router")
	}
	f.rt.balanceCalls++
	return new(big.Int).Set(f.rt.balance(token, holder)), nil
}

func (f *fakeEVM) Sweep(_ context.Context, router, token [20]byte, to string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", errRPCDown
	}
	if err := f.needRouter(router); err != nil {
		return "", err
	}
	f.rt.sweepCalls++
	// What eth_estimateGas would say.
	fwd := DepositAddress(router, f.rt.codeHash, to)
	bal := f.rt.balance(token, fwd)
	if bal.Sign() == 0 {
		return "", ErrNothingToSweep
	}
	if min := f.rt.vaultMin[token]; min != nil && bal.Cmp(min) < 0 {
		return "", ErrSweepBelowMinimum
	}
	if f.rt.noGas {
		return "", errors.New("evm: eth_sendRawTransaction: insufficient funds for gas * price + value (-32000)")
	}
	h := fmt.Sprintf("0x%064x", 0xd000+f.rt.sweepCalls)
	sw := fakeSweep{token: token, to: to, tx: h}
	if f.rt.hold {
		f.receipts[h] = ReceiptPending
		f.rt.queued = append(f.rt.queued, sw)
		return h, nil
	}
	f.execSweep(sw)
	return h, nil
}

// execSweep mines a sweep: the forwarder's whole balance goes into the vault
// as a deposit from the router, less the vault's fee.
func (f *fakeEVM) execSweep(sw fakeSweep) {
	fwd := DepositAddress(f.rt.router, f.rt.codeHash, sw.to)
	bal := new(big.Int).Set(f.rt.balance(sw.token, fwd))
	f.head++
	if bal.Sign() == 0 {
		f.receipts[sw.tx] = ReceiptFailed // NothingToSweep
		return
	}
	f.rt.balances[sw.token][fwd] = new(big.Int)
	id := uint64(len(f.deposits) + 1)
	fee := FeeOf(bal, f.depositBps)
	net := new(big.Int).Sub(bal, fee)
	f.deposits = append(f.deposits, DepositEvent{ID: id, Token: sw.token, From: f.rt.router, Amount: net, Fee: fee, To: sw.to, Block: f.head, TxHash: sw.tx})
	if f.reserve[sw.token] == nil {
		f.reserve[sw.token], f.obligations[sw.token] = new(big.Int), new(big.Int)
	}
	f.reserve[sw.token].Add(f.reserve[sw.token], bal)
	f.obligations[sw.token].Add(f.obligations[sw.token], net)
	sw.amount, sw.id = bal, id
	f.rt.sweeps = append(f.rt.sweeps, sw)
	f.receipts[sw.tx] = ReceiptOK
}

func (f *fakeEVM) mineSweeps() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sw := range f.rt.queued {
		f.execSweep(sw)
	}
	f.rt.queued = nil
}

func (f *fakeEVM) SweptLog(_ context.Context, router [20]byte, h string) (*SweptEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	if err := f.needRouter(router); err != nil {
		return nil, err
	}
	for _, sw := range f.rt.sweeps {
		if sw.tx == h {
			return &SweptEvent{Token: sw.token, Forwarder: DepositAddress(router, f.rt.codeHash, sw.to), Amount: new(big.Int).Set(sw.amount), DepositID: sw.id, To: sw.to}, nil
		}
	}
	return nil, nil
}

func (f *fakeEVM) routerCounts() (transfers, balances, sweeps, router int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rt == nil {
		return 0, 0, 0, 0
	}
	return f.rt.transferCalls, f.rt.balanceCalls, f.rt.sweepCalls, f.rt.routerCalls
}

const routedChainsJSON = `{"chains":[
  {"name":"Routed","chain_id":41001,"rpc":"http://r","explorer":"https://r.example","vault":"0x00000000000000000000000000000000000000a1","start_block":0,"confirmations":3,
   "router":"0x00000000000000000000000000000000000000b1","router_block":5,
   "tokens":[{"symbol":"USDC","address":"0x00000000000000000000000000000000000000c1","decimals":6},
             {"symbol":"USDT","address":"0x00000000000000000000000000000000000000c2","decimals":6,"min_deposit":"2000000"}]},
  {"name":"Plain","chain_id":41002,"rpc":"http://p","vault":"0x00000000000000000000000000000000000000a2","confirmations":2,
   "tokens":[{"symbol":"USDC","address":"0x00000000000000000000000000000000000000c3","decimals":6}]}
]}`

func TestParseChainsRouter(t *testing.T) {
	chains, err := ParseChains([]byte(routedChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !chains[0].HasRouter() || HexAddress(chains[0].Router) != "0x00000000000000000000000000000000000000b1" || chains[0].RouterBlock != 5 || chains[1].HasRouter() {
		t.Fatalf("router: %+v", chains)
	}
	noBlock := strings.Replace(routedChainsJSON, `"router_block":5,`, ``, 1)
	if c, err := ParseChains([]byte(strings.Replace(noBlock, `"start_block":0`, `"start_block":9`, 1))); err != nil || c[0].RouterBlock != 9 {
		t.Fatalf("router_block defaults to start_block: %v %+v", err, c)
	}
	for name, doc := range map[string]string{
		"bad router":         strings.Replace(routedChainsJSON, `"router":"0x00000000000000000000000000000000000000b1"`, `"router":"0xb1"`, 1),
		"router is vault":    strings.Replace(routedChainsJSON, `"router":"0x00000000000000000000000000000000000000b1"`, `"router":"0x00000000000000000000000000000000000000a1"`, 1),
		"block, no router":   strings.Replace(routedChainsJSON, `"router":"0x00000000000000000000000000000000000000b1",`, ``, 1),
		"zero router":        strings.Replace(routedChainsJSON, `"router":"0x00000000000000000000000000000000000000b1"`, `"router":"0x0000000000000000000000000000000000000000"`, 1),
		"unknown router key": strings.Replace(routedChainsJSON, `"router_block":5`, `"router_start":5`, 1),
	} {
		if _, err := ParseChains([]byte(doc)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDepositAddresses(t *testing.T) {
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	mp := mempool.New(ch, mempool.DefaultConfig())
	srv := rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: "t"}, rpc.Deps{Params: consensus.RegTest, Chain: ch, Pool: mp, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	client := wallet.NewClient("http://"+srv.Addr(), "t")
	mine := func(n int, addr string) {
		t.Helper()
		if _, err := client.Call("generatetoaddress", n, addr); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	cs, _ := guardians(t, 3)
	evmKeys := make([]*secp256k1.PrivateKey, 3)
	var evmAddrs [][20]byte
	for i := range evmKeys {
		evmKeys[i], _ = secp256k1.GeneratePrivateKey()
		evmAddrs = append(evmAddrs, AddressOf(evmKeys[i].PubKey()))
	}
	chains, err := ParseChains([]byte(routedChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	routed, plain := chains[0], chains[1]
	usdc, usdt, usdcP := routed.Tokens[0].Address, routed.Tokens[1].Address, plain.Tokens[0].Address
	hash := codeHash(t, testForwarderCodeHash)
	evmR := newFakeEVM(routed.ID, routed.Vault, evmAddrs, 2, 100, 100).withRouter(routed.Router, hash)
	evmP := newFakeEVM(plain.ID, plain.Vault, evmAddrs, 2, 100, 100)
	evms := map[uint64]EVM{routed.ID: evmR, plain.ID: evmP}

	dir := t.TempDir()
	mk := func(i int, leader bool, peers []string) *Service {
		t.Helper()
		s, err := New(Config{
			Params: consensus.RegTest, Node: client, Chains: chains, EVMs: evms, Custody: cs[i], EVMKey: evmKeys[i],
			Leader: leader, Peers: peers, Secret: testSecret, LeaderKey: cs[0].LocalKey(), Threshold: 2, L1Confirmations: 3,
			StatePath: filepath.Join(dir, fmt.Sprintf("g%d", i)), Logf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	f1, f2 := mk(1, false, nil), mk(2, false, nil)
	w1, w2 := httptest.NewServer(f1.Handler()), httptest.NewServer(f2.Handler())
	defer w1.Close()
	defer w2.Close()
	defer f1.Stop()
	defer f2.Stop()
	leader := mk(0, true, []string{w1.URL, w2.URL})
	defer func() { leader.Stop() }()
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leader.Handler().ServeHTTP(w, r) }))
	defer ws.Close()
	followersScan := func() {
		for _, f := range []*Service{f1, f2} {
			f.scanEVM(ctx)
			if err := f.scanL1(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A round as the loops would run it: the followers keep their view of
	// every chain current, so they can check any issuance the leader asks for.
	once := func(what string) {
		t.Helper()
		followersScan()
		if err := leader.Once(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	custody := cs[0].Address()
	mine(101, custody)
	priv, _ := btcec.NewPrivateKey()
	user := &wallet.Key{Priv: priv, XOnly: schnorr.SerializePubKey(priv.PubKey())}
	uAddr := user.Address(consensus.RegTest.HRP)
	mine(101, uAddr)
	once("first round") // reads the vault fees; no watches yet

	call := func(base, path string, hdr ...string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	register := func(to string) DepositAddressInfo {
		t.Helper()
		code, body, h := call(ws.URL, fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", routed.ID, to))
		if code != 200 || h.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("register %s: %d %s", to, code, body)
		}
		var info DepositAddressInfo
		if err := json.Unmarshal([]byte(body), &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	status := func(to string) DepositAddressStatus {
		t.Helper()
		code, body, h := call(ws.URL, fmt.Sprintf("/api/deposit-address/status?chain=%d&to=%s", routed.ID, to))
		if code != 200 || h.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("status %s: %d %s", to, code, body)
		}
		var st DepositAddressStatus
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// The address is the CREATE2 formula over the router, keccak256(to) and
	// the router's code hash, in EIP-55, with the chain's facts.
	depAddr := DepositAddress(routed.Router, hash, uAddr)
	info := register(uAddr)
	if info.Address != ChecksumAddress(depAddr) || info.Router != HexAddress(routed.Router) || info.ChainID != routed.ID || info.Chain != "Routed" || info.To != uAddr ||
		info.Confirmations != 3 || info.DepositFeeBps != 100 || info.Explorer != "https://r.example/address/"+info.Address || len(info.Tokens) != 2 ||
		info.Tokens[0].Symbol != "USDC" || info.Tokens[0].MinDeposit != "1000000" || info.Tokens[1].MinDeposit != "2000000" || !strings.Contains(info.Note, "USDC or USDT on Routed") {
		t.Fatalf("deposit address: %+v", info)
	}
	// Asking again, or with another spelling of the same address, is the same watch.
	if again := register(strings.ToUpper(uAddr)); again.Address != info.Address || again.To != uAddr {
		t.Fatalf("again: %+v", again)
	}
	if n := leader.Status().Chains[0].DepositAddresses; n != 1 {
		t.Fatalf("%d watches after asking twice", n)
	}
	for path, want := range map[string]int{
		fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", routed.ID, "rarg1notanaddress"):                       400,
		fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", routed.ID, strings.Replace(uAddr, "rarg", "targ", 1)): 400,
		fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", 999, uAddr):                                           400,
		fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", plain.ID, uAddr):                                      400,
		fmt.Sprintf("/api/deposit-address/status?chain=%d&to=%s", plain.ID, uAddr):                               400,
	} {
		if code, body, _ := call(ws.URL, path); code != want {
			t.Fatalf("%s: %d %s", path, code, body)
		}
	}
	if code, body, _ := call(w1.URL, fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", routed.ID, uAddr)); code != 503 || !strings.Contains(body, "leader") {
		t.Fatalf("follower: %d %s", code, body)
	}
	if n := f1.Status().Chains[0].DepositAddresses; n != 0 {
		t.Fatal("a follower registered a watch")
	}
	if st := status(uAddr); !st.Watched || st.Address != info.Address || len(st.Balances) != 2 || st.Balances[0].Amount != "0" || len(st.Sweeps) != 0 || len(st.Deposits) != 0 {
		t.Fatalf("fresh status: %+v", st)
	}
	// The QR of the deposit address, and of an Argonmine address.
	for _, d := range []string{info.Address, strings.ToLower(info.Address), uAddr} {
		if code, body, h := call(ws.URL, "/qr.svg?d="+d); code != 200 || h.Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(body, "<svg") {
			t.Fatalf("qr %s: %d %s", d, code, body)
		}
	}
	if code, _, _ := call(ws.URL, "/qr.svg?d=%3Cscript%3E"); code != 400 {
		t.Fatalf("qr of arbitrary text: %d", code)
	}

	// The new watch's balances are read once (a transfer may predate it).
	once("check")
	if _, b, s, _ := evmR.routerCounts(); b != 2 || s != 0 {
		t.Fatalf("balance reads %d, sweeps %d", b, s)
	}

	// 0.4 USDC arrives: seen at once, nothing swept before the confirmations,
	// and after them it is below the minimum, so it waits.
	evmR.transferIn(usdc, depAddr, big.NewInt(400_000))
	once("transfer seen")
	if st := status(uAddr); st.Balances[0].Amount != "400000" || st.Balances[0].LastTx == "" || len(st.Sweeps) != 0 {
		t.Fatalf("after the transfer: %+v", st)
	}
	_, b0, _, _ := evmR.routerCounts()
	once("unconfirmed")
	if _, b, s, _ := evmR.routerCounts(); b != b0 || s != 0 {
		t.Fatal("read or swept before the transfer had its confirmations")
	}
	evmR.advance(3)
	once("below minimum")
	if _, b, s, _ := evmR.routerCounts(); b != b0+1 || s != 0 {
		t.Fatalf("below the minimum: reads %d, sweeps %d", b-b0, s)
	}
	once("still below")
	if _, b, s, _ := evmR.routerCounts(); b != b0+1 || s != 0 {
		t.Fatal("read again without a new transfer")
	}
	if st := status(uAddr); st.Balances[0].Amount != "400000" || len(st.Sweeps) != 0 {
		t.Fatalf("waiting: %+v", st)
	}

	// A top-up of 0.8 USDC makes 1.2: swept once. The sweep is held in the
	// mempool; more arrives meanwhile and is confirmed, and the daemon is
	// restarted, but no second sweep is sent while the first is pending.
	evmR.mu.Lock()
	evmR.rt.hold = true
	evmR.mu.Unlock()
	evmR.transferIn(usdc, depAddr, big.NewInt(800_000))
	evmR.advance(3)
	once("top-up")
	if _, _, s, _ := evmR.routerCounts(); s != 1 {
		t.Fatalf("%d sweeps after the top-up", s)
	}
	st := status(uAddr)
	if len(st.Sweeps) != 1 || st.Sweeps[0].Status != SweepSent || st.Sweeps[0].Amount != "1200000" || st.Sweeps[0].TokenSymbol != "USDC" || st.Sweeps[0].Tx == "" {
		t.Fatalf("sweep sent: %+v", st.Sweeps)
	}
	evmR.transferIn(usdc, depAddr, big.NewInt(500_000))
	evmR.advance(3)
	once("pending 1")
	once("pending 2")
	if _, _, s, _ := evmR.routerCounts(); s != 1 {
		t.Fatalf("a second sweep was sent while one was pending (%d)", s)
	}
	leader.Stop()
	leader = mk(0, true, []string{w1.URL, w2.URL})
	if n := leader.Status().Chains[0].DepositAddresses; n != 1 {
		t.Fatalf("%d watches after the restart", n)
	}
	if st := status(uAddr); len(st.Sweeps) != 1 || st.Sweeps[0].Status != SweepSent || st.Balances[0].Amount != "1700000" {
		t.Fatalf("after the restart: %+v", st)
	}
	once("pending after restart")
	if _, _, s, _ := evmR.routerCounts(); s != 1 {
		t.Fatal("a second sweep was sent after the restart")
	}
	// Mined: it took the whole 1.7 USDC (the later transfer too); the vault
	// keeps 1% and the router's deposit is issued like any other.
	evmR.mineSweeps()
	once("sweep mined")
	st = status(uAddr)
	if len(st.Sweeps) != 1 || st.Sweeps[0].Status != SweepConfirmed || st.Sweeps[0].Amount != "1700000" || st.Sweeps[0].DepositID != 1 {
		t.Fatalf("sweep confirmed: %+v", st.Sweeps)
	}
	once("recheck")
	if st := status(uAddr); st.Balances[0].Amount != "0" || len(st.Deposits) != 1 || st.Deposits[0].Status != InboundPending {
		t.Fatalf("after the sweep: %+v", st)
	}
	d, ok := leader.Inbound(routed.ID, 1)
	if !ok || d.From != HexAddress(routed.Router) || d.To != uAddr || d.TokenAmount != "1683000" || d.TokenFee != "17000" || d.Amount != 168_300_000 || d.Status != InboundPending {
		t.Fatalf("router deposit: %+v", d)
	}
	evmR.advance(3)
	followersScan()
	once("issue")
	if d, _ := leader.Inbound(routed.ID, 1); d.Status != InboundIssued || d.IssueTx == "" {
		t.Fatalf("not issued: %+v", d)
	}
	mine(1, custody)
	followersScan()
	once("issued")
	if d, _ := leader.Inbound(routed.ID, 1); d.Status != InboundConfirmed {
		t.Fatalf("not confirmed: %+v", d)
	}
	if fd, _ := f1.Inbound(routed.ID, 1); fd == nil || fd.Status != InboundConfirmed {
		t.Fatalf("follower view: %+v", fd)
	}
	if st := status(uAddr); len(st.Deposits) != 1 || st.Deposits[0].Status != InboundConfirmed || st.Deposits[0].Amount != 168_300_000 {
		t.Fatalf("status after issuance: %+v", st.Deposits)
	}

	// The vault's own USDT minimum (5) is above the configured one (2): a
	// sweep of 3 is refused, which is not an error; 3 more and it goes.
	evmR.mu.Lock()
	evmR.rt.hold = false
	evmR.rt.vaultMin[usdt] = big.NewInt(5_000_000)
	evmR.mu.Unlock()
	evmR.transferIn(usdt, depAddr, big.NewInt(3_000_000))
	evmR.advance(3)
	once("vault minimum")
	_, _, s1, _ := evmR.routerCounts()
	once("vault minimum, again")
	if _, _, s, _ := evmR.routerCounts(); s != s1 {
		t.Fatal("retried a sweep below the vault's minimum without a new transfer")
	}
	evmR.transferIn(usdt, depAddr, big.NewInt(3_000_000))
	evmR.advance(3)
	once("above the vault minimum")
	once("confirm usdt")
	if st := status(uAddr); st.Sweeps[0].TokenSymbol != "USDT" || st.Sweeps[0].Status != SweepConfirmed || st.Sweeps[0].Amount != "6000000" || st.Balances[1].Amount != "0" {
		t.Fatalf("usdt sweep: %+v", st)
	}

	// No gas: the sweep waits with the reason, the chain shows it, and it
	// goes out once the leader has gas again.
	priv2, _ := btcec.NewPrivateKey()
	u2 := (&wallet.Key{Priv: priv2, XOnly: schnorr.SerializePubKey(priv2.PubKey())}).Address(consensus.RegTest.HRP)
	dep2 := DepositAddress(routed.Router, hash, u2)
	if info2 := register(u2); info2.Address != ChecksumAddress(dep2) {
		t.Fatal("second address")
	}
	once("second watch")
	evmR.mu.Lock()
	evmR.rt.noGas = true
	evmR.mu.Unlock()
	evmR.transferIn(usdc, dep2, usd(2, 6))
	evmR.advance(3)
	followersScan()
	if err := leader.Once(ctx); err == nil || !strings.Contains(err.Error(), "insufficient funds") {
		t.Fatalf("expected the gas problem, got %v", err)
	}
	if c := leader.Status().Chains[0]; !strings.Contains(c.Error, "no gas") || !strings.Contains(c.Error, "sweep") {
		t.Fatalf("chain error: %q", c.Error)
	}
	if st := status(u2); len(st.Sweeps) != 1 || st.Sweeps[0].Status != SweepPending || !strings.Contains(st.Sweeps[0].Error, "insufficient funds") {
		t.Fatalf("pending sweep: %+v", st.Sweeps)
	}
	evmR.mu.Lock()
	evmR.rt.noGas = false
	evmR.mu.Unlock()
	if w, ok := leader.st.GetWatch(routed.ID, u2); ok { // skip the backoff
		w.Tokens[HexAddress(usdc)].RetryAt = 0
		leader.st.PutWatch(w)
	}
	once("gas back")
	once("gas back, mined")
	if st := status(u2); len(st.Sweeps) != 1 || st.Sweeps[0].Status != SweepConfirmed || st.Sweeps[0].Attempts != 2 {
		t.Fatalf("after gas: %+v", st.Sweeps)
	}
	if c := leader.Status().Chains[0]; c.Error != "" {
		t.Fatalf("gas error did not clear: %q", c.Error)
	}

	// Watches that never received anything are dropped after 30 days; one
	// that received something stays.
	priv3, _ := btcec.NewPrivateKey()
	u3 := (&wallet.Key{Priv: priv3, XOnly: schnorr.SerializePubKey(priv3.PubKey())}).Address(consensus.RegTest.HRP)
	register(u3)
	once("third watch")
	old := time.Now().Add(-31 * 24 * time.Hour).Unix()
	for _, to := range []string{u3, uAddr} {
		w, _ := leader.st.GetWatch(routed.ID, to)
		w.Created, w.Touched = old, old
		leader.st.PutWatch(w)
	}
	if n := leader.pruneWatches(time.Now()); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if _, ok := leader.st.GetWatch(routed.ID, u3); ok {
		t.Fatal("unused watch kept")
	}
	if _, ok := leader.st.GetWatch(routed.ID, uAddr); !ok {
		t.Fatal("used watch dropped")
	}
	if n := leader.Status().Chains[0].DepositAddresses; n != 2 {
		t.Fatalf("%d watches after pruning", n)
	}

	// The cap: no new watch beyond it, with a clear error; an existing one
	// is still served.
	leader.watchCap = 2
	if code, body, _ := call(ws.URL, fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", routed.ID, u3)); code != 503 || !strings.Contains(body, "maximum") {
		t.Fatalf("over the cap: %d %s", code, body)
	}
	register(uAddr)
	leader.watchCap = watchCapDefault

	// Rate limit per client: a proxy on loopback passes the client in X-Forwarded-For.
	limited := 0
	for i := 0; i < 30; i++ {
		if code, _, _ := call(ws.URL, fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", routed.ID, uAddr), "X-Forwarded-For", "198.51.100.7, 203.0.113.9"); code == 429 {
			limited++
		}
	}
	if limited < 9 {
		t.Fatalf("only %d of 30 requests limited", limited)
	}
	if code, _, _ := call(ws.URL, fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", routed.ID, uAddr), "X-Forwarded-For", "203.0.113.10"); code != 200 {
		t.Fatal("another client was limited")
	}

	// A chain without a router is unaffected: its deposits are issued as
	// always and nothing of the router is ever called on it.
	evmP.addDeposit(usdcP, [20]byte{0xAB}, usd(10, 6), uAddr)
	evmP.advance(2)
	followersScan()
	once("plain deposit")
	if d, _ := leader.Inbound(plain.ID, 1); d == nil || d.Status != InboundIssued {
		t.Fatalf("plain deposit: %+v", d)
	}
	if tr, b, s, r := evmP.routerCounts(); tr+b+s+r != 0 || evmP.rt != nil {
		t.Fatal("router calls on a chain without a router")
	}
	_, body, _ := call(ws.URL, "/api/status")
	if !strings.Contains(body, `"router":"`+HexAddress(routed.Router)+`","deposit_addresses":2`) || !strings.Contains(body, `"name":"Plain"`) || !strings.Contains(body, `"router":"","deposit_addresses":0`) {
		t.Fatalf("status: %s", body)
	}
}

// TestDepositAddressRouterChecks: a router that serves another vault, or
// whose depositAddress view disagrees with the formula, turns the feature
// off for its chain, with the reason in the status.
func TestDepositAddressRouterChecks(t *testing.T) {
	chains, err := ParseChains([]byte(routedChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := guardians(t, 1)
	hash := codeHash(t, testForwarderCodeHash)
	for name, mod := range map[string]func(*fakeRouter){
		"wrong view":  func(r *fakeRouter) { r.wrongView = true },
		"wrong vault": func(r *fakeRouter) { r.vault[0] ^= 1 },
	} {
		evmR := newFakeEVM(chains[0].ID, chains[0].Vault, nil, 1, 100, 100).withRouter(chains[0].Router, hash)
		mod(evmR.rt)
		s, err := New(Config{Params: consensus.RegTest, Node: wallet.NewClient("http://127.0.0.1:1", "x"), Chains: chains,
			EVMs:    map[uint64]EVM{chains[0].ID: evmR, chains[1].ID: newFakeEVM(chains[1].ID, chains[1].Vault, nil, 1, 0, 0)},
			Custody: cs[0], Leader: true, Threshold: 1, StatePath: filepath.Join(t.TempDir(), "s"), Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		ws := httptest.NewServer(s.Handler())
		resp, err := http.Get(ws.URL + fmt.Sprintf("/api/deposit-address?chain=%d&to=%s", chains[0].ID, cs[0].Address()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 503 || !strings.Contains(string(b), "deposit addresses are off on Routed") {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, b)
		}
		c := s.Status().Chains[0]
		if c.Router != "" || !strings.Contains(c.RouterError, "off on Routed") || c.Error != "" {
			t.Fatalf("%s: status %+v", name, c)
		}
		ws.Close()
		s.Stop()
	}
	// The router of a chain is replaced in the configuration: existing
	// watches move to the address derived under the new router.
	dir := filepath.Join(t.TempDir(), "r")
	mkSvc := func(doc string) (*Service, *fakeEVM, []Chain) {
		t.Helper()
		cc, err := ParseChains([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		f := newFakeEVM(cc[0].ID, cc[0].Vault, nil, 1, 100, 100).withRouter(cc[0].Router, hash)
		s, err := New(Config{Params: consensus.RegTest, Node: wallet.NewClient("http://127.0.0.1:1", "x"), Chains: cc,
			EVMs: map[uint64]EVM{cc[0].ID: f, cc[1].ID: newFakeEVM(cc[1].ID, cc[1].Vault, nil, 1, 0, 0)}, Custody: cs[0], Leader: true, Threshold: 1, StatePath: dir, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return s, f, cc
	}
	to := cs[0].Address()
	s1, _, cc1 := mkSvc(routedChainsJSON)
	if _, err := s1.RegisterDepositAddress(context.Background(), cc1[0].ID, to); err != nil {
		t.Fatal(err)
	}
	s1.Stop()
	s2, f2, cc2 := mkSvc(strings.Replace(routedChainsJSON, "00000000000000000000000000000000000000b1", "00000000000000000000000000000000000000b2", 1))
	defer s2.Stop()
	if err := s2.depositAddressRound(context.Background(), s2.byID[cc2[0].ID]); err != nil {
		t.Fatal(err)
	}
	w, ok := s2.st.GetWatch(cc2[0].ID, to)
	if !ok || w.Address != HexAddress(DepositAddress(cc2[0].Router, hash, to)) || w.Router != HexAddress(cc2[0].Router) || !w.Check || w.Address == HexAddress(DepositAddress(cc1[0].Router, hash, to)) {
		t.Fatalf("after the router changed: %+v", w)
	}
	if err := s2.depositAddressRound(context.Background(), s2.byID[cc2[0].ID]); err != nil {
		t.Fatal(err)
	}
	if _, b, _, _ := f2.routerCounts(); b != 2 {
		t.Fatalf("the moved watch read %d balances, want 2", b)
	}

	// A chain configured with a router whose client cannot serve one is refused.
	type plainEVM struct{ EVM }
	_, err = New(Config{Params: consensus.RegTest, Node: wallet.NewClient("http://127.0.0.1:1", "x"), Chains: chains[:1],
		EVMs:    map[uint64]EVM{chains[0].ID: plainEVM{newFakeEVM(chains[0].ID, chains[0].Vault, nil, 1, 0, 0)}},
		Custody: cs[0], Threshold: 1, StatePath: filepath.Join(t.TempDir(), "p")})
	if err == nil || !strings.Contains(err.Error(), "cannot serve deposit addresses") {
		t.Fatalf("client without router support: %v", err)
	}
}
