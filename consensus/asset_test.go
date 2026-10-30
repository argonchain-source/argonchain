package consensus

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

func xonly(k *testKey) [32]byte {
	var a [32]byte
	copy(a[:], k.xonly)
	return a
}

// signIssuance fills in the issuance witness (index len(inputs)).
func signIssuance(t *testing.T, issuer *testKey, chainTag Hash, tx *Tx, spent []TxOut) {
	t.Helper()
	msg := NewSigHashCache(chainTag, tx, spent).IssuanceHash()
	sig, err := schnorr.Sign(issuer.priv, msg[:])
	if err != nil {
		t.Fatal(err)
	}
	tx.Witnesses[len(tx.Inputs)] = sig.Serialize()
}

func assetOut(k *testKey, asset Hash, amount Amount) TxOut {
	return TxOut{Amount: amount, Type: OutputTypeP2PK, Payload: append([]byte{}, k.xonly...), Asset: asset}
}

func TestAssetIDAndSymbolRules(t *testing.T) {
	issuer := newTestKey(t)
	a := AssetID(xonly(issuer), "UARG")
	if a == NativeAsset || a != AssetID(xonly(issuer), "UARG") {
		t.Fatal("asset id must be deterministic and non-zero")
	}
	if a == AssetID(xonly(issuer), "WARG") || a == AssetID(xonly(newTestKey(t)), "UARG") {
		t.Fatal("asset id must bind both issuer and symbol")
	}
	for _, ok := range []string{"A", "UARG", "ABCDEFGH", "A1B2"} {
		if !ValidSymbol(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "uarg", "ABCDEFGHI", "U-A", "U A", "é"} {
		if ValidSymbol(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
	is := Issuance{Asset: a, Issuer: xonly(issuer), Symbol: "UARG", Amount: 1}
	if err := CheckIssuance(&is); err != nil {
		t.Fatal(err)
	}
	wrong := is
	wrong.Symbol = "WARG"
	if err := CheckIssuance(&wrong); !errors.Is(err, ErrTxIssuanceID) {
		t.Fatalf("symbol mismatch: %v", err)
	}
	zero := is
	zero.Amount = 0
	if err := CheckIssuance(&zero); !errors.Is(err, ErrTxAssetAmount) {
		t.Fatalf("zero issuance: %v", err)
	}
	big := is
	big.Amount = MaxAssetAmount + 1
	if err := CheckIssuance(&big); !errors.Is(err, ErrTxAssetAmount) {
		t.Fatalf("huge issuance: %v", err)
	}
	if _, err := AddAssetAmounts(MaxAssetAmount, 1); !errors.Is(err, ErrTxAssetOverflow) {
		t.Fatal("asset sum must not exceed the bound")
	}
}

func TestAssetEncoding(t *testing.T) {
	issuer, holder := newTestKey(t), newTestKey(t)
	asset := AssetID(xonly(issuer), "UARG")
	tx := &Tx{
		Version:   TxVersionAssets,
		Inputs:    []TxIn{{PrevOut: OutPoint{TxID: Hash{1}, Index: 0}}},
		Outputs:   []TxOut{assetOut(holder, asset, 1000), holder.output(5)},
		Issuances: []Issuance{{Asset: asset, Issuer: xonly(issuer), Symbol: "UARG", Amount: 1000}},
		Witnesses: [][]byte{make([]byte, 64), make([]byte, 64)},
	}
	raw := tx.Serialize()
	back, err := DecodeTx(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Serialize(), raw) {
		t.Fatal("version-2 round trip changed bytes")
	}
	if back.Outputs[0].Asset != asset || back.Outputs[1].Asset != NativeAsset || len(back.Issuances) != 1 || back.Issuances[0].Symbol != "UARG" {
		t.Fatal("asset fields lost in round trip")
	}
	if back.TxID() != tx.TxID() {
		t.Fatal("txid changed in round trip")
	}
	// The txid commits to the asset id and to the issuance.
	other := *tx
	other.Outputs = []TxOut{assetOut(holder, AssetID(xonly(issuer), "WARG"), 1000), holder.output(5)}
	if other.TxID() == tx.TxID() {
		t.Fatal("txid must commit to output asset ids")
	}
	noIss := *tx
	noIss.Issuances = nil
	noIss.Witnesses = tx.Witnesses[:1]
	if noIss.TxID() == tx.TxID() {
		t.Fatal("txid must commit to the issuance")
	}
	// Version-1 encoding is untouched: no asset bytes, no issuance section.
	v1 := &Tx{Version: TxVersion, Inputs: tx.Inputs, Outputs: []TxOut{holder.output(5)}, Witnesses: [][]byte{make([]byte, 64)}}
	v2same := &Tx{Version: TxVersionAssets, Inputs: tx.Inputs, Outputs: []TxOut{holder.output(5)}, Witnesses: [][]byte{make([]byte, 64)}}
	if len(v2same.Serialize())-len(v1.Serialize()) != HashSize+1 {
		t.Fatalf("version 2 should add exactly one asset id per output and one issuance count byte")
	}
	d1, err := DecodeTx(v1.Serialize())
	if err != nil || d1.Outputs[0].Asset != NativeAsset || len(d1.Issuances) != 0 {
		t.Fatalf("version-1 decode: %v", err)
	}
	// Two issuances are rejected by the decoder (count cap) and by the checks.
	two := *tx
	two.Issuances = []Issuance{tx.Issuances[0], tx.Issuances[0]}
	two.Witnesses = [][]byte{make([]byte, 64), make([]byte, 64), make([]byte, 64)}
	if _, err := DecodeTx(two.Serialize()); err == nil {
		t.Fatal("decoder accepted two issuances")
	}
	if err := CheckTxStateless(&two); !errors.Is(err, ErrTxIssuanceCount) {
		t.Fatalf("two issuances: %v", err)
	}
}

func TestAssetStatelessRules(t *testing.T) {
	issuer, holder := newTestKey(t), newTestKey(t)
	asset := AssetID(xonly(issuer), "UARG")
	in := []TxIn{{PrevOut: OutPoint{TxID: Hash{1}, Index: 0}}}
	// Assets need version 2.
	v1 := &Tx{Version: TxVersion, Inputs: in, Outputs: []TxOut{assetOut(holder, asset, 1)}, Witnesses: [][]byte{nil}}
	if err := CheckTxStateless(v1); !errors.Is(err, ErrTxAssetVersion) {
		t.Fatalf("asset output in v1: %v", err)
	}
	v1i := &Tx{Version: TxVersion, Inputs: in, Outputs: []TxOut{holder.output(1)}, Issuances: []Issuance{{Asset: asset, Issuer: xonly(issuer), Symbol: "UARG", Amount: 1}}, Witnesses: [][]byte{nil, nil}}
	if err := CheckTxStateless(v1i); !errors.Is(err, ErrTxAssetVersion) {
		t.Fatalf("issuance in v1: %v", err)
	}
	// Witness count counts the issuance.
	v2 := &Tx{Version: TxVersionAssets, Inputs: in, Outputs: []TxOut{assetOut(holder, asset, 1)}, Issuances: []Issuance{{Asset: asset, Issuer: xonly(issuer), Symbol: "UARG", Amount: 1}}, Witnesses: [][]byte{nil}}
	if err := CheckTxStateless(v2); !errors.Is(err, ErrTxWitnessCount) {
		t.Fatalf("missing issuance witness: %v", err)
	}
	v2.Witnesses = [][]byte{nil, nil}
	if err := CheckTxStateless(v2); err != nil {
		t.Fatalf("valid v2: %v", err)
	}
	// Asset amounts: zero is invalid, NULLDATA cannot carry an asset.
	zero := &Tx{Version: TxVersionAssets, Inputs: in, Outputs: []TxOut{assetOut(holder, asset, 0)}, Witnesses: [][]byte{nil}}
	if err := CheckTxStateless(zero); !errors.Is(err, ErrTxAssetAmount) {
		t.Fatalf("zero asset output: %v", err)
	}
	nd := &Tx{Version: TxVersionAssets, Inputs: in, Outputs: []TxOut{{Type: OutputTypeNullData, Payload: []byte("x"), Asset: asset}}, Witnesses: [][]byte{nil}}
	if err := CheckTxStateless(nd); !errors.Is(err, ErrTxAssetNullData) {
		t.Fatalf("asset NULLDATA: %v", err)
	}
	// Per-asset sums are bounded.
	over := &Tx{Version: TxVersionAssets, Inputs: in, Outputs: []TxOut{assetOut(holder, asset, MaxAssetAmount), assetOut(holder, asset, 1)}, Witnesses: [][]byte{nil}}
	if err := CheckTxStateless(over); !errors.Is(err, ErrTxAssetOverflow) {
		t.Fatalf("asset overflow: %v", err)
	}
	// A coinbase can neither issue nor pay in assets.
	cb := &Tx{Version: TxVersionAssets, LockHeight: 7, Inputs: []TxIn{{PrevOut: NullOutPoint}}, Outputs: []TxOut{assetOut(holder, asset, 1)}, Witnesses: [][]byte{nil}}
	if err := CheckCoinbase(cb, 7); !errors.Is(err, ErrTxIssuanceCoinbase) {
		t.Fatalf("coinbase asset output: %v", err)
	}
	cbi := &Tx{Version: TxVersionAssets, LockHeight: 7, Inputs: []TxIn{{PrevOut: NullOutPoint}}, Outputs: []TxOut{holder.output(1)}, Issuances: []Issuance{{Asset: asset, Issuer: xonly(issuer), Symbol: "UARG", Amount: 1}}, Witnesses: [][]byte{nil, nil}}
	if err := CheckCoinbase(cbi, 7); !errors.Is(err, ErrTxIssuanceCoinbase) {
		t.Fatalf("coinbase issuance: %v", err)
	}
}

// TestAssetIssueTransferBurn runs the whole life of an asset on a regtest
// chain: issue, transfer with change, over-spend rejected, burn accepted,
// version-1 spend of an asset coin rejected, wrong issuer rejected.
func TestAssetIssueTransferBurn(t *testing.T) {
	c := newRegtestChain(t)
	miner := newTestKey(t)
	first := c.mine(t, miner, nil, 0)
	for c.headersLen() <= 100 {
		c.mine(t, miner, nil, 0)
	}
	issuer, alice, bob := newTestKey(t), newTestKey(t), newTestKey(t)
	asset := AssetID(xonly(issuer), "UARG")

	// Issue 1,000 units to alice, paying the fee from a mined coin.
	op := OutPoint{TxID: first.Txs[0].TxID(), Index: 0}
	coin, _ := c.view.Lookup(op)
	const fee = 1000
	issue := &Tx{
		Version:   TxVersionAssets,
		Inputs:    []TxIn{{PrevOut: op}},
		Outputs:   []TxOut{assetOut(alice, asset, 1000), miner.output(coin.Out.Amount - fee)},
		Issuances: []Issuance{{Asset: asset, Issuer: xonly(issuer), Symbol: "UARG", Amount: 1000}},
		Witnesses: make([][]byte, 2),
	}
	spent := []TxOut{coin.Out}
	miner.sign(t, c.p.ChainTag, issue, spent, 0)
	// Wrong issuer key: signature by someone else must fail.
	signIssuance(t, alice, c.p.ChainTag, issue, spent)
	if err := VerifyTxSignatures(c.p.ChainTag, issue, spent); !errors.Is(err, ErrTxIssuanceSignature) {
		t.Fatalf("wrong issuer accepted: %v", err)
	}
	signIssuance(t, issuer, c.p.ChainTag, issue, spent)
	if err := CheckTxStateless(issue); err != nil {
		t.Fatal(err)
	}
	if gotFee, _, err := CheckTxInputs(issue, c.view, uint32(c.headersLen())); err != nil || gotFee != fee {
		t.Fatalf("issue inputs: fee %d err %v", gotFee, err)
	}
	if err := VerifyTxSignatures(c.p.ChainTag, issue, spent); err != nil {
		t.Fatal(err)
	}
	// Issuing more than the issuance says is unbacked.
	greedy := *issue
	greedy.Outputs = []TxOut{assetOut(alice, asset, 1001), miner.output(coin.Out.Amount - fee)}
	if _, _, err := CheckTxInputs(&greedy, c.view, uint32(c.headersLen())); !errors.Is(err, ErrTxAssetUnbacked) {
		t.Fatalf("over-issue: %v", err)
	}
	c.mine(t, miner, []*Tx{issue}, fee)
	assetOp := OutPoint{TxID: issue.TxID(), Index: 0}
	ac, ok := c.view.Lookup(assetOp)
	if !ok || ac.Out.Asset != asset || ac.Out.Amount != 1000 {
		t.Fatalf("asset coin missing: %+v %v", ac, ok)
	}

	// Transfer 600 to bob with 400 change; the fee comes from an ARG coin.
	feeOp := OutPoint{TxID: issue.TxID(), Index: 1}
	feeCoin, _ := c.view.Lookup(feeOp)
	xfer := &Tx{
		Version:   TxVersionAssets,
		Inputs:    []TxIn{{PrevOut: assetOp}, {PrevOut: feeOp}},
		Outputs:   []TxOut{assetOut(bob, asset, 600), assetOut(alice, asset, 400), miner.output(feeCoin.Out.Amount - fee)},
		Witnesses: make([][]byte, 2),
	}
	spent2 := []TxOut{ac.Out, feeCoin.Out}
	alice.sign(t, c.p.ChainTag, xfer, spent2, 0)
	miner.sign(t, c.p.ChainTag, xfer, spent2, 1)
	if err := CheckTxStateless(xfer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CheckTxInputs(xfer, c.view, uint32(c.headersLen())); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTxSignatures(c.p.ChainTag, xfer, spent2); err != nil {
		t.Fatal(err)
	}
	// Lying about the spent asset changes the sighash.
	lied := []TxOut{{Amount: ac.Out.Amount, Type: ac.Out.Type, Payload: ac.Out.Payload, Asset: AssetID(xonly(issuer), "WARG")}, feeCoin.Out}
	if err := VerifyTxSignatures(c.p.ChainTag, xfer, lied); !errors.Is(err, ErrTxBadSignature) {
		t.Fatalf("asset lie: %v", err)
	}
	// Creating units out of nothing is rejected.
	more := *xfer
	more.Outputs = []TxOut{assetOut(bob, asset, 601), assetOut(alice, asset, 400), miner.output(feeCoin.Out.Amount - fee)}
	if _, _, err := CheckTxInputs(&more, c.view, uint32(c.headersLen())); !errors.Is(err, ErrTxAssetUnbacked) {
		t.Fatalf("unbacked transfer: %v", err)
	}
	// Paying an ARG output from asset units is rejected (assets are not money).
	swap := *xfer
	swap.Outputs = []TxOut{assetOut(bob, asset, 600), miner.output(feeCoin.Out.Amount + 1)}
	if _, _, err := CheckTxInputs(&swap, c.view, uint32(c.headersLen())); !errors.Is(err, ErrTxFeeNegative) {
		t.Fatalf("asset used as ARG: %v", err)
	}
	// A version-1 transaction may not touch asset coins.
	old := &Tx{Version: TxVersion, Inputs: []TxIn{{PrevOut: assetOp}, {PrevOut: feeOp}}, Outputs: []TxOut{miner.output(feeCoin.Out.Amount - fee)}, Witnesses: make([][]byte, 2)}
	if _, _, err := CheckTxInputs(old, c.view, uint32(c.headersLen())); !errors.Is(err, ErrTxAssetVersion) {
		t.Fatalf("v1 spend of asset coin: %v", err)
	}
	c.mine(t, miner, []*Tx{xfer}, fee)
	if bc, ok := c.view.Lookup(OutPoint{TxID: xfer.TxID(), Index: 0}); !ok || bc.Out.Asset != asset || bc.Out.Amount != 600 {
		t.Fatal("bob did not receive the asset")
	}

	// Burning: bob spends 600 and keeps only 100; 500 units are destroyed.
	bobOp := OutPoint{TxID: xfer.TxID(), Index: 0}
	bobCoin, _ := c.view.Lookup(bobOp)
	feeOp2 := OutPoint{TxID: xfer.TxID(), Index: 2}
	feeCoin2, _ := c.view.Lookup(feeOp2)
	burn := &Tx{
		Version:   TxVersionAssets,
		Inputs:    []TxIn{{PrevOut: bobOp}, {PrevOut: feeOp2}},
		Outputs:   []TxOut{assetOut(bob, asset, 100), {Type: OutputTypeNullData, Payload: []byte("BRG1 redeem")}, miner.output(feeCoin2.Out.Amount - fee)},
		Witnesses: make([][]byte, 2),
	}
	spent3 := []TxOut{bobCoin.Out, feeCoin2.Out}
	bob.sign(t, c.p.ChainTag, burn, spent3, 0)
	miner.sign(t, c.p.ChainTag, burn, spent3, 1)
	if err := CheckTxStateless(burn); err != nil {
		t.Fatal(err)
	}
	if gotFee, _, err := CheckTxInputs(burn, c.view, uint32(c.headersLen())); err != nil || gotFee != fee {
		t.Fatalf("burn: fee %d err %v", gotFee, err)
	}
	c.mine(t, miner, []*Tx{burn}, fee)
	if _, ok := c.view.Lookup(bobOp); ok {
		t.Fatal("burned input still unspent")
	}
	if nc, ok := c.view.Lookup(OutPoint{TxID: burn.TxID(), Index: 0}); !ok || nc.Out.Amount != 100 {
		t.Fatal("remaining units missing")
	}
	if _, ok := c.view.Lookup(OutPoint{TxID: burn.TxID(), Index: 1}); ok {
		t.Fatal("NULLDATA entered the UTXO set")
	}
}

func TestAssetSighashVersionSeparation(t *testing.T) {
	// Version-1 messages are unchanged by the asset fields; version-2
	// messages differ from version-1 ones for otherwise identical data.
	k := newTestKey(t)
	in := []TxIn{{PrevOut: OutPoint{TxID: Hash{9}, Index: 1}}}
	spent := []TxOut{k.output(10)}
	v1 := &Tx{Version: TxVersion, Inputs: in, Outputs: []TxOut{k.output(9)}, Witnesses: [][]byte{nil}}
	v2 := &Tx{Version: TxVersionAssets, Inputs: in, Outputs: []TxOut{k.output(9)}, Witnesses: [][]byte{nil}}
	if SigHash(RegTest.ChainTag, v1, spent, 0) == SigHash(RegTest.ChainTag, v2, spent, 0) {
		t.Fatal("version must be part of the sighash")
	}
	c := NewSigHashCache(RegTest.ChainTag, v2, spent)
	if c.IssuanceHash() == c.Hash(0) {
		t.Fatal("issuance sighash must differ from input sighashes")
	}
}
