package p2p

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"argonmine/consensus"
)

const (
	handshakeTimeout = 30 * time.Second
	idleTimeout      = 5 * time.Minute
	pingInterval     = 2 * time.Minute
	sendQueueLen     = 256
	// maxQueuedBytes bounds the bytes waiting to be written to one peer; a
	// peer that stops reading cannot make us buffer unboundedly.
	maxQueuedBytes = 32 << 20
	knownInvCap    = 20_000
	banThreshold   = 100

	// respondByteBudget is how far the getdata responder may run ahead of
	// the writer, in queued bytes. It leaves most of maxQueuedBytes and half
	// the send queue to announcements and other replies.
	respondByteBudget = 4 << 20
	// maxGetDataQueue bounds the items a peer may have waiting to be served.
	maxGetDataQueue = MaxInvMsg + maxBlocksPerGetData
	// Transaction announcements are batched: at most maxInvBatch items every
	// invFlushInterval, at most maxInvPending waiting.
	maxInvBatch      = 1000
	invFlushInterval = 100 * time.Millisecond
	maxInvPending    = 50_000
)

// Flags kept per inventory hash and peer.
const (
	invKnown     uint8 = 1 << iota // the peer has it, or was told about it
	invAnnounced                   // we announced it to this peer: it may fetch it once
	invDump                        // ...as part of a mempool announcement (counts against the dump budget)
	invServed                      // we already sent it on this connection
)

// Peer is one connection.
type Peer struct {
	conn     net.Conn
	inbound  bool
	addr     string // remote address as connected
	dialAddr string // the address we dialed (outbound), possibly a host name
	host     string
	// unknownIP marks an inbound peer whose source address does not
	// identify it (behind a proxy, or a private/loopback/shared address):
	// it is never banned or capped by address; misbehaviour disconnects.
	unknownIP bool
	nonce     uint64 // our version nonce on this connection
	srv       *Server
	sendCh    chan queued
	quit      chan struct{}
	quitOnce  sync.Once
	id        uint64
	drained   chan struct{} // signalled by the writer after each message

	mu            sync.Mutex
	version       *MsgVersion
	verackRecv    bool
	handshakeDone bool
	banScore      int
	lastPingNonce uint64
	lastPingSent  time.Time
	pingRTT       time.Duration
	knownInv      map[consensus.Hash]uint8
	knownOrder    []consensus.Hash
	// syncUnreliable marks a peer whose claimed height was never
	// substantiated with headers; it is not chosen as a sync peer again.
	syncUnreliable bool
	stat           hostStat // mempool and dump accounting when unknownIP (guarded by srv.statMu)

	// Pending getdata items, served in order by respondLoop.
	gdMu    sync.Mutex
	gdQueue []InvVect
	gdWake  chan struct{}

	// Transaction announcements waiting for the next batch.
	invMu      sync.Mutex
	invPending []InvVect

	// Transactions this peer announced that we have not asked for yet, and
	// those we asked it for (guarded by srv.txMu).
	txQueue []consensus.Hash
	txAsked map[consensus.Hash]struct{}

	lastUseful  atomic.Int64 // unix nanoseconds of the last new block or transaction from it; 0 = never
	lastBlock   atomic.Int64 // unix nanoseconds of the last new block from it
	busySince   atomic.Int64 // unix nanoseconds since the reader has been handling a message; 0 = idle
	syncDefer   atomic.Int64 // unix nanoseconds before which it is not chosen as sync peer
	queuedBytes atomic.Int64
	lastRecv    atomic.Int64 // unix seconds
	bytesIn     atomic.Int64
	bytesOut    atomic.Int64
	connTime    time.Time
}

type queued struct {
	msg  Message
	size int64
}

var peerIDs atomic.Uint64

