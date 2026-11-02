package consensus

import (
	"errors"
	"testing"
)

// coinSet is one of alice's purses: a UARG coin and an ARG coin for fees.
type coinSet struct{ asset, fee OutPoint }

// mintChain is a regtest chain with a mint window at heights 103..110, 150
// ARG a block, 20 ARG per unit of a UARG asset issued to alice in two
// purses, so two requests can sit in one block.
type mintChain struct {
	*regtestChain
	miner, issuer, alice, buyer *testKey
	asset                       Hash
	a, b                        coinSet
}

func newMintChain(t *testing.T) *mintChain {
	t.Helper()
	p := *RegTest
	c := newRegtestChain(t)
	c.p = &p
	m := &mintChain{regtestChain: c, miner: newTestKey(t), issuer: newTestKey(t), alice: newTestKey(t), buyer: newTestKey(t)}
	m.asset = AssetID(xonly(m.issuer), "UARG")
	key := xonly(m.buyer)
	if err := SetMint(c.p, &MintRule{Start: 103, End: 110, PerBlock: 150 * UnitsPerCoin, Rate: 20, Asset: m.asset, Key: key[:]}); err != nil {
		t.Fatal(err)
	}
	first := c.mine(t, m.miner, nil, 0)
	for c.headersLen() <= 100 {
		c.mine(t, m.miner, nil, 0)
	}
	// Height 100. Block 101: issue 1,000 UARG to alice, in two purses.
	op := OutPoint{TxID: first.Txs[0].TxID(), Index: 0}
	coin, _ := c.view.Lookup(op)
	const fee = 1000
	half := (coin.Out.Amount - fee) / 2
	issue := &Tx{
		Version:   TxVersionAssets,
		Inputs:    []TxIn{{PrevOut: op}},
		Outputs:   []TxOut{assetOut(m.alice, m.asset, 500*UnitsPerCoin), assetOut(m.alice, m.asset, 500*UnitsPerCoin), m.alice.output(half), m.alice.output(coin.Out.Amount - fee - half)},
		Issuances: []Issuance{{Asset: m.asset, Issuer: xonly(m.issuer), Symbol: "UARG", Amount: 1000 * UnitsPerCoin}},
		Witnesses: make([][]byte, 2),
	}
	m.miner.sign(t, c.p.ChainTag, issue, []TxOut{coin.Out}, 0)
	signIssuance(t, m.issuer, c.p.ChainTag, issue, []TxOut{coin.Out})
	c.mine(t, m.miner, []*Tx{issue}, fee)
	id := issue.TxID()
	m.a = coinSet{asset: OutPoint{TxID: id, Index: 0}, fee: OutPoint{TxID: id, Index: 2}}
	m.b = coinSet{asset: OutPoint{TxID: id, Index: 1}, fee: OutPoint{TxID: id, Index: 3}}
	c.mine(t, m.miner, nil, 0) // height 102
	return m
}

// request is alice's mint request from purse s, paying paid units to the
// mint key. It returns the request, its fee, and the purse as it would be
// after the request is confirmed.
func (m *mintChain) request(t *testing.T, s coinSet, paid Amount) (*Tx, Amount, coinSet) {
	t.Helper()
	ac, ok1 := m.view.Lookup(s.asset)
	fc, ok2 := m.view.Lookup(s.fee)
	if !ok1 || !ok2 {
		t.Fatal("purse not in the set")
	}
	const fee = 2000
	tx := &Tx{
		Version: TxVersionAssets,
		Inputs:  []TxIn{{PrevOut: s.asset}, {PrevOut: s.fee}},
		Outputs: []TxOut{
			assetOut(m.buyer, m.asset, paid),
			assetOut(m.alice, m.asset, ac.Out.Amount-paid),
			m.alice.output(fc.Out.Amount - fee),
			{Type: OutputTypeNullData, Payload: []byte(MintMemo)},
		},
		Witnesses: make([][]byte, 2),
	}
	spent := []TxOut{ac.Out, fc.Out}
	m.alice.sign(t, m.p.ChainTag, tx, spent, 0)
	m.alice.sign(t, m.p.ChainTag, tx, spent, 1)
	id := tx.TxID()
	return tx, fee, coinSet{asset: OutPoint{TxID: id, Index: 1}, fee: OutPoint{TxID: id, Index: 2}}
}

// block builds a block at the next height with txs and a coinbase paying
// the miner subsidy+fees, plus extra outputs.
func (m *mintChain) block(t *testing.T, txs []*Tx, fees Amount, extra ...TxOut) *Block {
	t.Helper()
	parent, ph := m.tip()
	cb := coinbaseTx(ph+1, m.miner, Subsidy(ph+1)+fees, []byte("test"))
	cb.Outputs = append(cb.Outputs, extra...)
	return buildBlock(t, m.p, parent, ph, parent.Time+TargetSpacingU64(), append([]*Tx{cb}, txs...))
}

