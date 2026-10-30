package wallet_test

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
	"argonmine/wallet"
)

// TestWalletAssets issues a test asset to wallet A, transfers part of it to
// wallet B, burns part with a memo, and checks that ARG accounting is never
// mixed with asset units (SPEC 3.5).
func TestWalletAssets(t *testing.T) {
	client := startNode(t)
	dir := t.TempDir()
	params := consensus.RegTest

	// The issuer is an independent key that mines for its own fee money.
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	issuer := &wallet.Key{Priv: priv, XOnly: schnorr.SerializePubKey(priv.PubKey())}
	issuerAddr := issuer.Address(params.HRP)
	if _, err := client.Call("generatetoaddress", 101, issuerAddr); err != nil {
		t.Fatal(err)
	}

	wa, _, err := wallet.Create(filepath.Join(dir, "a.json"), params, "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	wa.SetClient(client)
	addrA, _ := wa.NewAddress()
	wb, _, err := wallet.Create(filepath.Join(dir, "b.json"), params, "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	wb.SetClient(client)
	addrB, _ := wb.NewAddress()

	// Issue 1000 TST to A, spending the issuer's first coinbase for the fee.
	h1, err := client.BlockHash(1)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := client.Block(h1)
	if err != nil {
		t.Fatal(err)
	}
	cb := blk.Tx[0]
	cbID, _ := consensus.HashFromHex(cb.TxID)
	var xonly [32]byte
	copy(xonly[:], issuer.XOnly)
	asset := consensus.AssetID(xonly, "TST")
	destA, err := wallet.DecodeAddress(params.HRP, addrA)
	if err != nil {
		t.Fatal(err)
	}
	const issued = 1000 * consensus.UnitsPerCoin
	tx := &consensus.Tx{
		Version: consensus.TxVersionAssets,
		Inputs:  []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: cbID, Index: 0}}},
		Outputs: []consensus.TxOut{
			{Amount: issued, Type: destA.Type, Payload: destA.Payload, Asset: asset},
			issuer.Output(cb.Outputs[0].Amount - 10_000),
		},
		Issuances: []consensus.Issuance{{Asset: asset, Issuer: xonly, Symbol: "TST", Amount: issued}},
		Witnesses: make([][]byte, 2),
	}
	spent := []consensus.TxOut{issuer.Output(cb.Outputs[0].Amount)}
	if err := issuer.Sign(params.ChainTag, tx, spent, 0); err != nil {
		t.Fatal(err)
	}
	msg := consensus.SigHash(params.ChainTag, tx, spent, consensus.IssuanceSigIndex)
	sig, err := schnorr.Sign(priv, msg[:])
	if err != nil {
		t.Fatal(err)
	}
	tx.Witnesses[1] = sig.Serialize()
	if _, err := client.SendRawTransaction(hex.EncodeToString(tx.Serialize())); err != nil {
		t.Fatalf("issue: %v", err)
	}
	mine := func(n int, to string) {
		t.Helper()
		if _, err := client.Call("generatetoaddress", n, to); err != nil {
			t.Fatal(err)
		}
	}
	mine(1, issuerAddr) // height 102
	h, err := wa.Sync()
	if err != nil {
		t.Fatal(err)
	}

	bal := wa.AssetBalances()
	if len(bal) != 1 || bal[0].Asset != asset.String() || bal[0].Symbol != "TST" || bal[0].Confirmed != issued || bal[0].Locked != 0 {
		t.Fatalf("asset balances after issuance: %+v", bal)
	}
	if info, ok := wa.AssetInfoFor(asset.String()); !ok || info.Symbol != "TST" || info.Issuer != hex.EncodeToString(xonly[:]) {
		t.Fatalf("asset info: %+v %v", info, ok)
	}
	if s, im, l := wa.Balance(h); s != 0 || im != 0 || l != 0 {
		t.Fatalf("asset units leaked into the ARG balance: %d %d %d", s, im, l)
	}
	hist := wa.History()
	if len(hist) != 1 || hist[0].Assets[asset.String()] != issued || hist[0].Received != 0 {
		t.Fatalf("history after issuance: %+v", hist)
	}

	// Without ARG the fee cannot be paid.
	if _, err := wa.SendAsset(asset.String(), addrB, 400*consensus.UnitsPerCoin, 1); err == nil {
		t.Fatal("asset transfer without ARG for the fee succeeded")
	}
	mine(101, addrA) // A's first reward matures
	if _, err := wa.Sync(); err != nil {
		t.Fatal(err)
	}

	// Transfer 400 TST to B.
	txid, err := wa.SendAsset(asset.String(), addrB, 400*consensus.UnitsPerCoin, 1)
	if err != nil {
		t.Fatalf("send asset: %v", err)
	}
	bal = wa.AssetBalances()
	if len(bal) != 1 || bal[0].Confirmed != 0 || bal[0].Locked != issued {
		t.Fatalf("pending transfer should lock the whole coin: %+v", bal)
	}
	mine(1, issuerAddr)
	if _, err := wa.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := wb.Sync(); err != nil {
		t.Fatal(err)
	}
	if bal = wa.AssetBalances(); len(bal) != 1 || bal[0].Confirmed != 600*consensus.UnitsPerCoin || bal[0].Locked != 0 {
		t.Fatalf("A after transfer: %+v", bal)
	}
	if bal = wb.AssetBalances(); len(bal) != 1 || bal[0].Confirmed != 400*consensus.UnitsPerCoin || bal[0].Symbol != "TST" {
		t.Fatalf("B after transfer: %+v", bal)
	}
	var rec *wallet.HistoryRecord
	hist = wa.History()
	for i := range hist {
		if hist[i].TxID == txid {
			rec = &hist[i]
		}
	}
	if rec == nil || rec.Height == 0 || rec.Assets[asset.String()] != -400*consensus.UnitsPerCoin || rec.Fee <= 0 || rec.Net != -rec.Fee {
		t.Fatalf("transfer history record: %+v", rec)
	}

	// Burn 100 TST with a bridge-style memo.
	memo := append([]byte("BRG1"), make([]byte, 20)...)
	memo[4] = 0xAB
	burnID, err := wa.BurnAsset(asset.String(), 100*consensus.UnitsPerCoin, memo, 1)
	if err != nil {
		t.Fatalf("burn: %v", err)
	}
	mine(1, issuerAddr)
	hb, err := wa.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if bal = wa.AssetBalances(); len(bal) != 1 || bal[0].Confirmed != 500*consensus.UnitsPerCoin {
		t.Fatalf("A after burn: %+v", bal)
	}
	bh, _ := client.BlockHash(hb)
	bb, _ := client.Block(bh)
	found := false
	for _, btx := range bb.Tx {
		if btx.TxID != burnID {
			continue
		}
		for _, o := range btx.Outputs {
			if o.Type == consensus.OutputTypeNullData && o.Payload == hex.EncodeToString(memo) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("burn transaction does not carry the memo")
	}

	// Plain ARG payments keep working and never touch the asset coins.
	if _, err := wa.Send(addrB, 1*consensus.UnitsPerCoin, 1); err != nil {
		t.Fatalf("arg send: %v", err)
	}
	mine(1, issuerAddr)
	if _, err := wa.Sync(); err != nil {
		t.Fatal(err)
	}
	if bal = wa.AssetBalances(); len(bal) != 1 || bal[0].Confirmed != 500*consensus.UnitsPerCoin {
		t.Fatalf("A after arg send: %+v", bal)
	}
}
