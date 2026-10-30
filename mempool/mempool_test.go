package mempool

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
)

type key struct {
	priv  *btcec.PrivateKey
	xonly []byte
}

func newKey(t *testing.T) *key {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &key{priv: priv, xonly: schnorr.SerializePubKey(priv.PubKey())}
}

func (k *key) out(a consensus.Amount) consensus.TxOut {
	return consensus.TxOut{Amount: a, Type: consensus.OutputTypeP2PK, Payload: append([]byte{}, k.xonly...)}
}

type harness struct {
	t    *testing.T
	ch   *chain.Chain
	pool *Pool
	k    *key
	cb   []consensus.OutPoint // coinbase outpoints by height-1
}

func newHarness(t *testing.T, blocks int) *harness {
	t.Helper()
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Close() })
	h := &harness{t: t, ch: ch, k: newKey(t)}
	h.pool = New(ch, DefaultConfig())
	for i := 0; i < blocks; i++ {
		h.mine(nil, 0)
	}
	return h
}

func (h *harness) mine(txs []*consensus.Tx, fees consensus.Amount) *consensus.Block {
	h.t.Helper()
	parent := h.ch.Tip()
	height := parent.Height + 1
	cb := &consensus.Tx{Version: 1, LockHeight: height,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs:   []consensus.TxOut{h.k.out(consensus.Subsidy(height) + fees)},
		Witnesses: [][]byte{{byte(height)}},
	}
	blk := &consensus.Block{Txs: append([]*consensus.Tx{cb}, txs...)}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		h.t.Fatal(err)
	}
	blk.Header = consensus.Header{Version: 1, PrevHash: parent.Hash, TxRoot: txRoot, WitnessRoot: wRoot,
		Time: parent.Header.Time + 600, Bits: consensus.NextBits(consensus.RegTest, parent.Header.Time, parent.Height)}
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, consensus.RegTest.PowLimit) == nil {
			break
		}
	}
	if _, err := h.ch.ProcessBlock(blk, -1); err != nil {
		h.t.Fatalf("mine: %v", err)
	}
	h.cb = append(h.cb, consensus.OutPoint{TxID: cb.TxID(), Index: 0})
	return blk
}

// spend builds a signed tx from one coin (owned by signer) paying outs.
func (h *harness) spend(signer *key, from consensus.OutPoint, fromOut consensus.TxOut, outs ...consensus.TxOut) *consensus.Tx {
	h.t.Helper()
	tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: from}}, Outputs: outs, Witnesses: make([][]byte, 1)}
	msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, []consensus.TxOut{fromOut}, 0)
	sig, err := schnorr.Sign(signer.priv, msg[:])
	if err != nil {
		h.t.Fatal(err)
	}
	tx.Witnesses[0] = sig.Serialize()
	return tx
}

func (h *harness) matureCoin(i int) (consensus.OutPoint, consensus.TxOut) {
	h.t.Helper()
	op := h.cb[i]
	coin, ok := h.ch.LookupCoin(op)
	if !ok {
		h.t.Fatalf("coin %d missing", i)
	}
	return op, coin.Out
}

const coin = consensus.UnitsPerCoin

func TestAcceptAndConfirm(t *testing.T) {
	h := newHarness(t, 101) // coinbase 1 is mature at height 101 (spendable in block 102)
	op, out := h.matureCoin(0)
	r := newKey(t)
	tx := h.spend(h.k, op, out, r.out(49*coin))
	if err := h.pool.Accept(tx); err != nil {
		t.Fatal(err)
	}
	if !h.pool.Has(tx.TxID()) || h.pool.Count() != 1 {
		t.Fatal("not in pool")
	}
	if err := h.pool.Accept(tx); !errors.Is(err, ErrAlreadyInPool) {
		t.Fatalf("dup: %v", err)
	}
	sel, fees := h.pool.Select(consensus.MaxBlockSize)
	if len(sel) != 1 || fees != 1*coin {
		t.Fatalf("select: %d txs, fees %d", len(sel), fees)
	}
	// Mining it removes it from the pool via the subscription.
	h.mine(sel, fees)
	if h.pool.Count() != 0 {
		t.Fatal("confirmed tx still in pool")
	}
}

