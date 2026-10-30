package p2p

import (
	"strings"
	"testing"
	"time"

	"argonmine/consensus"
)

// TestReviewD_P2PMutatedBlockForksVictim: one unsolicited "block" message
// carrying an honest header with a tampered body (no proof of work needed)
// makes the victim reject the honest chain for good, and disconnect the
// honest peer that later serves it.
func TestReviewD_P2PMutatedBlockForksVictim(t *testing.T) {
	logs := &logSink{t: t}
	victim := newNodeWith(t, true, func(c *Config) { c.Logf = logs.logf })
	honest := newNode(t, true)
	k := newKey(t)
	b1 := mine(t, honest.ch, k, nil, 0)
	mine(t, honest.ch, k, nil, 0)

	// Attacker: copy b1's header, change the coinbase miner tag.
	r := dialRaw(t, victim.srv.ListenAddr())
	r.handshake(0)
	cb := *b1.Txs[0]
	cb.Witnesses = [][]byte{{0xEE}}
	r.send(&MsgBlock{Block: &consensus.Block{Header: b1.Header, Txs: []*consensus.Tx{&cb}}})
	closedByNode(r, 5*time.Second)

	if err := victim.srv.Connect(honest.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && victim.ch.Tip().Height < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if victim.ch.Tip().Height < 2 {
		dropped := 0
		for _, l := range logs.lines {
			if strings.Contains(l, "descends from an invalid block") {
				dropped++
			}
		}
		t.Errorf("VULNERABLE: victim stuck at height %d while honest chain is at %d; honest peer punished %d time(s) for serving the honest chain",
			victim.ch.Tip().Height, honest.ch.Tip().Height, dropped)
	}
}
