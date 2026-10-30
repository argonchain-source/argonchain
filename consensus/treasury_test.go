package consensus

import (
	"errors"
	"testing"
)

func TestTreasuryAmount(t *testing.T) {
	for _, c := range []struct {
		h    uint32
		want Amount
	}{
		{0, 0}, {299, 0}, // before the treasury height
		{300, UnitsPerCoin / 2}, {209_999, UnitsPerCoin / 2}, // 1% of 50 ARG
		{210_000, UnitsPerCoin / 4}, // 1% of 25 ARG after the first halving
	} {
		if got := TreasuryAmount(MainNet, c.h); got != c.want {
			t.Errorf("mainnet height %d: %d, want %d", c.h, got, c.want)
		}
	}
	for _, p := range []*Params{TestNet, RegTest} {
		if TreasuryAmount(p, 1000) != 0 {
			t.Errorf("%s has a treasury", p.Name)
		}
	}
	if len(MainNet.TreasuryKey) != P2PKPayloadLen || MainNet.TreasuryHeight != 300 {
		t.Fatalf("mainnet treasury %x from %d", MainNet.TreasuryKey, MainNet.TreasuryHeight)
	}
}

// A regtest chain with a treasury from height 3: blocks before it need no
// treasury output, blocks from it must pay at least 1% of the subsidy to the
// treasury key in ARG, and may still not pay more than subsidy + fees.
func TestTreasuryRule(t *testing.T) {
	p := *RegTest
	treasury := newTestKey(t)
	p.TreasuryHeight, p.TreasuryKey = 3, append([]byte(nil), treasury.xonly...)
	c := newRegtestChain(t)
	c.p = &p
	miner := newTestKey(t)
	c.mine(t, miner, nil, 0) // 1
	c.mine(t, miner, nil, 0) // 2: no treasury yet

	share := TreasuryAmount(&p, 3)
	if share != Subsidy(3)/100 || share == 0 {
		t.Fatalf("share %d", share)
	}
	try := func(outs ...TxOut) error {
		parent, ph := c.tip()
		cb := &Tx{Version: TxVersion, LockHeight: ph + 1, Inputs: []TxIn{{PrevOut: NullOutPoint}}, Outputs: outs, Witnesses: [][]byte{nil}}
		return c.accept(buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb}))
	}
	sub := Subsidy(3)
	if err := try(miner.output(sub)); !errors.Is(err, ErrBlockTreasury) {
		t.Fatalf("no treasury output: %v", err)
	}
	if err := try(miner.output(sub-share+1), treasury.output(share-1)); !errors.Is(err, ErrBlockTreasury) {
		t.Fatalf("one unit short: %v", err)
	}
	fake := treasury.output(share)
	fake.Asset = Hash{1}
	if err := try(miner.output(sub-share), fake); err == nil { // turned away (a coinbase pays in ARG only)
		t.Fatal("a coinbase paying the treasury in an asset was accepted")
	}
	if err := try(miner.output(sub), treasury.output(share)); !errors.Is(err, ErrBlockBadCoinbaseValue) {
		t.Fatalf("treasury on top of the full subsidy: %v", err)
	}
	if err := try(miner.output(sub-share), treasury.output(share)); err != nil {
		t.Fatalf("exact share: %v", err)
	}
	// Split over two outputs, and more than the share, is fine.
	if err := try(treasury.output(share/2), miner.output(sub-2*share), treasury.output(share+share/2)); err != nil {
		t.Fatalf("split share: %v", err)
	}
	if o, ok := TreasuryOutput(&p, 5); !ok || o.Amount != share || o.Type != OutputTypeP2PK {
		t.Fatalf("treasury output %+v %v", o, ok)
	}
}
