package p2p

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
)

type node struct {
	ch   *chain.Chain
	pool *mempool.Pool
	srv  *Server
}

func newNode(t *testing.T, listen bool) *node {
	t.Helper()
	return newNodeWith(t, listen, nil)
}

// newNodeWith is newNode with a config hook applied before the server starts.
func newNodeWith(t *testing.T, listen bool, mod func(*Config)) *node {
	t.Helper()
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := mempool.New(ch, mempool.DefaultConfig())
	cfg := Config{Params: consensus.RegTest, MaxOutbound: 2, Logf: t.Logf}
	if listen {
		cfg.Listen = "127.0.0.1:0"
	}
	if mod != nil {
		mod(&cfg)
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

type key struct {
	priv  *btcec.PrivateKey
	xonly []byte
}

func newKey(t *testing.T) *key {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &key{priv: priv, xonly: schnorr.SerializePubKey(priv.PubKey())}
}

func (k *key) out(a consensus.Amount) consensus.TxOut {
	return consensus.TxOut{Amount: a, Type: consensus.OutputTypeP2PK, Payload: append([]byte{}, k.xonly...)}
}

func mine(t *testing.T, ch *chain.Chain, k *key, txs []*consensus.Tx, fees consensus.Amount) *consensus.Block {
	t.Helper()
	parent := ch.Tip()
	h := parent.Height + 1
	cb := &consensus.Tx{Version: 1, LockHeight: h, Inputs: []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs: []consensus.TxOut{k.out(consensus.Subsidy(h) + fees)}, Witnesses: [][]byte{{byte(h)}}}
	blk := &consensus.Block{Txs: append([]*consensus.Tx{cb}, txs...)}
	txRoot, wRoot, _ := blk.ComputeRoots()
	now := uint64(time.Now().Unix())
	tm := parent.Header.Time + 1
	if now > tm {
		tm = now
	}
	blk.Header = consensus.Header{Version: 1, PrevHash: parent.Hash, TxRoot: txRoot, WitnessRoot: wRoot,
		Time: tm, Bits: consensus.NextBits(consensus.RegTest, parent.Header.Time, parent.Height)}
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, consensus.RegTest.PowLimit) == nil {
			break
		}
	}
	if _, err := ch.ProcessBlock(blk, -1); err != nil {
		t.Fatal(err)
	}
	return blk
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestWireRoundTrip(t *testing.T) {
	magic := consensus.RegTest.Magic
	k := newKey(t)
	blk := consensus.RegTest.Genesis
	msgs := []Message{
		&MsgVersion{Version: 1, Services: 1, Timestamp: 123, Nonce: 99, UserAgent: "/t/", StartHeight: 7, Genesis: consensus.RegTest.GenesisHash},
		&MsgVerack{},
		&MsgPing{Nonce: 5}, &MsgPong{Nonce: 5},
		&MsgGetHeaders{Locator: []consensus.Hash{blk.Header.Hash()}},
		&MsgHeaders{Headers: []consensus.Header{blk.Header, blk.Header}},
		&MsgInv{Items: []InvVect{{InvTx, consensus.Hash{1}}, {InvBlock, consensus.Hash{2}}}},
		&MsgGetData{Items: []InvVect{{InvBlock, consensus.Hash{3}}}},
		&MsgNotFound{Items: []InvVect{{InvTx, consensus.Hash{4}}}},
		&MsgBlock{Block: blk},
		&MsgTx{Tx: &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{}}, Outputs: []consensus.TxOut{k.out(1)}, Witnesses: [][]byte{make([]byte, 64)}}},
		&MsgGetAddr{},
		&MsgAddr{Addrs: []NetAddress{{Time: 1, Services: 1, Port: 9333}}},
		&MsgReject{Cmd: "block", Reason: "bad", Hash: consensus.Hash{9}},
	}
	for _, m := range msgs {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, magic, m); err != nil {
			t.Fatalf("%s: %v", m.Command(), err)
		}
		back, err := ReadMessage(&buf, magic)
		if err != nil {
			t.Fatalf("%s: %v", m.Command(), err)
		}
		a, _ := EncodeMessage(magic, m)
		b, _ := EncodeMessage(magic, back)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: round trip differs", m.Command())
		}
	}
	// Wrong magic, bad checksum, oversize.
	raw, _ := EncodeMessage(magic, &MsgPing{Nonce: 1})
	if _, err := ReadMessage(bytes.NewReader(raw), consensus.MainNet.Magic); !errors.Is(err, ErrBadMagic) {
		t.Fatal("magic")
	}
	raw[len(raw)-1] ^= 1
	if _, err := ReadMessage(bytes.NewReader(raw), magic); !errors.Is(err, ErrBadChecksum) {
		t.Fatal("checksum")
	}
	huge := append([]byte{}, magic[:]...)
	huge = append(huge, make([]byte, 12)...)
	huge = append(huge, 0xff, 0xff, 0xff, 0x7f, 0, 0, 0, 0)
	if _, err := ReadMessage(bytes.NewReader(huge), magic); !errors.Is(err, ErrPayloadSize) {
		t.Fatal("size")
	}
}

