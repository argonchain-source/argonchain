package p2p

import (
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"argonmine/consensus"
)

// Transaction relay: who we fetch announced transactions from, how we serve
// getdata without ever blocking a peer's reader, mempool announcements, and
// a small pool of transactions whose parent has not arrived yet.

const (
	maxGetDataTxs        = 500    // transaction items per getdata we send
	maxTxInFlightPerPeer = 1000   // transactions asked of one peer and not yet answered
	maxTxQueuePerPeer    = 20_000 // announced by one peer and not yet asked for
	maxTxAlternates      = 8      // other announcers remembered per transaction
	maxMempoolAnnounce   = 10_000 // transactions in one mempool announcement
	// The announcement order is recomputed whenever the pool changed, but
	// for a pool bigger than dumpCacheMinPool at most every dumpCacheFor.
	dumpCacheFor     = 2 * time.Second
	dumpCacheMinPool = 5_000

	maxOrphanTxs     = 100
	maxOrphanTxBytes = 5 << 20
	orphanTxMaxAge   = 5 * time.Minute
)

// txRequestTimeout is how long a transaction request may stay unanswered
// before it is asked of another announcer. A variable for tests.
var txRequestTimeout = 60 * time.Second

// txRequest is one transaction asked of one peer.
type txRequest struct {
	peer *Peer
	at   time.Time
	alts []*Peer // others that announced it, asked next
}

func (r *txRequest) addAlt(p *Peer) {
	if p == r.peer {
		return
	}
	for _, a := range r.alts {
		if a == p {
			return
		}
	}
	if len(r.alts) < maxTxAlternates {
		r.alts = append(r.alts, p)
	}
}

// catchingUp reports whether transaction announcements should be ignored
// for now: our tip is old and a sync peer is verifiably delivering headers
// we lacked. An unproven height claim never qualifies.
func (s *Server) catchingUp() bool {
	tip := s.ch.Tip()
	if time.Since(time.Unix(int64(tip.Header.Time), 0)) <= staleTipAge {
		return false
	}
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	return s.sync.peer != nil && s.sync.verified
}

// queueTxAnnouncements records transactions p announced and asks for them
// as its in-flight budget allows.
func (s *Server) queueTxAnnouncements(p *Peer, ids []consensus.Hash) {
	s.txMu.Lock()
	for _, h := range ids {
		if r, ok := s.txInFlight[h]; ok {
			r.addAlt(p)
			continue
		}
		if len(p.txQueue) >= maxTxQueuePerPeer {
			break
		}
		p.txQueue = append(p.txQueue, h)
	}
	s.txMu.Unlock()
	s.requestTxs(p)
}

// requestTxs asks p for queued announcements, at most maxGetDataTxs per
// getdata and maxTxInFlightPerPeer outstanding, never a transaction already
// asked of another peer.
func (s *Server) requestTxs(p *Peer) {
	if p.gone() {
		return
	}
	var items []InvVect
	now := time.Now()
	s.txMu.Lock()
	for len(p.txQueue) > 0 && len(p.txAsked) < maxTxInFlightPerPeer {
		h := p.txQueue[0]
		p.txQueue = p.txQueue[1:]
		if r, ok := s.txInFlight[h]; ok {
			r.addAlt(p)
			continue
		}
		if s.pool.Has(h) || s.pool.RecentlyRejected(h) || s.orphans.has(h) {
			continue
		}
		s.txInFlight[h] = &txRequest{peer: p, at: now}
		p.txAsked[h] = struct{}{}
		items = append(items, InvVect{Type: InvTx, Hash: h})
	}
	if len(p.txQueue) == 0 {
		p.txQueue = nil
	}
	s.txMu.Unlock()
	for i := 0; i < len(items); i += maxGetDataTxs {
		end := min(i+maxGetDataTxs, len(items))
		p.Send(&MsgGetData{Items: items[i:end]})
	}
}

// txAnswered clears p's request for h (a tx or notfound arrived) and
// returns the alternates to ask if the answer was useless.
func (s *Server) txAnswered(p *Peer, h consensus.Hash) []*Peer {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	delete(p.txAsked, h)
	r, ok := s.txInFlight[h]
	if !ok || r.peer != p {
		return nil
	}
	delete(s.txInFlight, h)
	return r.alts
}

