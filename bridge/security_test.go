package bridge

// Regression tests for the security review of the guardian daemon: the
// reviewer's proofs of concept, ported, each of which must now fail to
// exploit, and the tests for what replaced the broken mechanisms.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/miner"
	"argonmine/rpc"
	"argonmine/wallet"
)

const testSecret = "test-secret-that-is-at-least-32-characters-long"

type pocEnv struct {
	t        *testing.T
	ch       *chain.Chain
	mp       *mempool.Pool
	client   *wallet.Client
	cs       []*Custody
	evmKeys  []*secp256k1.PrivateKey
	evmAddrs [][20]byte
	chains   []Chain
	evm      *fakeEVM
	leader   *Service
	f1, f2   *Service
	urls     []string // leader, f1, f2
	user     *wallet.Key
	uAddr    string
	dir      string
	opts     []func(*Config)
}

// newPocEnv runs a regtest node and three guardians on one EVM chain
// (Local A: USDC and USDT, 5 confirmations, 1% fees each way).
func newPocEnv(t *testing.T, opts ...func(*Config)) *pocEnv {
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Close() })
	mp := mempool.New(ch, mempool.DefaultConfig())
	srv := rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: "t"}, rpc.Deps{Params: consensus.RegTest, Chain: ch, Pool: mp, Started: time.Now(), Logf: func(string, ...any) {}})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	e := &pocEnv{t: t, ch: ch, client: wallet.NewClient("http://"+srv.Addr(), "t"), mp: mp, dir: t.TempDir(), opts: opts}
	e.cs, _ = guardians(t, 3)
	e.evmKeys = make([]*secp256k1.PrivateKey, 3)
	for i := range e.evmKeys {
		e.evmKeys[i], _ = secp256k1.GeneratePrivateKey()
		e.evmAddrs = append(e.evmAddrs, AddressOf(e.evmKeys[i].PubKey()))
	}
	e.chains, err = ParseChains([]byte(testChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	e.chains = e.chains[:1]
	e.evm = newFakeEVM(e.chains[0].ID, e.chains[0].Vault, e.evmAddrs, 2, 100, 100)
	e.start("g", e.chains, e.evm)
	priv, _ := btcec.NewPrivateKey()
	e.user = &wallet.Key{Priv: priv, XOnly: schnorr.SerializePubKey(priv.PubKey())}
	e.uAddr = e.user.Address(consensus.RegTest.HRP)
	return e
}

// start opens the three guardians on state directories <prefix>0..2 and
// serves their handlers (the leader's public one too).
func (e *pocEnv) start(prefix string, chains []Chain, evm *fakeEVM) {
	e.f1 = e.mk(1, false, nil, prefix+"1", chains, evm)
	e.f2 = e.mk(2, false, nil, prefix+"2", chains, evm)
	w1, w2 := httptest.NewServer(e.f1.Handler()), httptest.NewServer(e.f2.Handler())
	e.t.Cleanup(w1.Close)
	e.t.Cleanup(w2.Close)
	e.leader = e.mk(0, true, []string{w1.URL, w2.URL}, prefix+"0", chains, evm)
	wl := httptest.NewServer(e.leader.Handler())
	e.t.Cleanup(wl.Close)
	e.urls = []string{wl.URL, w1.URL, w2.URL}
}

func (e *pocEnv) stop() {
	for _, s := range []*Service{e.leader, e.f1, e.f2} {
		s.Stop()
	}
}

func (e *pocEnv) mk(i int, leader bool, peers []string, name string, chains []Chain, evm *fakeEVM) *Service {
	cfg := Config{
		Params: consensus.RegTest, Node: e.client, Chains: chains, EVMs: map[uint64]EVM{chains[0].ID: evm}, Custody: e.cs[i], EVMKey: e.evmKeys[i],
		Leader: leader, Peers: peers, Secret: testSecret, LeaderKey: e.cs[0].LocalKey(), Threshold: 2, L1Confirmations: 3,
		StatePath: filepath.Join(e.dir, name), Logf: e.t.Logf,
	}
	for _, o := range e.opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(s.Stop)
	return s
}

func (e *pocEnv) mine(n int, addr string) {
	e.t.Helper()
	if _, err := e.client.Call("generatetoaddress", n, addr); err != nil {
		e.t.Fatal(err)
	}
}

func (e *pocEnv) scanAll() {
	e.t.Helper()
	ctx := context.Background()
	for _, s := range []*Service{e.leader, e.f1, e.f2} {
		s.scanEVM(ctx)
		if err := s.scanL1(ctx); err != nil {
			e.t.Fatal(err)
		}
	}
}

// round is one leader round as the loops would run it.
func (e *pocEnv) round() error {
	e.scanAll()
	return e.leader.Once(context.Background())
}

// fund mines eight custody coins (heights 1..8) and 101 blocks for the user
// (9..109): every custody coin is mature at once, and so are the user's
// first coinbases, which pay the fees of the burns.
func (e *pocEnv) fund() {
	e.mine(8, e.cs[0].Address())
	e.mine(101, e.uAddr)
}

// uargBalance sums the uARG outputs paying addr in the main chain.
func (e *pocEnv) uargBalance(addr string) consensus.Amount {
	info, _ := e.client.ChainInfo()
	asset := e.leader.Asset().String()
	var sum consensus.Amount
	for h := uint32(0); h <= info.Blocks; h++ {
		bh, _ := e.client.BlockHash(h)
		blk, _ := e.client.Block(bh)
		for _, tx := range blk.Tx {
			for _, o := range tx.Outputs {
				if o.Asset == asset && o.Address == addr {
					sum += o.Amount
				}
			}
		}
	}
	return sum
}

// matureCoins lists custody coins the node would let an issuance spend.
func (e *pocEnv) matureCoins(s *Service) []UTXO {
	info, _ := e.client.ChainInfo()
	var out []UTXO
	for _, c := range s.st.UTXOs() {
		if _, st, err := s.custodyCoin(c.Outpoint(), info.Blocks); err == nil && st == coinOK {
			out = append(out, c)
		}
	}
	return out
}

// issueTx builds a well-formed issuance for deps spending exactly one
// custody coin, with its deposit references and change.
func issueTx(s *Service, coin UTXO, deps []*Inbound) (*consensus.Tx, []consensus.TxOut, SignRequest) {
	tx := &consensus.Tx{Version: consensus.TxVersionAssets}
	var total consensus.Amount
	var refs []InboundRef
	for _, d := range deps {
		dest, _ := wallet.DecodeAddress(s.cfg.Params.HRP, d.To)
		tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: d.Amount, Type: dest.Type, Payload: dest.Payload, Asset: s.asset})
		total += d.Amount
		refs = append(refs, InboundRef{Chain: d.Chain, ID: d.ID})
	}
	memo, _ := EncodeIssueMemo(refs)
	tx.Issuances = []consensus.Issuance{{Asset: s.asset, Issuer: s.issuer, Symbol: s.cfg.AssetSymbol, Amount: total}}
	h, _ := consensus.HashFromHex(coin.TxID)
	tx.Inputs = []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: h, Index: coin.Index}}}
	spent := []consensus.TxOut{s.cfg.Custody.Output(coin.Amount)}
	tx.Witnesses = [][]byte{make([]byte, 64), make([]byte, 64)}
	tx.Outputs = append(tx.Outputs, s.cfg.Custody.Output(0), consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: memo})
	fee := consensus.Amount(tx.SerializedSize()+16) * s.cfg.FeeRate
	tx.Outputs[len(tx.Outputs)-2].Amount = coin.Amount - fee
	var id [8]byte
	rand.Read(id[:])
	req := SignRequest{ID: hex.EncodeToString(id[:]), TxHex: hex.EncodeToString(tx.Serialize()), Inbound: refs}
	for _, o := range spent {
		req.Spent = append(req.Spent, encodeTxOut(o))
	}
	return tx, spent, req
}

