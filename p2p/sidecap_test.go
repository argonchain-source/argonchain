package p2p

import (
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"argonmine/chain"
	"argonmine/consensus"
)

// Following a competing branch never depends on storing its lighter
// prefix: the sync keeps light headers in memory and fetches bodies once
// the branch outgrows our tip (keepBody, outside chain.MaxSideHeaders). So
// a full side-header cap (an attacker can precompute that many cheap
// children of genesis) cannot stop an ordinary race. (Review E: the light
// prefix went through chain.ProcessHeader, a full cap refused the winning
// branch's first block, the sync ended and every honest peer serving the
// branch was marked unreliable: a persistent split.)

// sideHeaders reads the chain's side-header counter.
func sideHeaders(ch *chain.Chain) int {
	v := reflect.ValueOf(ch).Elem().FieldByName("sideHeaders")
	return int(reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Int())
}

// setSideHeaders overwrites the victim chain's side-header counter, standing
// in for an attacker who pushed MaxSideHeaders cheap headers (the cap then
// has nothing it may evict: the worst case).
func setSideHeaders(t *testing.T, ch *chain.Chain, n int) {
	t.Helper()
	v := reflect.ValueOf(ch).Elem()
	mf := v.FieldByName("mu")
	mu := (*sync.RWMutex)(unsafe.Pointer(mf.UnsafeAddr()))
	mu.Lock()
	defer mu.Unlock()
	f := v.FieldByName("sideHeaders")
	reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().SetInt(int64(n))
}

// raceSetup: victim on G-A1-A2-A3, honest node on G-A1-A2-B3-B4 (heavier).
func raceSetup(t *testing.T) (victim, honest *node, logs *logSink) {
	t.Helper()
	logs = &logSink{t: t}
	victim = newNodeWith(t, false, func(c *Config) { c.Logf = logs.logf })
	honest = newNode(t, true)
	k := newKey(t)
	for i := 0; i < 2; i++ {
		b := mine(t, victim.ch, k, nil, 0)
		if _, err := honest.ch.ProcessBlock(b, -1); err != nil {
			t.Fatal(err)
		}
	}
	mine(t, victim.ch, k, nil, 0) // A3
	k2 := newKey(t)
	mine(t, honest.ch, k2, nil, 0) // B3
	mine(t, honest.ch, k2, nil, 0) // B4
	return victim, honest, logs
}

func TestHonestRaceReorg(t *testing.T) {
	victim, honest, _ := raceSetup(t)
	if err := victim.srv.Connect(honest.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reorganisation onto B4", 30*time.Second, func() bool {
		return victim.ch.Tip().Hash == honest.ch.Tip().Hash
	})
}

func TestFullSideHeaderCapDoesNotBlockHonestReorg(t *testing.T) {
	victim, honest, logs := raceSetup(t)
	setSideHeaders(t, victim.ch, chain.MaxSideHeaders)
	if err := victim.srv.Connect(honest.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && victim.ch.Tip().Hash != honest.ch.Tip().Hash {
		time.Sleep(50 * time.Millisecond)
	}
	unreliable := false
	for _, p := range victim.srv.peerList() {
		if p.unreliable() {
			unreliable = true
		}
	}
	if victim.ch.Tip().Hash != honest.ch.Tip().Hash {
		t.Fatalf("VULNERABLE: with the side-header cap full the victim never reorganises onto the heavier honest branch (tip %d)", victim.ch.Tip().Height)
	}
	if unreliable || logs.count("not keeping") != 0 {
		t.Fatal("the honest peer was held responsible for the full cap")
	}
	if n := sideHeaders(victim.ch); n != chain.MaxSideHeaders {
		t.Fatalf("side headers %d: the race went through the cap", n)
	}
}

// Unsolicited headers of a lighter branch (here forked at genesis, from a
// peer claiming no height) are checked and dropped when the sync ends:
// nothing of them is stored, so they cannot fill the cap.
func TestUnsolicitedLightHeadersNotStored(t *testing.T) {
	victim := newNode(t, true)
	k := newKey(t)
	for i := 0; i < 4; i++ {
		mine(t, victim.ch, k, nil, 0)
	}
	side := sideChain(t, 3)
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(0)
	hdrs := make([]consensus.Header, len(side))
	for i, b := range side {
		hdrs[i] = b.Header
	}
	r.send(&MsgHeaders{Headers: hdrs})
	r.send(&MsgPing{Nonce: 9})
	if r.expect(10*time.Second, func(m Message) bool { p, ok := m.(*MsgPong); return ok && p.Nonce == 9 }) == nil {
		t.Fatal("no pong")
	}
	if n := sideHeaders(victim.ch); n != 0 {
		t.Errorf("%d side headers stored from one unsolicited message of light headers", n)
	}
	for _, b := range side {
		if _, ok := victim.ch.Node(b.Header.Hash()); ok {
			t.Error("light header stored")
		}
	}
	if victim.srv.Syncing() || syncQueueLen(victim.srv) != 0 {
		t.Error("sync with the light branch still going")
	}
}
