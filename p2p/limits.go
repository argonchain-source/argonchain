package p2p

import (
	"net"
	"sort"
	"sync"
	"time"
)

// Inbound connection limits, sync-peer reliability memory, and the
// per-host accounting that keeps one source from making us upload our
// mempool over and over. See docs/NODE.md, "Connection limits".

const (
	// Accepted inbound connections per second (sustained) and burst.
	acceptRate  = 10
	acceptBurst = 20
	// When inbound slots are full a newcomer evicts an existing inbound
	// peer; these many with the best ping and these many that most recently
	// gave us a new block are never evicted.
	protectByPing   = 4
	protectByBlocks = 4

	// A sync peer must answer its first getheaders within this time.
	firstHeadersTimeout = 15 * time.Second
	// A host whose height claim proved empty is not chosen to sync from
	// again for this long.
	unreliableFor      = time.Hour
	maxUnreliableHosts = 1024

	maxSelfAddrs = 32
	selfFor      = 24 * time.Hour
	maxBanned    = 10_000

	// Mempool announcements (dumps): a host (or, for peers whose address is
	// unknown, one connection) may ask with "mempool" once per
	// mempoolRequestGap, and may fetch at most dumpBudget bytes of dumped
	// transactions per dumpWindow. All unknown-address peers together share
	// unknownDumpBudget and unknownMempoolBurst/Every requests.
	mempoolRequestGap   = 2 * time.Minute
	dumpWindow          = 10 * time.Minute
	dumpBudget          = 64 << 20
	unknownDumpBudget   = 256 << 20
	unknownMempoolBurst = 10
	unknownMempoolEvery = 10 * time.Second
	maxHostStats        = 10_000
)

// tokenBucket is a classic rate limiter.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

func (b *tokenBucket) allowAt(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (b *tokenBucket) allow() bool { return b.allowAt(time.Now()) }

// sharedNet is 100.64.0.0/10, the carrier-grade NAT range.
var sharedNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// localSource reports whether ip is an address many peers may share:
// loopback, private, link-local, carrier-grade NAT, or unparseable.
func localSource(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || sharedNet.Contains(ip)
}

// unknownSource reports whether an inbound connection from host says
// nothing about who the peer is, so it must not be banned or capped by it.
func (s *Server) unknownSource(host string) bool {
	if s.cfg.BehindProxy {
		return true
	}
	if s.cfg.LocalAddrsAreHosts {
		return false
	}
	return localSource(net.ParseIP(host))
}

// evictInbound disconnects one inbound peer to make room for a newcomer.
// Candidates exclude the sync peer and the few best by ping and by recent
// blocks; among the rest, one that never gave us anything new goes first,
// then the one least recently useful, ties to the longest connected.
// Returns false if every inbound peer is protected.
func (s *Server) evictInbound() bool {
	s.sync.mu.Lock()
	syncPeer := s.sync.peer
	s.sync.mu.Unlock()
	var cands []*Peer
	for _, p := range s.peerList() {
		if p.inbound && !p.gone() && p != syncPeer {
			cands = append(cands, p)
		}
	}
	protected := map[*Peer]bool{}
	var pinged []*Peer
	for _, p := range cands {
		if p.ping() > 0 {
			pinged = append(pinged, p)
		}
	}
	sort.Slice(pinged, func(i, j int) bool { return pinged[i].ping() < pinged[j].ping() })
	for i := 0; i < len(pinged) && i < protectByPing; i++ {
		protected[pinged[i]] = true
	}
	var blockers []*Peer
	for _, p := range cands {
		if p.lastBlock.Load() > 0 && !protected[p] {
			blockers = append(blockers, p)
		}
	}
	sort.Slice(blockers, func(i, j int) bool { return blockers[i].lastBlock.Load() > blockers[j].lastBlock.Load() })
	for i := 0; i < len(blockers) && i < protectByBlocks; i++ {
		protected[blockers[i]] = true
	}
	var victim *Peer
	for _, p := range cands {
		if protected[p] {
			continue
		}
		if victim == nil || evictFirst(p, victim) {
			victim = p
		}
	}
	if victim == nil {
		return false
	}
	s.logf("p2p: inbound slots full; evicting %s (connected %s ago)", victim.addr, time.Since(victim.connTime).Round(time.Second))
	victim.Disconnect()
	return true
}

func evictFirst(a, b *Peer) bool {
	ua, ub := a.lastUseful.Load(), b.lastUseful.Load()
	if ua != ub {
		return ua < ub // never useful (0) first, then least recently useful
	}
	return a.connTime.Before(b.connTime)
}

// reliabilityKey names what a failed height claim is remembered against:
// the dialed endpoint for outbound peers, the host for inbound peers whose
// address identifies them, nothing (this connection only) otherwise.
func reliabilityKey(p *Peer) string {
	switch {
	case !p.inbound:
		return p.addr
	case p.unknownIP:
		return ""
	default:
		return p.host
	}
}

// markUnreliable stops p, and for unreliableFor its host, from being chosen
// as a sync peer.
func (s *Server) markUnreliable(p *Peer) {
	p.setUnreliable()
	key := reliabilityKey(p)
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.unreliable) >= maxUnreliableHosts {
		pruneExpired(s.unreliable, now)
		evictSoonest(s.unreliable, maxUnreliableHosts-1)
	}
	s.unreliable[key] = now.Add(unreliableFor)
}