// postGuardian sends body to a guardian endpoint with the given headers and
// returns the status and the answer.
func postGuardian(t *testing.T, url string, body []byte, hdr http.Header) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestPoCDuplicateIssuanceWithSecretOnly (ported): with the shared secret
// alone nobody can act as the coordinator any more, and even the leader's
// own key cannot get a deposit issued before its confirmations, or issued
// a second time in a transaction that does not conflict with the first.
func TestPoCDuplicateIssuanceWithSecretOnly(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	id := e.evm.addDeposit(usdc, from, usd(1000, 6), e.uAddr) // 1000 USDC, 990 net, no confirmations yet
	e.scanAll()
	d, _ := e.leader.Inbound(e.chains[0].ID, id)
	if d.Status != InboundPending {
		t.Fatalf("deposit %+v", d)
	}
	coins := e.matureCoins(e.f1)
	if len(coins) < 4 {
		t.Fatalf("%d mature custody coins", len(coins))
	}

	// 1. The attack as the reviewer wrote it: the old HMAC over the body,
	// and the same with the secret bound to the new preimage but signed by
	// a key of the attacker's own. Every guardian refuses; the leader takes
	// no guardian requests at all.
	_, _, req := issueTx(e.leader, coins[0], []*Inbound{d})
	body, _ := json.Marshal(req)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	oldStyle := http.Header{"X-Guardian-Signature": {hex.EncodeToString(mac.Sum(nil))}}
	attacker, _ := btcec.NewPrivateKey()
	forged := func(path string) http.Header {
		nonce := make([]byte, authNonceLen)
		rand.Read(nonce)
		ts := time.Now().Unix()
		pre := authPreimage(consensus.RegTest.Name, e.cs[0].XOnly(), path, ts, nonce, body)
		msg := authMessage(pre)
		sig, _ := schnorr.Sign(attacker, msg[:])
		return http.Header{hdrGuardianTime: {fmt.Sprint(ts)}, hdrGuardianNonce: {hex.EncodeToString(nonce)}, hdrGuardianAuth: {hex.EncodeToString(sig.Serialize())}, hdrGuardianMAC: {authMAC(testSecret, pre)}}
	}
	for i, u := range e.urls {
		want := http.StatusUnauthorized
		if i == 0 {
			want = http.StatusForbidden
		}
		for _, hdr := range []http.Header{oldStyle, forged("/guardian/nonces")} {
			if code, ans := postGuardian(t, u+"/guardian/nonces", body, hdr); code != want {
				t.Fatalf("guardian %d answered a secret-only request: %d %s", i, code, ans)
			}
		}
	}

	// 2. The leader's own key (a compromised leader): the followers count
	// the confirmations themselves.
	var out struct {
		Nonces []string `json:"nonces"`
	}
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "confirmations") {
		t.Fatalf("a follower agreed to sign a deposit without confirmations: %v", err)
	}

	// 3. Confirmed, it is issued once in the ordinary way.
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.leader.Inbound(e.chains[0].ID, id); d.Status != InboundIssued {
		t.Fatalf("not issued: %+v", d)
	}
	signed := e.f1.st.SignedIssuesFor(InboundRef{Chain: e.chains[0].ID, ID: id})
	if len(signed) != 1 || len(signed[0].Inputs) == 0 {
		t.Fatalf("follower signing memory: %+v", signed)
	}
	first := signed[0]

	// 4. A second issuance of the same deposit that spends other custody
	// coins is refused by both followers; one that spends a coin of the
	// first (so at most one of them can confirm) is accepted.
	var other UTXO
	for _, c := range e.matureCoins(e.f1) {
		if c.Outpoint() != first.Inputs[0] {
			other = c
			break
		}
	}
	_, _, dup := issueTx(e.leader, other, []*Inbound{d})
	for _, u := range e.urls[1:] {
		if err := e.leader.callPeer(ctx, u, "/guardian/nonces", dup, &out); err == nil || !strings.Contains(err.Error(), "already signed issuance "+first.TxID) {
			t.Fatalf("a follower agreed to sign a second, non-conflicting issuance: %v", err)
		}
	}
	anchor, st, err := e.f1.custodyCoin(first.Inputs[0], 0)
	if err != nil || st == coinMissing {
		t.Fatalf("the first issuance's coin: %v %v", st, err)
	}
	_, _, conflicting := issueTx(e.leader, anchor, []*Inbound{d})
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", conflicting, &out); err != nil {
		t.Fatalf("a conflicting issuance was refused: %v", err)
	}

	// 5. Mined: the user holds the deposit's uARG exactly once.
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if got := e.uargBalance(e.uAddr); got != d.Amount {
		t.Fatalf("user holds %d uARG units for a deposit of %d", got, d.Amount)
	}
	for _, s := range []*Service{e.leader, e.f1, e.f2} {
		if x, _ := s.Inbound(e.chains[0].ID, id); x.Status != InboundConfirmed {
			t.Fatalf("not settled: %+v", x)
		}
	}
}

