package mempool

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
)

// A mint request enters the pool while the window is open, is selected up
// to the block's mint with its buyer reported, and leaves once the window
// closes.
func TestMintRequests(t *testing.T) {
	p := *consensus.RegTest
	ch, err := chain.Open(&p, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Close() })
	h := &harness{t: t, ch: ch, k: newKey(t)}
	h.pool = New(ch, DefaultConfig())
	issuer, alice, mintKey := newKey(t), newKey(t), newKey(t)
	var issuerX [32]byte
	copy(issuerX[:], issuer.xonly)
	asset := consensus.AssetID(issuerX, "UARG")
	// The window: blocks 104..105, 150 ARG a block, 20 ARG a unit.
	if err := consensus.SetMint(&p, &consensus.MintRule{Start: 104, End: 105, PerBlock: 150 * coin, Rate: 20, Asset: asset, Key: mintKey.xonly}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 102; i++ {
		h.mine(nil, 0)
	}
	// Block 103: issue 1,000 UARG to alice and give her ARG for fees.
	op, out := h.matureCoin(0)
	issue := &consensus.Tx{Version: consensus.TxVersionAssets, Inputs: []consensus.TxIn{{PrevOut: op}},
		Outputs: []consensus.TxOut{
			{Amount: 500 * coin, Type: consensus.OutputTypeP2PK, Payload: alice.xonly, Asset: asset},
			{Amount: 500 * coin, Type: consensus.OutputTypeP2PK, Payload: alice.xonly, Asset: asset},
			alice.out((out.Amount - 1000) / 2), alice.out(out.Amount - 1000 - (out.Amount-1000)/2)},
		Issuances: []consensus.Issuance{{Asset: asset, Issuer: issuerX, Symbol: "UARG", Amount: 1000 * coin}},
		Witnesses: make([][]byte, 2)}
	spent := []consensus.TxOut{out}
	sign := func(k *key, tx *consensus.Tx, spent []consensus.TxOut, i int) {
		msg := consensus.SigHash(p.ChainTag, tx, spent, uint32(i))
		sig, err := schnorr.Sign(k.priv, msg[:])
		if err != nil {
			t.Fatal(err)
		}
		tx.Witnesses[i] = sig.Serialize()
	}
	sign(h.k, issue, spent, 0)
	msg := consensus.NewSigHashCache(p.ChainTag, issue, spent).IssuanceHash()
	sig, _ := schnorr.Sign(issuer.priv, msg[:])
	issue.Witnesses[1] = sig.Serialize()
	h.mine([]*consensus.Tx{issue}, 1000)
	if h.ch.Tip().Height != 103 {
		t.Fatalf("height %d", h.ch.Tip().Height)
	}

	// Two purses, so two requests can be in flight at once.
	type purse struct{ asset, fee consensus.OutPoint }
	a := purse{consensus.OutPoint{TxID: issue.TxID(), Index: 0}, consensus.OutPoint{TxID: issue.TxID(), Index: 2}}
	b := purse{consensus.OutPoint{TxID: issue.TxID(), Index: 1}, consensus.OutPoint{TxID: issue.TxID(), Index: 3}}
	request := func(s purse, paid consensus.Amount) *consensus.Tx {
		ac, ok1 := h.ch.LookupCoin(s.asset)
		fc, ok2 := h.ch.LookupCoin(s.fee)
		if !ok1 || !ok2 {
			t.Fatal("purse not in the chain")
		}
		tx := &consensus.Tx{Version: consensus.TxVersionAssets, Inputs: []consensus.TxIn{{PrevOut: s.asset}, {PrevOut: s.fee}},
			Outputs: []consensus.TxOut{
				{Amount: paid, Type: consensus.OutputTypeP2PK, Payload: mintKey.xonly, Asset: asset},
				{Amount: ac.Out.Amount - paid, Type: consensus.OutputTypeP2PK, Payload: alice.xonly, Asset: asset},
				alice.out(fc.Out.Amount - 5000),
				{Type: consensus.OutputTypeNullData, Payload: []byte(consensus.MintMemo)},
			}, Witnesses: make([][]byte, 2)}
		sp := []consensus.TxOut{ac.Out, fc.Out}
		sign(alice, tx, sp, 0)
		sign(alice, tx, sp, 1)
		return tx
	}

	// Next block is 104: open. 5 UARG → 100 ARG for alice.
	r1 := request(a, 5*coin)
	if err := h.pool.Accept(r1); err != nil {
		t.Fatalf("accept: %v", err)
	}
	txs, _, mints := h.pool.Select(1 << 20)
	if len(txs) != 1 || mints.Total != 100*coin || len(mints.Payouts) != 1 || string(mints.Payouts[0].Key) != string(alice.xonly) {
		t.Fatalf("select: %d txs, mints %+v", len(txs), mints)
	}
	// Fee-free (policy): 1 uARG or more with no ARG at all is relayed; less
	// than 1 uARG without a fee is not.
	freeReq := func(s purse, paid consensus.Amount) *consensus.Tx {
		ac, _ := h.ch.LookupCoin(s.asset)
		tx := &consensus.Tx{Version: consensus.TxVersionAssets, Inputs: []consensus.TxIn{{PrevOut: s.asset}},
			Outputs: []consensus.TxOut{
				{Amount: paid, Type: consensus.OutputTypeP2PK, Payload: mintKey.xonly, Asset: asset},
				{Amount: ac.Out.Amount - paid, Type: consensus.OutputTypeP2PK, Payload: alice.xonly, Asset: asset},
				{Type: consensus.OutputTypeNullData, Payload: []byte(consensus.MintMemo)},
			}, Witnesses: make([][]byte, 1)}
		sign(alice, tx, []consensus.TxOut{ac.Out}, 0)
		return tx
	}
	if err := h.pool.Accept(freeReq(b, coin/2)); !errors.Is(err, ErrInsufficientFee) {
		t.Fatalf("free request under 1 uARG: %v", err)
	}
	free := freeReq(b, coin)
	if err := h.pool.Accept(free); err != nil {
		t.Fatalf("free request of 1 uARG: %v", err)
	}
	if e, ok := h.pool.txs[free.TxID()]; !ok || e.Fee != 0 || e.Mint != 20*coin {
		t.Fatalf("free request entry: %+v %v", e, ok)
	}
	h.pool.remove(free.TxID())

	// A second request would take the block over 150: it waits.
	r2 := request(b, 5*coin)
	if err := h.pool.Accept(r2); err != nil {
		t.Fatalf("accept second: %v", err)
	}
	txs, _, mints = h.pool.Select(1 << 20)
	if len(txs) != 1 || txs[0].TxID() != r1.TxID() || mints.Total != 100*coin {
		t.Fatalf("select with two: %d txs, mints %+v", len(txs), mints)
	}
	// Too large on its own: refused.
	a = purse{consensus.OutPoint{TxID: r1.TxID(), Index: 1}, consensus.OutPoint{TxID: r1.TxID(), Index: 2}}
	h.mineWith([]*consensus.Tx{r1}, 5000, alice.out(100*coin))
	big := request(a, 8*coin)
	if err := h.pool.Accept(big); !errors.Is(err, consensus.ErrTxMintTooLarge) {
		t.Fatalf("too large: %v", err)
	}

	// Block 104 took r1, paying alice; r2 is selected for 105.
	txs, _, mints = h.pool.Select(1 << 20)
	if len(txs) != 1 || txs[0].TxID() != r2.TxID() || mints.Total != 100*coin {
		t.Fatalf("after 104: %d txs, mints %+v", len(txs), mints)
	}
	// Block 105 (the last) without it; at 106 the window is closed: r2 is
	// not selected, and a full revalidation drops it.
	h.mine(nil, 0)
	txs, _, mints = h.pool.Select(1 << 20)
	if len(txs) != 0 || mints.Total != 0 {
		t.Fatalf("after the window: %d txs, mints %+v", len(txs), mints)
	}
	h.pool.Revalidate()
	if h.pool.Has(r2.TxID()) {
		t.Fatal("a closed-window request stayed in the pool")
	}
	if err := h.pool.Accept(r2); !errors.Is(err, consensus.ErrTxMintClosed) {
		t.Fatalf("accept after the window: %v", err)
	}
}