func (s *Server) isUnreliable(p *Peer) bool {
	if p.unreliable() {
		return true
	}
	key := reliabilityKey(p)
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.unreliable[key]
	return ok && time.Now().Before(until)
}

// markSelf remembers addresses that turned out to be this node.
func (s *Server) markSelf(addrs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, a := range addrs {
		if a == "" {
			continue
		}
		if _, ok := s.self[a]; !ok && len(s.self) >= maxSelfAddrs {
			pruneExpired(s.self, now)
			evictSoonest(s.self, maxSelfAddrs-1)
		}
		s.self[a] = now.Add(selfFor)
	}
}

// pruneMaps drops expired entries from the self, ban and reliability maps
// and stale host statistics.
func (s *Server) pruneMaps(now time.Time) {
	s.mu.Lock()
	pruneExpired(s.self, now)
	pruneExpired(s.banned, now)
	pruneExpired(s.unreliable, now)
	s.mu.Unlock()
	s.statMu.Lock()
	for k, st := range s.hostStats {
		if now.Sub(st.seen) > dumpWindow {
			delete(s.hostStats, k)
		}
	}
	s.statMu.Unlock()
}

func pruneExpired(m map[string]time.Time, now time.Time) {
	for k, until := range m {
		if now.After(until) {
			delete(m, k)
		}
	}
}

// evictSoonest removes the entries closest to expiry until at most n remain.
func evictSoonest(m map[string]time.Time, n int) {
	for len(m) > n {
		var key string
		var soonest time.Time
		first := true
		for k, until := range m {
			if first || until.Before(soonest) {
				key, soonest, first = k, until, false
			}
		}
		delete(m, key)
	}
}

// hostStat is per-host (per dialed endpoint for outbound peers, per
// connection for unknown-address peers) accounting of mempool requests and
// dumped bytes served.
type hostStat struct {
	lastMempool time.Time
	windowStart time.Time
	dumpBytes   int64
	seen        time.Time
}

func (st *hostStat) roll(now time.Time) {
	if now.Sub(st.windowStart) > dumpWindow {
		st.windowStart = now
		st.dumpBytes = 0
	}
}

// statForLocked returns the accounting record for p. statMu must be held.
func (s *Server) statForLocked(p *Peer, now time.Time) *hostStat {
	if p.unknownIP {
		return &p.stat
	}
	key := reliabilityKey(p) // the endpoint we dialed, or the inbound host
	st, ok := s.hostStats[key]
	if !ok {
		if len(s.hostStats) >= maxHostStats {
			for k, old := range s.hostStats {
				if now.Sub(old.seen) > dumpWindow {
					delete(s.hostStats, k)
				}
			}
			if len(s.hostStats) >= maxHostStats {
				s.hostStats = map[string]*hostStat{}
			}
		}
		st = &hostStat{}
		s.hostStats[key] = st
	}
	st.seen = now
	return st
}

// allowMempoolRequest applies the "mempool" request gap per host (per
// connection for unknown-address peers, with a shared rate).
func (s *Server) allowMempoolRequest(p *Peer) bool {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	now := time.Now()
	st := s.statForLocked(p, now)
	if !st.lastMempool.IsZero() && now.Sub(st.lastMempool) < mempoolRequestGap {
		return false
	}
	if p.unknownIP && !s.unknownMempool.allowAt(now) {
		return false
	}
	st.lastMempool = now
	return true
}

// chargeDump accounts size bytes of dumped transactions served to p and
// reports whether its budget (and the shared unknown-address budget) allows.
func (s *Server) chargeDump(p *Peer, size int) bool {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	now := time.Now()
	st := s.statForLocked(p, now)
	st.roll(now)
	if st.dumpBytes+int64(size) > dumpBudget {
		return false
	}
	if p.unknownIP {
		s.unknownDump.roll(now)
		if s.unknownDump.dumpBytes+int64(size) > unknownDumpBudget {
			return false
		}
		s.unknownDump.dumpBytes += int64(size)
	}
	st.dumpBytes += int64(size)
	return true
}