func TestRejects(t *testing.T) {
	h := newHarness(t, 101)
	op, out := h.matureCoin(0)
	r := newKey(t)

	// Coinbase.
	cb := &consensus.Tx{Version: 1, LockHeight: 1, Inputs: []consensus.TxIn{{PrevOut: consensus.NullOutPoint}}, Outputs: []consensus.TxOut{r.out(1)}, Witnesses: [][]byte{{}}}
	if err := h.pool.Accept(cb); !errors.Is(err, ErrCoinbase) {
		t.Fatalf("coinbase: %v", err)
	}
	// Immature coinbase (height 101's coinbase).
	imOp, imOut := h.matureCoin(100)
	if err := h.pool.Accept(h.spend(h.k, imOp, imOut, r.out(49*coin))); !errors.Is(err, consensus.ErrTxImmatureCoinbase) {
		t.Fatalf("immature: %v", err)
	}
	// Zero fee.
	if err := h.pool.Accept(h.spend(h.k, op, out, r.out(50*coin))); !errors.Is(err, ErrInsufficientFee) {
		t.Fatalf("fee: %v", err)
	}
	// Dust output.
	if err := h.pool.Accept(h.spend(h.k, op, out, r.out(49*coin), r.out(100))); !errors.Is(err, ErrDust) {
		t.Fatalf("dust: %v", err)
	}
	// Bad signature (signed by wrong key).
	if err := h.pool.Accept(h.spend(r, op, out, r.out(49*coin))); !errors.Is(err, consensus.ErrTxBadSignature) {
		t.Fatalf("sig: %v", err)
	}
	// Unknown input.
	bogus := consensus.OutPoint{TxID: consensus.TaggedHash("nope", nil)}
	if err := h.pool.Accept(h.spend(h.k, bogus, out, r.out(49*coin))); !errors.Is(err, consensus.ErrTxMissingInput) {
		t.Fatalf("missing: %v", err)
	}
	// Lock height in the future.
	tx := h.spend(h.k, op, out, r.out(49*coin))
	tx.LockHeight = 1000
	// re-sign since lock_height is in the sighash
	msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, []consensus.TxOut{out}, 0)
	sig, _ := schnorr.Sign(h.k.priv, msg[:])
	tx.Witnesses[0] = sig.Serialize()
	if err := h.pool.Accept(tx); !errors.Is(err, consensus.ErrTxLockHeight) {
		t.Fatalf("lock: %v", err)
	}
	if h.pool.Count() != 0 {
		t.Fatal("pool should be empty")
	}
}

func TestReplaceByFee(t *testing.T) {
	h := newHarness(t, 101)
	op, out := h.matureCoin(0)
	r := newKey(t)
	low := h.spend(h.k, op, out, r.out(49*coin))
	if err := h.pool.Accept(low); err != nil {
		t.Fatal(err)
	}
	// Same fee: rejected as conflict.
	same := h.spend(h.k, op, out, r.out(49*coin), consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: []byte("x")})
	if err := h.pool.Accept(same); !errors.Is(err, ErrConflict) {
		t.Fatalf("same fee: %v", err)
	}
	// Higher fee: replaces.
	high := h.spend(h.k, op, out, r.out(47*coin))
	if err := h.pool.Accept(high); err != nil {
		t.Fatalf("rbf: %v", err)
	}
	if h.pool.Has(low.TxID()) || !h.pool.Has(high.TxID()) || h.pool.Count() != 1 {
		t.Fatal("replacement bookkeeping")
	}
}