func TestSyncRelayAndTxPropagation(t *testing.T) {
	a := newNode(t, true)
	k := newKey(t)
	for i := 0; i < 105; i++ {
		mine(t, a.ch, k, nil, 0)
	}
	first, _ := a.ch.NodeAtHeight(1)
	firstBlk, _ := a.ch.Block(first.Hash)

	// B starts empty and syncs headers-first from A.
	b := newNode(t, false)
	if err := b.srv.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initial sync", 60*time.Second, func() bool { return b.ch.Tip().Height == 105 })
	if b.ch.Tip().Hash != a.ch.Tip().Hash {
		t.Fatal("tips differ")
	}
	waitFor(t, "sync to finish", 10*time.Second, func() bool { return !b.srv.Syncing() })

	// A mines a new block: relayed by inv to B.
	blk := mine(t, a.ch, k, nil, 0)
	waitFor(t, "block relay", 10*time.Second, func() bool { return b.ch.Tip().Hash == blk.Header.Hash() })

	// B mines a block (its node is behind no one): relayed to A.
	blk2 := mine(t, b.ch, k, nil, 0)
	waitFor(t, "reverse block relay", 10*time.Second, func() bool { return a.ch.Tip().Hash == blk2.Header.Hash() })

	// A tx submitted to B propagates to A's mempool.
	op := consensus.OutPoint{TxID: firstBlk.Txs[0].TxID(), Index: 0}
	coin, ok := b.ch.LookupCoin(op)
	if !ok {
		t.Fatal("coin missing on B")
	}
	r := newKey(t)
	tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}, Outputs: []consensus.TxOut{r.out(49 * consensus.UnitsPerCoin)}, Witnesses: make([][]byte, 1)}
	msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, []consensus.TxOut{coin.Out}, 0)
	sig, _ := schnorr.Sign(k.priv, msg[:])
	tx.Witnesses[0] = sig.Serialize()
	if err := b.pool.Accept(tx); err != nil {
		t.Fatal(err)
	}
	b.srv.BroadcastTx(tx)
	waitFor(t, "tx relay", 10*time.Second, func() bool { return a.pool.Has(tx.TxID()) })

	// A mines it; B's mempool drops it after the block arrives.
	sel, fees := a.pool.Select(consensus.MaxBlockSize)
	blk3 := mine(t, a.ch, k, sel, fees)
	waitFor(t, "block with tx", 10*time.Second, func() bool { return b.ch.Tip().Hash == blk3.Header.Hash() })
	waitFor(t, "mempool clear", 5*time.Second, func() bool { return b.pool.Count() == 0 })

	if len(a.srv.Peers()) != 1 || len(b.srv.Peers()) != 1 {
		t.Fatal("peer counts")
	}
}

// rawPeer speaks the wire protocol directly, to impersonate misbehaving peers.
type rawPeer struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *rawPeer {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &rawPeer{t: t, conn: conn, r: bufio.NewReader(conn)}
}

func (r *rawPeer) send(m Message) {
	r.t.Helper()
	if err := WriteMessage(r.conn, consensus.RegTest.Magic, m); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rawPeer) read(timeout time.Duration) Message {
	r.conn.SetReadDeadline(time.Now().Add(timeout))
	m, err := ReadMessage(r.r, consensus.RegTest.Magic)
	if err != nil {
		return nil
	}
	return m
}

// handshake performs the version exchange claiming the given height.
func (r *rawPeer) handshake(claimedHeight uint32) {
	r.t.Helper()
	r.send(&MsgVersion{Version: ProtocolVersion, Services: 1, Timestamp: time.Now().Unix(), Nonce: 42,
		UserAgent: "/evil/", StartHeight: claimedHeight, Genesis: consensus.RegTest.GenesisHash})
	gotVersion, gotVerack := false, false
	for !(gotVersion && gotVerack) {
		switch r.read(5 * time.Second).(type) {
		case *MsgVersion:
			gotVersion = true
			r.send(&MsgVerack{})
		case *MsgVerack:
			gotVerack = true
		case nil:
			r.t.Fatal("handshake timed out")
		}
	}
}