func newPeer(srv *Server, conn net.Conn, inbound bool) *Peer {
	addr := conn.RemoteAddr().String()
	host, _, _ := net.SplitHostPort(addr)
	var nb [8]byte
	rand.Read(nb[:])
	return &Peer{
		conn:     conn,
		inbound:  inbound,
		addr:     addr,
		host:     host,
		nonce:    binary.LittleEndian.Uint64(nb[:]),
		srv:      srv,
		sendCh:   make(chan queued, sendQueueLen),
		quit:     make(chan struct{}),
		drained:  make(chan struct{}, 1),
		gdWake:   make(chan struct{}, 1),
		id:       peerIDs.Add(1),
		knownInv: map[consensus.Hash]uint8{},
		txAsked:  map[consensus.Hash]struct{}{},
		connTime: time.Now(),
	}
}

// Addr is the remote address.
func (p *Peer) Addr() string { return p.addr }

// ID is a process-unique peer id.
func (p *Peer) ID() uint64 { return p.id }

// Inbound reports whether the peer connected to us.
func (p *Peer) Inbound() bool { return p.inbound }

// estimateSize is the queued-bytes accounting for a message before encoding.
func estimateSize(m Message) int64 {
	switch v := m.(type) {
	case *MsgBlock:
		if v.Raw != nil {
			return int64(len(v.Raw))
		}
		return int64(v.Block.SerializedSize())
	case *MsgTx:
		return int64(v.Tx.SerializedSize())
	case *MsgHeaders:
		return int64(len(v.Headers)) * consensus.HeaderSize
	case *MsgInv:
		return 9 + 33*int64(len(v.Items))
	case *MsgGetData:
		return 9 + 33*int64(len(v.Items))
	case *MsgNotFound:
		return 9 + 33*int64(len(v.Items))
	case *MsgAddr:
		return 9 + 34*int64(len(v.Addrs))
	default:
		return 256
	}
}

// Send queues a message without waiting. Drops the peer if its queue is
// full in messages or bytes (a slow or malicious reader). The reader
// goroutine only ever uses Send, so it never blocks on the peer.
func (p *Peer) Send(m Message) {
	size := estimateSize(m)
	if p.queuedBytes.Add(size) > maxQueuedBytes {
		p.srv.logf("peer %s: send queue over %d bytes, disconnecting", p.addr, maxQueuedBytes)
		p.Disconnect()
		return
	}
	select {
	case p.sendCh <- queued{m, size}:
	case <-p.quit:
	default:
		p.srv.logf("peer %s: send queue full, disconnecting", p.addr)
		p.Disconnect()
	}
}

// sendReply queues an answer to the peer's own request, waiting while the
// responder is respondByteBudget ahead of the writer or the send queue is
// half full. Only respondLoop calls it: a peer asking for a lot slows down
// its own answers and nothing else.
func (p *Peer) sendReply(m Message) {
	size := estimateSize(m)
	for {
		q := p.queuedBytes.Load()
		if (q == 0 || q+size <= respondByteBudget) && len(p.sendCh) < sendQueueLen/2 {
			break
		}
		select {
		case <-p.drained:
		case <-p.quit:
			return
		case <-time.After(time.Second):
		}
	}
	if p.queuedBytes.Add(size) > maxQueuedBytes {
		p.srv.logf("peer %s: send queue over %d bytes, disconnecting", p.addr, maxQueuedBytes)
		p.Disconnect()
		return
	}
	select {
	case p.sendCh <- queued{m, size}:
	case <-p.quit:
	}
}

// queueInv adds a transaction announcement to the next batch.
func (p *Peer) queueInv(it InvVect) {
	p.invMu.Lock()
	if len(p.invPending) < maxInvPending {
		p.invPending = append(p.invPending, it)
	}
	p.invMu.Unlock()
}

func (p *Peer) takeInvBatch() []InvVect {
	p.invMu.Lock()
	defer p.invMu.Unlock()
	n := len(p.invPending)
	if n == 0 {
		return nil
	}
	if n > maxInvBatch {
		n = maxInvBatch
	}
	batch := append([]InvVect(nil), p.invPending[:n]...)
	p.invPending = p.invPending[n:]
	if len(p.invPending) == 0 {
		p.invPending = nil
	}
	return batch
}

// Disconnect closes the connection; idempotent.
func (p *Peer) Disconnect() {
	p.quitOnce.Do(func() {
		close(p.quit)
		p.conn.Close()
	})
}

