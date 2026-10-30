package p2p

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
)

// Regression tests for the denial-of-service review (docs/GO-LIVE.md A12,
// A13): each shows that the attack from the review no longer works.

type logSink struct {
	t     *testing.T
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	l.t.Log(line)
}

func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// closedByNode drains r until the node closes it (true) or wait passes (false).
func closedByNode(r *rawPeer, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	buf := make([]byte, 4096)
	for {
		r.conn.SetReadDeadline(deadline)
		if _, err := r.r.Read(buf); err != nil {
			return !isTimeout(err)
		}
	}
}

// expect reads until a message satisfies match, or returns nil at timeout.
func (r *rawPeer) expect(timeout time.Duration, match func(Message) bool) Message {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m := r.read(time.Until(deadline))
		if m == nil {
			return nil
		}
		if match(m) {
			return m
		}
	}
	return nil
}

// signTx signs every input of tx, spending spent, with k.
func signTx(t *testing.T, k *key, tx *consensus.Tx, spent []consensus.TxOut) {
	t.Helper()
	tx.Witnesses = make([][]byte, len(tx.Inputs))
	for i := range tx.Inputs {
		msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, spent, uint32(i))
		sig, err := schnorr.Sign(k.priv, msg[:])
		if err != nil {
			t.Fatal(err)
		}
		tx.Witnesses[i] = sig.Serialize()
	}
}

// coinbaseCoin returns the coinbase output of block h on ch.
func coinbaseCoin(t *testing.T, ch *chain.Chain, h uint32) (consensus.OutPoint, consensus.TxOut) {
	t.Helper()
	n, _ := ch.NodeAtHeight(h)
	blk, err := ch.Block(n.Hash)
	if err != nil {
		t.Fatal(err)
	}
	op := consensus.OutPoint{TxID: blk.Txs[0].TxID(), Index: 0}
	c, ok := ch.LookupCoin(op)
	if !ok {
		t.Fatalf("coinbase %d spent or missing", h)
	}
	return op, c.Out
}

// fanOut splits a coin into n outputs of each.
func fanOut(t *testing.T, k *key, op consensus.OutPoint, out consensus.TxOut, n int, each consensus.Amount) *consensus.Tx {
	t.Helper()
	tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}}
	for i := 0; i < n; i++ {
		tx.Outputs = append(tx.Outputs, k.out(each))
	}
	signTx(t, k, tx, []consensus.TxOut{out})
	return tx
}

// H1: behind a proxy (and, by default, from a loopback address) one bad
// connection bans no one, and a full inbound table evicts an idle peer
// instead of refusing the newcomer.
func TestBehindProxyNeverBansAndEvicts(t *testing.T) {
	for _, mode := range []string{"behind-proxy", "loopback-default"} {
		t.Run(mode, func(t *testing.T) {
			a := newNodeWith(t, true, func(c *Config) {
				c.MaxInbound = 4
				c.BehindProxy = mode == "behind-proxy"
			})
			addr := a.srv.ListenAddr()
			bad := dialRaw(t, addr)
			bad.conn.Write(append([]byte("XXXX"), make([]byte, 20)...))
			if !closedByNode(bad, 5*time.Second) {
				t.Fatal("bad magic not disconnected")
			}
			early := dialRaw(t, addr)
			early.send(&MsgPing{Nonce: 1})
			if !closedByNode(early, 5*time.Second) {
				t.Fatal("message before handshake not disconnected")
			}
			if a.srv.isBanned("127.0.0.1") {
				t.Fatal("the shared source address was banned")
			}
			// Six peers through one address, four inbound slots: every
			// newcomer gets in (more than MaxInboundPerIP too); the oldest
			// idle peers make room.
			var rs []*rawPeer
			for i := 0; i < 6; i++ {
				r := dialRaw(t, addr)
				r.handshake(0)
				rs = append(rs, r)
				time.Sleep(50 * time.Millisecond)
			}
			waitFor(t, "four inbound", 5*time.Second, func() bool { return a.srv.inboundCount() == 4 })
			for i, r := range rs {
				if closed, want := closedByNode(r, 300*time.Millisecond), i < 2; closed != want {
					t.Fatalf("peer %d closed=%v, want %v", i, closed, want)
				}
			}
		})
	}
}