// A peer that claims a huge height but never delivers headers must not be
// able to keep the node in "syncing" forever or block announcements from
// honest peers (eclipse resistance).
func TestUnsubstantiatedHeightClaimDoesNotEclipse(t *testing.T) {
	a := newNode(t, true)
	k := newKey(t)
	mine(t, a.ch, k, nil, 0)

	evil := dialRaw(t, a.srv.ListenAddr())
	evil.handshake(1 << 31)
	// Serve every getheaders with an empty answer, forever, in the background.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			switch m := evil.read(500 * time.Millisecond).(type) {
			case *MsgGetHeaders:
				evil.send(&MsgHeaders{})
			case *MsgPing:
				evil.send(&MsgPong{Nonce: m.Nonce})
			case *MsgGetAddr:
				evil.send(&MsgAddr{})
			}
		}
	}()
	// The node may briefly pick the liar as sync peer; it must drop the claim.
	waitFor(t, "liar dismissed", 20*time.Second, func() bool { return !a.srv.Syncing() })

	// Meanwhile an honest peer's new block must still be accepted.
	b := newNode(t, false)
	if err := b.srv.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b synced", 20*time.Second, func() bool { return b.ch.Tip().Height == 1 })
	blk := mine(t, b.ch, k, nil, 0)
	waitFor(t, "honest block accepted despite liar", 20*time.Second, func() bool { return a.ch.Tip().Hash == blk.Header.Hash() })
	// The liar is never chosen as sync peer again.
	a.srv.startSync()
	if a.srv.Syncing() {
		t.Fatal("unreliable peer re-selected for sync")
	}
}

// With LocalAddrsAreHosts a loopback source identifies the peer, so the
// per-address cap applies (by default it is a shared, unknown address).
func TestPerIPInboundLimit(t *testing.T) {
	a := newNodeWith(t, true, func(c *Config) { c.LocalAddrsAreHosts = true })
	var conns []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", a.srv.ListenAddr())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		time.Sleep(50 * time.Millisecond)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	waitFor(t, "connections settle", 5*time.Second, func() bool { return a.srv.PeerCount() <= 3 })
	closed := 0
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil && !isTimeout(err) {
			closed++
		}
	}
	if closed < 2 {
		t.Fatalf("expected at least 2 of 5 same-IP connections refused, got %d", closed)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// Unknown message types are skipped, not punished (forward compatibility).
func TestUnknownCommandIgnored(t *testing.T) {
	a := newNode(t, true)
	p := dialRaw(t, a.srv.ListenAddr())
	p.handshake(0)
	// Hand-build a frame with an unknown command and empty payload.
	frame := append([]byte{}, consensus.RegTest.Magic[:]...)
	cmd := make([]byte, 12)
	copy(cmd, "futurething")
	frame = append(frame, cmd...)
	frame = append(frame, 0, 0, 0, 0)
	ck := checksum(nil)
	frame = append(frame, ck[:]...)
	if _, err := p.conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	p.send(&MsgPing{Nonce: 7})
	waitFor(t, "pong after unknown command", 5*time.Second, func() bool {
		m, ok := p.read(time.Second).(*MsgPong)
		return ok && m.Nonce == 7
	})
	if a.srv.PeerCount() != 1 {
		t.Fatal("peer was dropped for an unknown command")
	}
}

func TestWrongChainRejected(t *testing.T) {
	a := newNode(t, true)
	// A peer on a different genesis is disconnected during handshake.
	chB, err := chain.Open(consensus.TestNet, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer chB.Close()
	poolB := mempool.New(chB, mempool.DefaultConfig())
	// Same magic is needed to even parse frames: pretend testnet uses regtest's magic.
	params := *consensus.TestNet
	params.Magic = consensus.RegTest.Magic
	srvB := New(Config{Params: &params, Logf: t.Logf}, chB, poolB)
	if err := srvB.Start(); err != nil {
		t.Fatal(err)
	}
	defer srvB.Stop()
	if err := srvB.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "disconnect", 10*time.Second, func() bool { return a.srv.PeerCount() == 0 && srvB.PeerCount() == 0 })
}

func TestSelfConnectionDropped(t *testing.T) {
	a := newNode(t, true)
	if err := a.srv.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "self disconnect", 10*time.Second, func() bool { return a.srv.PeerCount() == 0 })
	if !a.srv.isSelf(a.srv.ListenAddr()) {
		t.Fatal("own address not remembered")
	}
	if err := a.srv.Connect(a.srv.ListenAddr()); err == nil {
		t.Fatal("dialed itself again")
	}
}