// Done returns a channel closed when the peer is gone.
func (p *Peer) Done() <-chan struct{} { return p.quit }

func (p *Peer) gone() bool {
	select {
	case <-p.quit:
		return true
	default:
		return false
	}
}

// setInvLocked ORs flags into the entry for h, adding it (and evicting the
// oldest entry past knownInvCap) if new.
func (p *Peer) setInvLocked(h consensus.Hash, flags uint8) {
	if f, ok := p.knownInv[h]; ok {
		p.knownInv[h] = f | flags
		return
	}
	p.knownInv[h] = flags
	p.knownOrder = append(p.knownOrder, h)
	if len(p.knownOrder) > knownInvCap {
		old := p.knownOrder[0]
		p.knownOrder = p.knownOrder[1:]
		delete(p.knownInv, old)
	}
}

// markKnown records that the peer has (or has been sent) an inventory item.
func (p *Peer) markKnown(h consensus.Hash) {
	p.mu.Lock()
	p.setInvLocked(h, invKnown)
	p.mu.Unlock()
}

// markAnnounced records that we announce transaction h to the peer, which
// may then fetch it once. A mempool announcement (dump) re-arms a
// transaction already served: the peer asked for the pending set again.
func (p *Peer) markAnnounced(h consensus.Hash, dump bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := invKnown | invAnnounced
	if dump {
		f |= invDump
		if cur, ok := p.knownInv[h]; ok {
			p.knownInv[h] = cur &^ invServed
		}
	}
	p.setInvLocked(h, f)
}

// takeAnnounced reports whether transaction h may be served to the peer now
// (we announced it and have not sent it yet) and marks it served. dump
// tells whether it was announced as part of a mempool dump.
func (p *Peer) takeAnnounced(h consensus.Hash) (dump, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.knownInv[h]
	if f&invAnnounced == 0 || f&invServed != 0 {
		return false, false
	}
	p.knownInv[h] = f | invServed
	return f&invDump != 0, true
}

func (p *Peer) knows(h consensus.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.knownInv[h] != 0
}

// markUseful records that the peer just gave us something new.
func (p *Peer) markUseful(block bool) {
	now := time.Now().UnixNano()
	p.lastUseful.Store(now)
	if block {
		p.lastBlock.Store(now)
	}
}

// misbehave adds to the ban score and disconnects (and bans, if its address
// identifies it) at threshold.
func (p *Peer) misbehave(points int, why string) {
	p.mu.Lock()
	p.banScore += points
	score := p.banScore
	p.mu.Unlock()
	p.srv.logf("peer %s misbehaving (+%d=%d): %s", p.addr, points, score, why)
	if score >= banThreshold {
		p.srv.ban(p)
		p.Disconnect()
	}
}

func (p *Peer) setUnreliable() {
	p.mu.Lock()
	p.syncUnreliable = true
	p.mu.Unlock()
}

func (p *Peer) unreliable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.syncUnreliable
}

// deferSync keeps p from being chosen as sync peer for d (it did nothing
// wrong; see abandonSyncLocked).
func (p *Peer) deferSync(d time.Duration) { p.syncDefer.Store(time.Now().Add(d).UnixNano()) }

func (p *Peer) syncDeferred() bool { return time.Now().UnixNano() < p.syncDefer.Load() }

// busy reports whether this node is still handling a message from p.
func (p *Peer) busy() bool { return p.busySince.Load() != 0 }

func (p *Peer) ping() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pingRTT
}

// Version returns the peer's version message (nil before handshake).
func (p *Peer) Version() *MsgVersion {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.version
}

// StartHeight is the height the peer announced at handshake (unverified).
func (p *Peer) StartHeight() uint32 {
	if v := p.Version(); v != nil {
		return v.StartHeight
	}
	return 0
}

func (p *Peer) run() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.writeLoop()
	}()
	go func() {
		defer wg.Done()
		p.respondLoop()
	}()
	if !p.inbound {
		p.Send(p.srv.versionMsg(p))
	}
	p.readLoop()
	wg.Wait()
}

