package bridge

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/miner"
	"argonmine/wallet"
)

// guardians creates n guardian keys and one Custody per guardian (each
// holding its own private key), all agreeing on the same custody address.
func guardians(t *testing.T, n int) ([]*Custody, [][]byte) {
	t.Helper()
	keys := make([]*btcec.PrivateKey, n)
	pubs := make([][]byte, n)
	for i := range keys {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = k
		pubs[i] = k.PubKey().SerializeCompressed()
	}
	cs := make([]*Custody, n)
	for i := range keys {
		c, err := NewCustody("rarg", pubs, keys[i])
		if err != nil {
			t.Fatal(err)
		}
		cs[i] = c
	}
	for i := 1; i < n; i++ {
		if cs[i].Address() != cs[0].Address() {
			t.Fatal("guardians disagree on the custody address")
		}
	}
	return cs, pubs
}

// signWithAll runs the two MuSig2 rounds among the given guardians for every
// input. It returns an error if the set cannot complete a signature.
func signWithAll(cs []*Custody, tx *consensus.Tx, spent []consensus.TxOut) error {
	for i := range tx.Inputs {
		msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, spent, uint32(i))
		sessions := make([]*Session, len(cs))
		nonces := make([][]byte, len(cs))
		for g, c := range cs {
			s, err := c.NewSession()
			if err != nil {
				return err
			}
			sessions[g] = s
			nonces[g] = s.PublicNonce()
		}
		for g, s := range sessions {
			for h, n := range nonces {
				if h == g {
					continue
				}
				if _, err := s.RegisterNonce(n); err != nil {
					return err
				}
			}
		}
		partials := make([][]byte, len(cs))
		for g, s := range sessions {
			p, err := s.Sign(msg)
			if err != nil {
				return err
			}
			partials[g] = p
		}
		// Guardian 0 combines the others' partials; any guardian could.
		for g := 1; g < len(cs); g++ {
			if _, err := sessions[0].Combine(partials[g]); err != nil {
				return err
			}
		}
		sig, err := sessions[0].Final()
		if err != nil {
			return err
		}
		tx.Witnesses[i] = sig
	}
	return nil
}

func TestCustodySpendIsValidOnChain(t *testing.T) {
	cs, pubs := guardians(t, 3)
	custody := cs[0]

	// Observer (no key) derives the same address and cannot sign.
	obs, err := NewCustody("rarg", pubs, nil)
	if err != nil || obs.Address() != custody.Address() || obs.CanSign() {
		t.Fatal("observer custody")
	}
	if _, err := obs.NewSession(); err == nil {
		t.Fatal("observer must not sign")
	}

	// Fund custody on a regtest chain: mine 101 blocks paying it.
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	for i := 0; i < 101; i++ {
		blk, err := miner.BuildTemplate(ch, pool, custody.Output(0), []byte("b"), int64(ch.Tip().Header.Time)+600)
		if err != nil {
			t.Fatal(err)
		}
		for n := uint64(0); ; n++ {
			blk.Header.Nonce = n
			if consensus.CheckProofOfWork(&blk.Header, consensus.RegTest.PowLimit) == nil {
				break
			}
		}
		if _, err := ch.ProcessBlock(blk, -1); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := ch.NodeAtHeight(1)
	blk1, _ := ch.Block(first.Hash)
	op := consensus.OutPoint{TxID: blk1.Txs[0].TxID(), Index: 0}
	coin, ok := ch.LookupCoin(op)
	if !ok || !custody.PaysCustody(&coin.Out) {
		t.Fatal("custody coin missing")
	}

	// Release 30 ARG to a user, change back to custody.
	user, _ := btcec.NewPrivateKey()
	userOut, _ := wallet.DecodeAddress("rarg", mustAddr(t, user))
	tx := &consensus.Tx{Version: 1,
		Inputs:    []consensus.TxIn{{PrevOut: op}},
		Outputs:   []consensus.TxOut{{Amount: 30 * consensus.UnitsPerCoin, Type: userOut.Type, Payload: userOut.Payload}, custody.Output(20*consensus.UnitsPerCoin - 1000)},
		Witnesses: make([][]byte, 1),
	}
	spent := []consensus.TxOut{coin.Out}
	if err := signWithAll(cs, tx, spent); err != nil {
		t.Fatal(err)
	}
	if err := consensus.VerifyTxSignatures(consensus.RegTest.ChainTag, tx, spent); err != nil {
		t.Fatalf("aggregate signature invalid: %v", err)
	}
	if err := pool.Accept(tx); err != nil {
		t.Fatalf("mempool rejected release: %v", err)
	}

	// Two guardians alone cannot complete a signature: the sessions know
	// there are three signers and refuse to sign without the third nonce.
	bad := &consensus.Tx{Version: 1, Inputs: tx.Inputs, Outputs: tx.Outputs, Witnesses: make([][]byte, 1)}
	if err := signWithAll(cs[:2], bad, spent); err == nil {
		t.Fatal("2 of 3 guardians completed an n-of-n signature")
	}
}

func mustAddr(t *testing.T, k *btcec.PrivateKey) string {
	t.Helper()
	c, err := NewCustody("rarg", [][]byte{k.PubKey().SerializeCompressed()}, k)
	if err != nil {
		t.Fatal(err)
	}
	return c.Address()
}

func TestCustodyRejectsBadKeys(t *testing.T) {
	k, _ := btcec.NewPrivateKey()
	good := k.PubKey().SerializeCompressed()
	if _, err := NewCustody("rarg", nil, nil); err == nil {
		t.Fatal("empty set")
	}
	if _, err := NewCustody("rarg", [][]byte{good[:32]}, nil); err == nil {
		t.Fatal("short key")
	}
	if _, err := NewCustody("rarg", [][]byte{good, good}, nil); err == nil {
		t.Fatal("duplicate key")
	}
	other, _ := btcec.NewPrivateKey()
	if _, err := NewCustody("rarg", [][]byte{good}, other); err == nil {
		t.Fatal("local key not in set")
	}
	keys, err := ParseGuardianKeys([]string{"zz"})
	if err == nil || keys != nil {
		t.Fatal("bad hex accepted")
	}
}
