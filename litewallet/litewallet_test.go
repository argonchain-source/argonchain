package litewallet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/rpc"
	"argonmine/swap/quote"
	"argonmine/swapaddr"
	"argonmine/wallet"
)

const unit = consensus.Amount(consensus.UnitsPerCoin)

type node struct {
	t    *testing.T
	c    *wallet.Client
	pool *mempool.Pool
	sink string
}

func newNode(t *testing.T) *node {
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
	n := &node{t: t, c: wallet.NewClient("http://"+srv.Addr(), "t"), pool: mp}
	n.sink, _ = randomAddress()
	return n
}

func randomAddress() (string, *btcec.PrivateKey) {
	k, _ := btcec.NewPrivateKey()
	a, _ := wallet.EncodeAddress(consensus.RegTest.HRP, consensus.OutputTypeP2PK, schnorr.SerializePubKey(k.PubKey()))
	return a, k
}

func (n *node) mine(k int, addr string) {
	n.t.Helper()
	if _, err := n.c.Call("generatetoaddress", k, addr); err != nil {
		n.t.Fatal(err)
	}
}

func (n *node) send(hexTx string) string {
	n.t.Helper()
	id, err := n.c.SendRawTransaction(hexTx)
	if err != nil {
		n.t.Fatalf("the node refused the payment: %v", err)
	}
	return id
}

// coins lists the unspent outputs paying addr on the main chain, as the
// explorer does, and the tip height.
func (n *node) coins(addr string) ([]Coin, uint32) {
	n.t.Helper()
	info, err := n.c.ChainInfo()
	if err != nil {
		n.t.Fatal(err)
	}
	unspent := map[string]Coin{}
	var order []string
	for h := uint32(0); h <= info.Blocks; h++ {
		bh, err := n.c.BlockHash(h)
		if err != nil {
			n.t.Fatal(err)
		}
		blk, err := n.c.Block(bh)
		if err != nil {
			n.t.Fatal(err)
		}
		for ti, tx := range blk.Tx {
			for _, in := range tx.Inputs {
				delete(unspent, fmt.Sprintf("%s:%d", in.TxID, in.Index))
			}
			for _, o := range tx.Outputs {
				if o.Address != addr {
					continue
				}
				k := fmt.Sprintf("%s:%d", tx.TxID, o.N)
				unspent[k] = Coin{TxID: tx.TxID, Index: uint32(o.N), Amount: o.Amount, Asset: o.Asset, Height: blk.Height, Coinbase: ti == 0}
				order = append(order, k)
			}
		}
	}
	var out []Coin
	for _, k := range order {
		if c, ok := unspent[k]; ok {
			out = append(out, c)
		}
	}
	return out, info.Blocks
}

// issue gives each recipient amount of a new asset named symbol, from the
// mature coinbase issuer was paid at height h.
func (n *node) issue(issuer *btcec.PrivateKey, h uint32, symbol string, to []string, amount consensus.Amount) consensus.Hash {
	n.t.Helper()
	ix := schnorr.SerializePubKey(issuer.PubKey())
	var i32 [32]byte
	copy(i32[:], ix)
	id := consensus.AssetID(i32, symbol)
	bh, _ := n.c.BlockHash(h)
	blk, _ := n.c.Block(bh)
	cb := blk.Tx[0]
	cbID, _ := consensus.HashFromHex(cb.TxID)
	tx := &consensus.Tx{Version: consensus.TxVersionAssets, Inputs: []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: cbID, Index: 0}}}}
	for _, a := range to {
		d, err := wallet.DecodeAddress(consensus.RegTest.HRP, a)
		if err != nil {
			n.t.Fatal(err)
		}
		tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: amount, Type: consensus.OutputTypeP2PK, Payload: d.Payload, Asset: id})
	}
	tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: cb.Outputs[0].Amount - 50_000, Type: consensus.OutputTypeP2PK, Payload: ix})
	tx.Issuances = []consensus.Issuance{{Asset: id, Issuer: i32, Symbol: symbol, Amount: amount * consensus.Amount(len(to))}}
	tx.Witnesses = make([][]byte, 2)
	spent := []consensus.TxOut{{Amount: cb.Outputs[0].Amount, Type: consensus.OutputTypeP2PK, Payload: ix}}
	for w, idx := range []uint32{0, consensus.IssuanceSigIndex} {
		hh := consensus.SigHash(consensus.RegTest.ChainTag, tx, spent, idx)
		sig, err := schnorr.Sign(issuer, hh[:])
		if err != nil {
			n.t.Fatal(err)
		}
		tx.Witnesses[w] = sig.Serialize()
	}
	n.send(hex.EncodeToString(tx.Serialize()))
	return id
}

