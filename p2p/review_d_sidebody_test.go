package p2p

import (
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
)

// Review D, lighter-branch bodies: any peer could become the sync peer with
// an unsolicited "headers" message that connects to a known block, and
// every body of the branch it announced was then fetched and stored in full
// (keepBody), although the branch had less work than the tip, outside
// chain.MaxSideHeaders. Now bodies are fetched only for a queued branch with
// more work than the tip; the headers of a branch that never gets there are
// dropped when the sync ends (nothing stored); unsolicited bodies of a
// lighter branch keep only their headers, under the cap; and a peer already
// found unreliable is not adopted from unsolicited headers.

// sideChain mines n blocks from genesis on a chain of its own.
func sideChain(t *testing.T, n int) []*consensus.Block {
	t.Helper()
	ach, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ach.Close()
	ak := newKey(t)
	var side []*consensus.Block
	for i := 0; i < n; i++ {
		side = append(side, mine(t, ach, ak, nil, 0))
	}
	return side
}

func TestReviewD_LighterBranchBodiesStoredViaSync(t *testing.T) {
	victim := newNode(t, true)
	k := newKey(t)
	for i := 0; i < 5; i++ {
		mine(t, victim.ch, k, nil, 0)
	}
	side := sideChain(t, 3) // the attacker's lighter branch from genesis

	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(0) // no height claim at all
	hdrs := make([]consensus.Header, len(side))
	for i, b := range side {
		hdrs[i] = b.Header
	}
	r.send(&MsgHeaders{Headers: hdrs})
	r.send(&MsgPing{Nonce: 3})

	requested := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m := r.read(time.Until(deadline))
		if m == nil {
			break
		}
		gd, ok := m.(*MsgGetData)
		if !ok {
			continue
		}
		for _, it := range gd.Items {
			for _, b := range side {
				if it.Type == InvBlock && it.Hash == b.Header.Hash() {
					requested++
					r.send(&MsgBlock{Block: b})
				}
			}
		}
	}
	// The sync dropped the lighter branch: nothing of it is in the index.
	for _, b := range side {
		if _, ok := victim.ch.Node(b.Header.Hash()); ok {
			t.Errorf("header %s of a lighter branch stored by the sync", b.Header.Hash())
		}
	}
	// Unsolicited bodies of those headers are not stored either (only their
	// headers, as for any relayed block of a lighter branch).
	for _, b := range side {
		r.send(&MsgBlock{Block: b})
	}
	r.send(&MsgPing{Nonce: 4})
	if r.expect(5*time.Second, func(m Message) bool { p, ok := m.(*MsgPong); return ok && p.Nonce == 4 }) == nil {
		t.Fatal("no pong")
	}
	stored, headerOnly := 0, 0
	for _, b := range side {
		if n, ok := victim.ch.Node(b.Header.Hash()); ok {
			switch n.Status {
			case chain.StatusHaveData, chain.StatusValid:
				stored++
			case chain.StatusHeaderOnly:
				headerOnly++
			}
		}
	}
	t.Logf("victim tip height %d; lighter-branch bodies requested %d, stored in full %d, header-only %d of %d",
		victim.ch.Tip().Height, requested, stored, headerOnly, len(side))
	if requested > 0 || stored > 0 {
		t.Errorf("VULNERABLE: %d bodies of a lighter branch (work below the tip) requested and %d stored in full", requested, stored)
	}
	if headerOnly != len(side) {
		t.Errorf("lighter headers not kept header-only: %d of %d", headerOnly, len(side))
	}
	if victim.srv.Syncing() || victim.ch.Tip().Height != 5 {
		t.Errorf("syncing %v, tip %d", victim.srv.Syncing(), victim.ch.Tip().Height)
	}
}

// A branch whose first headers are lighter than the tip and the rest
// heavier still wins: the heavier part is queued and fetched, the lighter
// prefix kept header-only and its bodies fetched when needed.
func TestLighterPrefixThenHeavierBranchSyncs(t *testing.T) {
	victim := newNode(t, false)
	k := newKey(t)
	for i := 0; i < 5; i++ {
		mine(t, victim.ch, k, nil, 0)
	}
	other := newNode(t, true)
	k2 := newKey(t)
	for i := 0; i < 8; i++ {
		mine(t, other.ch, k2, nil, 0)
	}
	if err := victim.srv.Connect(other.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reorganisation onto the heavier branch", 60*time.Second, func() bool {
		return victim.ch.Tip().Hash == other.ch.Tip().Hash
	})
	waitFor(t, "sync to finish", 10*time.Second, func() bool { return !victim.srv.Syncing() })
}

// Unsolicited headers from a peer already marked unreliable do not make it
// the sync peer.
func TestUnreliablePeerNotAdoptedFromHeaders(t *testing.T) {
	victim := newNode(t, true)
	side := sideChain(t, 3) // heavier than the victim's genesis-only chain
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(0)
	waitFor(t, "peer registered", 5*time.Second, func() bool {
		for _, p := range victim.srv.peerList() {
			if p.isHandshakeDone() {
				return true
			}
		}
		return false
	})
	for _, p := range victim.srv.peerList() {
		victim.srv.markUnreliable(p)
	}
	hdrs := make([]consensus.Header, len(side))
	for i, b := range side {
		hdrs[i] = b.Header
	}
	r.send(&MsgHeaders{Headers: hdrs})
	r.send(&MsgPing{Nonce: 5})
	got := r.expect(5*time.Second, func(m Message) bool {
		if p, ok := m.(*MsgPong); ok && p.Nonce == 5 {
			return true
		}
		return asksFor(side[0].Header.Hash())(m)
	})
	if _, pong := got.(*MsgPong); !pong {
		t.Fatalf("unreliable peer adopted as sync peer from unsolicited headers (got %T)", got)
	}
	if victim.srv.Syncing() {
		t.Fatal("syncing from an unreliable peer")
	}
}