func TestInvalidBlockBansPeer(t *testing.T) {
	a := newNodeWith(t, true, func(c *Config) { c.LocalAddrsAreHosts = true })
	b := newNode(t, false)
	k := newKey(t)
	mine(t, a.ch, k, nil, 0)
	if err := b.srv.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sync", 20*time.Second, func() bool { return b.ch.Tip().Height == 1 && !b.srv.Syncing() })

	// B pushes a block with an overpaying coinbase to A.
	parent := b.ch.Tip()
	h := parent.Height + 1
	cb := &consensus.Tx{Version: 1, LockHeight: h, Inputs: []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs: []consensus.TxOut{k.out(consensus.Subsidy(h) + 1)}, Witnesses: [][]byte{{}}}
	blk := &consensus.Block{Txs: []*consensus.Tx{cb}}
	txRoot, wRoot, _ := blk.ComputeRoots()
	blk.Header = consensus.Header{Version: 1, PrevHash: parent.Hash, TxRoot: txRoot, WitnessRoot: wRoot,
		Time: uint64(time.Now().Unix()), Bits: consensus.RegTest.PowLimitBits}
	for n := uint64(0); ; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, consensus.RegTest.PowLimit) == nil {
			break
		}
	}
	for _, p := range b.srv.peerList() {
		p.Send(&MsgBlock{Block: blk})
	}
	waitFor(t, "ban", 10*time.Second, func() bool { return a.srv.PeerCount() == 0 })
	if !a.srv.isBanned("127.0.0.1") {
		t.Fatal("peer not banned")
	}
	if a.ch.Tip().Height != 1 {
		t.Fatal("invalid block accepted")
	}
}

// TestLargeMempoolAnnouncedOnConnect: a node with more pending transactions
// than a peer's send queue holds announces them all on connect; the peer
// fetches them all and neither side drops the other for a full queue.
func TestLargeMempoolAnnouncedOnConnect(t *testing.T) {
	const n = 400 // more than sendQueueLen
	a := newNode(t, true)
	k := newKey(t)
	for i := 0; i < 101; i++ {
		mine(t, a.ch, k, nil, 0)
	}
	first, _ := a.ch.NodeAtHeight(1)
	firstBlk, _ := a.ch.Block(first.Hash)
	sign := func(tx *consensus.Tx, spent []consensus.TxOut) {
		for i := range tx.Inputs {
			msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, spent, uint32(i))
			sig, err := schnorr.Sign(k.priv, msg[:])
			if err != nil {
				t.Fatal(err)
			}
			tx.Witnesses[i] = sig.Serialize()
		}
	}
	// Split the first coinbase into n coins, confirmed in a block.
	op := consensus.OutPoint{TxID: firstBlk.Txs[0].TxID(), Index: 0}
	coin, ok := a.ch.LookupCoin(op)
	if !ok {
		t.Fatal("coinbase missing")
	}
	const each = consensus.UnitsPerCoin / 10
	fan := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}, Witnesses: make([][]byte, 1)}
	for i := 0; i < n; i++ {
		fan.Outputs = append(fan.Outputs, k.out(each))
	}
	sign(fan, []consensus.TxOut{coin.Out})
	mine(t, a.ch, k, []*consensus.Tx{fan}, coin.Out.Amount-n*each)
	// n independent spends wait in A's mempool.
	for i := 0; i < n; i++ {
		in := consensus.OutPoint{TxID: fan.TxID(), Index: uint32(i)}
		tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: in}}, Outputs: []consensus.TxOut{k.out(each - 10_000)}, Witnesses: make([][]byte, 1)}
		sign(tx, []consensus.TxOut{k.out(each)})
		if err := a.pool.Accept(tx); err != nil {
			t.Fatalf("tx %d: %v", i, err)
		}
	}
	if a.pool.Count() != n {
		t.Fatalf("A holds %d transactions", a.pool.Count())
	}
	b := newNode(t, false)
	if err := b.srv.Connect(a.srv.ListenAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sync", 60*time.Second, func() bool { return b.ch.Tip().Hash == a.ch.Tip().Hash })
	waitFor(t, "mempool on connect", 30*time.Second, func() bool { return b.pool.Count() == n })
	if len(a.srv.Peers()) != 1 || len(b.srv.Peers()) != 1 {
		t.Fatalf("peers after the transfer: A %d, B %d", len(a.srv.Peers()), len(b.srv.Peers()))
	}
}