func newAccount(t *testing.T) *Account {
	t.Helper()
	m, err := wallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	a, err := FromMnemonic(consensus.RegTest, m)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// spend applies a payment to a coin list: its inputs leave, its change arrives.
func spend(coins []Coin, spent, arrived []Coin) []Coin {
	gone := map[string]bool{}
	for _, c := range spent {
		gone[c.key()] = true
	}
	var out []Coin
	for _, c := range coins {
		if !gone[c.key()] {
			out = append(out, c)
		}
	}
	return append(out, arrived...)
}

func decode(t *testing.T, h string) *consensus.Tx {
	t.Helper()
	raw, _ := hex.DecodeString(h)
	tx, err := consensus.DecodeTx(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// Every kind of payment the browser wallet makes is accepted by a node:
// ARG, an asset, the whole balance of each, and payments that spend the
// change of the one before while it is still unconfirmed.
func TestPaymentsAreAcceptedByTheNode(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	acct := newAccount(t)
	issuerAddr, issuer := randomAddress()
	n.mine(2, acct.Address())
	n.mine(1, issuerAddr)
	n.mine(100, n.sink)
	asset := n.issue(issuer, 3, "UARG", []string{acct.Address()}, 50*unit)
	n.mine(1, n.sink)
	coins, tip := n.coins(acct.Address())
	if got := Balance(coins, "", tip); got != 100*unit {
		t.Fatalf("ARG balance %d, want 100 ARG", got)
	}
	if got := Balance(coins, asset.String(), tip); got != 50*unit {
		t.Fatalf("asset balance %d, want 50", got)
	}
	friend, _ := randomAddress()
	cfg := mempool.DefaultConfig()

	// ARG with change.
	p, err := acct.Pay(coins, tip, PayRequest{To: friend, Amount: 15 * unit / 10})
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, p.Hex)
	if tx.Version != consensus.TxVersion || p.Amount != 15*unit/10 || len(p.Change) != 1 || p.Change[0].Asset != "" {
		t.Fatalf("ARG payment: version %d, amount %d, change %+v", tx.Version, p.Amount, p.Change)
	}
	if min := mempool.MinRelayFee(cfg, tx); p.Fee < min || p.Fee != DefaultFeeRate*consensus.Amount(tx.SerializedSize()) {
		t.Fatalf("fee %d for %d bytes (relay minimum %d)", p.Fee, tx.SerializedSize(), min)
	}
	if id := n.send(p.Hex); id != p.TxID {
		t.Fatalf("node reports %s, payment says %s", id, p.TxID)
	}
	coins = spend(coins, p.Spent, p.Change)

	// An asset, paying the fee in ARG; the change of the first payment is
	// still unconfirmed and may be picked.
	p, err = acct.Pay(coins, tip, PayRequest{Asset: asset.String(), To: friend, Amount: 10 * unit})
	if err != nil {
		t.Fatal(err)
	}
	tx = decode(t, p.Hex)
	if tx.Version != consensus.TxVersionAssets || p.Amount != 10*unit || p.Fee < mempool.MinRelayFee(cfg, tx) {
		t.Fatalf("asset payment: version %d, amount %d, fee %d", tx.Version, p.Amount, p.Fee)
	}
	var assetChange consensus.Amount
	for _, c := range p.Change {
		if c.Asset == asset.String() {
			assetChange += c.Amount
		}
	}
	if assetChange != 40*unit {
		t.Fatalf("asset change %d, want 40", assetChange)
	}
	n.send(p.Hex)
	coins = spend(coins, p.Spent, p.Change)

	// Everything of the asset, then everything of ARG.
	p, err = acct.Pay(coins, tip, PayRequest{Asset: asset.String(), To: friend, Max: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Amount != 40*unit {
		t.Fatalf("whole asset balance: paid %d, want 40", p.Amount)
	}
	n.send(p.Hex)
	coins = spend(coins, p.Spent, p.Change)
	p, err = acct.Pay(coins, tip, PayRequest{To: friend, Max: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Change) != 0 || p.Amount+p.Fee != Balance(coins, "", tip) {
		t.Fatalf("whole ARG balance: paid %d + fee %d of %d, change %+v", p.Amount, p.Fee, Balance(coins, "", tip), p.Change)
	}
	n.send(p.Hex)
	n.mine(1, n.sink)
	coins, tip = n.coins(acct.Address())
	if len(coins) != 0 {
		t.Fatalf("after paying everything the account still has %+v", coins)
	}
	got, _ := n.coins(friend)
	var argGot, assetGot consensus.Amount
	for _, c := range got {
		if c.Asset == "" {
			argGot += c.Amount
		} else {
			assetGot += c.Amount
		}
	}
	if assetGot != 50*unit || argGot < 99*unit || argGot >= 100*unit {
		t.Fatalf("the friend got %d ARG units and %d asset units", argGot, assetGot)
	}
}

// Payments the wallet must refuse: more than it has, an asset without ARG
// for the fee, a coin to a swap address other than uARG, and mining
// rewards before they mature.
func TestPaymentsRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	acct, poor := newAccount(t), newAccount(t)
	issuerAddr, issuer := randomAddress()
	n.mine(1, acct.Address())
	n.mine(1, issuerAddr)
	n.mine(100, n.sink)
	uarg := n.issue(issuer, 2, "UARG", []string{acct.Address(), poor.Address()}, 20*unit)
	n.mine(1, n.sink)
	n.mine(1, acct.Address()) // immature
	coins, tip := n.coins(acct.Address())
	friend, _ := randomAddress()

	if got := Balance(coins, "", tip); got != 50*unit {
		t.Fatalf("spendable ARG %d: the immature reward must not count", got)
	}
	if _, err := acct.Pay(coins, tip, PayRequest{To: friend, Amount: 60 * unit}); !errors.Is(err, ErrFunds) {
		t.Fatalf("paying more than the mature balance: %v", err)
	}
	if _, err := acct.Pay(coins, tip, PayRequest{To: friend, Amount: 100}); err == nil {
		t.Fatal("a payment below the dust limit was built")
	}
	if _, err := acct.Pay(coins, tip, PayRequest{Asset: uarg.String(), To: friend, Amount: 21 * unit}); !errors.Is(err, ErrFunds) {
		t.Fatalf("paying more of the asset than held: %v", err)
	}
	pc, ptip := n.coins(poor.Address())
	if _, err := poor.Pay(pc, ptip, PayRequest{Asset: uarg.String(), To: friend, Amount: unit}); !errors.Is(err, ErrFeeFunds) {
		t.Fatalf("an asset payment without ARG for the fee: %v", err)
	}
	if _, err := acct.Pay(coins, tip, PayRequest{To: "arg1notanaddress", Amount: unit}); err == nil {
		t.Fatal("a payment to a malformed address was built")
	}

	var to [20]byte
	to[19] = 7
	swapAddr, err := swapaddr.Address(consensus.RegTest.HRP, swapaddr.Dest{ChainID: 11155111, Symbol: "USDC", To: to})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acct.Pay(coins, tip, PayRequest{To: swapAddr, Amount: unit, UARG: uarg.String()}); !errors.Is(err, ErrSwapAddr) {
		t.Fatalf("ARG to a swap address: %v", err)
	}
	if _, err := acct.Pay(coins, tip, PayRequest{Asset: uarg.String(), To: swapAddr, Amount: unit}); !errors.Is(err, ErrSwapAddr) {
		t.Fatalf("an asset not named as uARG to a swap address: %v", err)
	}
	// A payload that starts like a swap address but is not canonical.
	d, _ := wallet.DecodeAddress(consensus.RegTest.HRP, swapAddr)
	bad := append([]byte{}, d.Payload...)
	bad[2] = 0x11
	badAddr, _ := wallet.EncodeAddress(consensus.RegTest.HRP, consensus.OutputTypeP2PK, bad)
	if _, err := acct.Pay(coins, tip, PayRequest{Asset: uarg.String(), To: badAddr, Amount: unit, UARG: uarg.String()}); !errors.Is(err, ErrSwapAddr) {
		t.Fatalf("uARG to a look-alike swap address: %v", err)
	}
	p, err := acct.Pay(coins, tip, PayRequest{Asset: uarg.String(), To: swapAddr, Amount: unit, UARG: strings.ToUpper(uarg.String())})
	if err != nil {
		t.Fatalf("uARG to a swap address: %v", err)
	}
	n.send(p.Hex)
}

func TestRelayPolicyMatchesMempool(t *testing.T) {
	cfg := mempool.DefaultConfig()
	if assetOutputFee != cfg.AssetOutputFee || freeAssetOutputs != mempool.FreeAssetOutputs || DefaultFeeRate < cfg.MinFeeRate {
		t.Fatalf("the browser wallet's relay policy (%d per asset output beyond %d, rate %d) differs from the nodes' (%d beyond %d, rate %d)",
			assetOutputFee, freeAssetOutputs, DefaultFeeRate, cfg.AssetOutputFee, mempool.FreeAssetOutputs, cfg.MinFeeRate)
	}
	if wallet.DustLimit != cfg.DustLimit {
		t.Fatalf("dust limit %d, nodes %d", wallet.DustLimit, cfg.DustLimit)
	}
}

func TestSealOpen(t *testing.T) {
	t.Parallel()
	m, _ := wallet.NewMnemonic()
	s, acct, err := Seal(consensus.TestNet, "  "+strings.ToUpper(m)+" ", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if s.Address != acct.Address() || !strings.HasPrefix(s.Address, consensus.TestNet.HRP+"1") || strings.Contains(s.Data, hex.EncodeToString([]byte(m))[:20]) {
		t.Fatalf("sealed %+v", s)
	}
	got, back, err := Open(consensus.TestNet, s, "correct horse")
	if err != nil || got != m || back.Address() != acct.Address() {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, _, err := Open(consensus.TestNet, s, "correct horsE"); !errors.Is(err, ErrPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := Open(consensus.RegTest, s, "correct horse"); err == nil {
		t.Fatal("opened on another network")
	}
	other := *s
	other.Address = newAccount(t).Address()
	if _, _, err := Open(consensus.TestNet, &other, "correct horse"); err == nil {
		t.Fatal("a sealed phrase passed off as another address opened")
	}
	huge := *s
	huge.MemoryKiB = 4 << 20
	if _, _, err := Open(consensus.TestNet, &huge, "correct horse"); err == nil {
		t.Fatal("opened with a 4 GiB key derivation")
	}
	if _, _, err := Seal(consensus.TestNet, m, "short"); err == nil {
		t.Fatal("sealed under a 5-character password")
	}
	if _, _, err := Seal(consensus.TestNet, "not a phrase", "long enough"); err == nil {
		t.Fatal("sealed a phrase that is not one")
	}
}

// The review shows a watch account's preview; the payment signed after the
// password is entered must be exactly that one.
func TestWatchPreviewIsThePayment(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	acct := newAccount(t)
	issuerAddr, issuer := randomAddress()
	n.mine(3, acct.Address())
	n.mine(1, issuerAddr)
	n.mine(100, n.sink)
	asset := n.issue(issuer, 4, "UARG", []string{acct.Address()}, 9*unit)
	n.mine(1, n.sink)
	coins, tip := n.coins(acct.Address())
	watch, err := Watch(consensus.RegTest, strings.ToUpper(acct.Address()))
	if err != nil || watch.CanSign() || watch.Address() != acct.Address() {
		t.Fatalf("watch account: %v", err)
	}
	friend, _ := randomAddress()
	for _, r := range []PayRequest{
		{To: friend, Amount: 70 * unit},
		{To: friend, Max: true},
		{Asset: asset.String(), To: friend, Amount: 4 * unit},
		{Asset: asset.String(), To: friend, Max: true},
	} {
		pre, err := watch.Pay(coins, tip, r)
		if err != nil {
			t.Fatal(err)
		}
		p, err := acct.Pay(coins, tip, r)
		if err != nil {
			t.Fatal(err)
		}
		if pre.Hex != "" || pre.TxID != p.TxID || pre.Fee != p.Fee || pre.Amount != p.Amount || len(pre.Spent) != len(p.Spent) || len(pre.Change) != len(p.Change) {
			t.Fatalf("preview %+v differs from payment %+v", pre, p)
		}
	}
	if _, err := watch.SignQuote(&quote.Quote{}, QuoteCheck{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("a watch account signed a quote: %v", err)
	}
}

// The swap service takes only confirmed coins: unconfirmed change and swap
// proceeds are never offered, and a swap that needs them says to wait.
func TestQuoteRequestOffersConfirmedCoinsOnly(t *testing.T) {
	acct := newAccount(t)
	assets := map[string]consensus.Hash{"ARG": consensus.NativeAsset, "UARG": {1}, "WARG": {2}}
	w := consensus.Hash{2}.String()
	coins := []Coin{
		{TxID: strings.Repeat("aa", 32), Index: 0, Amount: 2 * unit, Asset: w, Height: 50},
		{TxID: strings.Repeat("bb", 32), Index: 1, Amount: 3 * unit, Asset: w}, // unconfirmed
	}
	req, offered, err := acct.QuoteRequest(coins, 60, assets, "WARG", "UARG", 2*unit)
	if err != nil || len(offered) != 1 || offered[0].Height != 50 || len(req.Inputs) != 1 {
		t.Fatalf("offered %+v: %v", offered, err)
	}
	if _, _, err := acct.QuoteRequest(coins, 60, assets, "WARG", "UARG", 4*unit); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("a swap needing unconfirmed coins: %v", err)
	}
	if _, _, err := acct.QuoteRequest(coins, 60, assets, "WARG", "UARG", 6*unit); !errors.Is(err, ErrFunds) {
		t.Fatalf("a swap of more than held: %v", err)
	}
}

// A withdrawal: uARG to a swap address with the bridge's memo in a NULLDATA
// output, accepted by the node; a memo too long for one is refused.
func TestPayWithMemo(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	acct := newAccount(t)
	issuerAddr, issuer := randomAddress()
	n.mine(2, acct.Address())
	n.mine(1, issuerAddr)
	n.mine(100, n.sink)
	uarg := n.issue(issuer, 3, "UARG", []string{acct.Address()}, 20*unit)
	n.mine(1, n.sink)
	coins, tip := n.coins(acct.Address())
	var to [20]byte
	to[19] = 0x42
	swapAddr, err := swapaddr.Address(consensus.RegTest.HRP, swapaddr.Dest{ChainID: 11155111, Symbol: "USDC", To: to})
	if err != nil {
		t.Fatal(err)
	}
	req := PayRequest{Asset: uarg.String(), To: swapAddr, Amount: 5 * unit, UARG: uarg.String(), Memo: " K7M2-X9RT "}
	watch, err := Watch(consensus.RegTest, acct.Address())
	if err != nil {
		t.Fatal(err)
	}
	pre, err := watch.Pay(coins, tip, req)
	if err != nil {
		t.Fatal(err)
	}
	p, err := acct.Pay(coins, tip, req)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Fee != p.Fee || pre.TxID != p.TxID {
		t.Fatalf("preview %s fee %d, payment %s fee %d", pre.TxID, pre.Fee, p.TxID, p.Fee)
	}
	tx := decode(t, p.Hex)
	memos := 0
	for _, o := range tx.Outputs {
		if o.Type == consensus.OutputTypeNullData {
			memos++
			if string(o.Payload) != "K7M2-X9RT" || o.Amount != 0 || o.Asset != consensus.NativeAsset {
				t.Fatalf("memo output %+v", o)
			}
		}
	}
	if memos != 1 || p.Fee < mempool.MinRelayFee(mempool.DefaultConfig(), tx) {
		t.Fatalf("%d memo outputs, fee %d", memos, p.Fee)
	}
	if id := n.send(p.Hex); id != p.TxID {
		t.Fatalf("node reports %s, payment says %s", id, p.TxID)
	}
	req.Memo = strings.Repeat("x", consensus.MaxNullDataLen+1)
	if _, err := acct.Pay(coins, tip, req); err == nil || !strings.Contains(err.Error(), "memo") {
		t.Fatalf("an 81-byte memo: %v", err)
	}
}
