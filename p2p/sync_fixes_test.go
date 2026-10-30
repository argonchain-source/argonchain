package p2p

import (
	"net"
	"sync"
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
)

// Regression tests for the second review of headers-first sync (review E).

// lightPrefixPair mines n blocks on a victim and n+2 from genesis on an
// honest node, both exchanging headers in batches of 20, the victim's sync
// queue holding at most 60: the honest branch's light prefix (n blocks) is
// longer than the queue.
func lightPrefixPair(t *testing.T, n int) (victim, honest *node, logs *logSink) {
	t.Helper()
	logs = &logSink{t: t}
	victim = newNodeWith(t, false, func(c *Config) {
		c.Logf = logs.logf
		c.headersPerMsg = 20
		c.maxSyncQueue = 60
	})
	honest = newNodeWith(t, true, func(c *Config) {
		c.headersPerMsg = 20
		c.Logf = func(string, ...any) {}
	})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		k := newKey(t)
		for i := 0; i < n; i++ {
			mineFast(t, victim.ch, k)
		}
	}()
	go func() {
		defer wg.Done()
		k := newKey(t)
		for i := 0; i < n+2; i++ {
			mineFast(t, honest.ch, k)
		}
	}()
	wg.Wait()
	return victim, honest, logs
}

// A light prefix longer than the sync queue is moved into the index
// header-only, oldest first; once the branch outgrows the tip those bodies
// are fetched first, a window at a time, refilled as it drains (no fixed
// cut-off), and the branch wins.
func TestLongLightPrefixOverflowsToIndex(t *testing.T) {
	victim, honest, logs := lightPrefixPair(t, 150)
	if err := victim.srv.Connect(honest.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reorganisation onto the honest branch", 120*time.Second, func() bool {
		return victim.ch.Tip().Hash == honest.ch.Tip().Hash
	})
	if n := logs.count("header-only blocks below the branch"); n < 2 {
		t.Fatalf("prefix windows fetched: %d (the overflow path was not exercised)", n)
	}
	if logs.count("stalled") != 0 || logs.count("misbehaving") != 0 {
		t.Fatal("honest peer dropped or punished")
	}
	waitFor(t, "sync to finish", 10*time.Second, func() bool { return !victim.srv.Syncing() })
	if n := sideHeaders(victim.ch); n != 0 {
		t.Fatalf("%d header-only entries left after the reorganisation", n)
	}
}

// With the side-header cap full (and nothing in it the newcomers may
// displace) the light prefix stays in memory instead, and the branch still
// wins; the honest peer is not held responsible.
func TestLongLightPrefixKeptInMemoryWhenCapFull(t *testing.T) {
	victim, honest, logs := lightPrefixPair(t, 150)
	setSideHeaders(t, victim.ch, chain.MaxSideHeaders)
	if err := victim.srv.Connect(honest.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reorganisation onto the honest branch", 120*time.Second, func() bool {
		return victim.ch.Tip().Hash == honest.ch.Tip().Hash
	})
	if logs.count("did not take light headers") == 0 {
		t.Fatal("the cap did not refuse anything: test proves nothing")
	}
	for _, p := range victim.srv.peerList() {
		if p.unreliable() {
			t.Fatal("honest peer marked unreliable")
		}
	}
	if n := sideHeaders(victim.ch); n != chain.MaxSideHeaders {
		t.Fatalf("side headers %d", n)
	}
}

// A peer claiming to be ahead whose full batch holds only blocks of our own
// main chain is marked unreliable (as one sending nothing). A full batch of
// side headers we hold header-only is not "nothing": the next is asked for.
func TestFullBatchOfKnownHeaders(t *testing.T) {
	logs := &logSink{t: t}
	victim := newNodeWith(t, true, func(c *Config) { c.Logf = logs.logf; c.headersPerMsg = 10 })
	k := newKey(t)
	var own []consensus.Header
	for i := 0; i < 12; i++ {
		own = append(own, mine(t, victim.ch, k, nil, 0).Header)
	}
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(1000)
	if r.expect(10*time.Second, isGetHeaders) == nil {
		t.Fatal("not chosen as sync peer")
	}
	r.send(&MsgHeaders{Headers: own[:10]})
	waitFor(t, "claim dismissed", 10*time.Second, func() bool {
		return logs.count("sent no headers we lack") == 1 && !victim.srv.Syncing()
	})
	for _, p := range victim.srv.peerList() {
		if !p.unreliable() {
			t.Fatal("peer echoing our own chain not marked unreliable")
		}
	}

	// Side headers we hold header-only, from genesis.
	side := sideChain(t, 10)
	for _, b := range side {
		if err := victim.ch.ProcessHeader(&b.Header, -1); err != nil {
			t.Fatal(err)
		}
	}
	r2 := dialRaw(t, victim.srv.ListenAddr())
	r2.handshake(1000)
	if r2.expect(10*time.Second, isGetHeaders) == nil {
		t.Fatal("second peer not chosen as sync peer")
	}
	hdrs := make([]consensus.Header, len(side))
	for i, b := range side {
		hdrs[i] = b.Header
	}
	r2.send(&MsgHeaders{Headers: hdrs})
	last := hdrs[len(hdrs)-1].Hash()
	if r2.expect(10*time.Second, func(m Message) bool {
		gh, ok := m.(*MsgGetHeaders)
		return ok && len(gh.Locator) > 0 && gh.Locator[0] == last
	}) == nil {
		t.Fatal("the headers after a full batch of known side headers were not asked for")
	}
	if logs.count("sent no headers we lack") != 1 {
		t.Fatal("peer serving a known side branch marked unreliable")
	}
}

