package chain

import (
	"errors"
	"testing"

	"argonmine/consensus"
)

// TestReviewD_MutatedBodyPoisonsHonestHeader: a peer that relays an honest,
// PoW-valid header with a body that does not match its merkle roots gets the
// honest block marked invalid, permanently. The real body is then refused as
// a duplicate and every honest descendant as an invalid ancestor.
func TestReviewD_MutatedBodyPoisonsHonestHeader(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	honest := makeBlock(t, c.Params(), c.Tip(), k, nil, 0, 7)

	// Attacker: same header, coinbase miner tag changed (no PoW needed).
	cb := *honest.Txs[0]
	cb.Witnesses = [][]byte{{0x99}}
	mutated := &consensus.Block{Header: honest.Header, Txs: []*consensus.Tx{&cb}}
	_, err := c.ProcessBlock(mutated, -1)
	t.Logf("mutated body: %v", err)

	n, ok := c.Node(honest.Header.Hash())
	if ok {
		t.Logf("honest header status after mutated body: %d (3 = invalid)", n.Status)
	}

	_, err = c.ProcessBlock(honest, -1)
	t.Logf("genuine body afterwards: %v", err)
	if errors.Is(err, ErrDuplicateBlock) && ok && n.Status == StatusInvalid {
		// Honest child is refused too.
		hn := &Node{Hash: honest.Header.Hash(), Header: honest.Header, Height: 1}
		child := makeBlock(t, c.Params(), hn, k, nil, 0, 8)
		_, cerr := c.ProcessBlock(child, -1)
		t.Logf("honest child: %v", cerr)
		t.Errorf("VULNERABLE: honest block %s permanently invalid after a mutated body; tip stays at %d",
			honest.Header.Hash(), c.Tip().Height)
	}
}

// TestReviewD_MutatedBodyOnHeaderOnlyNode: same, via attachBodyLocked (a
// side-chain header whose branch later becomes the heaviest).
func TestReviewD_MutatedBodyOnHeaderOnlyNode(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	g := c.Tip()
	a1 := mustProcess(t, c, makeBlock(t, c.Params(), g, k, nil, 0, 1))
	_ = a1
	b1 := makeBlock(t, c.Params(), g, k, nil, 0, 2) // side, header only
	if _, err := c.ProcessBlock(b1, -1); err != nil {
		t.Fatal(err)
	}
	bn, _ := c.Node(b1.Header.Hash())
	b2 := makeBlock(t, c.Params(), bn, k, nil, 0, 3)
	_, err := c.ProcessBlock(b2, -1) // heavier: asks for b1's body
	if !errors.Is(err, ErrMissingBlockData) {
		t.Fatalf("expected missing data, got %v", err)
	}
	cb := *b1.Txs[0]
	cb.Witnesses = [][]byte{{0x42}}
	_, err = c.ProcessBlock(&consensus.Block{Header: b1.Header, Txs: []*consensus.Tx{&cb}}, -1)
	t.Logf("mutated b1 body: %v", err)
	_, err = c.ProcessBlock(b1, -1)
	t.Logf("genuine b1 body: %v", err)
	bn, _ = c.Node(b1.Header.Hash())
	if bn.Status == StatusInvalid {
		t.Errorf("VULNERABLE: heavier honest branch b1/b2 marked invalid by a mutated body; tip %d (%s)",
			c.Tip().Height, c.Tip().Hash)
	}
}