func TestAcceptTokenBucket(t *testing.T) {
	b := newTokenBucket(acceptRate, acceptBurst)
	now := time.Now()
	b.last = now
	n := 0
	for i := 0; i < 100; i++ {
		if b.allowAt(now) {
			n++
		}
	}
	if n != acceptBurst {
		t.Fatalf("burst %d", n)
	}
	if !b.allowAt(now.Add(100*time.Millisecond)) || b.allowAt(now.Add(100*time.Millisecond)) {
		t.Fatal("refill rate")
	}
	n = 0
	for i := 0; i < 100; i++ {
		if b.allowAt(now.Add(1100 * time.Millisecond)) {
			n++
		}
	}
	if n != acceptRate {
		t.Fatalf("refilled %d in a second", n)
	}
}

// H2: a peer claiming a huge height and never answering cannot switch off
// transaction relay; it is dropped after firstHeadersTimeout and, its
// address identifying it, not chosen again when it reconnects.
func TestFakeHeightPeerDoesNotBlockTxRelay(t *testing.T) {
	a := newNodeWith(t, true, func(c *Config) { c.LocalAddrsAreHosts = true })
	k := newKey(t)
	for i := 0; i < 101; i++ {
		mine(t, a.ch, k, nil, 0)
	}
	b := newNode(t, false)
	if err := b.srv.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B synced", 60*time.Second, func() bool { return b.ch.Tip().Height == 101 && !b.srv.Syncing() && !a.srv.Syncing() })

	liar := dialRaw(t, a.srv.ListenAddr())
	liar.handshake(4_000_000_000)
	waitFor(t, "A syncing from the liar", 10*time.Second, func() bool { return a.srv.Syncing() })
	picked := time.Now()

	op, out := coinbaseCoin(t, b.ch, 1)
	tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}, Outputs: []consensus.TxOut{newKey(t).out(49 * consensus.UnitsPerCoin)}}
	signTx(t, k, tx, []consensus.TxOut{out})
	if err := b.pool.Accept(tx); err != nil {
		t.Fatal(err)
	}
	b.srv.BroadcastTx(tx)
	waitFor(t, "tx relayed while the liar holds the sync slot", 10*time.Second, func() bool { return a.pool.Has(tx.TxID()) })

	waitFor(t, "liar dropped", 30*time.Second, func() bool { return !a.srv.Syncing() })
	if el := time.Since(picked); el > firstHeadersTimeout+10*time.Second {
		t.Fatalf("liar held the sync slot for %s", el)
	}
	again := dialRaw(t, a.srv.ListenAddr())
	again.handshake(4_000_000_000)
	time.Sleep(500 * time.Millisecond)
	a.srv.startSync()
	if a.srv.Syncing() {
		t.Fatal("a host whose claim proved empty was chosen again")
	}
}

// M1: two nodes whose mempools hold 2,000 transactions the other lacks
// exchange them without stalling or dropping each other.
func TestMutualLargeMempoolsExchange(t *testing.T) {
	sa, sb := &logSink{t: t}, &logSink{t: t}
	a := newNodeWith(t, true, func(c *Config) { c.Logf = sa.logf })
	k := newKey(t)
	for i := 0; i < 102; i++ {
		mine(t, a.ch, k, nil, 0)
	}
	const n, outs = 2000, 40
	opA, outA := coinbaseCoin(t, a.ch, 1)
	opB, outB := coinbaseCoin(t, a.ch, 2)
	fanA := fanOut(t, k, opA, outA, n, 200_000)
	fanB := fanOut(t, k, opB, outB, n, 200_000)
	mine(t, a.ch, k, []*consensus.Tx{fanA, fanB}, outA.Amount+outB.Amount-2*n*200_000)

	chB, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for h := uint32(1); h <= a.ch.Tip().Height; h++ {
		nd, _ := a.ch.NodeAtHeight(h)
		blk, _ := a.ch.Block(nd.Hash)
		if _, err := chB.ProcessBlock(blk, -1); err != nil {
			t.Fatal(err)
		}
	}
	poolB := mempool.New(chB, mempool.DefaultConfig())
	fill := func(pool *mempool.Pool, fan *consensus.Tx) {
		for i := 0; i < n; i++ {
			tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: fan.TxID(), Index: uint32(i)}}}}
			for j := 0; j < outs; j++ {
				tx.Outputs = append(tx.Outputs, k.out(600))
			}
			signTx(t, k, tx, []consensus.TxOut{fan.Outputs[i]})
			if err := pool.Accept(tx); err != nil {
				t.Fatal(err)
			}
		}
	}
	fill(a.pool, fanA)
	fill(poolB, fanB)
	t.Logf("A pool %d txs %d bytes; B pool %d txs %d bytes", a.pool.Count(), a.pool.Bytes(), poolB.Count(), poolB.Bytes())

	srvB := New(Config{Params: consensus.RegTest, MaxOutbound: 2, Logf: sb.logf, Connect: []string{a.srv.ListenAddr()}}, chB, poolB)
	if err := srvB.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srvB.Stop(); chB.Close() })
	start := time.Now()
	waitFor(t, "both pools complete", 90*time.Second, func() bool { return a.pool.Count() == 2*n && poolB.Count() == 2*n })
	t.Logf("exchanged %d transactions each way in %s", n, time.Since(start).Round(time.Millisecond))
	if d := sa.count("disconnected") + sb.count("disconnected"); d != 0 {
		t.Fatalf("%d disconnects during the exchange", d)
	}
	if len(a.srv.Peers()) != 1 || len(srvB.Peers()) != 1 {
		t.Fatal("peer counts")
	}
}