// mineWith is mine with extra coinbase outputs (a block's mint).
func (h *harness) mineWith(txs []*consensus.Tx, fees consensus.Amount, extra ...consensus.TxOut) *consensus.Block {
	h.t.Helper()
	parent := h.ch.Tip()
	height := parent.Height + 1
	cb := &consensus.Tx{Version: 1, LockHeight: height,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs:   append([]consensus.TxOut{h.k.out(consensus.Subsidy(height) + fees)}, extra...),
		Witnesses: [][]byte{{byte(height)}},
	}
	blk := &consensus.Block{Txs: append([]*consensus.Tx{cb}, txs...)}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		h.t.Fatal(err)
	}
	p := h.ch.Params()
	blk.Header = consensus.Header{Version: 1, PrevHash: parent.Hash, TxRoot: txRoot, WitnessRoot: wRoot,
		Time: parent.Header.Time + 600, Bits: consensus.NextBits(p, parent.Header.Time, parent.Height)}
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, p.PowLimit) == nil {
			break
		}
	}
	if _, err := h.ch.ProcessBlock(blk, -1); err != nil {
		h.t.Fatalf("mine: %v", err)
	}
	h.cb = append(h.cb, consensus.OutPoint{TxID: cb.TxID(), Index: 0})
	return blk
}
