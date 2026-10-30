package p2p

import (
	"sync/atomic"
	"testing"
	"time"

	"argonmine/consensus"
)

// A batch of headers is one chain whose first new header connects before
// any proof of work is checked, and the first new header is checked alone:
// the parallel check (one Argon2id per worker at once) is only for a peer
// that has just delivered a valid, new, connecting header. (Review E: any
// peer could become the sync peer with one unsolicited batch of junk and
// make the node compute one Argon2id per worker, GOMAXPROCS of them, before
// the ban, where the sequential check stopped after one.)

// linkedHeaders builds n headers on top of parent, each the parent of the
// next, with the given bits and no real proof of work.
func linkedHeaders(parent consensus.Header, n int, salt byte, bits uint32) []consensus.Header {
	out := make([]consensus.Header, n)
	prev := parent
	for i := range out {
		out[i] = consensus.Header{Version: 1, PrevHash: prev.Hash(),
			TxRoot: consensus.TaggedHash("junk", []byte{salt, byte(i), byte(i >> 8)}),
			Time:   prev.Time + 600, Bits: bits, Nonce: uint64(i)}
		prev = out[i]
	}
	return out
}

func countingNode(t *testing.T, calls *atomic.Int64, logs *logSink) *node {
	return newNodeWith(t, true, func(c *Config) {
		c.Logf = logs.logf
		c.checkHeader = func(h *consensus.Header) error {
			calls.Add(1)
			return consensus.CheckHeaderStateless(consensus.RegTest, h)
		}
	})
}

func TestJunkHeaderBatchCostsOneProofOfWork(t *testing.T) {
	var calls atomic.Int64
	logs := &logSink{t: t}
	victim := countingNode(t, &calls, logs)
	// Junk: one connected chain from genesis whose own (hard) target every
	// hash misses.
	hdrs := linkedHeaders(consensus.RegTest.Genesis.Header, MaxHeadersMsg, 9, 0x1d00ffff)
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(0)
	r.send(&MsgHeaders{Headers: hdrs})
	closed := closedByNode(r, 10*time.Second)
	if n := calls.Load(); n != 1 {
		t.Errorf("AMPLIFIED: %d proof-of-work evaluations for one unsolicited junk batch, want 1", n)
	}
	if !closed {
		t.Error("peer sending junk headers not disconnected")
	}
}

// Headers that do not connect, or are not one chain, cost no proof of work
// and earn no sync progress.
func TestNonConnectingHeadersCostNothing(t *testing.T) {
	var calls atomic.Int64
	logs := &logSink{t: t}
	victim := countingNode(t, &calls, logs)
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(1000)
	if r.expect(10*time.Second, isGetHeaders) == nil {
		t.Fatal("not chosen as sync peer")
	}
	victim.srv.sync.mu.Lock()
	before := victim.srv.sync.lastProgress
	victim.srv.sync.mu.Unlock()
	time.Sleep(20 * time.Millisecond)

	nowhere := consensus.Header{Version: 1, PrevHash: consensus.TaggedHash("nowhere", nil), Time: consensus.RegTest.Genesis.Header.Time}
	r.send(&MsgHeaders{Headers: linkedHeaders(nowhere, 50, 1, consensus.RegTest.PowLimitBits)})
	broken := linkedHeaders(consensus.RegTest.Genesis.Header, 50, 2, consensus.RegTest.PowLimitBits)
	broken[30].PrevHash = consensus.TaggedHash("gap", nil)
	r.send(&MsgHeaders{Headers: broken})
	r.send(&MsgPing{Nonce: 3})
	if r.expect(10*time.Second, func(m Message) bool { p, ok := m.(*MsgPong); return ok && p.Nonce == 3 }) == nil {
		t.Fatal("no pong")
	}
	victim.srv.sync.mu.Lock()
	after := victim.srv.sync.lastProgress
	victim.srv.sync.mu.Unlock()
	if n := calls.Load(); n != 0 {
		t.Errorf("%d proof-of-work evaluations for headers that do not connect", n)
	}
	if !after.Equal(before) {
		t.Error("sync progress credited for headers that do not connect")
	}
	if logs.count("non-connecting headers") != 1 || logs.count("non-continuous headers") != 1 {
		t.Error("misbehaviour not recorded")
	}
}