func TestMintBlock(t *testing.T) {
	m := newMintChain(t)
	if _, h := m.tip(); h != 102 {
		t.Fatalf("height %d", h)
	}
	// Block 103 is the first of the window. 5 UARG → 100 ARG to alice.
	req, fee, nextA := m.request(t, m.a, 5*UnitsPerCoin)
	want := Amount(100 * UnitsPerCoin)

	// The coinbase must pay alice.
	if err := m.accept(m.block(t, []*Tx{req}, fee)); !errors.Is(err, ErrBlockMintUnpaid) {
		t.Fatalf("unpaid buyer: %v", err)
	}
	// Paying her but keeping the mint too is more than the block may create.
	if err := m.accept(m.block(t, []*Tx{req}, fee+want, m.alice.output(want))); !errors.Is(err, ErrBlockBadCoinbaseValue) {
		t.Fatalf("greedy coinbase: %v", err)
	}
	// Paid short.
	if err := m.accept(m.block(t, []*Tx{req}, fee, m.alice.output(want-1))); !errors.Is(err, ErrBlockMintUnpaid) {
		t.Fatalf("short: %v", err)
	}
	// Right: the miner's subsidy and fee, alice's 100 ARG.
	good := m.block(t, []*Tx{req}, fee, m.alice.output(want))
	if err := m.accept(good); err != nil {
		t.Fatalf("good block: %v", err)
	}
	m.a = nextA
	coin, ok := m.view.Lookup(OutPoint{TxID: good.Txs[0].TxID(), Index: 1})
	if !ok || coin.Out.Amount != want || !coin.Coinbase || string(coin.Out.Payload) != string(m.alice.output(0).Payload) {
		t.Fatalf("alice's coin: %+v %v", coin, ok)
	}
	paid, ok := m.view.Lookup(OutPoint{TxID: req.TxID(), Index: 0})
	if !ok || paid.Out.Asset != m.asset || paid.Out.Amount != 5*UnitsPerCoin {
		t.Fatalf("the mint key's coin: %+v %v", paid, ok)
	}

	// Block 104: two requests of 100 ARG exceed the block's 150.
	r1, f1, _ := m.request(t, m.a, 5*UnitsPerCoin)
	r2, f2, _ := m.request(t, m.b, 5*UnitsPerCoin)
	if err := m.accept(m.block(t, []*Tx{r1, r2}, f1+f2, m.alice.output(2*want))); !errors.Is(err, ErrBlockMintCap) {
		t.Fatalf("over the block: %v", err)
	}
	// One request of more than the block's mint is invalid on its own.
	big, fb, _ := m.request(t, m.a, 8*UnitsPerCoin) // 160 ARG
	if err := m.accept(m.block(t, []*Tx{big}, fb, m.alice.output(160*UnitsPerCoin))); !errors.Is(err, ErrTxMintTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	// Two buyers' worth in one block are paid in one output to the same
	// key; a block mints exactly its cap.
	r3, f3, nextA := m.request(t, m.a, 5*UnitsPerCoin)     // 100
	r4, f4, nextB := m.request(t, m.b, 25*UnitsPerCoin/10) // 50
	if err := m.accept(m.block(t, []*Tx{r3, r4}, f3+f4, m.alice.output(150*UnitsPerCoin))); err != nil {
		t.Fatalf("two requests at the cap: %v", err)
	}
	m.a, m.b = nextA, nextB

	// After the window (height 111) a request is invalid even when paid for.
	for {
		if _, h := m.tip(); h >= 110 {
			break
		}
		m.mine(t, m.miner, nil, 0)
	}
	late, fl, _ := m.request(t, m.a, 5*UnitsPerCoin)
	if err := m.accept(m.block(t, []*Tx{late}, fl, m.alice.output(want))); !errors.Is(err, ErrTxMintClosed) {
		t.Fatalf("after the window: %v", err)
	}
	// On a network without a window the memo means nothing: the request is
	// a plain payment, and the coinbase cannot pay for it.
	m.p.Mint = nil
	if err := m.accept(m.block(t, []*Tx{late}, fl, m.alice.output(want))); !errors.Is(err, ErrBlockBadCoinbaseValue) {
		t.Fatalf("no window, coinbase paid: %v", err)
	}
	if err := m.accept(m.block(t, []*Tx{late}, fl)); err != nil {
		t.Fatalf("no window: %v", err)
	}
}