// M2: chains of unconfirmed transactions, children paying more than their
// parents, all reach a newly connected peer (parents are announced first).
func TestChainedTxsAllArrive(t *testing.T) {
	a := newNode(t, true)
	k := newKey(t)
	for i := 0; i < 101; i++ {
		mine(t, a.ch, k, nil, 0)
	}
	const chains, depth = 100, 12
	op, out := coinbaseCoin(t, a.ch, 1)
	const each = consensus.UnitsPerCoin / 10
	fan := fanOut(t, k, op, out, chains, each)
	mine(t, a.ch, k, []*consensus.Tx{fan}, out.Amount-chains*each)
	for c := 0; c < chains; c++ {
		prev := consensus.OutPoint{TxID: fan.TxID(), Index: uint32(c)}
		prevOut := fan.Outputs[c]
		for d := 0; d < depth; d++ {
			tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: prev}},
				Outputs: []consensus.TxOut{k.out(prevOut.Amount - consensus.Amount(1000*(d+1)))}}
			signTx(t, k, tx, []consensus.TxOut{prevOut})
			if err := a.pool.Accept(tx); err != nil {
				t.Fatalf("chain %d depth %d: %v", c, d, err)
			}
			prev, prevOut = consensus.OutPoint{TxID: tx.TxID()}, tx.Outputs[0]
		}
	}
	b := newNode(t, false)
	if err := b.srv.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sync", 60*time.Second, func() bool { return b.ch.Tip().Hash == a.ch.Tip().Hash })
	waitFor(t, "every chained tx", 60*time.Second, func() bool { return b.pool.Count() == chains*depth })
}