func TestChainedSpendsAndOrdering(t *testing.T) {
	h := newHarness(t, 101)
	op, out := h.matureCoin(0)
	a, b := newKey(t), newKey(t)
	// parent pays 1 coin fee; child pays 5 coins (higher rate) but must come after parent.
	parent := h.spend(h.k, op, out, a.out(49*coin))
	child := h.spend(a, consensus.OutPoint{TxID: parent.TxID()}, parent.Outputs[0], b.out(44*coin))
	// Child first is rejected (parent unknown to pool and chain).
	if err := h.pool.Accept(child); !errors.Is(err, consensus.ErrTxMissingInput) {
		t.Fatalf("orphan child: %v", err)
	}
	if err := h.pool.Accept(parent); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.Accept(child); err != nil {
		t.Fatal(err)
	}
	sel, fees := h.pool.Select(consensus.MaxBlockSize)
	if len(sel) != 2 || sel[0].TxID() != parent.TxID() || sel[1].TxID() != child.TxID() || fees != 6*coin {
		t.Fatalf("ordering: n=%d fees=%d", len(sel), fees)
	}
	// Size budget that fits only the parent excludes the child too.
	sel, _ = h.pool.Select(parent.SerializedSize())
	if len(sel) != 1 || sel[0].TxID() != parent.TxID() {
		t.Fatal("size budget")
	}
	// Replacing the parent evicts the child.
	parent2 := h.spend(h.k, op, out, a.out(40*coin))
	if err := h.pool.Accept(parent2); err != nil {
		t.Fatal(err)
	}
	if h.pool.Has(child.TxID()) || h.pool.Count() != 1 {
		t.Fatal("descendant not evicted with replaced parent")
	}
	// The block must validate with pool-selected txs.
	sel, fees = h.pool.Select(consensus.MaxBlockSize)
	h.mine(sel, fees)
	if h.pool.Count() != 0 {
		t.Fatal("pool not cleared")
	}
}

func TestBlockDoubleSpendEvictsConflict(t *testing.T) {
	h := newHarness(t, 101)
	op, out := h.matureCoin(0)
	r := newKey(t)
	inPool := h.spend(h.k, op, out, r.out(49*coin))
	if err := h.pool.Accept(inPool); err != nil {
		t.Fatal(err)
	}
	// A block confirms a different spend of the same coin.
	other := h.spend(h.k, op, out, r.out(48*coin))
	h.mine([]*consensus.Tx{other}, 2*coin)
	if h.pool.Count() != 0 {
		t.Fatal("conflicting pool tx survived block")
	}
}

func TestPoolFullEviction(t *testing.T) {
	h := newHarness(t, 103)
	cfg := DefaultConfig()
	_, out0 := h.matureCoin(0)
	size := h.spend(h.k, h.cb[0], out0, newKey(t).out(49*coin)).SerializedSize()
	cfg.MaxBytes = size + 1 // room for one
	h.pool = New(h.ch, cfg)
	r := newKey(t)
	var txs []*consensus.Tx
	for i, fee := range []consensus.Amount{1 * coin, 2 * coin, 3 * coin} {
		op, out := h.matureCoin(i)
		txs = append(txs, h.spend(h.k, op, out, r.out(50*coin-fee)))
	}
	// Each better-paying tx evicts the cheaper one before it.
	for i, tx := range txs {
		if err := h.pool.Accept(tx); err != nil {
			t.Fatalf("tx %d: %v", i, err)
		}
		if h.pool.Count() != 1 || !h.pool.Has(tx.TxID()) || h.pool.Bytes() > cfg.MaxBytes {
			t.Fatalf("after tx %d: count=%d bytes=%d", i, h.pool.Count(), h.pool.Bytes())
		}
	}
	// A new tx with a fee lower than everything present is refused.
	op, out := h.matureCoin(0)
	cheap := h.spend(h.k, op, out, r.out(50*coin-consensus.Amount(size)-1))
	if err := h.pool.Accept(cheap); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("pool full: %v", err)
	}
}
