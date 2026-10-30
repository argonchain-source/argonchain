package p2p

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
)

// mineFast extends ch's tip by one empty block, trying four nonces at a time
// (regtest: about half of all nonces pass), with timestamps kept near now so
// long chains stay inside the future-time bound. The proof of work is
// checked through ch.CheckHeaderPoW, so ProcessBlock does not pay again.
func mineFast(t *testing.T, ch *chain.Chain, k *key) *consensus.Block {
	t.Helper()
	parent := ch.Tip()
	h := parent.Height + 1
	cb := &consensus.Tx{Version: 1, LockHeight: h, Inputs: []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs: []consensus.TxOut{k.out(consensus.Subsidy(h))}, Witnesses: [][]byte{{byte(h), byte(h >> 8)}}}
	blk := &consensus.Block{Txs: []*consensus.Tx{cb}}
	txRoot, wRoot, _ := blk.ComputeRoots()
	tm := uint64(time.Now().Unix())
	if mtp := chain.MedianTimePast(parent); tm <= mtp {
		tm = mtp + 1
	}
	hdr := consensus.Header{Version: 1, PrevHash: parent.Hash, TxRoot: txRoot, WitnessRoot: wRoot,
		Time: tm, Bits: consensus.NextBits(consensus.RegTest, parent.Header.Time, parent.Height)}
	const par = 4
	var found atomic.Int64
	found.Store(-1)
	for base := uint64(0); found.Load() < 0; base += par {
		var wg sync.WaitGroup
		for i := uint64(0); i < par; i++ {
			wg.Add(1)
			go func(n uint64) {
				defer wg.Done()
				c := hdr
				c.Nonce = n
				if ch.CheckHeaderPoW(&c) == nil {
					for {
						cur := found.Load()
						if (cur >= 0 && uint64(cur) <= n) || found.CompareAndSwap(cur, int64(n)) {
							return
						}
					}
				}
			}(base + i)
		}
		wg.Wait()
	}
	hdr.Nonce = uint64(found.Load())
	blk.Header = hdr
	if _, err := ch.ProcessBlock(blk, -1); err != nil {
		t.Fatal(err)
	}
	return blk
}

// A heavier honest branch forked at genesis whose first n blocks have no
// more work than the victim's tip must win. Its light prefix is held in the
// sync queue (nothing stored) and bodies are fetched once the queued branch
// outgrows the tip; a prefix longer than the queue is moved into the index
// header-only and its bodies fetched first, a window at a time (see
// TestLongLightPrefixOverflowsToIndex for that path at a small scale).
// Opt-in: REVIEWE_N sets n (6200 takes several minutes). (Review E: above
// about 6,000 every wanted body triggered a reorganisation that read every
// stored body, the reader blocked, the honest peer was dropped as stalled
// and the wanted bodies were lost.)
func TestLongLightPrefixReorg(t *testing.T) {
	s := os.Getenv("REVIEWE_N")
	if s == "" {
		t.Skip("mines thousands of blocks: set REVIEWE_N (e.g. 6200)")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logSink{t: t}
	victim := newNodeWith(t, false, func(c *Config) {
		c.Logf = func(format string, args ...any) { logs.logf(format, args...) }
	})
	honest := newNodeWith(t, true, func(c *Config) { c.Logf = func(string, ...any) {} })
	start := time.Now()
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
	t.Logf("mined %d and %d blocks in %s", n, n+2, time.Since(start).Round(time.Second))
	syncStart := time.Now()
	if err := victim.srv.Connect(honest.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	limit := 120*time.Second + time.Duration(n)*30*time.Millisecond
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) && victim.ch.Tip().Hash != honest.ch.Tip().Hash {
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("victim tip %d, honest tip %d, after %s; 'not keeping' logs %d, 'stalled' %d, 'misbehaving' %d",
		victim.ch.Tip().Height, honest.ch.Tip().Height, time.Since(syncStart).Round(time.Millisecond),
		logs.count("not keeping"), logs.count("stalled"), logs.count("misbehaving"))
	if victim.ch.Tip().Hash != honest.ch.Tip().Hash {
		t.Fatalf("honest heavier branch with a %d-block lighter prefix not adopted within %s", n, limit)
	}
}