// M2: a child that arrives before its parent waits in the orphan pool;
// M2/M1: a transaction is served once per announcement, only if announced,
// and "mempool" requests are rate-limited.
func TestTxRelayRules(t *testing.T) {
	a := newNode(t, true)
	k := newKey(t)
	for i := 0; i < 102; i++ {
		mine(t, a.ch, k, nil, 0)
	}
	r := newKey(t)

	t.Run("orphan", func(t *testing.T) {
		op, out := coinbaseCoin(t, a.ch, 1)
		parent := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}, Outputs: []consensus.TxOut{r.out(49 * consensus.UnitsPerCoin)}}
		signTx(t, k, parent, []consensus.TxOut{out})
		child := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: parent.TxID()}}}, Outputs: []consensus.TxOut{r.out(48 * consensus.UnitsPerCoin)}}
		signTx(t, r, child, parent.Outputs)
		ev := dialRaw(t, a.srv.ListenAddr())
		ev.handshake(a.ch.Tip().Height)
		ev.send(&MsgTx{Tx: child})
		waitFor(t, "child held as orphan", 5*time.Second, func() bool { return a.srv.orphans.count() == 1 })
		ev.send(&MsgTx{Tx: parent})
		waitFor(t, "parent and child in pool", 5*time.Second, func() bool { return a.pool.Has(parent.TxID()) && a.pool.Has(child.TxID()) })
		if a.srv.orphans.count() != 0 {
			t.Fatal("orphan left behind")
		}
	})

	t.Run("served-once", func(t *testing.T) {
		op, out := coinbaseCoin(t, a.ch, 2)
		tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}, Outputs: []consensus.TxOut{r.out(49 * consensus.UnitsPerCoin)}}
		signTx(t, k, tx, []consensus.TxOut{out})
		if err := a.pool.Accept(tx); err != nil {
			t.Fatal(err)
		}
		txid := tx.TxID()
		p := dialRaw(t, a.srv.ListenAddr())
		p.handshake(a.ch.Tip().Height)
		announces := func(m Message) bool {
			if inv, ok := m.(*MsgInv); ok {
				for _, it := range inv.Items {
					if it.Hash == txid {
						return true
					}
				}
			}
			return false
		}
		isTx := func(m Message) bool { mt, ok := m.(*MsgTx); return ok && mt.Tx.TxID() == txid }
		isNotFound := func(m Message) bool { _, ok := m.(*MsgNotFound); return ok }
		get := &MsgGetData{Items: []InvVect{{Type: InvTx, Hash: txid}}}
		// Never announced on this connection yet? It is: the connect dump.
		if p.expect(5*time.Second, announces) == nil {
			t.Fatal("mempool not announced on connect")
		}
		p.send(get)
		if p.expect(5*time.Second, isTx) == nil {
			t.Fatal("announced tx not served")
		}
		p.send(get)
		if p.expect(5*time.Second, isNotFound) == nil {
			t.Fatal("same tx served twice on one connection")
		}
		p.send(&MsgGetData{Items: []InvVect{{Type: InvTx, Hash: consensus.Hash{1, 2, 3}}}})
		if p.expect(5*time.Second, isNotFound) == nil {
			t.Fatal("unknown tx: no notfound")
		}
		// One "mempool" request re-announces (and re-arms) the pool...
		p.send(&MsgMempool{})
		if p.expect(5*time.Second, announces) == nil {
			t.Fatal("mempool request not answered")
		}
		p.send(get)
		if p.expect(5*time.Second, isTx) == nil {
			t.Fatal("re-announced tx not served")
		}
		// ...a second one within mempoolRequestGap is ignored.
		p.send(&MsgMempool{})
		if p.expect(time.Second, announces) != nil {
			t.Fatal("mempool request gap not applied")
		}
		p.send(get)
		if p.expect(5*time.Second, isNotFound) == nil {
			t.Fatal("served again without a new announcement")
		}
	})
}

// L1: a -connect peer given by host name gets exactly one connection.
func TestConnectByHostnameOnce(t *testing.T) {
	a := newNode(t, true)
	_, port, _ := net.SplitHostPort(a.srv.ListenAddr())
	b := newNodeWith(t, false, func(c *Config) { c.Connect = []string{"localhost:" + port} })
	time.Sleep(3*maintainEvery - 2*time.Second)
	out := 0
	for _, p := range b.srv.peerList() {
		if !p.inbound && !p.gone() {
			out++
		}
	}
	if out != 1 || a.srv.inboundCount() != 1 {
		t.Fatalf("B has %d outbound connections to the one -connect peer, A has %d inbound", out, a.srv.inboundCount())
	}
}

// L2: the self map is bounded and pruned; every connection has its own
// version nonce.
func TestSelfMapBoundedAndFreshNonces(t *testing.T) {
	a := newNode(t, true)
	for i := 0; i < 100; i++ {
		a.srv.markSelf(fmt.Sprintf("203.0.113.%d:9333", i))
	}
	a.srv.mu.Lock()
	n := len(a.srv.self)
	for k := range a.srv.self {
		a.srv.self[k] = time.Now().Add(-time.Second)
	}
	a.srv.mu.Unlock()
	if n > maxSelfAddrs {
		t.Fatalf("self map holds %d", n)
	}
	a.srv.pruneMaps(time.Now())
	a.srv.mu.Lock()
	n = len(a.srv.self)
	a.srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d expired self entries kept", n)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	p1, p2 := newPeer(a.srv, c1, false), newPeer(a.srv, c2, false)
	if a.srv.versionMsg(p1).Nonce == a.srv.versionMsg(p2).Nonce {
		t.Fatal("connections share a nonce")
	}
}

// testAddr is a routable address numbered i, vouched for at when.
func testAddr(i int, when time.Time) NetAddress {
	var a NetAddress
	copy(a.IP[:], net.IPv4(11, byte(i>>16), byte(i>>8), byte(i)).To16())
	a.Port = 9333
	a.Services = ServiceNetwork
	a.Time = uint64(when.Unix())
	return a
}