// TestGuardianRequestAuth: only fresh requests signed by the leader's key,
// with the secret, each accepted once, reach a follower's signing code.
func TestGuardianRequestAuth(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	url := e.urls[1] + "/guardian/sign-release"
	body := []byte(`{"txid":"` + strings.Repeat("00", 32) + `"}`)
	now := time.Now()
	nonce := func() []byte { b := make([]byte, authNonceLen); rand.Read(b); return b }
	// An authenticated request gets past the check: the burn is unknown (400).
	good, err := e.leader.guardianHeadersAt("/guardian/sign-release", body, now.Unix(), nonce())
	if err != nil {
		t.Fatal(err)
	}
	if code, ans := postGuardian(t, url, body, good); code != http.StatusBadRequest || !strings.Contains(ans, "not found") {
		t.Fatalf("authenticated request: %d %s", code, ans)
	}
	// Replayed as it was: refused.
	if code, ans := postGuardian(t, url, body, good); code != http.StatusUnauthorized || !strings.Contains(ans, "replayed") {
		t.Fatalf("replay: %d %s", code, ans)
	}
	refused := func(what string, hdr http.Header, b []byte, u string) {
		t.Helper()
		if code, ans := postGuardian(t, u, b, hdr); code != http.StatusUnauthorized {
			t.Fatalf("%s: %d %s", what, code, ans)
		}
	}
	// Signed by another guardian's key (f2 is a guardian, not the leader).
	h, _ := e.f2.guardianHeadersAt("/guardian/sign-release", body, now.Unix(), nonce())
	refused("signed by a follower", h, body, url)
	// Stale and future timestamps.
	h, _ = e.leader.guardianHeadersAt("/guardian/sign-release", body, now.Add(-2*authSkew).Unix(), nonce())
	refused("stale", h, body, url)
	h, _ = e.leader.guardianHeadersAt("/guardian/sign-release", body, now.Add(2*authSkew).Unix(), nonce())
	refused("from the future", h, body, url)
	// The right key but the wrong secret.
	n := nonce()
	h, _ = e.leader.guardianHeadersAt("/guardian/sign-release", body, now.Unix(), n)
	h.Set(hdrGuardianMAC, authMAC("another-secret-that-is-at-least-32-chars", authPreimage(consensus.RegTest.Name, e.cs[0].XOnly(), "/guardian/sign-release", now.Unix(), n, body)))
	refused("wrong secret", h, body, url)
	// A signature for one path or body is no good for another.
	h, _ = e.leader.guardianHeadersAt("/guardian/sign-release", body, now.Unix(), nonce())
	refused("other path", h, body, e.urls[1]+"/guardian/nonces")
	h, _ = e.leader.guardianHeadersAt("/guardian/sign-release", body, now.Unix(), nonce())
	refused("other body", h, []byte(`{"txid":"01"}`), url)
	// No headers at all.
	refused("bare", http.Header{}, body, url)
	// The leader refuses guardian requests whoever signs them.
	h, _ = e.leader.guardianHeadersAt("/guardian/sign-release", body, now.Unix(), nonce())
	if code, _ := postGuardian(t, e.urls[0]+"/guardian/sign-release", body, h); code != http.StatusForbidden {
		t.Fatalf("leader answered a guardian request: %d", code)
	}

	// Configuration: a follower without the leader's key, or with a key that
	// is not a guardian's, or a short secret, does not start.
	base := Config{Params: consensus.RegTest, Node: e.client, Chains: e.chains, EVMs: map[uint64]EVM{e.chains[0].ID: e.evm}, Custody: e.cs[1], Threshold: 2,
		Secret: testSecret, StatePath: filepath.Join(t.TempDir(), "x")}
	stranger, _ := btcec.NewPrivateKey()
	for name, mod := range map[string]func(*Config){
		"no leader key":        func(c *Config) {},
		"stranger as leader":   func(c *Config) { c.LeaderKey = stranger.PubKey().SerializeCompressed() },
		"itself as leader":     func(c *Config) { c.LeaderKey = e.cs[1].LocalKey() },
		"short secret":         func(c *Config) { c.LeaderKey, c.Secret = e.cs[0].LocalKey(), "short" },
		"leader, short secret": func(c *Config) { c.Leader, c.Peers, c.Secret = true, []string{"http://x"}, "short" },
	} {
		cfg := base
		mod(&cfg)
		if s, err := New(cfg); err == nil {
			s.Stop()
			t.Errorf("%s: started", name)
		}
	}
}

// TestPoCRewindSwallowsNextDeposit (ported): a cursor that disagrees with
// the node, or a node that cannot be reached, no longer re-indexes a
// settled issuance; the next deposit with the same address and amount is
// issued like any other.
func TestPoCRewindSwallowsNextDeposit(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	id1 := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d1, _ := e.leader.Inbound(e.chains[0].ID, id1)
	if d1.Status != InboundConfirmed {
		t.Fatalf("first deposit %+v", d1)
	}
	before := e.uargBalance(e.uAddr)
	// The cursor's hash goes wrong: the guardian's own record of the block
	// agrees with the node, so nothing is undone.
	h, hash, _ := e.leader.st.Cursor("l1")
	e.leader.st.SetCursor("l1", h, "00")
	if err := e.leader.scanL1(ctx); err != nil {
		t.Fatal(err)
	}
	if h2, hash2, _ := e.leader.st.Cursor("l1"); h2 < h || (h2 == h && hash2 != hash) {
		t.Fatalf("cursor after the check: %d %s", h2, hash2)
	}
	// The node cannot be reached: an error, not a reorganisation.
	node := e.leader.cfg.Node
	e.leader.cfg.Node = wallet.NewClient("http://127.0.0.1:1", "t")
	if err := e.leader.scanL1(ctx); err == nil {
		t.Fatal("scan without a node succeeded")
	}
	e.leader.cfg.Node = node
	if d, _ := e.leader.Inbound(e.chains[0].ID, id1); d.Status != InboundConfirmed || d.IssueTx != d1.IssueTx {
		t.Fatalf("first deposit after the checks: %+v", d)
	}
	// Same user, same amount: issued.
	id2 := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.leader.Inbound(e.chains[0].ID, id2)
	if d2.Status != InboundConfirmed || d2.IssueTx == d1.IssueTx {
		t.Fatalf("second deposit: %+v", d2)
	}
	if after := e.uargBalance(e.uAddr); after != before+d2.Amount {
		t.Fatalf("uARG before %d, after %d: the second deposit was not issued", before, after)
	}
}

