package p2p

import (
	"errors"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"argonmine/consensus"
)

// Review D, sync stall: onHeaders verified the proof of work of a full
// MaxHeadersMsg batch (one Argon2id each, sequentially, about 35 ms idle and
// 80 ms under mining load) before it recorded any progress, while
// syncWatchdog drops a sync peer (and marks it unreliable for an hour) after
// syncStallLimit without progress. A batch took longer than the limit, so a
// fresh node never got past its first 2000 headers. Now the batch is
// verified in parallel, progress is recorded as each header passes, headers
// already queued or known are not checked again, and the chain remembers
// the headers it verified so their bodies skip the second Argon2id.

// fakeHeaders builds n connecting headers on top of parent with regtest's
// bits and no real proof of work (for a node whose checkHeader is a stub).
func fakeHeaders(parent consensus.Header, n int, salt byte) []consensus.Header {
	out := make([]consensus.Header, n)
	prev := parent
	for i := range out {
		out[i] = consensus.Header{Version: 1, PrevHash: prev.Hash(),
			TxRoot: consensus.TaggedHash("fake", []byte{salt, byte(i), byte(i >> 8)}),
			Time:   prev.Time + 600, Bits: consensus.RegTest.PowLimitBits, Nonce: uint64(i)}
		prev = out[i]
	}
	return out
}

func isGetHeaders(m Message) bool { _, ok := m.(*MsgGetHeaders); return ok }

func asksFor(h consensus.Hash) func(Message) bool {
	return func(m Message) bool {
		gd, ok := m.(*MsgGetData)
		if !ok {
			return false
		}
		for _, it := range gd.Items {
			if it.Type == InvBlock && it.Hash == h {
				return true
			}
		}
		return false
	}
}

func syncQueueLen(s *Server) int {
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	return len(s.sync.queue)
}

// A full batch whose verification takes several times the stall limit is
// verified, queued and followed up (bodies and the next headers asked for)
// without the watchdog dropping the sync peer. Deterministic: each header's
// check is a 5 ms stub, four workers, a 400 ms stall limit.
func TestReviewD_HeaderBatchVerifyVsStallLimit(t *testing.T) {
	const perHeader, workers, stall = 5 * time.Millisecond, 4, 400 * time.Millisecond
	var calls atomic.Int64
	logs := &logSink{t: t}
	victim := newNodeWith(t, true, func(c *Config) {
		c.Logf = logs.logf
		c.syncStallLimit = stall
		c.syncWatchdogEvery = 50 * time.Millisecond
		c.verifyWorkers = workers
		c.checkHeader = func(*consensus.Header) error {
			calls.Add(1)
			time.Sleep(perHeader)
			return nil
		}
	})
	hdrs := fakeHeaders(consensus.RegTest.Genesis.Header, MaxHeadersMsg, 1)
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(MaxHeadersMsg)
	if r.expect(10*time.Second, isGetHeaders) == nil {
		t.Fatal("not chosen as sync peer")
	}
	start := time.Now()
	r.send(&MsgHeaders{Headers: hdrs})
	last := hdrs[len(hdrs)-1].Hash()
	next := r.expect(60*time.Second, func(m Message) bool {
		gh, ok := m.(*MsgGetHeaders)
		return ok && len(gh.Locator) > 0 && gh.Locator[0] == last
	})
	took := time.Since(start)
	if next == nil {
		t.Fatalf("VULNERABLE: the batch was never completed (%d of %d headers checked); watchdog drops: %d",
			calls.Load(), MaxHeadersMsg, logs.count("stalled"))
	}
	if r.expect(10*time.Second, asksFor(hdrs[0].Hash())) == nil {
		t.Fatal("no bodies asked for after the batch")
	}
	t.Logf("%d headers verified in %s (stall limit %s)", calls.Load(), took.Round(time.Millisecond), stall)
	if took <= stall {
		t.Fatalf("the batch took %s, not longer than the stall limit: the test proves nothing", took)
	}
	if n := logs.count("stalled"); n != 0 {
		t.Fatalf("sync peer dropped as stalled %d time(s) while delivering valid headers", n)
	}
	if calls.Load() != MaxHeadersMsg {
		t.Fatalf("%d header checks for %d headers", calls.Load(), MaxHeadersMsg)
	}
}

// The parallel check reaches the verdict checking in order would: the
// first failing header (by position) is the one reported, however the
// workers finish, and nothing of the batch is queued.
func TestHeaderBatchFirstFailureWins(t *testing.T) {
	hdrs := fakeHeaders(consensus.RegTest.Genesis.Header, MaxHeadersMsg, 2)
	bad := map[consensus.Hash]string{hdrs[700].Hash(): "bad 700", hdrs[1500].Hash(): "bad 1500", hdrs[1999].Hash(): "bad 1999"}
	var mu sync.Mutex
	rng := rand.New(rand.NewSource(1))
	logs := &logSink{t: t}
	victim := newNodeWith(t, true, func(c *Config) {
		c.Logf = logs.logf
		c.verifyWorkers = 8
		c.checkHeader = func(h *consensus.Header) error {
			mu.Lock()
			d := time.Duration(rng.Intn(500)) * time.Microsecond
			mu.Unlock()
			if msg, ok := bad[h.Hash()]; ok {
				if msg == "bad 700" {
					d = 50 * time.Millisecond // let a later failure be found first
				}
				time.Sleep(d)
				return errors.New(msg)
			}
			time.Sleep(d)
			return nil
		}
	})
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(MaxHeadersMsg)
	if r.expect(10*time.Second, isGetHeaders) == nil {
		t.Fatal("not chosen as sync peer")
	}
	r.send(&MsgHeaders{Headers: hdrs})
	if !closedByNode(r, 30*time.Second) {
		t.Fatal("peer with an invalid header not disconnected")
	}
	if logs.count("header fails proof of work: bad 700") != 1 || logs.count("bad 1500") != 0 || logs.count("bad 1999") != 0 {
		t.Fatal("the reported failure is not the first one in the batch")
	}
	if syncQueueLen(victim.srv) != 0 || victim.ch.Tip().Height != 0 {
		t.Fatal("headers of a failed batch were kept")
	}
}