// Review D L4a: the address book used to refuse every new address once it
// held 10,000 and never forgot one. Gossiped addresses now expire, a full
// book makes room by dropping a gossiped, never-tried address (from the
// source holding the most), each source is capped, and operator-supplied
// and tried addresses are never displaced by gossip.
func TestAddrBookExpiryReplacementPerSource(t *testing.T) {
	now := time.Now()
	b := newAddrBook("", false)
	// Per-source cap.
	stored := 0
	for i := 0; i < maxAddrsPerSource+100; i++ {
		if b.addAt(testAddr(i, now), false, "198.51.100.1", now) {
			stored++
		}
	}
	if stored != maxAddrsPerSource {
		t.Fatalf("one source stored %d addresses, cap %d", stored, maxAddrsPerSource)
	}
	// Fill the book from many sources.
	n := stored
	for src := 0; n < maxAddrs; src++ {
		for j := 0; j < maxAddrsPerSource/2 && n < maxAddrs; j++ {
			if b.addAt(testAddr(100_000+n, now.Add(-time.Duration(src)*time.Hour)), false, fmt.Sprintf("203.0.113.%d", src), now) {
				n++
			}
		}
	}
	if b.count() != maxAddrs {
		t.Fatalf("book holds %d", b.count())
	}
	// Full: a newcomer from a new source still gets in, displacing an
	// address of the biggest source.
	if !b.addAt(testAddr(900_000, now), false, "192.0.2.77", now) {
		t.Fatal("VULNERABLE: a full address book refuses every new address")
	}
	b.mu.Lock()
	big := b.bySource["198.51.100.1"]
	b.mu.Unlock()
	if b.count() != maxAddrs || big != maxAddrsPerSource-1 {
		t.Fatalf("after replacement: %d entries, biggest source holds %d", b.count(), big)
	}
	// Stale gossip is refused, and expires.
	if b.addAt(testAddr(900_001, now.Add(-addrHorizon-time.Hour)), false, "192.0.2.78", now) {
		t.Fatal("stale address stored")
	}
	before := b.count()
	b.expire(now.Add(addrHorizon + 2*time.Hour))
	if b.count() != 0 {
		t.Fatalf("%d of %d gossiped addresses survived the horizon", b.count(), before)
	}
	// Operator-supplied and tried addresses do not expire and are not
	// displaced; a book full of them refuses gossip.
	b = newAddrBook("", false)
	for i := 0; i < maxAddrs; i++ {
		a := testAddr(i, now)
		if i%2 == 0 {
			b.addAt(a, true, "", now)
		} else {
			b.addAt(a, false, fmt.Sprintf("203.0.113.%d", i%200), now)
			b.tried(a.String())
		}
	}
	if b.addAt(testAddr(800_000, now), false, "192.0.2.1", now) {
		t.Fatal("gossip displaced an operator-supplied or tried address")
	}
	b.expire(now.Add(10 * addrHorizon))
	if b.count() != maxAddrs {
		t.Fatalf("operator-supplied or tried addresses expired: %d left", b.count())
	}
	// Timestamps from the future count as now.
	b = newAddrBook("", false)
	b.addAt(testAddr(1, now.Add(48*time.Hour)), false, "192.0.2.1", now)
	for _, e := range b.addrs {
		if e.addr.Time > uint64(now.Unix()) || e.seen.After(now) {
			t.Fatal("future timestamp kept")
		}
	}
}

// Gossip from one peer is capped by its source over the wire too.
func TestAddrMessagesCappedPerSource(t *testing.T) {
	a := newNodeWith(t, true, func(c *Config) { c.LocalAddrsAreHosts = true })
	r := dialRaw(t, a.srv.ListenAddr())
	r.handshake(0)
	now := time.Now()
	for i := 0; i < 2*maxAddrsPerSource; i += maxAddrsPerMsg {
		var msg MsgAddr
		for j := i; j < i+maxAddrsPerMsg; j++ {
			msg.Addrs = append(msg.Addrs, testAddr(j, now))
		}
		r.send(&msg)
	}
	r.send(&MsgPing{Nonce: 6})
	if r.expect(10*time.Second, func(m Message) bool { p, ok := m.(*MsgPong); return ok && p.Nonce == 6 }) == nil {
		t.Fatal("no pong")
	}
	if n := a.srv.addrs.count(); n != maxAddrsPerSource {
		t.Fatalf("one peer stored %d addresses, cap %d", n, maxAddrsPerSource)
	}
}