// countingReader tallies bytes read from the connection.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func (p *Peer) readLoop() {
	defer func() {
		p.Disconnect()
		p.srv.removePeer(p)
	}()
	r := bufio.NewReaderSize(&countingReader{r: p.conn, n: &p.bytesIn}, 64<<10)
	for {
		timeout := idleTimeout
		if !p.isHandshakeDone() {
			timeout = handshakeTimeout
		}
		p.conn.SetReadDeadline(time.Now().Add(timeout))
		msg, err := ReadMessage(r, p.srv.cfg.Params.Magic)
		if errors.Is(err, ErrBadCommand) {
			continue // unknown message type from a newer peer: skip it
		}
		if err != nil {
			if errors.Is(err, ErrBadMagic) || errors.Is(err, ErrBadChecksum) || errors.Is(err, ErrMsgMalformed) || errors.Is(err, ErrPayloadSize) {
				p.misbehave(banThreshold, err.Error())
			}
			return
		}
		p.lastRecv.Store(time.Now().Unix())
		if !p.isHandshakeDone() {
			switch msg.(type) {
			case *MsgVersion, *MsgVerack:
			default:
				p.misbehave(banThreshold, "message before handshake")
				return
			}
		}
		start := time.Now()
		p.busySince.Store(start.UnixNano())
		p.srv.handleMessage(p, msg)
		p.busySince.Store(0)
		if d := time.Since(start); d >= busyCreditMin {
			p.srv.creditSyncBusy(p, d)
		}
		if p.gone() {
			return
		}
	}
}

func (p *Peer) writeLoop() {
	w := bufio.NewWriterSize(p.conn, 64<<10)
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	invTicker := time.NewTicker(invFlushInterval)
	defer invTicker.Stop()
	write := func(m Message) bool {
		p.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		b, err := EncodeMessage(p.srv.cfg.Params.Magic, m)
		if err != nil {
			p.srv.logf("peer %s: encode %s: %v", p.addr, m.Command(), err)
			return true
		}
		if _, err := w.Write(b); err != nil {
			p.Disconnect()
			return false
		}
		p.bytesOut.Add(int64(len(b)))
		return true
	}
	flush := func() bool {
		if err := w.Flush(); err != nil {
			p.Disconnect()
			return false
		}
		return true
	}
	for {
		select {
		case <-p.quit:
			return
		case q := <-p.sendCh:
			ok := write(q.msg)
			p.queuedBytes.Add(-q.size)
			select {
			case p.drained <- struct{}{}:
			default:
			}
			if !ok {
				return
			}
			if len(p.sendCh) == 0 && !flush() {
				return
			}
		case <-invTicker.C:
			if !p.isHandshakeDone() {
				continue
			}
			if batch := p.takeInvBatch(); batch != nil {
				if !write(&MsgInv{Items: batch}) || !flush() {
					return
				}
			}
		case <-ticker.C:
			if p.isHandshakeDone() {
				nonce := uint64(time.Now().UnixNano())
				p.mu.Lock()
				p.lastPingNonce = nonce
				p.lastPingSent = time.Now()
				p.mu.Unlock()
				select {
				case p.sendCh <- queued{&MsgPing{Nonce: nonce}, 64}:
					p.queuedBytes.Add(64)
				default:
				}
			}
		}
	}
}

func (p *Peer) isHandshakeDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handshakeDone
}

// Info is a snapshot for RPC.
type Info struct {
	ID          uint64
	Addr        string
	Inbound     bool
	UserAgent   string
	Version     uint32
	StartHeight uint32
	BanScore    int
	PingMs      int64
	ConnTime    time.Time
	BytesIn     int64
	BytesOut    int64
}

func (p *Peer) info() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	in := Info{ID: p.id, Addr: p.addr, Inbound: p.inbound, BanScore: p.banScore, PingMs: p.pingRTT.Milliseconds(),
		ConnTime: p.connTime, BytesIn: p.bytesIn.Load(), BytesOut: p.bytesOut.Load()}
	if p.version != nil {
		in.UserAgent = p.version.UserAgent
		in.Version = p.version.Version
		in.StartHeight = p.version.StartHeight
	}
	return in
}
