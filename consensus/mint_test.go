package consensus

import (
	"errors"
	"testing"
)

func testMintParams(t *testing.T) (*Params, *MintRule) {
	t.Helper()
	p := *RegTest
	p.Mint = nil
	r := &MintRule{Start: 10, End: 20, PerBlock: 500 * UnitsPerCoin, Rate: 20, Asset: AssetID([32]byte{9}, "UARG"), Key: make([]byte, P2PKPayloadLen)}
	r.Key[0] = 0xAA
	if err := SetMint(&p, r); err != nil {
		t.Fatal(err)
	}
	return &p, p.Mint
}

func mintRequest(r *MintRule, paid Amount, memo string) *Tx {
	return &Tx{Version: TxVersionAssets, Outputs: []TxOut{
		{Amount: paid, Type: OutputTypeP2PK, Payload: r.Key, Asset: r.Asset},
		{Type: OutputTypeNullData, Payload: []byte(memo)},
	}}
}

func TestMintRule(t *testing.T) {
	if MainNet.Mint == nil || MainNet.Mint.Start != 2_925 || MainNet.Mint.End != 4_924 || MainNet.Mint.PerBlock != 500*UnitsPerCoin || MainNet.Mint.Rate != 20 {
		t.Fatalf("mainnet mint window: %+v", MainNet.Mint)
	}
	if string(MainNet.Mint.Key) != string(MainNet.TreasuryKey) {
		t.Fatal("mainnet pays the mint to a key other than the treasury's")
	}
	if err := SetMint(MainNet, nil); err == nil {
		t.Fatal("mainnet's window could be changed")
	}
	p, r := testMintParams(t)
	for _, h := range []uint32{9, 21} {
		if MintOpen(p, h) {
			t.Errorf("open at %d", h)
		}
	}
	for _, h := range []uint32{10, 15, 20} {
		if !MintOpen(p, h) {
			t.Errorf("closed at %d", h)
		}
	}
	if MintOpen(RegTest, 15) {
		t.Error("regtest has a window by default")
	}
	// The flag form round-trips.
	s := "10:20:500:20:" + r.Asset.String() + ":aa00000000000000000000000000000000000000000000000000000000000000"
	got, err := ParseMintRule(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Start != 10 || got.End != 20 || got.PerBlock != r.PerBlock || got.Rate != 20 || got.Asset != r.Asset || string(got.Key) != string(r.Key) {
		t.Fatalf("parsed %+v", got)
	}
	for _, bad := range []string{"", "1:2:3", "20:10:500:20:" + r.Asset.String() + ":aa", "10:20:0:20:" + r.Asset.String() + ":" + s[len(s)-64:], "10:20:500:20:" + NativeAsset.String() + ":" + s[len(s)-64:]} {
		if _, err := ParseMintRule(bad); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestMintAmount(t *testing.T) {
	p, r := testMintParams(t)
	buyer := TxOut{Amount: 1, Type: OutputTypeP2PK, Payload: make([]byte, P2PKPayloadLen)}
	buyer.Payload[0] = 7
	spent := []TxOut{buyer}

	// Not a request: nothing.
	plain := mintRequest(r, 5*UnitsPerCoin, "hello")
	if k, a, err := MintAmount(p, plain, spent, 15); k != nil || a != 0 || err != nil {
		t.Fatalf("plain: %x %d %v", k, a, err)
	}
	// 5 uARG at 20 → 100 ARG to the buyer's key.
	k, a, err := MintAmount(p, mintRequest(r, 5*UnitsPerCoin, MintMemo), spent, 15)
	if err != nil || a != 100*UnitsPerCoin || string(k) != string(buyer.Payload) {
		t.Fatalf("request: %x %d %v", k, a, err)
	}
	// Outside the window, unpaid, too large, no key, no window.
	if _, _, err := MintAmount(p, mintRequest(r, 5*UnitsPerCoin, MintMemo), spent, 21); !errors.Is(err, ErrTxMintClosed) {
		t.Fatalf("closed: %v", err)
	}
	unpaid := mintRequest(r, 5*UnitsPerCoin, MintMemo)
	unpaid.Outputs[0].Payload = buyer.Payload // paid to someone else
	if _, _, err := MintAmount(p, unpaid, spent, 15); !errors.Is(err, ErrTxMintUnpaid) {
		t.Fatalf("unpaid: %v", err)
	}
	wrongAsset := mintRequest(r, 5*UnitsPerCoin, MintMemo)
	wrongAsset.Outputs[0].Asset = AssetID([32]byte{1}, "UARG")
	if _, _, err := MintAmount(p, wrongAsset, spent, 15); !errors.Is(err, ErrTxMintUnpaid) {
		t.Fatalf("wrong asset: %v", err)
	}
	if _, _, err := MintAmount(p, mintRequest(r, 25*UnitsPerCoin+1, MintMemo), spent, 15); !errors.Is(err, ErrTxMintTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if _, a, err := MintAmount(p, mintRequest(r, 25*UnitsPerCoin, MintMemo), spent, 15); err != nil || a != r.PerBlock {
		t.Fatalf("exactly the block: %d %v", a, err)
	}
	if _, _, err := MintAmount(p, mintRequest(r, 5*UnitsPerCoin, MintMemo), nil, 15); !errors.Is(err, ErrTxMintBuyer) {
		t.Fatalf("no key: %v", err)
	}
	if k, a, err := MintAmount(RegTest, mintRequest(r, 5*UnitsPerCoin, MintMemo), spent, 15); k != nil || a != 0 || err != nil {
		t.Fatalf("no window: %x %d %v", k, a, err)
	}
	if k, a, err := MintAmount(p, mintRequest(r, 5*UnitsPerCoin, MintMemo), spent, 9); k != nil || a != 0 || err != nil {
		t.Fatalf("before the window: %x %d %v", k, a, err)
	}
}

func TestMintsAndCoinbase(t *testing.T) {
	_, r := testMintParams(t)
	k1, k2 := make([]byte, P2PKPayloadLen), make([]byte, P2PKPayloadLen)
	k1[0], k2[0] = 1, 2
	var m Mints
	if err := m.Add(r, k1, 100*UnitsPerCoin); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(r, k2, 150*UnitsPerCoin); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(r, k1, 250*UnitsPerCoin); err != nil {
		t.Fatal(err)
	}
	if m.Total != 500*UnitsPerCoin || len(m.Payouts) != 2 || m.Payouts[0].Amount != 350*UnitsPerCoin || m.Payouts[1].Amount != 150*UnitsPerCoin {
		t.Fatalf("mints %+v", m)
	}
	if err := m.Add(r, k2, 1); !errors.Is(err, ErrBlockMintCap) {
		t.Fatalf("over the block: %v", err)
	}
	outs := m.Outputs()
	if len(outs) != 2 || string(outs[0].Payload) != string(k1) || outs[1].Amount != 150*UnitsPerCoin {
		t.Fatalf("outputs %+v", outs)
	}
	cb := &Tx{Outputs: append([]TxOut{{Amount: 50 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: make([]byte, P2PKPayloadLen)}}, outs...)}
	if err := CheckMintPaid(cb, &m); err != nil {
		t.Fatal(err)
	}
	// Paid in two outputs to the same key: fine. Paid short: not.
	split := &Tx{Outputs: []TxOut{{Amount: 300 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: k1}, {Amount: 50 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: k1}, {Amount: 150 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: k2}}}
	if err := CheckMintPaid(split, &m); err != nil {
		t.Fatal(err)
	}
	short := &Tx{Outputs: []TxOut{{Amount: 350 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: k1}, {Amount: 149 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: k2}}}
	if err := CheckMintPaid(short, &m); !errors.Is(err, ErrBlockMintUnpaid) {
		t.Fatalf("short: %v", err)
	}
	// An asset output to the key does not count.
	asset := &Tx{Outputs: []TxOut{{Amount: 350 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: k1}, {Amount: 150 * UnitsPerCoin, Type: OutputTypeP2PK, Payload: k2, Asset: r.Asset}}}
	if err := CheckMintPaid(asset, &m); !errors.Is(err, ErrBlockMintUnpaid) {
		t.Fatalf("asset: %v", err)
	}
}
