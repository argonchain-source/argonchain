package p2p

import (
	"testing"
	"time"
)

// A successful connection (tried) refreshes the timestamp an address is
// gossiped with, not only our own "seen" time: a peer we connect to every
// day is passed on as fresh, so other nodes, which expire gossip by that
// timestamp (addrHorizon), keep propagating it. (Review E: it went out with
// the timestamp it was first learned with and was dropped as stale.)
func TestTriedAddressGossipedFresh(t *testing.T) {
	now := time.Now()
	a := newAddrBook("", false)
	x := testAddr(7, now.Add(-29*24*time.Hour)) // learned a while ago
	if !a.addAt(x, false, "198.51.100.7", now) {
		t.Fatal("not stored")
	}
	a.tried(x.String()) // we just connected to it successfully
	out := a.sample(10)
	if len(out) != 1 {
		t.Fatalf("sample: %d", len(out))
	}
	if age := time.Since(time.Unix(int64(out[0].Time), 0)); age > time.Minute {
		t.Fatalf("tried address gossiped with a timestamp %s old", age.Round(time.Hour))
	}
	b := newAddrBook("", false)
	later := now.Add(48 * time.Hour) // two days later, still connected daily
	if !b.addAt(out[0], false, "203.0.113.9", later) {
		t.Errorf("a peer we connect to is dropped by other nodes as stale")
	}
}
