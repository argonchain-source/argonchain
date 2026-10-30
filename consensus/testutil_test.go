package consensus

import (
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// testKey is a keypair for building spendable outputs in tests.
type testKey struct {
	priv  *btcec.PrivateKey
	xonly []byte // 32-byte x-only pubkey
}

func newTestKey(t *testing.T) *testKey {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &testKey{priv: priv, xonly: schnorr.SerializePubKey(priv.PubKey())}
}

func (k *testKey) output(amount Amount) TxOut {
	return TxOut{Amount: amount, Type: OutputTypeP2PK, Payload: append([]byte{}, k.xonly...)}
}

// sign fills in witness i of tx with a valid signature over sighash(tx, i).
func (k *testKey) sign(t *testing.T, chainTag Hash, tx *Tx, spent []TxOut, i int) {
	t.Helper()
	msg := SigHash(chainTag, tx, spent, uint32(i))
	sig, err := schnorr.Sign(k.priv, msg[:])
	if err != nil {
		t.Fatal(err)
	}
	tx.Witnesses[i] = sig.Serialize()
}

// mineHeader finds a nonce satisfying the header's own Bits. Only used on
// regtest where the target is ~2^255 (one or two attempts).
func mineHeader(t *testing.T, p *Params, h *Header) {
	t.Helper()
	for n := uint64(0); n < 1<<20; n++ {
		h.Nonce = n
		if CheckProofOfWork(h, p.PowLimit) == nil {
			return
		}
	}
	t.Fatal("could not mine header")
}

// coinbaseTx builds a coinbase paying amount to key at height.
func coinbaseTx(height uint32, key *testKey, amount Amount, tag []byte) *Tx {
	return &Tx{
		Version:    TxVersion,
		LockHeight: height,
		Inputs:     []TxIn{{PrevOut: NullOutPoint}},
		Outputs:    []TxOut{key.output(amount)},
		Witnesses:  [][]byte{tag},
	}
}

// buildBlock assembles a block on parent with the given txs, computing roots
// and mining the nonce (regtest).
func buildBlock(t *testing.T, p *Params, parent *Header, parentHeight uint32, time uint64, txs []*Tx) *Block {
	t.Helper()
	blk := &Block{Txs: txs}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		t.Fatal(err)
	}
	blk.Header = Header{
		Version:     HeaderVersion,
		PrevHash:    parent.Hash(),
		TxRoot:      txRoot,
		WitnessRoot: wRoot,
		Time:        time,
		Bits:        NextBits(p, parent.Time, parentHeight),
	}
	mineHeader(t, p, &blk.Header)
	return blk
}

func bigFromHex(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		t.Fatalf("bad hex %s", s)
	}
	return v
}