// txDone forgets any request for h: we have it now.
func (s *Server) txDone(h consensus.Hash) *Peer {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	r, ok := s.txInFlight[h]
	if !ok {
		return nil
	}
	delete(s.txInFlight, h)
	delete(r.peer.txAsked, h)
	return r.peer
}

// retryTx asks the next connected alternate for h.
func (s *Server) retryTx(h consensus.Hash, alts []*Peer) {
	for len(alts) > 0 {
		next := alts[0]
		alts = alts[1:]
		if next.gone() {
			continue
		}
		if s.pool.Has(h) || s.pool.RecentlyRejected(h) || s.orphans.has(h) {
			return
		}
		s.txMu.Lock()
		if _, busy := s.txInFlight[h]; busy {
			s.txMu.Unlock()
			return
		}
		s.txInFlight[h] = &txRequest{peer: next, at: time.Now(), alts: alts}
		next.txAsked[h] = struct{}{}
		s.txMu.Unlock()
		next.Send(&MsgGetData{Items: []InvVect{{Type: InvTx, Hash: h}}})
		return
	}
}

// expireTxRequests moves requests that went unanswered for txRequestTimeout
// (or whose peer left) to the next announcer.
func (s *Server) expireTxRequests() {
	type retry struct {
		h    consensus.Hash
		alts []*Peer
	}
	var retries []retry
	touched := map[*Peer]struct{}{}
	now := time.Now()
	s.txMu.Lock()
	for h, r := range s.txInFlight {
		if r.peer.gone() || now.Sub(r.at) > txRequestTimeout {
			delete(s.txInFlight, h)
			delete(r.peer.txAsked, h)
			touched[r.peer] = struct{}{}
			retries = append(retries, retry{h, r.alts})
		}
	}
	s.txMu.Unlock()
	for _, rt := range retries {
		s.retryTx(rt.h, rt.alts)
	}
	for p := range touched {
		s.requestTxs(p)
	}
}

// peerGoneTx hands p's outstanding requests to their alternates.
func (s *Server) peerGoneTx(p *Peer) {
	type retry struct {
		h    consensus.Hash
		alts []*Peer
	}
	var retries []retry
	s.txMu.Lock()
	for h := range p.txAsked {
		if r, ok := s.txInFlight[h]; ok && r.peer == p {
			delete(s.txInFlight, h)
			retries = append(retries, retry{h, r.alts})
		}
	}
	p.txAsked = map[consensus.Hash]struct{}{}
	p.txQueue = nil
	s.txMu.Unlock()
	for _, rt := range retries {
		s.retryTx(rt.h, rt.alts)
	}
}

func (s *Server) onTx(p *Peer, tx *consensus.Tx) {
	txid := tx.TxID()
	p.markKnown(txid)
	alts := s.txAnswered(p, txid)
	err := s.pool.Accept(tx)
	switch {
	case err == nil:
		p.markUseful(false)
		s.txAccepted(tx, txid, p)
	case errors.Is(err, consensus.ErrTxMissingInput):
		s.orphans.add(tx, p)
	default:
		if errors.Is(err, consensus.ErrTxBadSignature) || errors.Is(err, consensus.ErrTxBadPubKey) {
			p.misbehave(10, "tx with invalid signature")
		}
		// A failure that does not condemn the txid (a bad witness, say) may
		// be this copy only: ask someone else who announced it.
		if !s.pool.Has(txid) && !s.pool.RecentlyRejected(txid) {
			s.retryTx(txid, alts)
		}
	}
	s.requestTxs(p)
}

// txAccepted relays a transaction that just entered the pool and admits
// orphans waiting for it.
func (s *Server) txAccepted(tx *consensus.Tx, txid consensus.Hash, from *Peer) {
	if other := s.txDone(txid); other != nil && other != from {
		s.requestTxs(other)
	}
	if s.OnTx != nil {
		s.OnTx(tx)
	}
	s.relayInv(InvVect{Type: InvTx, Hash: txid}, from)
	s.acceptOrphansOf(txid)
}