// TestPoCEVMReorgedDepositStillIssued (ported): a deposit whose event left
// the EVM chain is not issued, by the leader or by a follower asked directly.
// Since the second review the scan acts on final blocks only and looks a
// deposit up again by its transaction when its block is reached: one whose
// transaction is gone is marked reorged (it was held as pending before).
func TestPoCEVMReorgedDepositStillIssued(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	id := e.evm.addDeposit(usdc, from, usd(500, 6), e.uAddr)
	e.scanAll()
	d, _ := e.leader.Inbound(e.chains[0].ID, id)
	// Reorg: the deposit disappears from the chain.
	e.evm.mu.Lock()
	e.evm.deposits = nil
	e.evm.mu.Unlock()
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if got, _ := e.leader.Inbound(e.chains[0].ID, id); got.Status != InboundReorged || !strings.Contains(got.Error, "no longer in block") {
		t.Fatalf("reorged-out deposit: %+v", got)
	}
	if b := e.uargBalance(e.uAddr); b != 0 {
		t.Fatalf("user holds %d uARG", b)
	}
	if st := e.leader.Status(); st.Reorged != 1 || st.Pending != 0 {
		t.Fatalf("reorged deposits in the status: %d (pending %d)", st.Reorged, st.Pending)
	}
	_, _, req := issueTx(e.leader, e.matureCoins(e.f1)[0], []*Inbound{d})
	var out struct{}
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "not in block") {
		t.Fatalf("follower signed a reorged-out deposit: %v", err)
	}
}

// burnFirstCoin burns the user's newest uARG coin with memo, paying the fee
// from the coinbase of block cbHeight, and returns the burn's id.
func (e *pocEnv) burnFirstCoin(memo []byte, cbHeight uint32) string {
	t := e.t
	t.Helper()
	asset := e.leader.Asset()
	info, _ := e.client.ChainInfo()
	var ucoin wallet.TxOutJSON
	var utx string
	for h := info.Blocks; h > 0 && utx == ""; h-- {
		bh, _ := e.client.BlockHash(h)
		blk, _ := e.client.Block(bh)
		for _, tx := range blk.Tx {
			for _, o := range tx.Outputs {
				if o.Asset == asset.String() && o.Address == e.uAddr {
					ucoin, utx = o, tx.TxID
				}
			}
		}
	}
	cbBlock, _ := e.client.Block(mustBlockHash(t, e.client, cbHeight))
	cb := cbBlock.Tx[0]
	tx := &consensus.Tx{Version: consensus.TxVersionAssets,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: mustHash(t, utx), Index: uint32(ucoin.N)}}, {PrevOut: consensus.OutPoint{TxID: mustHash(t, cb.TxID), Index: 0}}},
		Outputs:   []consensus.TxOut{{Type: consensus.OutputTypeNullData, Payload: memo}, e.user.Output(cb.Outputs[0].Amount - 5000)},
		Witnesses: make([][]byte, 2)}
	spent := []consensus.TxOut{{Amount: ucoin.Amount, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly, Asset: asset}, {Amount: cb.Outputs[0].Amount, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly}}
	for i := range tx.Inputs {
		if err := e.user.Sign(consensus.RegTest.ChainTag, tx, spent, i); err != nil {
			t.Fatal(err)
		}
	}
	burn, err := e.client.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
	if err != nil {
		t.Fatal(err)
	}
	return burn
}