// Time this node spends handling the sync peer's message is not the peer's
// delay: verifying a batch slowly (here each header's check takes twice
// the stall limit) does not get the peer dropped as stalled.
func TestBusyHandlingNotCountedAsStall(t *testing.T) {
	logs := &logSink{t: t}
	victim := newNodeWith(t, true, func(c *Config) {
		c.Logf = logs.logf
		c.syncStallLimit = 200 * time.Millisecond
		c.syncWatchdogEvery = 20 * time.Millisecond
		c.verifyWorkers = 1
		c.checkHeader = func(*consensus.Header) error { time.Sleep(400 * time.Millisecond); return nil }
	})
	hdrs := fakeHeaders(consensus.RegTest.Genesis.Header, 4, 7)
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(4)
	if r.expect(10*time.Second, isGetHeaders) == nil {
		t.Fatal("not chosen as sync peer")
	}
	r.send(&MsgHeaders{Headers: hdrs})
	if r.expect(10*time.Second, asksFor(hdrs[0].Hash())) == nil {
		t.Fatal("bodies never asked for")
	}
	if n := logs.count("stalled"); n != 0 {
		t.Fatalf("sync peer dropped as stalled %d time(s) while we were verifying its headers", n)
	}
}

// An orphan whose body does not match its header is refused and its sender
// punished; it does not take the orphan slot of the real block (orphans
// are keyed by parent), which connects once its parent arrives.
func TestMutatedOrphanPunished(t *testing.T) {
	logs := &logSink{t: t}
	victim := newNodeWith(t, true, func(c *Config) { c.Logf = logs.logf })
	side := sideChain(t, 2)
	b1, b2 := side[0], side[1]
	honest := dialRaw(t, victim.srv.ListenAddr())
	honest.handshake(0)
	honest.send(&MsgBlock{Block: b2}) // orphan: cached
	honest.send(&MsgPing{Nonce: 1})
	if honest.expect(10*time.Second, func(m Message) bool { p, ok := m.(*MsgPong); return ok && p.Nonce == 1 }) == nil {
		t.Fatal("no pong")
	}
	evil := dialRaw(t, victim.srv.ListenAddr())
	evil.handshake(0)
	cb := *b2.Txs[0]
	cb.Witnesses = [][]byte{{0xEE}}
	evil.send(&MsgBlock{Block: &consensus.Block{Header: b2.Header, Txs: []*consensus.Tx{&cb}}})
	if !closedByNode(evil, 10*time.Second) {
		t.Error("sender of a mutated orphan not disconnected")
	}
	honest.send(&MsgBlock{Block: b1})
	waitFor(t, "the real orphan connected", 10*time.Second, func() bool { return victim.ch.Tip().Hash == b2.Header.Hash() })
}

// A body wanted for a reorganisation that its peer never sends is asked of
// another peer, and the reorganisation completes. (Review E: wanted bodies
// were asked for once; if that peer did not answer, they were lost.)
func TestWantedBodyRetriedFromAnotherPeer(t *testing.T) {
	logs := &logSink{t: t}
	victim := newNodeWith(t, true, func(c *Config) {
		c.Logf = logs.logf
		c.wantedTimeout = 300 * time.Millisecond
		c.syncWatchdogEvery = 50 * time.Millisecond
	})
	k := newKey(t)
	mine(t, victim.ch, k, nil, 0)
	mine(t, victim.ch, k, nil, 0)
	side := sideChain(t, 3) // heavier at its third block
	silent := dialRaw(t, victim.srv.ListenAddr())
	silent.handshake(0)
	for _, b := range side {
		silent.send(&MsgBlock{Block: b})
	}
	if silent.expect(10*time.Second, asksFor(side[0].Header.Hash())) == nil {
		t.Fatal("missing bodies not asked for")
	}
	helper := dialRaw(t, victim.srv.ListenAddr())
	helper.handshake(0)
	served := map[consensus.Hash]bool{}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && victim.ch.Tip().Hash != side[2].Header.Hash() {
		m := helper.read(200 * time.Millisecond)
		gd, ok := m.(*MsgGetData)
		if !ok {
			continue
		}
		for _, it := range gd.Items {
			for _, b := range side {
				if it.Type == InvBlock && it.Hash == b.Header.Hash() && !served[it.Hash] {
					served[it.Hash] = true
					helper.send(&MsgBlock{Block: b})
				}
			}
		}
	}
	if victim.ch.Tip().Hash != side[2].Header.Hash() {
		t.Fatalf("reorganisation never completed (bodies asked of the other peer: %d)", len(served))
	}
}

// The wanted set is bounded, and drops what the index no longer needs.
func TestWantedBounded(t *testing.T) {
	n := newNode(t, false)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	p := newPeer(n.srv, c1, false)
	var hashes []consensus.Hash
	for i := 0; i < 3000; i++ {
		hashes = append(hashes, consensus.TaggedHash("want", []byte{byte(i), byte(i >> 8)}))
	}
	for i := 0; i < 30; i++ {
		n.srv.requestBodies(p, hashes[i*100:(i+1)*100])
	}
	n.srv.sync.mu.Lock()
	got := len(n.srv.sync.wanted)
	n.srv.sync.mu.Unlock()
	if got != maxWanted {
		t.Fatalf("wanted holds %d, bound %d", got, maxWanted)
	}
	n.srv.retryWanted() // none of them is a header-only block of ours
	n.srv.sync.mu.Lock()
	got = len(n.srv.sync.wanted)
	n.srv.sync.mu.Unlock()
	if got != 0 {
		t.Fatalf("%d stale wanted entries kept", got)
	}
}