// Headers already known or queued are not checked again.
func TestHeaderVerifySkipsQueuedAndKnown(t *testing.T) {
	var calls atomic.Int64
	victim := newNodeWith(t, true, func(c *Config) {
		c.checkHeader = func(*consensus.Header) error { calls.Add(1); return nil }
	})
	k := newKey(t)
	var batch []consensus.Header
	for i := 0; i < 3; i++ {
		batch = append(batch, mine(t, victim.ch, k, nil, 0).Header)
	}
	ext := fakeHeaders(batch[2], 7, 3)
	batch = append(batch, ext...)
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(10)
	if r.expect(10*time.Second, isGetHeaders) == nil {
		t.Fatal("not chosen as sync peer")
	}
	r.send(&MsgHeaders{Headers: batch})
	if r.expect(10*time.Second, asksFor(ext[0].Hash())) == nil {
		t.Fatal("no bodies asked for")
	}
	if calls.Load() != int64(len(ext)) {
		t.Fatalf("%d checks, want %d (the known headers are not checked)", calls.Load(), len(ext))
	}
	r.send(&MsgHeaders{Headers: batch}) // again: all known or queued now
	r.send(&MsgPing{Nonce: 9})
	if r.expect(10*time.Second, func(m Message) bool { p, ok := m.(*MsgPong); return ok && p.Nonce == 9 }) == nil {
		t.Fatal("no pong")
	}
	if calls.Load() != int64(len(ext)) {
		t.Fatalf("queued headers checked again: %d checks", calls.Load())
	}
	if syncQueueLen(victim.srv) != len(ext) {
		t.Fatalf("queue %d, want %d", syncQueueLen(victim.srv), len(ext))
	}
}

// Measurement (real Argon2id): a full batch through the parallel check,
// idle and with the miner on all but one CPU, and the longest time without
// recorded progress, which is what the watchdog compares to syncStallLimit.
func TestReviewD_HeaderBatchVerifyMeasure(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	const sample = 400
	n := newNode(t, false)
	hdrs := make([]consensus.Header, sample)
	for i := range hdrs {
		hdrs[i] = consensus.MainNet.Genesis.Header
		hdrs[i].Nonce = uint64(i) + 1
	}
	todo := make([]int, sample)
	for i := range todo {
		todo[i] = i
	}
	var mu sync.Mutex
	var lastNote time.Time
	var maxGap time.Duration
	n.srv.cfg.checkHeader = func(h *consensus.Header) error {
		_ = consensus.CheckHeaderStateless(consensus.MainNet, h) // the cost, whatever the verdict
		mu.Lock()
		if now := time.Now(); !lastNote.IsZero() && now.Sub(lastNote) > maxGap {
			maxGap = now.Sub(lastNote)
		}
		lastNote = time.Now()
		mu.Unlock()
		return nil
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	p := newPeer(n.srv, c1, false)
	measure := func(load int) (time.Duration, time.Duration) {
		var stop atomic.Bool
		var wg sync.WaitGroup
		for i := 0; i < load; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				h := consensus.MainNet.Genesis.Header
				for nonce := uint64(i) << 32; !stop.Load(); nonce++ {
					h.Nonce = nonce
					consensus.PowHash(h.Serialize())
				}
			}(i)
		}
		n.srv.sync.mu.Lock()
		n.srv.sync.peer = p
		n.srv.sync.gotHeaders = true
		n.srv.sync.lastProgress = time.Now()
		n.srv.sync.mu.Unlock()
		mu.Lock()
		lastNote, maxGap = time.Now(), 0
		mu.Unlock()
		t0 := time.Now()
		if ok, err := n.srv.verifyHeaders(p, hdrs, todo); !ok || err != nil {
			t.Fatalf("verify: %v %v", ok, err)
		}
		d := time.Since(t0)
		stop.Store(true)
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return d * MaxHeadersMsg / sample, maxGap
	}
	idle, idleGap := measure(0)
	busy, busyGap := measure(runtime.NumCPU() - 1)
	t.Logf("one %d-header batch on %d workers: %s idle (longest gap without progress %s), %s with the miner on %d threads (gap %s); stall limit %s",
		MaxHeadersMsg, runtime.GOMAXPROCS(0), idle.Round(time.Millisecond), idleGap.Round(time.Millisecond),
		busy.Round(time.Millisecond), runtime.NumCPU()-1, busyGap.Round(time.Millisecond), syncStallLimit)
	if busyGap > syncStallLimit || idleGap > syncStallLimit {
		t.Errorf("progress gap exceeds syncStallLimit: the sync peer would be dropped mid-batch")
	}
}