// issueAndBurn issues a 100 USDC deposit and burns it for USDC on the
// chain's current vault, released once. It returns the burn's id.
func (e *pocEnv) issueAndBurn() string {
	t := e.t
	t.Helper()
	ctx := context.Background()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	var recipient [20]byte
	recipient[19] = 0xCD
	burn := e.burnFirstCoin(EncodeMemo(recipient), 9)
	e.mine(4, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.leader.Once(ctx)
	if len(e.evm.releases) != 1 {
		t.Fatalf("first release: %d", len(e.evm.releases))
	}
	return burn
}

// TestPoCRedeployRepaysOldBurn (ported): a vault replaced on the same chain
// with every guardian's state rebuilt from nothing, following the
// documented procedure (l1_start_height set to the current height plus
// one), does not pay an old burn again; neither does a vault replaced under
// guardians that keep their state, even without l1_start_height.
func TestPoCRedeployRepaysOldBurn(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	burn := e.issueAndBurn()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte

	// A second burn, still waiting for its confirmations when the vault is
	// replaced.
	e.evm.addDeposit(usdc, from, usd(50, 6), e.uAddr)
	e.evm.advance(10)
	e.round()
	e.mine(1, e.uAddr)
	e.round()
	var recipient [20]byte
	recipient[19] = 0xCD
	late := e.burnFirstCoin(EncodeMemo(recipient), 10)
	e.mine(1, e.uAddr)
	e.scanAll()
	if r, ok := e.leader.st.GetRedemption(late); !ok || r.Status != RedemptionPending {
		t.Fatalf("second burn: %+v", r)
	}
	info, _ := e.client.ChainInfo()

	// (a) Fresh state everywhere, the documented procedure followed.
	newChains := append([]Chain{}, e.chains...)
	newChains[0].Vault[19] = 0x78
	newChains[0].L1StartHeight = info.Blocks + 1
	evm2 := newFakeEVM(newChains[0].ID, newChains[0].Vault, e.evmAddrs, 2, 100, 100)
	evm2.addDeposit(usdc, from, usd(1000, 6), e.uAddr) // other users' money in the new vault
	g1 := e.mk(1, false, nil, "n1", newChains, evm2)
	g2 := e.mk(2, false, nil, "n2", newChains, evm2)
	s1, s2 := httptest.NewServer(g1.Handler()), httptest.NewServer(g2.Handler())
	defer s1.Close()
	defer s2.Close()
	l2 := e.mk(0, true, []string{s1.URL, s2.URL}, "n0", newChains, evm2)
	for _, g := range []*Service{g1, g2} {
		g.scanEVM(ctx)
		g.scanL1(ctx)
	}
	l2.Once(ctx)
	e.mine(3, e.uAddr)
	l2.Once(ctx)
	if len(evm2.releases) != 0 {
		t.Fatalf("the new vault paid %d old burns", len(evm2.releases))
	}
	for _, id := range []string{burn, late} {
		if r, ok := l2.st.GetRedemption(id); !ok || r.Status != RedemptionUnreleasable || !strings.Contains(r.Error, "predates") {
			t.Fatalf("burn %s on the new vault: %+v", id, r)
		}
		if _, err := g1.SignReleaseLocally(ctx, id); err == nil || !strings.Contains(err.Error(), "predates") {
			t.Fatalf("a follower signed burn %s below l1_start_height: %v", id, err)
		}
	}

	// (b) The same guardians, their state kept, the vault replaced without
	// l1_start_height: the new vault starts a new epoch at the height the
	// guardians first see it. (It numbers its deposits above the two of the
	// old vault, as a replacement must: one that does not is never used,
	// TestVaultReplacementDepositIDs, and its payouts wait.)
	e.stop()
	same := append([]Chain{}, e.chains...)
	same[0].Vault[19] = 0x79
	evm3 := newFakeEVM(same[0].ID, same[0].Vault, e.evmAddrs, 2, 100, 100)
	evm3.firstID = 3
	evm3.addDeposit(usdc, from, usd(1000, 6), e.uAddr)
	e.start("g", same, evm3)
	e.mine(3, e.uAddr)
	e.round()
	e.leader.Once(ctx)
	if len(evm3.releases) != 0 {
		t.Fatalf("the replaced vault paid %d burns made before it", len(evm3.releases))
	}
	if r, _ := e.leader.st.GetRedemption(late); r.Status != RedemptionUnreleasable || !strings.Contains(r.Error, "predates") {
		t.Fatalf("pending burn after the vault changed: %+v", r)
	}
}

// TestBurnSignedOnce: a guardian signs a burn for one release only. After
// the vault is replaced and every guardian's state is rebuilt from an older
// format, with the epochs and the redemption itself forgotten (so nothing
// else stands in the way), the burn is still not signed for the new vault.
func TestBurnSignedOnce(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	burn := e.issueAndBurn()
	// Asked again for the same release, a follower signs the same thing.
	sig1, err := e.f1.SignReleaseLocally(ctx, burn)
	if err != nil {
		t.Fatal(err)
	}
	sig2, err := e.f1.SignReleaseLocally(ctx, burn)
	if err != nil || !bytes.Equal(sig1, sig2) {
		t.Fatalf("the same release signed differently: %v", err)
	}
	e.stop()
	for _, name := range []string{"g0", "g1", "g2"} {
		st, err := openState(filepath.Join(e.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var ops []kvOp
		st.eachKey([]byte(pfxEpoch), func(k []byte) { ops = append(ops, kvOp{key: k}) })
		ops = append(ops, kvOp{key: redemptionKey(burn)})
		if err := st.write(ops, true); err != nil {
			t.Fatal(err)
		}
		st.put([]byte("format"), 3)
		st.Close()
	}
	newChains := append([]Chain{}, e.chains...)
	newChains[0].Vault[19] = 0x78
	evm2 := newFakeEVM(newChains[0].ID, newChains[0].Vault, e.evmAddrs, 2, 100, 100)
	var from [20]byte
	evm2.addDeposit(newChains[0].Tokens[0].Address, from, usd(1000, 6), e.uAddr)
	e.start("g", newChains, evm2)
	if e.leader.st.MigratedFrom != 3 {
		t.Fatal("the state was not migrated")
	}
	e.round()
	r, ok := e.leader.st.GetRedemption(burn)
	if !ok {
		t.Fatal("the rescan did not find the burn again")
	}
	e.leader.Once(ctx)
	if len(evm2.releases) != 0 {
		t.Fatal("a burn was released twice, on two vaults")
	}
	if r, _ = e.leader.st.GetRedemption(burn); r.Status != RedemptionUnreleasable || !strings.Contains(r.Error, "already signed") {
		t.Fatalf("burn after the rebuild: %+v", r)
	}
	if _, err := e.f1.SignReleaseLocally(ctx, burn); err == nil || !strings.Contains(err.Error(), "already signed") {
		t.Fatalf("a follower signed the burn for a second vault: %v", err)
	}
}

// TestIssueMemo pins the deposit-reference memo and its limits.
func TestIssueMemo(t *testing.T) {
	refs := []InboundRef{{Chain: 11155111, ID: 1}, {Chain: 97, ID: 300}}
	m, err := EncodeIssueMemo(refs)
	if err != nil {
		t.Fatal(err)
	}
	// "BRI1", 11155111 = a7 ed a8 05, 1 = 01, 97 = 61, 300 = ac 02.
	if hex.EncodeToString(m) != hex.EncodeToString([]byte("BRI1"))+"a7eda805"+"01"+"61"+"ac02" {
		t.Fatalf("memo %x", m)
	}
	got, ok := DecodeIssueMemo(m)
	if !ok || len(got) != 2 || got[0] != refs[0] || got[1] != refs[1] {
		t.Fatalf("decoded %+v %v", got, ok)
	}
	// Fifteen Sepolia deposits fit (4 + 15 × 5 = 79 bytes), sixteen do not.
	var many []InboundRef
	for i := 1; i <= 16; i++ {
		many = append(many, InboundRef{Chain: 11155111, ID: uint64(i)})
	}
	if _, err := EncodeIssueMemo(many[:15]); err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeIssueMemo(many); err == nil {
		t.Fatal("an 84-byte memo was built")
	}
	for name, b := range map[string][]byte{
		"empty":         []byte("BRI1"),
		"other prefix":  append([]byte("BRG1"), m[4:]...),
		"no id":         append([]byte("BRI1"), 0x61),
		"zero id":       append([]byte("BRI1"), 0x61, 0x00),
		"zero chain":    append([]byte("BRI1"), 0x00, 0x01),
		"duplicate":     append([]byte("BRI1"), 0x61, 0x01, 0x61, 0x01),
		"non-minimal":   append([]byte("BRI1"), 0xe1, 0x00, 0x01),
		"too long":      append([]byte("BRI1"), bytes.Repeat([]byte{0x61, 0x01}, 40)...),
		"redeem memo":   EncodeMemo([20]byte{1}),
		"redeem memo 2": EncodeRedeemMemo(97, [20]byte{1}, [20]byte{2}),
	} {
		if _, ok := DecodeIssueMemo(b); ok {
			t.Errorf("%s decoded", name)
		}
	}
	if _, err := EncodeIssueMemo([]InboundRef{{Chain: 0, ID: 1}}); err == nil {
		t.Fatal("a zero chain id was referenced")
	}
}

// TestIssuanceFeePolicy: the issuance fee follows the nodes' relay policy
// for asset outputs.
func TestIssuanceFeePolicy(t *testing.T) {
	if assetOutputFee != mempool.DefaultConfig().AssetOutputFee || freeAssetOutputs != mempool.FreeAssetOutputs {
		t.Fatalf("the bridge's asset output fee (%d beyond %d) differs from the relay policy (%d beyond %d)", assetOutputFee, freeAssetOutputs, mempool.DefaultConfig().AssetOutputFee, mempool.FreeAssetOutputs)
	}
	tx := &consensus.Tx{Version: consensus.TxVersionAssets}
	for i := 0; i < 5; i++ {
		tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: 1, Type: consensus.OutputTypeP2PK, Payload: make([]byte, 32), Asset: consensus.Hash{1}})
	}
	if got, want := assetSurcharge(tx), mempool.AssetSurcharge(mempool.DefaultConfig(), tx); got != want || got == 0 {
		t.Fatalf("surcharge %d, policy %d", got, want)
	}
}

// mineAlt mines a block on chB paying to.
func mineAlt(t *testing.T, chB *chain.Chain, to consensus.TxOut) *consensus.Block {
	t.Helper()
	blk, err := miner.BuildTemplate(chB, mempool.New(chB, mempool.DefaultConfig()), to, []byte("alt"), int64(chB.Tip().Header.Time)+600)
	if err != nil {
		t.Fatal(err)
	}
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, consensus.RegTest.PowLimit) == nil {
			break
		}
	}
	if _, err := chB.ProcessBlock(blk, -1); err != nil {
		t.Fatal(err)
	}
	return blk
}

