package swap

import (
	"context"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/rpc"
	"argonmine/wallet"
)

const unit = consensus.Amount(consensus.UnitsPerCoin)

// clock is real time plus whatever a test moved it by.
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.off) }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	node  *wallet.Client
	pool  *mempool.Pool
	maker *wallet.Wallet
	user  *wallet.Wallet
	m     *Maker
	c     *Client
	uarg  consensus.Hash
	sink  string // where blocks pay when nobody should get the reward
	clk   *clock
	srv   *httptest.Server
}

// newEnv runs a regtest node, a maker with 6 mature coinbases (300 ARG)
// and 500 uARG, and a user with 4 mature coinbases (200 ARG) and 200 uARG.
// The price is 0.10 uARG per ARG with a 1% spread; wrapping is free.
func newEnv(t *testing.T) *env {
	t.Helper()
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
	e := &env{t: t, node: wallet.NewClient("http://"+srv.Addr(), "t"), pool: mp, clk: &clock{}}
	dir := t.TempDir()
	mk := func(name string) *wallet.Wallet {
		w, _, err := wallet.Create(filepath.Join(dir, name+".wallet"), consensus.RegTest, "pw", "")
		if err != nil {
			t.Fatal(err)
		}
		w.SetClient(e.node)
		return w
	}
	e.maker, e.user = mk("maker"), mk("user")
	sinkKey, _ := btcec.NewPrivateKey()
	e.sink, _ = wallet.EncodeAddress(consensus.RegTest.HRP, consensus.OutputTypeP2PK, schnorr.SerializePubKey(sinkKey.PubKey()))

	// The bridge's stand-in: a key that issues UARG.
	issuer, _ := btcec.NewPrivateKey()
	ix := schnorr.SerializePubKey(issuer.PubKey())
	iaddr, _ := wallet.EncodeAddress(consensus.RegTest.HRP, consensus.OutputTypeP2PK, ix)
	var i32 [32]byte
	copy(i32[:], ix)
	e.uarg = consensus.AssetID(i32, "UARG")

	makerAddr, _ := e.maker.ReceiveAddress()
	userAddr, _ := e.user.ReceiveAddress()
	e.mine(6, makerAddr)
	e.mine(4, userAddr)
	e.mine(1, iaddr)
	e.mine(100, e.sink)

	// Issue 700 UARG: 500 to the maker, 200 to the user.
	info, _ := e.node.ChainInfo()
	bh, _ := e.node.BlockHash(11)
	blk, _ := e.node.Block(bh)
	cb := blk.Tx[0]
	if cb.Outputs[0].Address != iaddr {
		t.Fatalf("block 11 pays %s", cb.Outputs[0].Address)
	}
	cbID, _ := consensus.HashFromHex(cb.TxID)
	mp_, _ := wallet.DecodeAddress(consensus.RegTest.HRP, makerAddr)
	up, _ := wallet.DecodeAddress(consensus.RegTest.HRP, userAddr)
	tx := &consensus.Tx{Version: consensus.TxVersionAssets,
		Inputs: []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: cbID, Index: 0}}},
		Outputs: []consensus.TxOut{
			{Amount: 500 * unit, Type: consensus.OutputTypeP2PK, Payload: mp_.Payload, Asset: e.uarg},
			{Amount: 200 * unit, Type: consensus.OutputTypeP2PK, Payload: up.Payload, Asset: e.uarg},
			{Amount: cb.Outputs[0].Amount - 20_000, Type: consensus.OutputTypeP2PK, Payload: ix},
		},
		Issuances: []consensus.Issuance{{Asset: e.uarg, Issuer: i32, Symbol: "UARG", Amount: 700 * unit}},
		Witnesses: make([][]byte, 2)}
	spent := []consensus.TxOut{{Amount: cb.Outputs[0].Amount, Type: consensus.OutputTypeP2PK, Payload: ix}}
	for _, idx := range []uint32{0, consensus.IssuanceSigIndex} {
		h := consensus.SigHash(consensus.RegTest.ChainTag, tx, spent, idx)
		sig, err := schnorr.Sign(issuer, h[:])
		if err != nil {
			t.Fatal(err)
		}
		if idx == 0 {
			tx.Witnesses[0] = sig.Serialize()
		} else {
			tx.Witnesses[1] = sig.Serialize()
		}
	}
	if _, err := e.node.SendRawTransaction(hex.EncodeToString(tx.Serialize())); err != nil {
		t.Fatalf("issuing UARG at height %d: %v", info.Blocks, err)
	}
	e.mine(1, e.sink)

	m, err := New(Config{Params: consensus.RegTest, Node: e.node, Wallet: e.maker, UARG: e.uarg, Price: big.NewRat(1, 10), SpreadBps: 100,
		MaxIn: map[string]consensus.Amount{UARG: 1000 * unit}, LedgerPath: filepath.Join(dir, "ledger.json"), Now: e.clk.now, Logf: t.Logf, RatePerSec: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	e.srv = httptest.NewServer(m.Handler())
	t.Cleanup(e.srv.Close)
	e.c = NewClient(e.srv.URL)
	e.sync()
	return e
}

func (e *env) mine(n int, addr string) {
	e.t.Helper()
	if _, err := e.node.Call("generatetoaddress", n, addr); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) sync() {
	e.t.Helper()
	for _, w := range []*wallet.Wallet{e.maker, e.user} {
		if _, err := w.Sync(); err != nil {
			e.t.Fatal(err)
		}
	}
}

// bal is the user's confirmed holding of a coin.
func (e *env) bal(w *wallet.Wallet, coin string) consensus.Amount {
	e.t.Helper()
	if coin == ARG {
		info, _ := e.node.ChainInfo()
		s, _, _ := w.Balance(info.Blocks)
		return s
	}
	id := e.uarg.String()
	if coin == WARG {
		id = e.m.WARG().String()
	}
	for _, b := range w.AssetBalances() {
		if b.Asset == id {
			return b.Confirmed
		}
	}
	return 0
}

func (e *env) assets() map[string]consensus.Hash {
	e.t.Helper()
	info, err := e.c.Info(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := Assets(info, e.uarg.String())
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func TestSwapEveryPair(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	assets := e.assets()
	cases := []struct {
		from, to string
		in, out  consensus.Amount
	}{
		{ARG, UARG, 10 * unit, 99_000_000},        // 10 ARG × 0.10 × 0.99
		{UARG, ARG, 5 * unit, 4950_000_000},       // 5 uARG ÷ 0.10 × 0.99
		{ARG, WARG, 20 * unit, 20 * unit},         // 1:1
		{WARG, UARG, 5 * unit, 49_500_000},        // like ARG
		{UARG, WARG, 2 * unit, 1980_000_000},      // like ARG
		{WARG, ARG, 3 * unit, 3 * unit},           // 1:1
		{ARG, UARG, 123_456_789, 12_222_222},      // rounding down
		{UARG, ARG, 1_000_000, 9_900_000},         // the smallest swap
		{WARG, ARG, 1980_000_000, 1980_000_000},   // the wARG bought with uARG
		{WARG, UARG, 2 * unit, 19_800_000},        // more of the first wrap
		{ARG, WARG, 50_000_000_00, 50_000_000_00}, // 50 ARG
	}
	var supply consensus.Amount
	for _, c := range cases {
		fromBefore, toBefore := e.bal(e.user, c.from), e.bal(e.user, c.to)
		p, err := Prepare(ctx, e.user, e.c, assets, c.from, c.to, c.in, 0)
		if err != nil {
			t.Fatalf("%s -> %s: %v", c.from, c.to, err)
		}
		if p.Quote.AmountOut != c.out {
			t.Fatalf("%s -> %s: quoted %d, want %d", c.from, c.to, p.Quote.AmountOut, c.out)
		}
		txid, err := p.Execute(ctx)
		if err != nil {
			t.Fatalf("%s -> %s: %v", c.from, c.to, err)
		}
		if ids, _ := e.node.RawMempool(); len(ids) != 1 || ids[0] != txid {
			t.Fatalf("%s -> %s: mempool %v, want %s", c.from, c.to, ids, txid)
		}
		e.mine(1, e.sink)
		e.sync()
		if got := fromBefore - e.bal(e.user, c.from); got != c.in {
			t.Fatalf("%s -> %s: the user paid %d %s, want %d", c.from, c.to, got, c.from, c.in)
		}
		if got := e.bal(e.user, c.to) - toBefore; got != c.out {
			t.Fatalf("%s -> %s: the user got %d %s, want %d", c.from, c.to, got, c.to, c.out)
		}
		if c.to == WARG {
			supply += c.out
		}
		if c.from == WARG {
			supply -= c.in
		}
		if s := e.m.wargSupply(); s != supply {
			t.Fatalf("after %s -> %s: wARG supply %d, want %d", c.from, c.to, s, supply)
		}
		if s := e.bal(e.user, WARG); s != supply {
			t.Fatalf("after %s -> %s: the user holds %d wARG, the ledger says %d", c.from, c.to, s, supply)
		}
	}
	// The maker always holds at least the ARG behind the wARG out there.
	if r := e.bal(e.maker, ARG); r < supply {
		t.Fatalf("maker holds %d ARG behind %d wARG", r, supply)
	}
	info, err := e.c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.WARGSupply != supply || len(info.Pairs) != 6 {
		t.Fatalf("info: supply %d, %d pairs", info.WARGSupply, len(info.Pairs))
	}
}

func mustHash(t *testing.T, s string) consensus.Hash {
	h, err := consensus.HashFromHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// quoteFor asks for a quote with the user's coins of from and returns it
// with the Expect a wallet would check it with.
func (e *env) quoteFor(from, to string, amount consensus.Amount) (*Quote, Expect) {
	e.t.Helper()
	assets := e.assets()
	asset := ""
	if from != ARG {
		asset = assets[from].String()
	}
	coins, err := e.user.SpendableCoins(asset)
	if err != nil {
		e.t.Fatal(err)
	}
	ex := Expect{From: assets[from], To: assets[to], AmountIn: amount, Coins: map[consensus.OutPoint]consensus.TxOut{}, Owns: e.user.Owns, Tip: uint32(e.user.SyncedHeight())}
	req := &QuoteRequest{From: from, To: to, Amount: amount}
	var have consensus.Amount
	for _, k := range coins {
		if have >= amount {
			break
		}
		ex.Coins[consensus.OutPoint{TxID: mustHash(e.t, k.TxID), Index: k.Index}] = k.Out
		req.Inputs = append(req.Inputs, Outpoint{TxID: k.TxID, Index: k.Index})
		have += k.Amount
	}
	req.Receive, _ = e.user.ReceiveAddress()
	req.Change, _ = e.user.ChangeAddress()
	q, err := e.c.Quote(context.Background(), req)
	if err != nil {
		e.t.Fatal(err)
	}
	ex.MinOut = q.AmountOut
	return q, ex
}

// retx re-encodes a quote after f changes its transaction.
func retx(t *testing.T, q *Quote, f func(tx *consensus.Tx, q *Quote)) *Quote {
	t.Helper()
	tx, _, err := q.Decode()
	if err != nil {
		t.Fatal(err)
	}
	c := *q
	c.Spent = append([]SpentOut{}, q.Spent...)
	c.UserInputs = append([]int{}, q.UserInputs...)
	f(tx, &c)
	c.Tx = hex.EncodeToString(tx.Serialize())
	return &c
}

// A wallet refuses every quote that would take more than it agreed to
// give, pay it less than quoted, or let the maker hold it to a later block.
func TestCheckQuoteRefuses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	q, ex := e.quoteFor(UARG, ARG, 3*unit)
	if _, _, err := CheckQuote(q, ex); err != nil {
		t.Fatalf("the honest quote: %v", err)
	}
	makerPay, _ := wallet.DecodeAddress(consensus.RegTest.HRP, e.m.Address())
	cases := map[string]*Quote{
		"pays the wallet less": retx(t, q, func(tx *consensus.Tx, _ *Quote) { tx.Outputs[0].Amount-- }),
		"keeps the wallet's change": retx(t, q, func(tx *consensus.Tx, _ *Quote) {
			for i := range tx.Outputs {
				if tx.Outputs[i].Asset == e.uarg && e.user.Owns(tx.Outputs[i]) {
					tx.Outputs[i].Payload = makerPay.Payload
				}
			}
		}),
		"valid only later":     retx(t, q, func(tx *consensus.Tx, _ *Quote) { tx.LockHeight = 10_000 }),
		"quotes less":          retx(t, q, func(_ *consensus.Tx, c *Quote) { c.AmountOut = ex.MinOut - 1 }),
		"another amount in":    retx(t, q, func(_ *consensus.Tx, c *Quote) { c.AmountIn++ }),
		"hides a coin of ours": retx(t, q, func(_ *consensus.Tx, c *Quote) { c.UserInputs = c.UserInputs[1:] }),
		"misdescribes a coin":  retx(t, q, func(_ *consensus.Tx, c *Quote) { c.Spent[c.UserInputs[0]].Amount++ }),
		"signs a maker coin":   retx(t, q, func(tx *consensus.Tx, c *Quote) { c.UserInputs = append(c.UserInputs, len(tx.Inputs)-1) }),
		"pays in another coin": retx(t, q, func(tx *consensus.Tx, _ *Quote) {
			for i := range tx.Outputs {
				if tx.Outputs[i].Asset == consensus.NativeAsset && e.user.Owns(tx.Outputs[i]) {
					tx.Outputs[i].Asset = e.uarg
				}
			}
		}),
	}
	for name, bad := range cases {
		if bad == nil {
			continue
		}
		if _, _, err := CheckQuote(bad, ex); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A wallet will not sign a coin described differently from its record.
	bad := retx(t, q, func(_ *consensus.Tx, c *Quote) { c.Spent[c.UserInputs[0]].Amount++ })
	tx, spent, err := bad.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.user.SignInputs(tx, spent, bad.UserInputs); err == nil {
		t.Fatal("the wallet signed a coin described with another amount")
	}
}

// The maker holds each quote for its time only, once, and only with the
// wallet's own valid signatures; it never pays out ARG that backs wARG.
func TestMakerRefuses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	assets := e.assets()

	// Expired.
	p, err := Prepare(ctx, e.user, e.c, assets, ARG, UARG, 2*unit, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.add(31 * time.Second)
	if err := e.user.SignInputs(p.tx, p.spent, p.Quote.UserInputs); err != nil {
		t.Fatal(err)
	}
	req := &SwapRequest{QuoteID: p.Quote.ID}
	for _, i := range p.Quote.UserInputs {
		req.Sigs = append(req.Sigs, Sig{Index: i, Sig: hex.EncodeToString(p.tx.Witnesses[i])})
	}
	if _, err := e.c.Swap(ctx, req); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired quote: %v", err)
	}
	if _, err := e.c.Swap(ctx, req); err == nil || !strings.Contains(err.Error(), "unknown or was used") {
		t.Fatalf("used quote: %v", err)
	}

	// A signature over another transaction.
	p, err = Prepare(ctx, e.user, e.c, assets, ARG, UARG, 2*unit, 0)
	if err != nil {
		t.Fatal(err)
	}
	req = &SwapRequest{QuoteID: p.Quote.ID}
	other := *p.tx
	other.Witnesses = make([][]byte, len(p.tx.Witnesses))
	other.LockHeight = 1 // the wallet's real signature, over another transaction
	if err := e.user.SignInputs(&other, p.spent, p.Quote.UserInputs); err != nil {
		t.Fatal(err)
	}
	for _, i := range p.Quote.UserInputs {
		req.Sigs = append(req.Sigs, Sig{Index: i, Sig: hex.EncodeToString(other.Witnesses[i])})
	}
	if _, err := e.c.Swap(ctx, req); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("bad signature: %v", err)
	}

	// Coins the user does not have, coins named twice, the maker's own.
	makerCoins, _ := e.maker.SpendableCoins("")
	userCoins, _ := e.user.SpendableCoins("")
	recv, _ := e.user.ReceiveAddress()
	chg, _ := e.user.ChangeAddress()
	for name, in := range map[string][]Outpoint{
		"unknown coin": {{TxID: strings.Repeat("ab", 32), Index: 0}},
		"named twice":  {{TxID: userCoins[0].TxID, Index: userCoins[0].Index}, {TxID: userCoins[0].TxID, Index: userCoins[0].Index}},
		"maker's coin": {{TxID: makerCoins[0].TxID, Index: makerCoins[0].Index}},
	} {
		if _, err := e.c.Quote(ctx, &QuoteRequest{From: ARG, To: UARG, Amount: unit, Inputs: in, Receive: recv, Change: chg}); err == nil {
			t.Errorf("%s: quoted", name)
		}
	}
	// Paying to the maker's own address.
	if _, err := e.c.Quote(ctx, &QuoteRequest{From: ARG, To: UARG, Amount: unit, Inputs: []Outpoint{{TxID: userCoins[0].TxID, Index: userCoins[0].Index}}, Receive: e.m.Address(), Change: chg}); err == nil {
		t.Error("quoted a swap paying the maker's address")
	}

	// The reserve: wrap 150 ARG, then ask for more ARG than the maker holds
	// beyond what backs the 150 wARG.
	if p, err := Prepare(ctx, e.user, e.c, assets, ARG, WARG, 150*unit, 0); err != nil {
		t.Fatal(err)
	} else if _, err := p.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.sink)
	e.sync()
	info, err := e.c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var avail consensus.Amount
	for _, pr := range info.Pairs {
		if pr.From == UARG && pr.To == ARG {
			avail = pr.Available
		}
	}
	if avail <= 0 || avail >= e.bal(e.maker, ARG)-info.WARGSupply+1 {
		t.Fatalf("UARG -> ARG available %d with %d ARG behind %d wARG", avail, e.bal(e.maker, ARG), info.WARGSupply)
	}
	// uARG for just over the free ARG: 0.10 uARG per ARG, 1% spread.
	ask := applyRate(avail+10*unit, big.NewRat(1, 10)) * 102 / 100
	if _, err := Prepare(ctx, e.user, e.c, assets, UARG, ARG, ask, 0); err == nil || !strings.Contains(err.Error(), "cannot pay that much ARG") {
		t.Fatalf("a swap into the reserve: %v", err)
	}
	// Unwrapping is always possible: the reserve is there for it.
	if p, err := Prepare(ctx, e.user, e.c, assets, WARG, ARG, 150*unit, 0); err != nil {
		t.Fatalf("unwrap: %v", err)
	} else if _, err := p.Execute(ctx); err != nil {
		t.Fatalf("unwrap: %v", err)
	}

	// The HTTP surface: method, size.
	resp, err := http.Get(e.srv.URL + "/api/quote")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/quote: %d", resp.StatusCode)
	}
	resp, err = http.Post(e.srv.URL+"/api/quote", "application/json", strings.NewReader(`{"from":"ARG","inputs":[`+strings.Repeat(`{"txid":"00","index":0},`, 4000)+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized quote request: %d", resp.StatusCode)
	}
}

// The relay fee the maker pays is the mempool's minimum.
func TestRelayFeeMatchesMempool(t *testing.T) {
	m := &Maker{cfg: Config{FeeRate: 1}}
	tx := &consensus.Tx{Version: 2, Inputs: []consensus.TxIn{{}}, Witnesses: [][]byte{make([]byte, 64)}}
	for n := 0; n < 6; n++ {
		tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: 1, Type: consensus.OutputTypeP2PK, Payload: make([]byte, 32), Asset: consensus.Hash{1}})
		if got, want := m.relayFee(tx), mempool.MinRelayFee(mempool.DefaultConfig(), tx); got != want {
			t.Fatalf("%d asset outputs: %d, mempool wants %d", n+1, got, want)
		}
	}
}
