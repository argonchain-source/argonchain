package chain

import (
	"errors"
	"testing"
)

// A stored body that does not even decode (disk corruption) is treated like
// one that does not match its header: reorganize forgets it (the node goes
// back to header-only) and reports it missing, so the body fetched again is
// accepted and the heavier branch connects. (Review E: it used to be
// reported missing while its node stayed StatusHaveData, and the refetched
// body was refused as a duplicate for good.)
func TestUndecodableStoredBodyRefetched(t *testing.T) {
	c := openTemp(t)
	p := c.Params()
	k := newKey(t)
	a1 := mustProcess(t, c, makeBlock(t, p, c.Tip(), k, nil, 0, 1))
	mustProcess(t, c, makeBlock(t, p, a1, k, nil, 0, 2)) // A2, the tip
	b2 := makeBlock(t, p, a1, k, nil, 0, 3)              // equal-work side block
	if _, err := c.ProcessBlockOpt(b2, -1, true); err != nil {
		t.Fatal(err)
	}
	if err := c.db.Put(blockKey(b2.Header.Hash()), []byte{0xde, 0xad}, nil); err != nil {
		t.Fatal(err)
	}
	b2n, _ := c.Node(b2.Header.Hash())
	b3 := makeBlock(t, p, b2n, k, nil, 0, 4) // makes B heavier
	_, err := c.ProcessBlock(b3, -1)
	if !errors.Is(err, ErrMissingBlockData) {
		t.Fatalf("expected missing data, got %v", err)
	}
	if st, _ := c.Status(b2n.Hash); st != StatusHeaderOnly {
		t.Fatalf("undecodable body not forgotten: status %d", st)
	}
	// The node that asked for it delivers the genuine body again.
	if _, err = c.ProcessBlock(b2, -1); err != nil {
		t.Fatalf("re-delivered genuine body: %v", err)
	}
	if c.Tip().Hash != b3.Header.Hash() {
		t.Errorf("STUCK: heavier branch never connected (tip height %d)", c.Tip().Height)
	}
}