// acceptOrphansOf admits orphans that spend the outputs of parent, and
// recursively their own orphan children.
func (s *Server) acceptOrphansOf(parent consensus.Hash) {
	queue := []consensus.Hash{parent}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		for _, o := range s.orphans.takeChildren(h) {
			if s.acceptOrphan(o) {
				queue = append(queue, o.txid)
			}
		}
	}
}

// acceptOrphan tries one orphan again; true if it entered the pool.
func (s *Server) acceptOrphan(o *orphanTx) bool {
	err := s.pool.Accept(o.tx)
	if err == nil {
		if o.from != nil {
			o.from.markUseful(false)
		}
		if s.OnTx != nil {
			s.OnTx(o.tx)
		}
		s.txDone(o.txid)
		s.relayInv(InvVect{Type: InvTx, Hash: o.txid}, o.from)
		return true
	}
	if errors.Is(err, consensus.ErrTxMissingInput) {
		s.orphans.readd(o) // another parent is still missing
	}
	return false
}

// retryAllOrphans runs every orphan through the pool again: a new block may
// have confirmed the coins they spend (typical right after catching up).
func (s *Server) retryAllOrphans() {
	for _, o := range s.orphans.takeAll() {
		if s.acceptOrphan(o) {
			s.acceptOrphansOf(o.txid)
		}
	}
}

// BroadcastTx announces a locally created transaction (already in the pool).
func (s *Server) BroadcastTx(tx *consensus.Tx) {
	txid := tx.TxID()
	s.txDone(txid)
	s.relayInv(InvVect{Type: InvTx, Hash: txid}, nil)
	s.acceptOrphansOf(txid)
}

// ---- serving getdata ----

func (s *Server) onGetData(p *Peer, m *MsgGetData) {
	items := make([]InvVect, 0, len(m.Items))
	blocks := 0
	for _, it := range m.Items {
		if it.Type == InvBlock {
			if blocks >= maxBlocksPerGetData {
				continue // silently cap; a well-behaved peer asks for at most 16 at once
			}
			blocks++
		}
		items = append(items, it)
	}
	p.gdMu.Lock()
	room := max(maxGetDataQueue-len(p.gdQueue), 0)
	dropped := 0
	if len(items) > room {
		dropped = len(items) - room
		items = items[:room]
	}
	p.gdQueue = append(p.gdQueue, items...)
	p.gdMu.Unlock()
	if dropped > 0 {
		s.logf("peer %s: %d getdata items over the queue limit dropped", p.addr, dropped)
	}
	select {
	case p.gdWake <- struct{}{}:
	default:
	}
}

// respondLoop serves the peer's getdata requests in order, from its own
// goroutine, so that the reader never waits for the peer to drain its
// queue: two nodes asking each other for a lot cannot deadlock.
func (p *Peer) respondLoop() {
	s := p.srv
	for {
		select {
		case <-p.quit:
			return
		case <-p.gdWake:
		}
		for {
			p.gdMu.Lock()
			n := min(len(p.gdQueue), 64)
			if n == 0 {
				p.gdQueue = nil
				p.gdMu.Unlock()
				break
			}
			batch := append([]InvVect(nil), p.gdQueue[:n]...)
			p.gdQueue = p.gdQueue[n:]
			p.gdMu.Unlock()
			var missing []InvVect
			for _, it := range batch {
				if p.gone() {
					return
				}
				if !s.serveItem(p, it) {
					missing = append(missing, it)
				}
			}
			if len(missing) > 0 {
				p.sendReply(&MsgNotFound{Items: missing})
			}
		}
	}
}

// serveItem sends one requested item; false means "not found" (or not for
// this peer: a transaction we never announced to it, one already sent, or
// one over its mempool budget).
func (s *Server) serveItem(p *Peer, it InvVect) bool {
	switch it.Type {
	case InvBlock:
		raw, err := s.ch.RawBlock(it.Hash)
		if err != nil {
			return false
		}
		p.markKnown(it.Hash)
		p.sendReply(&MsgBlock{Raw: raw})
		return true
	case InvTx:
		dump, ok := p.takeAnnounced(it.Hash)
		if !ok {
			return false
		}
		tx, ok := s.pool.Get(it.Hash)
		if !ok {
			return false
		}
		if dump && !s.chargeDump(p, tx.SerializedSize()) {
			return false
		}
		p.sendReply(&MsgTx{Tx: tx})
		return true
	}
	return false
}