// TestSettlementAcrossReorg: an issuance carries its deposit references in
// a NULLDATA output and every guardian settles from them. Argonmine then
// reorganises the issuance (and a block paying custody) away: every
// guardian undoes exactly what those blocks did; the issuance mined again
// settles the same deposit again; and the next deposit with the same
// address and amount is issued, not swallowed.
func TestSettlementAcrossReorg(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	id1 := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d, _ := e.leader.Inbound(e.chains[0].ID, id1)
	issue := d.IssueTx
	// The block that mines the issuance pays its coinbase to custody.
	e.mine(1, e.cs[0].Address())
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	info, _ := e.client.ChainInfo()
	x := info.Blocks
	blk, _ := e.client.Block(mustBlockHash(t, e.client, x))
	var issueHex, coinbase string
	var change uint32
	for _, tx := range blk.Tx {
		if tx.Coinbase {
			coinbase = tx.TxID
		}
		if tx.TxID != issue {
			continue
		}
		issueHex = tx.Hex
		var memo []byte
		for _, o := range tx.Outputs {
			if o.Type == consensus.OutputTypeNullData {
				memo, _ = hex.DecodeString(o.Payload)
			}
			if o.Asset == "" && o.Address == e.cs[0].Address() {
				change = uint32(o.N)
			}
		}
		if refs, ok := DecodeIssueMemo(memo); !ok || len(refs) != 1 || refs[0] != (InboundRef{Chain: e.chains[0].ID, ID: id1}) {
			t.Fatalf("issuance memo %x", memo)
		}
	}
	if issueHex == "" {
		t.Fatal("issuance not in its block")
	}
	for _, s := range []*Service{e.leader, e.f1, e.f2} {
		if x, _ := s.Inbound(e.chains[0].ID, id1); x.Status != InboundConfirmed || x.IssueTx != issue {
			t.Fatalf("not settled by its references: %+v", x)
		}
		if !s.st.HasUTXO(coinbase, 0) || !s.st.HasUTXO(issue, change) {
			t.Fatal("custody coins of the block not recorded")
		}
	}
	spentCoin := e.f1.st.SignedIssuesFor(InboundRef{Chain: e.chains[0].ID, ID: id1})[0].Inputs[0]

	// Reorganise: a branch from block x-1, two blocks long, without the
	// issuance.
	chB, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer chB.Close()
	for h := uint32(1); h < x; h++ {
		n, _ := e.ch.NodeAtHeight(h)
		b, err := e.ch.Block(n.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chB.ProcessBlock(b, -1); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := btcec.NewPrivateKey()
	pay := consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: schnorr.SerializePubKey(other.PubKey())}
	b1, b2 := mineAlt(t, chB, pay), mineAlt(t, chB, pay)
	if _, err := e.ch.ProcessBlockOpt(b1, -1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ch.ProcessBlockOpt(b2, -1, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.ch.NodeAtHeight(x); n.Hash != b1.Header.Hash() {
		t.Fatal("the node did not reorganise")
	}
	e.scanAll()
	for i, s := range []*Service{e.leader, e.f1, e.f2} {
		x, _ := s.Inbound(e.chains[0].ID, id1)
		want := InboundPending
		if i == 0 {
			want = InboundIssued // as it was before the block: broadcast by the leader
		}
		if x.Status != want {
			t.Fatalf("guardian %d after the reorg: %+v", i, x)
		}
		if s.st.HasUTXO(coinbase, 0) {
			t.Fatalf("guardian %d kept the custody coinbase of an orphaned block", i)
		}
		// The issuance's change: gone on the followers; on the leader, which
		// broadcast it, back to what it was before the block (unconfirmed).
		var u UTXO
		if has := s.st.get(utxoKey(issue, change), &u); (i > 0 && has) || (i == 0 && (!has || u.Height != 0)) {
			t.Fatalf("guardian %d: change of the orphaned issuance %v %+v", i, has, u)
		}
		if _, ok := s.st.CustodySpend(spentCoin); ok {
			t.Fatalf("guardian %d kept the spend of an orphaned block", i)
		}
		if txid, idx, _ := parseOutpoint(spentCoin); i > 0 && !s.st.HasUTXO(txid, idx) {
			t.Fatalf("guardian %d did not get the coin the orphaned issuance spent back", i)
		}
	}
	// Mined again (the same transaction: its coin is unspent on the new
	// branch), it settles the same deposit again.
	if _, err := e.client.SendRawTransaction(issueHex); err != nil && !e.mp.Has(mustHash(t, issue)) {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	before := e.uargBalance(e.uAddr)
	for _, s := range []*Service{e.leader, e.f1, e.f2} {
		if x, _ := s.Inbound(e.chains[0].ID, id1); x.Status != InboundConfirmed || x.IssueTx != issue {
			t.Fatalf("not settled again: %+v", x)
		}
	}
	// The same address and amount again: a new deposit, issued.
	id2 := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.leader.Inbound(e.chains[0].ID, id2)
	if d2.Status != InboundConfirmed || e.uargBalance(e.uAddr) != before+d2.Amount {
		t.Fatalf("second deposit: %+v", d2)
	}
}

// TestIssuanceLimits: a deposit above the per-issuance limit waits and is
// reported; the daily limit holds back what would exceed it; and each
// follower enforces both limits itself.
func TestIssuanceLimits(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t, func(c *Config) { c.MaxIssue, c.MaxIssueDay = 50*consensus.UnitsPerCoin, 100*consensus.UnitsPerCoin })
	ctx := context.Background()
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	big := e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr) // 99 uARG: above the per-issuance limit
	var small []uint64
	for i := 0; i < 3; i++ {
		small = append(small, e.evm.addDeposit(usdc, from, usd(40, 6), e.uAddr)) // 39.6 uARG each
	}
	e.evm.advance(10)
	for i := 0; i < 3; i++ {
		if err := e.round(); err != nil {
			t.Fatal(err)
		}
		e.mine(1, e.uAddr)
	}
	e.round()
	ref := func(id uint64) *Inbound { d, _ := e.leader.Inbound(e.chains[0].ID, id); return d }
	if d := ref(big); d.Status != InboundPending || !strings.Contains(d.Error, "per-issuance limit") {
		t.Fatalf("large deposit: %+v", d)
	}
	if ref(small[0]).Status != InboundConfirmed || ref(small[1]).Status != InboundConfirmed {
		t.Fatalf("small deposits: %+v %+v", ref(small[0]), ref(small[1]))
	}
	if d := ref(small[2]); d.Status != InboundPending || !strings.Contains(d.Error, "daily issuance limit") {
		t.Fatalf("third small deposit: %+v", d)
	}
	st := e.leader.Status()
	if st.Held != 2 || st.IssueLimit != 50*consensus.UnitsPerCoin || st.IssueDayLimit != 100*consensus.UnitsPerCoin || st.IssuedDay != 2*3_960_000_000 {
		t.Fatalf("status: held %d, limits %d %d, issued today %d", st.Held, st.IssueLimit, st.IssueDayLimit, st.IssuedDay)
	}
	// A leader that ignores the limits gets nothing signed.
	coins := e.matureCoins(e.f1)
	var out struct{}
	_, _, req := issueTx(e.leader, coins[0], []*Inbound{ref(big)})
	if err := e.leader.callPeer(ctx, e.urls[1], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "per-issuance limit") {
		t.Fatalf("follower signed above its per-issuance limit: %v", err)
	}
	_, _, req = issueTx(e.leader, coins[1], []*Inbound{ref(small[2])})
	if err := e.leader.callPeer(ctx, e.urls[2], "/guardian/nonces", req, &out); err == nil || !strings.Contains(err.Error(), "daily limit") {
		t.Fatalf("follower signed above its daily limit: %v", err)
	}
}

// offlineService is a leader with no node, for the API tests.
func offlineService(t *testing.T) *Service {
	t.Helper()
	chains, err := ParseChains([]byte(testChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := guardians(t, 1)
	evms := map[uint64]EVM{}
	for _, c := range chains {
		evms[c.ID] = newFakeEVM(c.ID, c.Vault, nil, 1, 0, 0)
	}
	s, err := New(Config{Params: consensus.RegTest, Node: wallet.NewClient("http://127.0.0.1:1", "x"), Chains: chains, EVMs: evms,
		Custody: cs[0], Leader: true, Threshold: 1, StatePath: filepath.Join(t.TempDir(), "s"), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s
}

// TestHandoffCodes: ten characters without look-alikes, shown in two
// groups, read once, and rate-limited per client.
func TestHandoffCodes(t *testing.T) {
	s := offlineService(t)
	ws := httptest.NewServer(s.Handler())
	defer ws.Close()
	call := func(method, path, ip, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ws.URL+path, strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	to := s.cfg.Custody.Address()
	post := func(ip string) (int, Handoff) {
		code, body := call(http.MethodPost, "/api/handoff", ip, `{"kind":"send","amount":"1.5","to":"`+to+`"}`)
		var h Handoff
		json.Unmarshal([]byte(body), &h)
		return code, h
	}
	code, h := post("203.0.113.1")
	if code != 200 || !regexp.MustCompile(`^[2-9A-HJ-NP-Z]{5}-[2-9A-HJ-NP-Z]{5}$`).MatchString(h.Code) || h.Amount != "1.50000000" || h.Asset != "uARG" {
		t.Fatalf("hand-off: %d %+v", code, h)
	}
	// A send names its coin (uARG unless ARG), and never goes to a swap address.
	if code, body := call(http.MethodPost, "/api/handoff", "203.0.113.9", `{"kind":"send","asset":"arg","amount":"2","to":"`+to+`"}`); code != 200 || !strings.Contains(body, `"asset":"ARG"`) {
		t.Fatalf("ARG send hand-off: %d %s", code, body)
	}
	if code, _ := call(http.MethodPost, "/api/handoff", "203.0.113.9", `{"kind":"send","asset":"WARG","amount":"2","to":"`+to+`"}`); code != 400 {
		t.Fatalf("wARG send hand-off: %d", code)
	}
	sa, _ := SwapAddress(consensus.RegTest.HRP, SwapDest{ChainID: 1, Symbol: "USDC", To: [20]byte{1}})
	if code, body := call(http.MethodPost, "/api/handoff", "203.0.113.9", `{"kind":"send","amount":"2","to":"`+sa+`"}`); code != 400 || !strings.Contains(body, "swap address") {
		t.Fatalf("send hand-off to a swap address: %d %s", code, body)
	}
	// Read once, typed any way, then gone.
	typed := strings.ToLower(strings.ReplaceAll(h.Code, "-", " "))
	if code, body := call(http.MethodGet, "/api/handoff/"+strings.ReplaceAll(typed, " ", "%20"), "203.0.113.2", ""); code != 200 || !strings.Contains(body, `"to":"`+to+`"`) {
		t.Fatalf("read: %d %s", code, body)
	}
	if code, _ := call(http.MethodGet, "/api/handoff/"+h.Code, "203.0.113.2", ""); code != 404 {
		t.Fatalf("a hand-off was read twice: %d", code)
	}
	// Reading is rate-limited per client: a guesser is cut off.
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		code, _ := call(http.MethodGet, "/api/handoff/AAAAA-AAAAA", "198.51.100.7", "")
		limited = code == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("hand-off reads are not rate-limited")
	}
	// So is making them; and one client holds a bounded number at a time.
	n := 0
	for i := 0; i < 15; i++ {
		if code, _ := post("198.51.100.8"); code == 200 {
			n++
		} else if code != http.StatusTooManyRequests {
			t.Fatalf("hand-off %d: %d", i, code)
		}
	}
	if n == 0 || n > handoffPerIP {
		t.Fatalf("%d hand-offs from one client", n)
	}
	st := newHandoffStore()
	for i := 0; i < handoffPerIP; i++ {
		if _, err := st.put(Handoff{Kind: "send"}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.put(Handoff{Kind: "send"}, "x"); err == nil {
		t.Fatal("one client exceeded its pending hand-offs")
	}
	if _, err := st.put(Handoff{Kind: "send"}, "y"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "12345-67890", "ABCDE-FGHIO", "ABCDE-FGHJ", "ABCDEFGHJKL"} {
		if normalizeHandoffCode(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestPublicAPIStatusCacheAndErrors: the status is computed at most once a
// few seconds; filtered lists come from the indexes; and no URL path, query
// string or credential reaches the public API in an error.
func TestPublicAPIStatusCacheAndErrors(t *testing.T) {
	s := offlineService(t)
	ws := httptest.NewServer(s.Handler())
	defer ws.Close()
	get := func(path string) string {
		t.Helper()
		resp, err := http.Get(ws.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	status := func() Status {
		var st Status
		if err := json.Unmarshal([]byte(get("/api/status")), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := status(); st.Deposits != 0 {
		t.Fatalf("deposits %d", st.Deposits)
	}
	key := "SECRETKEY123"
	s.chains[0].setScanErr(fmt.Errorf(`evm: Post "https://user:pw@eth-sepolia.example.com/v2/%s?apikey=%s": dial tcp: i/o timeout`, key, key))
	s.setErr(fmt.Errorf("Local A: evm: Post \"wss://rpc.example.org/ws/%s\": EOF; token=%s", key, key))
	s.st.PutInbound(&Inbound{Chain: 31337, ID: 1, To: "rarg1abc", EVMTx: "0xAA", Status: InboundPending, Error: "https://x.example/" + key, Created: 5})
	s.st.PutRedemption(&Redemption{TxID: "ab", Status: RedemptionPending, Error: `Post "https://rpc.example/` + key + `": refused`})
	// Served from the cache: the new deposit is not counted yet.
	if st := status(); st.Deposits != 0 {
		t.Fatal("the status was not cached")
	}
	s.snapMu.Lock()
	s.snap.at = time.Now().Add(-time.Minute)
	s.snapMu.Unlock()
	st := status()
	if st.Deposits != 1 || st.Held != 1 {
		t.Fatalf("status after the cache expired: %d deposits, %d held", st.Deposits, st.Held)
	}
	if !strings.Contains(st.Chains[0].Error, "https://eth-sepolia.example.com") || !strings.Contains(st.LastError, "wss://rpc.example.org") {
		t.Fatalf("errors lost their host: %q %q", st.Chains[0].Error, st.LastError)
	}
	for _, body := range []string{get("/api/status"), get("/api/deposits"), get("/api/deposits?to=rarg1abc"), get("/api/redemptions"), get("/api/redemptions?txids=ab"), get("/api/deposit/0xaa"), get("/api/redemption/ab")} {
		if strings.Contains(body, key) || strings.Contains(body, "user:pw") {
			t.Fatalf("a secret reached the public API: %s", body)
		}
	}
	if body := get("/api/deposits?to=rarg1abc"); !strings.Contains(body, `"id":1`) {
		t.Fatalf("filtered deposits: %s", body)
	}
	if body := get("/api/deposits?to=rarg1zzz"); body != "[]\n" {
		t.Fatalf("filtered deposits for another address: %s", body)
	}
	if body := get("/api/deposit/0xaa"); !strings.Contains(body, `"id":1`) {
		t.Fatalf("deposit by transaction: %s", body)
	}
	for in, want := range map[string]string{
		`Post "https://a.example/v3/KEY": EOF`:      `Post "https://a.example": EOF`,
		`see https://a.example:8545/path?x=1.`:      `see https://a.example:8545.`,
		`apikey=KEY&x=1 failed`:                     `apikey=[redacted]&x=1 failed`,
		`dial tcp: lookup a.example: no such host`:  `dial tcp: lookup a.example: no such host`,
		`(http://127.0.0.1:19332/?token=abc) down`:  `(http://127.0.0.1:19332) down`,
		`password=hunter2 and secret=s3 and auth=x`: `password=[redacted] and secret=[redacted] and auth=[redacted]`,
	} {
		if got := publicErr(in); got != want {
			t.Errorf("publicErr(%q) = %q, want %q", in, got, want)
		}
	}
}
