package chain

import (
	"errors"
	"testing"

	"argonmine/consensus"
)

// setMaxSide lowers the side-header cap for a test.
func setMaxSide(c *Chain, n int) {
	c.mu.Lock()
	c.maxSide = n
	c.mu.Unlock()
}

func sideCount(c *Chain) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sideHeaders
}

// The side-header cap cannot be held by cheap headers forked far below the
// tip: a header forked near the tip (almost the tip's work) takes the place
// of the one with the least work. A header with less work than everything
// in a full cap is refused, a parent is never evicted for its own child,
// and evictions are persistent.
func TestSideHeaderCapEvictsLeastWorkFirst(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	k := newKey(t)
	p := c.Params()
	g := c.Tip()
	main := []*Node{g}
	for i := 0; i < 4; i++ {
		main = append(main, mustProcess(t, c, makeBlock(t, p, main[len(main)-1], k, nil, 0, byte(i+1))))
	}
	setMaxSide(c, 5)
	// The attacker's way to fill the cap: children of genesis.
	var cheap []*consensus.Header
	for i := 0; i < 5; i++ {
		h := makeBlock(t, p, g, k, nil, 0, byte(100+i)).Header
		if err := c.ProcessHeader(&h, -1); err != nil {
			t.Fatal(err)
		}
		cheap = append(cheap, &h)
	}
	if sideCount(c) != 5 {
		t.Fatalf("side headers %d", sideCount(c))
	}
	// An honest one-block race near the tip: its first block has the tip's
	// work and gets in (by ProcessHeader, and as a relayed light block).
	race := makeBlock(t, p, main[3], k, nil, 0, 50)
	if err := c.ProcessHeader(&race.Header, -1); err != nil {
		t.Fatalf("VULNERABLE: a recent fork refused while the cap holds cheap deep forks: %v", err)
	}
	race2 := makeBlock(t, p, main[2], k, nil, 0, 51)
	if _, err := c.ProcessBlock(race2, -1); err != nil {
		t.Fatalf("relayed light block near the tip refused: %v", err)
	}
	gone := 0
	for _, h := range cheap {
		if _, ok := c.Node(h.Hash()); !ok {
			gone++
		}
	}
	if gone != 2 || sideCount(c) != 5 {
		t.Fatalf("%d cheap headers evicted, side headers %d; want 2 and 5", gone, sideCount(c))
	}
	// The cap now holds 3 genesis children and 2 recent forks. Fill it with
	// recent forks; then a genesis child has less work than all of them.
	for i := 0; i < 3; i++ {
		h := makeBlock(t, p, main[3], k, nil, 0, byte(60+i)).Header
		if err := c.ProcessHeader(&h, -1); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range cheap {
		if _, ok := c.Node(h.Hash()); ok {
			t.Fatal("a genesis child outlived recent forks")
		}
	}
	late := makeBlock(t, p, g, k, nil, 0, 70).Header
	if err := c.ProcessHeader(&late, -1); !errors.Is(err, ErrTooManyHeaders) {
		t.Fatalf("cheap header into a cap of heavier entries: %v", err)
	}
	c.Close()

	// Persistent: the evicted headers are gone after a restart.
	c2, err := Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if sideCount(c2) != 5 {
		t.Fatalf("after restart: %d side headers", sideCount(c2))
	}
	for _, h := range cheap {
		if _, ok := c2.Node(h.Hash()); ok {
			t.Fatal("evicted header back after restart")
		}
	}
	if _, ok := c2.Node(race.Header.Hash()); !ok {
		t.Fatal("recent fork lost on restart")
	}
}

// A header-only parent is never evicted to make room for its child (which
// would orphan the child), and a node with children is never evicted.
func TestSideHeaderCapSparesParent(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	p := c.Params()
	g := c.Tip()
	a1 := mustProcess(t, c, makeBlock(t, p, g, k, nil, 0, 1))
	mustProcess(t, c, makeBlock(t, p, a1, k, nil, 0, 2))
	mustProcess(t, c, makeBlock(t, p, c.Tip(), k, nil, 0, 3))
	setMaxSide(c, 2)
	x := makeBlock(t, p, g, k, nil, 0, 10).Header
	if err := c.ProcessHeader(&x, -1); err != nil {
		t.Fatal(err)
	}
	h1blk := makeBlock(t, p, g, k, nil, 0, 11) // equal work to x, arrived later: evicted first
	if err := c.ProcessHeader(&h1blk.Header, -1); err != nil {
		t.Fatal(err)
	}
	h1, _ := c.Node(h1blk.Header.Hash())
	h2 := makeBlock(t, p, h1, k, nil, 0, 12).Header
	if err := c.ProcessHeader(&h2, -1); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Node(h1.Hash); !ok {
		t.Fatal("parent evicted for its own child")
	}
	if _, ok := c.Node(x.Hash()); ok {
		t.Fatal("x should have made room")
	}
	// Full again (h1, h2): h1 has a child, so only h2 can go, and a
	// newcomer with less work than h2 is refused.
	y := makeBlock(t, p, g, k, nil, 0, 13).Header
	if err := c.ProcessHeader(&y, -1); !errors.Is(err, ErrTooManyHeaders) {
		t.Fatalf("lighter newcomer: %v", err)
	}
	if _, ok := c.Node(h1.Hash); !ok {
		t.Fatal("a node with a child was evicted")
	}
}

// ProcessHeaders stores a run in order and reports how far it got.
func TestProcessHeadersRun(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	p := c.Params()
	g := c.Tip()
	tip := g
	for i := 0; i < 5; i++ {
		tip = mustProcess(t, c, makeBlock(t, p, tip, k, nil, 0, byte(i+1)))
	}
	var run []consensus.Header
	parent := g
	for i := 0; i < 4; i++ {
		b := makeBlock(t, p, parent, k, nil, 0, byte(20+i))
		run = append(run, b.Header)
		parent = &Node{Hash: b.Header.Hash(), Header: b.Header, Height: parent.Height + 1}
	}
	if n, err := c.ProcessHeaders(run[:2], -1); n != 2 || err != nil {
		t.Fatalf("first two: %d %v", n, err)
	}
	bad := run[3]
	bad.Bits-- // contextually invalid
	n, err := c.ProcessHeaders([]consensus.Header{run[0], run[1], run[2], bad}, -1)
	if n != 3 || err == nil {
		t.Fatalf("run with a bad header: %d %v", n, err)
	}
	if sideCount(c) != 3 {
		t.Fatalf("side headers %d", sideCount(c))
	}
}

// reorganize looks at statuses before reading any body: a branch missing a
// body reports it without touching the others (here a corrupt one, which
// is only found, forgotten and fetched again once nothing is missing).
func TestReorganizeChecksStatusesBeforeReadingBodies(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	p := c.Params()
	g := c.Tip()
	tip := g
	for i := 0; i < 3; i++ {
		tip = mustProcess(t, c, makeBlock(t, p, tip, k, nil, 0, byte(i+1)))
	}
	b1 := makeBlock(t, p, g, k, nil, 0, 11)
	if _, err := c.ProcessBlockOpt(b1, -1, true); err != nil {
		t.Fatal(err)
	}
	b1n, _ := c.Node(b1.Header.Hash())
	b2 := makeBlock(t, p, b1n, k, nil, 0, 12)
	if err := c.ProcessHeader(&b2.Header, -1); err != nil {
		t.Fatal(err)
	}
	b2n, _ := c.Node(b2.Header.Hash())
	b3 := makeBlock(t, p, b2n, k, nil, 0, 13)
	if _, err := c.ProcessBlockOpt(b3, -1, true); err != nil {
		t.Fatal(err)
	}
	b3n, _ := c.Node(b3.Header.Hash())
	if err := c.db.Put(blockKey(b1.Header.Hash()), []byte{1, 2, 3}, nil); err != nil {
		t.Fatal(err)
	}
	b4 := makeBlock(t, p, b3n, k, nil, 0, 14) // heavier than the tip
	_, err := c.ProcessBlock(b4, -1)
	var md *MissingDataError
	if !errors.As(err, &md) || len(md.Hashes) != 1 || md.Hashes[0] != b2.Header.Hash() {
		t.Fatalf("want b2 missing, got %v", err)
	}
	if st, _ := c.Status(b1.Header.Hash()); st != StatusHaveData {
		t.Fatalf("b1 status %d: its body was read although b2 was missing", st)
	}
	// b2 arrives: now b1's corrupt body is found and asked for again.
	_, err = c.ProcessBlock(b2, -1)
	if !errors.As(err, &md) || len(md.Hashes) != 1 || md.Hashes[0] != b1.Header.Hash() {
		t.Fatalf("want b1 missing, got %v", err)
	}
	if st, _ := c.Status(b1.Header.Hash()); st != StatusHeaderOnly {
		t.Fatalf("corrupt b1 not forgotten: status %d", st)
	}
	if _, err := c.ProcessBlock(b1, -1); err != nil {
		t.Fatal(err)
	}
	if c.Tip().Hash != b4.Header.Hash() {
		t.Fatalf("tip height %d, want b4", c.Tip().Height)
	}
}

// forgetBodyLocked leaves the node as it was when the write fails.
func TestForgetBodyRevertsOnWriteFailure(t *testing.T) {
	c := openTemp(t)
	k := newKey(t)
	p := c.Params()
	mustProcess(t, c, makeBlock(t, p, c.Tip(), k, nil, 0, 1))
	g, _ := c.Node(consensus.RegTest.GenesisHash)
	side := makeBlock(t, p, g, k, nil, 0, 2)
	if _, err := c.ProcessBlockOpt(side, -1, true); err != nil {
		t.Fatal(err)
	}
	n, _ := c.Node(side.Header.Hash())
	before := sideCount(c)
	c.db.Close()
	c.mu.Lock()
	err := c.forgetBodyLocked(n)
	c.mu.Unlock()
	if !IsStorage(err) {
		t.Fatalf("want a storage error, got %v", err)
	}
	if n.Status != StatusHaveData || sideCount(c) != before {
		t.Fatalf("status %d, side headers %d after a failed forget", n.Status, sideCount(c))
	}
}
