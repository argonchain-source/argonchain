package p2p

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
)

// nodeFromDir is newNodeWith for an existing chain directory.
func nodeFromDir(t *testing.T, dir string, listen bool, logf func(string, ...any)) *node {
	t.Helper()
	ch, err := chain.Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := mempool.New(ch, mempool.DefaultConfig())
	cfg := Config{Params: consensus.RegTest, MaxOutbound: 2, Logf: logf}
	if listen {
		cfg.Listen = "127.0.0.1:0"
	}
	srv := New(cfg, ch, pool)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Stop()
		ch.Close()
	})
	return &node{ch: ch, pool: pool, srv: srv}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	ents, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// mineCached mines n blocks from genesis into dir once (kept for reruns).
func mineCached(t *testing.T, dir string, n int) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
		return
	}
	os.RemoveAll(dir)
	ch, err := chain.Open(consensus.RegTest, dir)
	if err != nil {
		t.Fatal(err)
	}
	k := newKey(t)
	for i := 0; i < n; i++ {
		mineFast(t, ch, k)
	}
	ch.Close()
	os.WriteFile(filepath.Join(dir, "done"), nil, 0o600)
}

// Opt-in diagnostic run of the deep light-prefix reorg (REVIEWE_CACHE: a
// directory for the mined chains; REVIEWE_N: the victim's height, default
// 6200; the honest chain is two blocks longer).
func TestDeepReorgDiag(t *testing.T) {
	cache := os.Getenv("REVIEWE_CACHE")
	if cache == "" {
		t.Skip("set REVIEWE_CACHE")
	}
	n := 6200
	if s := os.Getenv("REVIEWE_N"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	vdir := filepath.Join(cache, "victim"+strconv.Itoa(n))
	hdir := filepath.Join(cache, "honest"+strconv.Itoa(n+2))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); mineCached(t, vdir, n) }()
	go func() { defer wg.Done(); mineCached(t, hdir, n+2) }()
	wg.Wait()
	vtmp, htmp := t.TempDir(), t.TempDir()
	copyDir(t, vdir, vtmp)
	copyDir(t, hdir, htmp)
	os.Remove(filepath.Join(vtmp, "done"))
	os.Remove(filepath.Join(htmp, "done"))

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		l := strings.TrimSpace(fmt.Sprintf(format, args...))
		mu.Lock()
		lines = append(lines, l)
		mu.Unlock()
	}
	victim := nodeFromDir(t, vtmp, false, logf)
	honest := nodeFromDir(t, htmp, true, func(string, ...any) {})
	var bHashes []consensus.Hash
	for h := uint32(1); h <= honest.ch.Tip().Height; h++ {
		nd, _ := honest.ch.NodeAtHeight(h)
		bHashes = append(bHashes, nd.Hash)
	}
	report := func(tag string) {
		counts := map[chain.Status]int{}
		unknown := 0
		firstHO := -1
		for i, h := range bHashes {
			nd, ok := victim.ch.Node(h)
			if !ok {
				unknown++
				continue
			}
			counts[nd.Status]++
			if nd.Status == chain.StatusHeaderOnly && firstHO < 0 {
				firstHO = i + 1
			}
		}
		victim.srv.sync.mu.Lock()
		q, rq, wd, syncing, done := len(victim.srv.sync.queue), len(victim.srv.sync.requested), len(victim.srv.sync.wanted), victim.srv.sync.peer != nil, victim.srv.sync.headersDone
		victim.srv.sync.mu.Unlock()
		mu.Lock()
		acc, miss := 0, 0
		var other []string
		for _, l := range lines {
			switch {
			case strings.Contains(l, "accepted block"):
				acc++
			case strings.Contains(l, "missing"):
				miss++
			default:
				other = append(other, l)
			}
		}
		if len(other) > 6 {
			other = other[len(other)-6:]
		}
		mu.Unlock()
		t.Logf("[%s] tip %d; B: unknown %d headerOnly %d haveData %d valid %d invalid %d (first header-only B%d); queue %d requested %d wanted %d syncing %v headersDone %v; accepted-logs %d missing-logs %d; recent: %q",
			tag, victim.ch.Tip().Height, unknown, counts[chain.StatusHeaderOnly], counts[chain.StatusHaveData], counts[chain.StatusValid], counts[chain.StatusInvalid], firstHO,
			q, rq, wd, syncing, done, acc, miss, other)
	}
	if err := victim.srv.Connect(honest.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	limit := 10 * time.Minute
	for time.Since(start) < limit && victim.ch.Tip().Hash != honest.ch.Tip().Hash {
		time.Sleep(15 * time.Second)
		report(time.Since(start).Round(time.Second).String())
	}
	report("end")
	if victim.ch.Tip().Hash != honest.ch.Tip().Hash {
		t.Fatalf("not reorganised after %s", time.Since(start).Round(time.Second))
	}
	t.Logf("reorganised after %s", time.Since(start).Round(time.Second))
}