// ---- mempool announcements ----

// announceMempool announces our pending transactions to p: parents before
// children, higher fee rate first, at most maxMempoolAnnounce, sent in
// batches of maxInvBatch. Fetching them counts against p's dump budget.
func (s *Server) announceMempool(p *Peer) {
	for _, id := range s.mempoolOrder() {
		p.markAnnounced(id, true)
		p.queueInv(InvVect{Type: InvTx, Hash: id})
	}
}

// mempoolOrder is the pool's announcement order. It is cached while the
// pool is unchanged and, for a big pool, recomputed at most every
// dumpCacheFor, so that a stream of new connections cannot keep the pool
// busy sorting.
func (s *Server) mempoolOrder() []consensus.Hash {
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()
	gen := s.pool.Generation()
	fresh := s.dumpIDs != nil && (gen == s.dumpGen ||
		(time.Since(s.dumpAt) < dumpCacheFor && s.pool.Count() > dumpCacheMinPool))
	if !fresh {
		s.dumpIDs = s.pool.Ordered(maxMempoolAnnounce)
		s.dumpGen = gen
		s.dumpAt = time.Now()
	}
	return s.dumpIDs
}

// ---- orphan transactions ----

type orphanTx struct {
	tx    *consensus.Tx
	txid  consensus.Hash
	size  int
	from  *Peer
	added time.Time
}

// orphanTxPool holds a few transactions whose inputs are unknown, typically
// a child that arrived before its parent. Bounded in count and bytes;
// random eviction, so an attacker cannot reliably flush a chosen orphan.
type orphanTxPool struct {
	mu    sync.Mutex
	m     map[consensus.Hash]*orphanTx
	bytes int
}

func newOrphanTxPool() *orphanTxPool { return &orphanTxPool{m: map[consensus.Hash]*orphanTx{}} }

func (o *orphanTxPool) add(tx *consensus.Tx, from *Peer) {
	o.readd(&orphanTx{tx: tx, txid: tx.TxID(), size: tx.SerializedSize(), from: from, added: time.Now()})
}

// readd stores an orphan, keeping its original arrival time.
func (o *orphanTxPool) readd(t *orphanTx) {
	if t.size > consensus.MaxTxSize {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.m[t.txid]; ok {
		return
	}
	for len(o.m) > 0 && (len(o.m) >= maxOrphanTxs || o.bytes+t.size > maxOrphanTxBytes) {
		o.evictRandomLocked()
	}
	o.m[t.txid] = t
	o.bytes += t.size
}

func (o *orphanTxPool) evictRandomLocked() {
	i := rand.IntN(len(o.m))
	for k, t := range o.m {
		if i == 0 {
			delete(o.m, k)
			o.bytes -= t.size
			return
		}
		i--
	}
}

// forgetPeer drops references to a disconnected peer, so its state is not
// kept alive by the orphans it sent.
func (o *orphanTxPool) forgetPeer(p *Peer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, t := range o.m {
		if t.from == p {
			t.from = nil
		}
	}
}

func (o *orphanTxPool) has(h consensus.Hash) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.m[h]
	return ok
}

func (o *orphanTxPool) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.m)
}

// takeChildren removes and returns the orphans spending an output of parent.
func (o *orphanTxPool) takeChildren(parent consensus.Hash) []*orphanTx {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []*orphanTx
	for k, t := range o.m {
		for _, in := range t.tx.Inputs {
			if in.PrevOut.TxID == parent {
				out = append(out, t)
				delete(o.m, k)
				o.bytes -= t.size
				break
			}
		}
	}
	return out
}

func (o *orphanTxPool) takeAll() []*orphanTx {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]*orphanTx, 0, len(o.m))
	for _, t := range o.m {
		out = append(out, t)
	}
	o.m = map[consensus.Hash]*orphanTx{}
	o.bytes = 0
	return out
}

func (o *orphanTxPool) expire(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, t := range o.m {
		if now.Sub(t.added) > orphanTxMaxAge {
			delete(o.m, k)
			o.bytes -= t.size
		}
	}
}
