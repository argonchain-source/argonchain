package p2p

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
)

// Config for the P2P server.
type Config struct {
	Params          *consensus.Params
	Listen          string   // "" disables inbound connections
	Seeds           []string // host:port bootstrap peers
	DNSSeeds        []string // hostnames whose A/AAAA records list seed nodes (default port)
	Connect         []string // peers to keep connected to (addnode)
	MaxOutbound     int
	MaxInbound      int
	MaxInboundPerIP int
	UserAgent       string
	DataDir         string // for peers.json; "" disables persistence
	Logf            func(format string, args ...any)
	// AllowLocalAddrs accepts gossiped private/loopback addresses (tests,
	// private networks). Off by default: only globally routable addresses
	// learned from peers are stored.
	AllowLocalAddrs bool
	// BehindProxy says inbound connections arrive through a TCP proxy or
	// load balancer, so their source address says nothing about the peer:
	// inbound peers are never banned or capped by address (misbehaviour
	// only disconnects). Set it on a node behind Railway's TCP proxy.
	BehindProxy bool
	// LocalAddrsAreHosts treats inbound connections from loopback, private
	// and carrier-grade NAT addresses as identifying the peer (bans and the
	// per-address cap apply). Off by default: such a source is usually a
	// proxy, NAT or container network in front of many peers, and is
	// treated like BehindProxy. For private networks and tests.
	LocalAddrsAreHosts bool

	// Test hooks; zero values mean the defaults.
	syncStallLimit    time.Duration                   // syncStallLimit
	syncWatchdogEvery time.Duration                   // 5s
	verifyWorkers     int                             // runtime.GOMAXPROCS(0)
	checkHeader       func(h *consensus.Header) error // chain.CheckHeaderPoW
	maxSyncQueue      int                             // maxSyncQueue
	headersPerMsg     int                             // MaxHeadersMsg (sent per headers message; a full one means more follow)
	wantedTimeout     time.Duration                   // requestTimeout (a body wanted for a reorg is asked again after this)
}

const (
	blocksInFlight      = 16
	maxBlocksPerGetData = 32
	syncStallLimit      = 90 * time.Second
	requestTimeout      = 60 * time.Second
	parkDuration        = 60 * time.Second
	maintainEvery       = 5 * time.Second
	orphanCacheSize     = 64
	orphanMaxAge        = 10 * time.Minute
	banDuration         = 24 * time.Hour
	dialTimeout         = 10 * time.Second
	maxAddrsPerMsg      = 50 // stored from one addr message
	// maxSyncQueue bounds the headers of the branch being synced that are
	// held in memory (light ones waiting to see whether the branch outgrows
	// our tip, heavier ones waiting for their bodies). The next batch is
	// asked for once the queue is down to maxSyncQueue-MaxHeadersMsg, so
	// bodies always have headers ahead of them, and the headers' proof of
	// work is still in the chain's memo (see chain.CheckHeaderPoW) when the
	// bodies arrive. A lighter prefix longer than that is moved into the
	// block index, header-only, oldest first (chain.ProcessHeaders).
	maxSyncQueue = 4 * MaxHeadersMsg
	// maxLightQueueFactor: if the index refuses those light headers (its
	// side-header cap is full of entries with more work), they stay in
	// memory, up to this many times maxSyncQueue, before the sync is given up.
	maxLightQueueFactor = 8
	// maxHeaderRestarts bounds how often one sync downloads the branch's
	// headers again because the block it hung off left the index.
	maxHeaderRestarts = 3
	// syncDeferFor: a sync peer whose branch could not be held is not chosen
	// again for this long (it is not marked unreliable: nothing it did was
	// wrong).
	syncDeferFor = 10 * time.Minute
	// maxWantedPerRequest bounds the bodies one MissingDataError asks a peer
	// for at once; the rest are asked for as those arrive.
	maxWantedPerRequest = 4 * maxBlocksPerGetData
	// maxWanted bounds the bodies asked for reorganisations and not yet
	// received (all peers together); maxWantedTries the times one is asked
	// for (of a different peer each time, if there is one).
	maxWanted      = 1024
	maxWantedTries = 4
	// busyCreditMin: handling one message of the sync peer for at least this
	// long gives the peer that time back (see creditSyncBusy).
	busyCreditMin = 50 * time.Millisecond
	// staleTipAge: a tip older than this means we are catching up, and
	// transaction announcements wait (if a sync peer is delivering headers).
	staleTipAge = 2 * time.Hour
)

// Server runs the P2P network for one node.
type Server struct {
	cfg  Config
	ch   *chain.Chain
	pool *mempool.Pool

	mu         sync.Mutex
	peers      map[*Peer]struct{}
	banned     map[string]time.Time
	self       map[string]time.Time // dial addresses that turned out to be this node (a public proxy or NAT reflection)
	unreliable map[string]time.Time // endpoints/hosts whose height claim proved empty
	outNonces  map[uint64]struct{}  // version nonces of our outbound connections (self-dial detection)
	listener   net.Listener
	addrs      *addrBook
	quit       chan struct{}
	wg         sync.WaitGroup
	started    bool
	lastDNS    time.Time

	acceptLimit    *tokenBucket
	unknownMempool *tokenBucket

	statMu      sync.Mutex
	hostStats   map[string]*hostStat
	unknownDump hostStat // shared by all unknown-address peers

	txMu       sync.Mutex
	txInFlight map[consensus.Hash]*txRequest
	orphans    *orphanTxPool

	dumpMu  sync.Mutex
	dumpIDs []consensus.Hash
	dumpGen uint64
	dumpAt  time.Time

	lastOrphanTip consensus.Hash

	sync syncState

	// OnTx, if set, is called for every transaction accepted into the pool
	// from the network (wallets use it).
	OnTx func(tx *consensus.Tx)
}

type orphan struct {
	blk   *consensus.Block
	added time.Time
}

type syncState struct {
	mu   sync.Mutex
	peer *Peer
	// queue holds the sync peer's branch, in chain order: headers whose
	// proof of work we checked, the first a child of a block in the index.
	// Bodies are fetched only while the branch has more work than our tip;
	// a branch that never gets there is dropped when the sync ends, nothing
	// of it stored.
	queue  []consensus.Header
	queued map[consensus.Hash]struct{}
	// queueWork is the total work of the branch ending at the queue's last
	// header.
	queueWork *big.Int
	// prefix lists, oldest first, header-only index blocks below the queue
	// (a light prefix moved to the index for room, or side blocks we kept
	// header-only) whose bodies are fetched before the queue's own, a
	// window at a time; prefixDone: there are none left.
	prefix     []consensus.Hash
	prefixDone bool
	// lastHeader is the last header of the sync peer's latest batch that we
	// now hold (queued or in the index): the next getheaders starts there.
	lastHeader consensus.Hash
	// headersPaused: the peer has more headers, but the queue is too full to
	// take another batch; asked for once bodies drain it.
	headersPaused bool
	requested     map[consensus.Hash]time.Time // block hash -> when asked (sync queue)
	parked        map[consensus.Hash]time.Time // rejected transiently; retry after parkDuration
	lastProgress  time.Time
	headersDone   bool
	headersAsked  time.Time                 // when the first getheaders went to this sync peer
	gotHeaders    bool                      // it answered at all
	verified      bool                      // it delivered a branch with more work than ours
	restarts      int                       // header downloads restarted (see restartHeadersLocked)
	capWarned     bool                      // logged that the index refused light headers
	orphans       map[consensus.Hash]orphan // keyed by prev hash
	// wanted are bodies requested for a reorg (MissingDataError), any peer.
	wanted map[consensus.Hash]*wantedBody
}

// wantedBody is a body asked for a reorganisation.
type wantedBody struct {
	at    time.Time // when last asked (zero: asked again at once)
	from  *Peer     // who was asked
	tries int
}

// New creates a server. Call Start to run it.
func New(cfg Config, ch *chain.Chain, pool *mempool.Pool) *Server {
	if cfg.MaxOutbound == 0 {
		cfg.MaxOutbound = 8
	}
	if cfg.MaxInbound == 0 {
		cfg.MaxInbound = 125
	}
	if cfg.MaxInboundPerIP == 0 {
		cfg.MaxInboundPerIP = 3
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "/argonmine:0.1/"
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.syncStallLimit == 0 {
		cfg.syncStallLimit = syncStallLimit
	}
	if cfg.syncWatchdogEvery == 0 {
		cfg.syncWatchdogEvery = 5 * time.Second
	}
	if cfg.checkHeader == nil {
		cfg.checkHeader = ch.CheckHeaderPoW
	}
	if cfg.maxSyncQueue == 0 {
		cfg.maxSyncQueue = maxSyncQueue
	}
	if cfg.headersPerMsg <= 0 || cfg.headersPerMsg > MaxHeadersMsg {
		cfg.headersPerMsg = MaxHeadersMsg
	}
	if cfg.wantedTimeout == 0 {
		cfg.wantedTimeout = requestTimeout
	}
	s := &Server{
		cfg:            cfg,
		ch:             ch,
		pool:           pool,
		peers:          map[*Peer]struct{}{},
		banned:         map[string]time.Time{},
		self:           map[string]time.Time{},
		unreliable:     map[string]time.Time{},
		outNonces:      map[uint64]struct{}{},
		quit:           make(chan struct{}),
		addrs:          newAddrBook(cfg.DataDir, cfg.AllowLocalAddrs),
		acceptLimit:    newTokenBucket(acceptRate, acceptBurst),
		unknownMempool: newTokenBucket(1/unknownMempoolEvery.Seconds(), unknownMempoolBurst),
		hostStats:      map[string]*hostStat{},
		txInFlight:     map[consensus.Hash]*txRequest{},
		orphans:        newOrphanTxPool(),
	}
	s.sync.queued = map[consensus.Hash]struct{}{}
	s.sync.requested = map[consensus.Hash]time.Time{}
	s.sync.parked = map[consensus.Hash]time.Time{}
	s.sync.orphans = map[consensus.Hash]orphan{}
	s.sync.wanted = map[consensus.Hash]*wantedBody{}
	for _, seed := range cfg.Seeds {
		s.addrs.addString(seed, true)
	}
	ch.Subscribe(s.onTip)
	return s
}

func (s *Server) logf(format string, args ...any) { s.cfg.Logf(format, args...) }

// Start begins listening and dialing.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	s.started = true
	if s.cfg.Listen != "" {
		l, err := net.Listen("tcp", s.cfg.Listen)
		if err != nil {
			return err
		}
		s.listener = l
		s.wg.Add(1)
		go s.acceptLoop(l)
		s.logf("p2p: listening on %s", l.Addr())
	}
	s.wg.Add(2)
	go s.maintainLoop()
	go s.syncWatchdog()
	return nil
}

// ListenAddr returns the bound listen address ("" if not listening).
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Stop disconnects everything and waits for goroutines.
func (s *Server) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	close(s.quit)
	if s.listener != nil {
		s.listener.Close()
	}
	peers := make([]*Peer, 0, len(s.peers))
	for p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	for _, p := range peers {
		p.Disconnect()
	}
	s.wg.Wait()
	s.addrs.save()
}

func (s *Server) acceptLoop(l net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		if !s.acceptLimit.allow() {
			conn.Close() // more than acceptRate per second: shed load
			continue
		}
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		unknown := s.unknownSource(host)
		if !unknown && (s.isBanned(host) || s.inboundFrom(host) >= s.cfg.MaxInboundPerIP) {
			conn.Close()
			continue
		}
		if s.inboundCount() >= s.cfg.MaxInbound && !s.evictInbound() {
			conn.Close()
			continue
		}
		s.addPeer(conn, true, unknown, "")
	}
}

func (s *Server) addPeer(conn net.Conn, inbound, unknownIP bool, dialAddr string) *Peer {
	p := newPeer(s, conn, inbound)
	p.unknownIP = unknownIP
	p.dialAddr = dialAddr
	s.mu.Lock()
	s.peers[p] = struct{}{}
	if !inbound {
		s.outNonces[p.nonce] = struct{}{}
	}
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		p.run()
	}()
	return p
}

func (s *Server) removePeer(p *Peer) {
	s.mu.Lock()
	delete(s.peers, p)
	delete(s.outNonces, p.nonce)
	s.mu.Unlock()
	s.sync.mu.Lock()
	if s.sync.peer == p {
		s.resetSyncLocked()
	}
	s.sync.mu.Unlock()
	s.peerGoneTx(p)
	s.orphans.forgetPeer(p)
	s.logf("p2p: disconnected %s", p.addr)
}

// Connect dials addr and adds it as an outbound peer.
func (s *Server) Connect(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if s.isBanned(host) {
		return errors.New("p2p: address is banned")
	}
	if s.isSelf(addr) {
		return errors.New("p2p: address is this node")
	}
	if s.connectedTo(addr) {
		return errors.New("p2p: already connected")
	}
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return err
	}
	s.addPeer(conn, false, false, addr)
	s.addrs.addString(addr, true)
	s.addrs.tried(addr)
	return nil
}

// addrForms returns addr and, for a host name, every resolved ip:port, so
// that a -connect entry given by name matches the connection it made.
func addrForms(addr string) map[string]struct{} {
	forms := map[string]struct{}{addr: {}}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return forms
	}
	if ip := net.ParseIP(host); ip != nil {
		forms[net.JoinHostPort(ip.String(), port)] = struct{}{}
		return forms
	}
	if ips, err := net.LookupIP(host); err == nil {
		for _, ip := range ips {
			forms[net.JoinHostPort(ip.String(), port)] = struct{}{}
		}
	}
	return forms
}

// connectedTo reports whether a live connection matches addr, comparing
// both the address dialed and the address connected to.
func (s *Server) connectedTo(addr string) bool {
	forms := addrForms(addr)
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.peers {
		if p.gone() {
			continue
		}
		if _, ok := forms[p.addr]; ok {
			return true
		}
		if p.dialAddr != "" {
			if _, ok := forms[p.dialAddr]; ok {
				return true
			}
		}
	}
	return false
}

func (s *Server) inboundCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p := range s.peers {
		if p.inbound && !p.gone() {
			n++
		}
	}
	return n
}

// inboundFrom counts inbound peers identified by host (unknown-address
// peers never count against a host).
func (s *Server) inboundFrom(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p := range s.peers {
		if p.inbound && !p.unknownIP && !p.gone() && p.host == host {
			n++
		}
	}
	return n
}

func (s *Server) outboundCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p := range s.peers {
		if !p.inbound {
			n++
		}
	}
	return n
}

func (s *Server) peerList() []*Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Peer, 0, len(s.peers))
	for p := range s.peers {
		out = append(out, p)
	}
	return out
}

// PeerCount returns the number of connected peers.
func (s *Server) PeerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// Peers returns a snapshot of peer info for RPC.
func (s *Server) Peers() []Info {
	ps := s.peerList()
	out := make([]Info, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Syncing reports whether an initial block download is in progress.
func (s *Server) Syncing() bool {
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	return s.sync.peer != nil
}

// ban bans p's host for banDuration, unless its address does not identify
// it (behind a proxy, or a shared local address): then the caller's
// disconnect is all that happens, so one bad peer cannot lock out everyone
// arriving through the same address.
func (s *Server) ban(p *Peer) {
	if p.unknownIP {
		s.logf("p2p: disconnecting %s, not banning: its address is shared (proxy or NAT)", p.addr)
		return
	}
	s.mu.Lock()
	if _, ok := s.banned[p.host]; !ok && len(s.banned) >= maxBanned {
		pruneExpired(s.banned, time.Now())
		evictSoonest(s.banned, maxBanned-1)
	}
	s.banned[p.host] = time.Now().Add(banDuration)
	s.mu.Unlock()
	s.logf("p2p: banned %s for %s", p.host, banDuration)
}

// isSelf reports whether addr was previously dialed and answered with this
// node's own nonce. Seeds commonly list a node's own public endpoint.
func (s *Server) isSelf(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.self[addr]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(s.self, addr)
		return false
	}
	return true
}

func (s *Server) isOutboundNonce(n uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.outNonces[n]
	return ok
}

func (s *Server) isBanned(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.banned[host]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(s.banned, host)
		return false
	}
	return true
}

func (s *Server) maintainLoop() {
	defer s.wg.Done()
	s.maintain()
	t := time.NewTicker(maintainEvery)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			s.maintain()
		}
	}
}

// maintain keeps outbound connections up and triggers sync.
func (s *Server) maintain() {
	for _, addr := range s.cfg.Connect {
		if !s.connectedTo(addr) {
			if err := s.Connect(addr); err != nil {
				s.logf("p2p: connect %s: %v", addr, err)
			}
		}
	}
	if len(s.cfg.DNSSeeds) > 0 && s.addrs.count() < 10 && time.Since(s.lastDNS) > 10*time.Minute {
		s.lastDNS = time.Now()
		for _, host := range s.cfg.DNSSeeds {
			ips, err := net.LookupIP(host)
			if err != nil {
				s.logf("p2p: dns seed %s: %v", host, err)
				continue
			}
			for _, ip := range ips {
				var a NetAddress
				copy(a.IP[:], ip.To16())
				a.Port = s.cfg.Params.P2PPort
				a.Services = ServiceNetwork
				a.Time = uint64(time.Now().Unix())
				s.addrs.add(a, true)
			}
			s.logf("p2p: dns seed %s gave %d addresses", host, len(ips))
		}
	}
	need := s.cfg.MaxOutbound - s.outboundCount()
	if need > 0 {
		exclude := map[string]struct{}{}
		for _, p := range s.peerList() {
			exclude[p.addr] = struct{}{}
			if p.dialAddr != "" {
				exclude[p.dialAddr] = struct{}{}
			}
		}
		if la := s.ListenAddr(); la != "" {
			exclude[la] = struct{}{}
		}
		s.mu.Lock()
		for addr := range s.self {
			exclude[addr] = struct{}{}
		}
		s.mu.Unlock()
		for _, addr := range s.addrs.random(need, exclude) {
			if err := s.Connect(addr); err != nil {
				s.addrs.failed(addr)
			}
		}
	}
	s.startSync()
	s.expireOrphans()
	now := time.Now()
	s.pruneMaps(now)
	s.addrs.expire(now)
	s.orphans.expire(now)
	s.expireTxRequests()
	if tip := s.ch.Tip().Hash; tip != s.lastOrphanTip {
		s.lastOrphanTip = tip
		s.retryAllOrphans()
	}
}

func (s *Server) syncWatchdog() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.syncWatchdogEvery)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			s.retryWanted()
			s.sync.mu.Lock()
			p := s.sync.peer
			var silent, stalled bool
			if p != nil {
				s.syncStepLocked() // re-requests timed-out bodies
				// While we are still handling one of its messages, the
				// peer's next answer cannot have been read: our backlog is
				// not its delay.
				if s.sync.peer == p && !p.busy() {
					silent = !s.sync.gotHeaders && time.Since(s.sync.headersAsked) > firstHeadersTimeout
					stalled = time.Since(s.sync.lastProgress) > s.cfg.syncStallLimit
				}
			}
			s.sync.mu.Unlock()
			switch {
			case silent:
				s.logf("p2p: sync peer %s did not answer getheaders within %s, dropping", p.addr, firstHeadersTimeout)
				s.markUnreliable(p)
				p.Disconnect()
			case stalled:
				s.logf("p2p: sync peer %s stalled, dropping", p.addr)
				s.markUnreliable(p)
				p.Disconnect()
			}
		}
	}
}

// versionMsg is our version message on p's connection; each connection
// carries its own random nonce.
func (s *Server) versionMsg(p *Peer) *MsgVersion {
	return &MsgVersion{
		Version:     ProtocolVersion,
		Services:    ServiceNetwork,
		Timestamp:   time.Now().Unix(),
		Nonce:       p.nonce,
		UserAgent:   s.cfg.UserAgent,
		StartHeight: s.ch.Tip().Height,
		Genesis:     s.cfg.Params.GenesisHash,
	}
}

// handleMessage dispatches one message from p.
func (s *Server) handleMessage(p *Peer, msg Message) {
	switch m := msg.(type) {
	case *MsgVersion:
		s.onVersion(p, m)
	case *MsgVerack:
		p.mu.Lock()
		p.verackRecv = true
		done := p.version != nil && !p.handshakeDone
		if done {
			p.handshakeDone = true
		}
		p.mu.Unlock()
		if done {
			s.onHandshake(p)
		}
	case *MsgPing:
		p.Send(&MsgPong{Nonce: m.Nonce})
	case *MsgPong:
		p.mu.Lock()
		if m.Nonce == p.lastPingNonce {
			p.pingRTT = time.Since(p.lastPingSent)
		}
		p.mu.Unlock()
	case *MsgGetHeaders:
		s.onGetHeaders(p, m)
	case *MsgHeaders:
		s.onHeaders(p, m)
	case *MsgInv:
		s.onInv(p, m)
	case *MsgGetData:
		s.onGetData(p, m)
	case *MsgBlock:
		s.onBlock(p, m.Block)
	case *MsgTx:
		s.onTx(p, m.Tx)
	case *MsgMempool:
		if s.allowMempoolRequest(p) {
			s.announceMempool(p)
		}
	case *MsgGetAddr:
		p.Send(&MsgAddr{Addrs: s.addrs.sample(MaxAddrMsg)})
	case *MsgAddr:
		n := 0
		for _, a := range m.Addrs {
			if s.addrs.addFrom(a, addrSource(p)) {
				n++
				if n >= maxAddrsPerMsg {
					break
				}
			}
		}
	case *MsgNotFound:
		s.sync.mu.Lock()
		for _, it := range m.Items {
			if it.Type == InvBlock {
				delete(s.sync.requested, it.Hash)
				if w, ok := s.sync.wanted[it.Hash]; ok && w.from == p {
					w.at = time.Time{} // ask another peer at the next watchdog round
				}
			}
		}
		s.sync.mu.Unlock()
		for _, it := range m.Items {
			if it.Type == InvTx {
				if alts := s.txAnswered(p, it.Hash); len(alts) > 0 {
					s.retryTx(it.Hash, alts)
				}
			}
		}
		s.requestTxs(p)
	case *MsgReject:
		s.logf("p2p: %s rejected %s: %s", p.addr, m.Cmd, m.Reason)
	}
}

func (s *Server) onVersion(p *Peer, m *MsgVersion) {
	if m.Genesis != s.cfg.Params.GenesisHash {
		s.logf("p2p: %s is on a different chain, disconnecting", p.addr)
		p.Disconnect()
		return
	}
	if p.inbound && s.isOutboundNonce(m.Nonce) {
		// One of our own outbound connections reached us. Echo its nonce so
		// the dialing side recognises itself and records the address; this
		// side only knows the proxy's or NAT's source. Hang up shortly
		// after, in case the dialer never does.
		v := s.versionMsg(p)
		v.Nonce = m.Nonce
		p.Send(v)
		time.AfterFunc(2*time.Second, p.Disconnect)
		return
	}
	if !p.inbound && m.Nonce == p.nonce {
		s.markSelf(p.addr, p.dialAddr)
		s.logf("p2p: %s is this node (seen through a proxy or NAT); not dialing it again for %s", p.addr, selfFor)
		p.Disconnect()
		return
	}
	if m.Version < ProtocolVersion {
		p.Disconnect()
		return
	}
	p.mu.Lock()
	if p.version != nil {
		p.mu.Unlock()
		p.misbehave(banThreshold, "duplicate version")
		return
	}
	p.version = m
	done := p.verackRecv && !p.handshakeDone
	if done {
		p.handshakeDone = true
	}
	p.mu.Unlock()
	if p.inbound {
		p.Send(s.versionMsg(p))
	}
	p.Send(&MsgVerack{})
	if done {
		s.onHandshake(p)
	}
}

func (s *Server) onHandshake(p *Peer) {
	s.logf("p2p: handshake with %s (%s, height %d)", p.addr, p.Version().UserAgent, p.StartHeight())
	p.Send(&MsgGetAddr{})
	// Announce what is waiting in our mempool. A peer that restarted (and
	// lost its pool) or a miner that just joined then fetches those
	// transactions at once instead of waiting for a fresh relay that may
	// never come; it asks only for what it does not have. A peer that is
	// still downloading blocks ignores this and asks again with "mempool"
	// once it has caught up. Parents go before children, the best-paying
	// first, in batches; fetching them counts against the peer's budget.
	s.announceMempool(p)
	if !p.inbound {
		s.addrs.tried(p.addr)
	}
	s.startSync()
}

// ---- headers-first sync ----
//
// The sync peer's headers are checked (each its proof of work, the batch
// one connected chain) and held in memory, in s.sync.queue, as one branch
// hanging off a block in the index. Bodies are fetched, oldest first, only
// while that branch has more work than our tip. A branch that never gets
// there is dropped when the sync ends, and nothing of it is stored, so an
// ordinary race (the winning branch's first block no heavier than our tip)
// is followed however full the index's side-header cap is. Only a lighter
// prefix too long for the queue is moved into the index, header-only
// (chain.ProcessHeaders); if the branch wins, those bodies are fetched
// first (s.sync.prefix), a window at a time.

func (s *Server) resetSyncLocked() {
	s.sync.peer = nil
	s.clearQueueLocked()
	s.sync.lastHeader = consensus.Hash{}
	s.sync.headersPaused = false
	s.sync.requested = map[consensus.Hash]time.Time{}
	s.sync.headersDone = false
	s.sync.gotHeaders = false
	s.sync.verified = false
	s.sync.restarts = 0
	s.sync.capWarned = false
}

// beginSyncLocked makes p the sync peer.
func (s *Server) beginSyncLocked(p *Peer) {
	s.resetSyncLocked()
	s.sync.peer = p
	now := time.Now()
	s.sync.lastProgress = now
	s.sync.headersAsked = now
}

// startSync picks a sync peer if none and some peer claims to be ahead,
// preferring outbound peers (ones we chose) over inbound claims. The claim
// is unverified; a peer that does not answer within firstHeadersTimeout, or
// answers with nothing new, is marked unreliable (its host too, for an
// hour, when its address identifies it) and skipped next time.
func (s *Server) startSync() {
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	if s.sync.peer != nil {
		return
	}
	tip := s.ch.Tip()
	var best *Peer
	for _, p := range s.peerList() {
		if !p.isHandshakeDone() || p.gone() || p.StartHeight() <= tip.Height || s.isUnreliable(p) || p.syncDeferred() {
			continue
		}
		if best == nil || (best.inbound && !p.inbound) ||
			(best.inbound == p.inbound && p.StartHeight() > best.StartHeight()) {
			best = p
		}
	}
	if best == nil {
		return
	}
	s.beginSyncLocked(best)
	best.Send(&MsgGetHeaders{Locator: s.locator(tip)})
	s.logf("p2p: syncing from %s (claims height %d, ours %d)", best.addr, best.StartHeight(), tip.Height)
}

// locator lists main-chain hashes densely near the tip and sparsely
// further back, ending at genesis.
func (s *Server) locator(tip *chain.Node) []consensus.Hash {
	var out []consensus.Hash
	step := uint32(1)
	h := int64(tip.Height)
	for h >= 0 {
		n, ok := s.ch.NodeAtHeight(uint32(h))
		if !ok {
			break
		}
		out = append(out, n.Hash)
		if len(out) >= MaxLocator-1 {
			break
		}
		if len(out) > 10 {
			step *= 2
		}
		h -= int64(step)
	}
	if len(out) == 0 || out[len(out)-1] != s.cfg.Params.GenesisHash {
		out = append(out, s.cfg.Params.GenesisHash)
	}
	return out
}

func (s *Server) onGetHeaders(p *Peer, m *MsgGetHeaders) {
	// First locator hash on our main chain; both lookups are O(1).
	var start *chain.Node
	for _, h := range m.Locator {
		if n, ok := s.ch.Node(h); ok && s.ch.IsMainChain(n) {
			start = n
			break
		}
	}
	if start == nil {
		start, _ = s.ch.Node(s.cfg.Params.GenesisHash)
	}
	tip := s.ch.Tip()
	limit := s.cfg.headersPerMsg
	hdrs := make([]consensus.Header, 0, limit)
	for h := start.Height + 1; h <= tip.Height && len(hdrs) < limit; h++ {
		n, ok := s.ch.NodeAtHeight(h)
		if !ok {
			break
		}
		hdrs = append(hdrs, n.Header)
		if n.Hash == m.Stop {
			break
		}
	}
	p.Send(&MsgHeaders{Headers: hdrs})
}

func (s *Server) onHeaders(p *Peer, m *MsgHeaders) {
	hdrs := m.Headers
	hashes := make([]consensus.Hash, len(hdrs))
	for i := range hdrs {
		hashes[i] = hdrs[i].Hash()
		if i > 0 && hdrs[i].PrevHash != hashes[i-1] {
			// Not one chain (which also rules out a header repeated in the
			// batch): no honest peer sends that, and none of it is checked.
			p.misbehave(20, "non-continuous headers")
			return
		}
	}

	// Decide whether this peer's headers are wanted, and which are new,
	// without holding the lock through the expensive proof-of-work checks.
	s.sync.mu.Lock()
	if s.sync.peer == nil {
		// Unsolicited announcement: adopt the peer only if the headers
		// connect to something we have (otherwise they are noise) and it has
		// not already proved an unreliable sync peer.
		if len(hdrs) == 0 || s.isUnreliable(p) || p.syncDeferred() {
			s.sync.mu.Unlock()
			return
		}
		if _, ok := s.ch.Node(hdrs[0].PrevHash); !ok {
			s.sync.mu.Unlock()
			return
		}
		s.beginSyncLocked(p)
	}
	if s.sync.peer != p {
		s.sync.mu.Unlock()
		return
	}
	s.sync.gotHeaders = true
	// The batch is one chain, so the headers we already hold (in the index
	// or queued) come first. The first new one must connect to something we
	// hold before any proof of work is checked.
	first := len(hdrs)
	for i := range hdrs {
		if !s.holdsLocked(hashes[i]) {
			first = i
			break
		}
	}
	if first < len(hdrs) && !s.holdsLocked(hdrs[first].PrevHash) {
		s.sync.mu.Unlock()
		p.misbehave(20, "non-connecting headers")
		return
	}
	s.sync.mu.Unlock()

	// Proof of work for the new headers, outside the lock (see
	// verifyHeaders: the first alone, the rest in parallel), recording
	// progress as each passes so that a long batch is not mistaken for a
	// stall. Every header credited is new and connects.
	todo := make([]int, 0, len(hdrs)-first)
	for i := first; i < len(hdrs); i++ {
		todo = append(todo, i)
	}
	ok, err := s.verifyHeaders(p, hdrs, todo)
	if !ok {
		return // the peer left, or is no longer the sync peer
	}
	if err != nil {
		p.misbehave(banThreshold, "header fails proof of work: "+err.Error())
		return
	}

	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	if s.sync.peer != p {
		return
	}
	tip := s.ch.Tip()
	maxQ := s.cfg.maxSyncQueue
	appended, truncated, onlyMain := 0, false, true
	overflowTried := false // one move into the index makes room for a whole batch
	for i := range hdrs {
		h, hash := &hdrs[i], hashes[i]
		if n, known := s.ch.Node(hash); known {
			if !s.ch.IsMainChain(n) {
				onlyMain = false
			}
			s.sync.lastHeader = hash
			continue
		}
		onlyMain = false
		if _, q := s.sync.queued[hash]; q {
			s.sync.lastHeader = hash
			continue
		}
		if i < first {
			// We held it when we looked, and do not any more: check it now.
			if err := s.cfg.checkHeader(h); err != nil {
				p.misbehave(banThreshold, "header fails proof of work: "+err.Error())
				return
			}
		}
		if !s.attachToQueueLocked(p, h) {
			return
		}
		if len(s.sync.queue) >= maxQ {
			light := !s.queueHeavierLocked(tip)
			if light && !overflowTried {
				overflowTried = true
				if !s.overflowLocked(p) {
					return
				}
			}
			if len(s.sync.queue) >= maxQ && (!light || len(s.sync.queue) >= maxQ*maxLightQueueFactor) {
				truncated = true // asked for again once the queue has room
				break
			}
		}
		s.sync.queueWork.Add(s.sync.queueWork, headerWork(h))
		s.sync.queue = append(s.sync.queue, *h)
		s.sync.queued[hash] = struct{}{}
		s.sync.lastHeader = hash
		appended++
	}
	if appended > 0 {
		s.sync.lastProgress = time.Now()
	}
	if s.queueHeavierLocked(tip) {
		s.sync.verified = true
	}
	full := len(hdrs) >= s.cfg.headersPerMsg
	if appended == 0 && len(s.sync.queue) == 0 && p.StartHeight() > tip.Height && (onlyMain || !full) {
		// The peer claimed to be ahead of us but sent nothing we lack: only
		// blocks of our own chain, or a chain that ended there. The claim is
		// unsubstantiated. Keep the connection (it may still relay honestly)
		// but do not pick it as a sync peer again. (A full batch of side
		// headers we hold may be followed by more: asked for below.)
		s.logf("p2p: %s claimed height %d but sent no headers we lack; ignoring its claim", p.addr, p.StartHeight())
		s.markUnreliable(p)
		s.resetSyncLocked()
		return
	}
	if full || truncated {
		s.sync.headersDone = false
		s.requestHeadersLocked()
	} else {
		s.sync.headersDone = true
		s.sync.headersPaused = false
	}
	s.syncStepLocked()
}

// holdsLocked reports whether header h is in the index or queued.
func (s *Server) holdsLocked(h consensus.Hash) bool {
	if _, q := s.sync.queued[h]; q {
		return true
	}
	_, known := s.ch.Node(h)
	return known
}

// verifyHeaders checks the proof of work of hdrs[i] for every i in todo
// (ascending) and records sync progress for p after each header that
// passes. The first is checked alone: a peer gets the parallel check (one
// Argon2id per worker at once, up to GOMAXPROCS) only after delivering a
// valid header, so a batch of junk costs one evaluation before the ban. The
// verdict is the one checking them one by one in order gives: the error of
// the lowest-index header that fails, or nil. ok is false if p stopped
// being the sync peer (or left) before the check was complete; nothing is
// then known about the batch.
func (s *Server) verifyHeaders(p *Peer, hdrs []consensus.Header, todo []int) (ok bool, err error) {
	if len(todo) == 0 {
		return true, nil
	}
	if err := s.cfg.checkHeader(&hdrs[todo[0]]); err != nil {
		return true, err
	}
	if !s.noteHeaderProgress(p) {
		return false, nil
	}
	todo = todo[1:]
	if len(todo) == 0 {
		return true, nil
	}
	workers := s.cfg.verifyWorkers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > len(todo) {
		workers = len(todo)
	}
	errs := make([]error, len(todo))
	var next atomic.Int64      // next position in todo to check
	var firstFail atomic.Int64 // lowest failing position found so far
	firstFail.Store(int64(len(todo)))
	var aborted atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !aborted.Load() {
				k := next.Add(1) - 1
				// Positions are handed out in order: once past a failure,
				// nothing later can change the verdict.
				if k >= int64(len(todo)) || k > firstFail.Load() {
					return
				}
				if e := s.cfg.checkHeader(&hdrs[todo[k]]); e != nil {
					errs[k] = e
					for {
						cur := firstFail.Load()
						if k >= cur || firstFail.CompareAndSwap(cur, k) {
							break
						}
					}
					continue
				}
				if !s.noteHeaderProgress(p) {
					aborted.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	if aborted.Load() {
		return false, nil
	}
	if f := firstFail.Load(); f < int64(len(todo)) {
		// Every position before f was checked and passed (only positions
		// after a known failure are skipped), so f is the first failure.
		return true, errs[f]
	}
	return true, nil
}

// noteHeaderProgress records that p, if still the sync peer, just delivered
// a new, connecting header that passed its proof of work.
func (s *Server) noteHeaderProgress(p *Peer) bool {
	if p.gone() {
		return false
	}
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	if s.sync.peer != p {
		return false
	}
	s.sync.lastProgress = time.Now()
	return true
}

// creditSyncBusy gives the sync peer back the time d this node spent
// handling one of its messages: meanwhile its next answer could not be
// read, so that time is our backlog, not the peer's delay.
func (s *Server) creditSyncBusy(p *Peer, d time.Duration) {
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	if s.sync.peer != p {
		return
	}
	now := time.Now()
	shift := func(t time.Time) time.Time {
		if t = t.Add(d); t.After(now) {
			return now
		}
		return t
	}
	s.sync.lastProgress = shift(s.sync.lastProgress)
	s.sync.headersAsked = shift(s.sync.headersAsked)
}

// headerWork is the work a header's (already checked) target represents.
func headerWork(h *consensus.Header) *big.Int {
	target, err := consensus.CompactToTarget(h.Bits)
	if err != nil {
		return new(big.Int)
	}
	return consensus.BlockWork(target)
}

// queueHeavierLocked reports whether the queued branch has more work than
// tip (only then are its bodies fetched).
func (s *Server) queueHeavierLocked(tip *chain.Node) bool {
	return len(s.sync.queue) > 0 && s.sync.queueWork != nil && s.sync.queueWork.Cmp(tip.Work) > 0
}

// attachToQueueLocked makes the queue ready to take h, which is neither in
// the index nor queued: h extends the queued branch, or starts a new one off
// a block in the index (replacing the queued branch, if any: the peer
// switched branches), or forks off inside the queue (cut back to h's
// parent). It returns false, after punishing p, if h connects to nothing we
// hold or to an invalid block.
func (s *Server) attachToQueueLocked(p *Peer, h *consensus.Header) bool {
	if n := len(s.sync.queue); n > 0 && s.sync.queue[n-1].Hash() == h.PrevHash {
		return true
	}
	if parent, ok := s.ch.Node(h.PrevHash); ok {
		if st, _ := s.ch.Status(parent.Hash); st == chain.StatusInvalid {
			p.misbehave(banThreshold, "header "+h.Hash().String()+" descends from an invalid block")
			return false
		}
		s.clearQueueLocked()
		s.sync.queueWork = new(big.Int).Set(parent.Work)
		return true
	}
	if _, q := s.sync.queued[h.PrevHash]; q {
		s.truncateQueueAfterLocked(h.PrevHash)
		return true
	}
	p.misbehave(20, "non-connecting headers")
	return false
}

// truncateQueueAfterLocked drops the queued headers after the one hashed h.
func (s *Server) truncateQueueAfterLocked(h consensus.Hash) {
	for i := range s.sync.queue {
		if s.sync.queue[i].Hash() != h {
			continue
		}
		for j := i + 1; j < len(s.sync.queue); j++ {
			d := &s.sync.queue[j]
			delete(s.sync.queued, d.Hash())
			s.sync.queueWork.Sub(s.sync.queueWork, headerWork(d))
		}
		s.sync.queue = s.sync.queue[:i+1]
		return
	}
}

func (s *Server) clearQueueLocked() {
	s.sync.queue = nil
	s.sync.queued = map[consensus.Hash]struct{}{}
	s.sync.queueWork = nil
	s.sync.prefix = nil
	s.sync.prefixDone = false
}

// overflowLocked moves the oldest headers of a queued branch that has no
// more work than the tip into the block index, header-only, until the
// queue has room for another batch (chain.ProcessHeaders, under
// chain.MaxSideHeaders). What the index does not take (its cap full of
// entries with more work, a header too far in the future, a storage
// failure: none of it the peer's fault) stays queued and the sync goes on.
// It returns false if p was punished for an invalid header.
func (s *Server) overflowLocked(p *Peer) bool {
	room := s.cfg.maxSyncQueue - s.cfg.headersPerMsg
	if room < 0 {
		room = 0
	}
	n := len(s.sync.queue) - room
	if n <= 0 {
		return true
	}
	stored, err := s.ch.ProcessHeaders(s.sync.queue[:n], time.Now().Unix())
	if stored > 0 {
		for i := 0; i < stored; i++ {
			delete(s.sync.queued, s.sync.queue[i].Hash())
		}
		s.sync.queue = s.sync.queue[stored:]
		s.sync.prefixDone = false
	}
	switch {
	case err == nil:
		return true
	case errors.Is(err, chain.ErrTooManyHeaders), errors.Is(err, consensus.ErrTimeTooNew),
		errors.Is(err, consensus.ErrBlockUnknownParent), errors.Is(err, chain.ErrHeaderNeedsBody), chain.IsStorage(err):
		if !s.sync.capWarned {
			s.sync.capWarned = true
			s.logf("p2p: the block index did not take light headers from %s (%v); keeping them in memory", p.addr, err)
		}
		return true
	default:
		p.misbehave(banThreshold, "invalid header: "+err.Error())
		return false
	}
}

// requestHeadersLocked asks the sync peer for the headers after the last
// one it sent. If the queue cannot take another full batch: a heavier
// branch waits for bodies to drain it (headersPaused); a lighter one moves
// its oldest headers into the index (overflowLocked) or, if the index will
// not take them, keeps them in memory up to maxLightQueueFactor times the
// queue's size.
func (s *Server) requestHeadersLocked() {
	p := s.sync.peer
	if p == nil {
		return
	}
	maxQ := s.cfg.maxSyncQueue
	room := maxQ - s.cfg.headersPerMsg
	if room < 0 {
		room = 0
	}
	if len(s.sync.queue) > room {
		if s.queueHeavierLocked(s.ch.Tip()) {
			s.sync.headersPaused = true
			return
		}
		if !s.overflowLocked(p) {
			return
		}
		if len(s.sync.queue) > room && len(s.sync.queue) >= maxQ*maxLightQueueFactor {
			s.abandonSyncLocked(p, "more headers of a branch lighter than ours than can be held")
			return
		}
	}
	s.sync.headersPaused = false
	loc := []consensus.Hash{}
	if s.sync.lastHeader != (consensus.Hash{}) {
		loc = append(loc, s.sync.lastHeader)
	}
	loc = append(loc, s.locator(s.ch.Tip())...)
	if len(loc) > MaxLocator {
		loc = loc[:MaxLocator]
	}
	p.Send(&MsgGetHeaders{Locator: loc})
}

// abandonSyncLocked ends the sync with p without holding anything against
// it beyond not choosing it again for syncDeferFor.
func (s *Server) abandonSyncLocked(p *Peer, why string) {
	s.logf("p2p: giving up the sync with %s: %s; not syncing from it for %s", p.addr, why, syncDeferFor)
	p.deferSync(syncDeferFor)
	s.resetSyncLocked()
}

// restartHeadersLocked drops the queue and asks the sync peer for its
// branch again from where it leaves our chain: the block the queue hung
// off has left the index (an evicted header-only entry).
func (s *Server) restartHeadersLocked(p *Peer) {
	s.sync.restarts++
	if s.sync.restarts > maxHeaderRestarts {
		s.abandonSyncLocked(p, "its branch keeps losing its place in the block index")
		return
	}
	s.logf("p2p: the start of the branch from %s left the block index; fetching its headers again", p.addr)
	s.clearQueueLocked()
	s.sync.lastHeader = consensus.Hash{}
	s.sync.headersDone = false
	s.requestHeadersLocked()
}

// syncStepLocked moves the sync on after any change: a branch that ended
// without more work than the tip is dropped, a paused header request goes
// out once the queue has room, bodies are requested, and the sync ends
// when the peer's chain is exhausted.
func (s *Server) syncStepLocked() {
	p := s.sync.peer
	if p == nil {
		return
	}
	if s.sync.headersDone && len(s.sync.queue) > 0 && !s.queueHeavierLocked(s.ch.Tip()) {
		// Nothing of a branch that never outgrew our chain is kept.
		s.clearQueueLocked()
	}
	if s.sync.headersPaused {
		s.requestHeadersLocked()
		if s.sync.peer != p {
			return
		}
	}
	s.requestBlocksLocked()
	s.finishSyncIfDoneLocked()
}

// requestBlocksLocked asks the sync peer for bodies, oldest first: those of
// the branch's header-only index blocks (fillPrefixLocked), then the
// queue's, keeping at most blocksInFlight outstanding, skipping parked
// hashes and re-asking for requests that timed out. Nothing is asked for
// unless the queued branch has more work than the tip (a minimum chain work
// check).
func (s *Server) requestBlocksLocked() {
	p := s.sync.peer
	if p == nil {
		return
	}
	now := time.Now()
	for h, t := range s.sync.requested {
		if now.Sub(t) > requestTimeout {
			delete(s.sync.requested, h)
		}
	}
	for h, t := range s.sync.parked {
		if now.Sub(t) > parkDuration {
			delete(s.sync.parked, h)
		}
	}
	if !s.queueHeavierLocked(s.ch.Tip()) || !s.fillPrefixLocked(p) {
		return
	}
	var items []InvVect
	ask := func(h consensus.Hash) bool { // false: ask for nothing further now
		if len(s.sync.requested) >= blocksInFlight {
			return false
		}
		if _, r := s.sync.requested[h]; r {
			return true
		}
		if _, parked := s.sync.parked[h]; parked {
			return false // in order: nothing after it can connect yet
		}
		s.sync.requested[h] = now
		items = append(items, InvVect{Type: InvBlock, Hash: h})
		return true
	}
	if len(s.sync.prefix) > 0 {
		for _, h := range s.sync.prefix {
			if !ask(h) {
				break
			}
		}
	} else {
		for i := range s.sync.queue {
			if !ask(s.sync.queue[i].Hash()) {
				break
			}
		}
	}
	if len(items) > 0 {
		p.Send(&MsgGetData{Items: items})
	}
}

// fillPrefixLocked makes sure the bodies of header-only index blocks below
// the queue are fetched before the queue's own: it lists the next window
// of them, oldest first, whenever the last window has arrived. It returns
// false if the queued branch cannot be followed as it is (the sync was
// restarted, given up, or the peer punished).
func (s *Server) fillPrefixLocked(p *Peer) bool {
	if len(s.sync.prefix) > 0 || s.sync.prefixDone {
		return true
	}
	hashes, known, invalid := s.ch.MissingBodies(s.sync.queue[0].PrevHash, s.cfg.headersPerMsg)
	switch {
	case invalid:
		p.misbehave(banThreshold, "headers build on an invalid block")
		s.clearQueueLocked()
		return false
	case !known:
		s.restartHeadersLocked(p)
		return false
	case len(hashes) == 0:
		s.sync.prefixDone = true
	default:
		s.sync.prefix = hashes
		s.logf("p2p: fetching %d bodies of header-only blocks below the branch from %s first", len(hashes), p.addr)
	}
	return true
}

// popFetchedLocked drops from the front of the prefix and the queue the
// blocks whose bodies are now in (or that proved invalid).
func (s *Server) popFetchedLocked() {
	for len(s.sync.prefix) > 0 {
		if st, ok := s.ch.Status(s.sync.prefix[0]); ok && st == chain.StatusHeaderOnly {
			break
		}
		s.sync.prefix = s.sync.prefix[1:]
	}
	for len(s.sync.queue) > 0 {
		h := s.sync.queue[0].Hash()
		if st, ok := s.ch.Status(h); !ok || st == chain.StatusHeaderOnly {
			break
		}
		s.sync.queue = s.sync.queue[1:]
		delete(s.sync.queued, h)
	}
}

// finishSyncIfDoneLocked ends the sync when the peer's header chain is
// exhausted. The peer's claimed height is not consulted: anything further it
// has will arrive as announcements.
func (s *Server) finishSyncIfDoneLocked() {
	if s.sync.peer == nil || len(s.sync.queue) > 0 || !s.sync.headersDone {
		return
	}
	s.logf("p2p: sync with %s complete at height %d", s.sync.peer.addr, s.ch.Tip().Height)
	// Transactions announced while we were behind could not be checked; ask
	// for the pending set again now that their inputs are known.
	s.sync.peer.Send(&MsgMempool{})
	s.resetSyncLocked()
}

func (s *Server) onInv(p *Peer, m *MsgInv) {
	var want []InvVect
	var txs []consensus.Hash
	for _, it := range m.Items {
		p.markKnown(it.Hash)
		switch it.Type {
		case InvBlock:
			if s.ch.HasData(it.Hash) {
				continue
			}
			s.sync.mu.Lock()
			_, queued := s.sync.queued[it.Hash]
			_, requested := s.sync.requested[it.Hash]
			syncing := s.sync.peer != nil
			s.sync.mu.Unlock()
			if queued || requested {
				continue
			}
			// A header we already know (side chain) or a child of something
			// we have: fetch it from the announcer, sync or not. Unknown
			// lineage while not syncing: ask for headers instead.
			if n, known := s.ch.Node(it.Hash); known && n.Status == chain.StatusHeaderOnly {
				continue // body only matters if its branch wins; see MissingDataError
			}
			want = append(want, it)
			_ = syncing
		case InvTx:
			txs = append(txs, it.Hash)
		}
	}
	if len(want) > 0 {
		p.Send(&MsgGetData{Items: want})
	}
	// While downloading blocks a transaction's inputs are usually unknown;
	// it is fetched after the sync instead (MsgMempool). Only a sync that
	// is really delivering headers on an old tip counts: a peer merely
	// claiming a great height cannot switch transaction relay off.
	if len(txs) > 0 && !s.catchingUp() {
		s.queueTxAnnouncements(p, txs)
	}
}

func (s *Server) onBlock(p *Peer, blk *consensus.Block) {
	hash := blk.Header.Hash()
	p.markKnown(hash)

	s.sync.mu.Lock()
	_, fromQueue := s.sync.requested[hash]
	_, wanted := s.sync.wanted[hash]
	delete(s.sync.requested, hash)
	delete(s.sync.wanted, hash)
	fromSyncPeer := s.sync.peer == p
	s.sync.mu.Unlock()

	if !fromQueue && !wanted {
		if st, known := s.ch.Status(hash); known && st == chain.StatusHeaderOnly {
			// A body nobody asked for, of a header whose branch has no more
			// work than ours (or its body would have been asked for): not
			// worth the disk. It is fetched if the branch ever wins.
			return
		}
	}

	// Bodies downloaded through headers-first sync, or explicitly wanted for
	// a reorg, are kept even on a (currently) lighter branch.
	accepted := s.processBlock(p, blk, fromQueue || wanted)
	if accepted {
		p.markUseful(true)
	}

	s.sync.mu.Lock()
	if accepted && fromSyncPeer {
		s.sync.lastProgress = time.Now()
	}
	s.popFetchedLocked()
	if fromSyncPeer {
		s.syncStepLocked()
	}
	s.sync.mu.Unlock()
}

// processBlock feeds a block to the chain and turns the verdict into peer
// handling. Returns true if the block was stored (new knowledge).
func (s *Server) processBlock(p *Peer, blk *consensus.Block, keepBody bool) bool {
	hash := blk.Header.Hash()
	_, err := s.ch.ProcessBlockOpt(blk, time.Now().Unix(), keepBody)
	switch {
	case err == nil:
		s.logf("p2p: accepted block %s height %d from %s", hash, s.heightOf(hash), p.addr)
		s.retryOrphans(p, hash)
		return true
	case errors.Is(err, chain.ErrDuplicateBlock):
		return false
	case errors.Is(err, chain.ErrMissingBlockData):
		// The branch is heavier but we dropped some bodies: fetch them.
		var md *chain.MissingDataError
		if errors.As(err, &md) {
			s.requestBodies(p, md.Hashes)
		}
		return true
	case errors.Is(err, consensus.ErrBlockUnknownParent):
		s.cacheOrphan(p, blk)
		return false
	case errors.Is(err, consensus.ErrTimeTooNew):
		s.logf("p2p: block %s from %s is too far in the future; parked", hash, p.addr)
		s.park(hash)
		return false
	case chain.IsStorage(err):
		s.logf("p2p: storage problem while processing %s: %v", hash, err)
		return false
	case errors.Is(err, chain.ErrTooManyHeaders):
		s.logf("p2p: side-chain header cap reached; ignoring %s", hash)
		return false
	case errors.Is(err, consensus.ErrBlockMutated):
		// The header may be a real block: only this peer's copy is wrong.
		p.misbehave(banThreshold, "block "+hash.String()+" with a body that does not match its header")
		return false
	default:
		p.misbehave(banThreshold, "invalid block "+hash.String()+": "+err.Error())
		return false
	}
}

func (s *Server) park(h consensus.Hash) {
	s.sync.mu.Lock()
	s.sync.parked[h] = time.Now()
	s.sync.mu.Unlock()
}

// requestBodies asks p for bodies the chain needs to evaluate a heavier
// branch (oldest first), at most maxWantedPerRequest at once and maxWanted
// outstanding in all; the rest are asked for as these arrive, and
// retryWanted asks again for any that do not.
func (s *Server) requestBodies(p *Peer, hashes []consensus.Hash) {
	s.sync.mu.Lock()
	now := time.Now()
	var items []InvVect
	for _, h := range hashes {
		if len(items) >= maxWantedPerRequest {
			break
		}
		if _, r := s.sync.requested[h]; r {
			continue // the sync is fetching it
		}
		w, ok := s.sync.wanted[h]
		if ok && now.Sub(w.at) < s.cfg.wantedTimeout {
			continue
		}
		if !ok {
			if len(s.sync.wanted) >= maxWanted {
				break
			}
			w = &wantedBody{}
			s.sync.wanted[h] = w
		}
		w.at, w.from = now, p
		w.tries++
		items = append(items, InvVect{Type: InvBlock, Hash: h})
	}
	s.sync.mu.Unlock()
	sendGetData(p, items)
}

func sendGetData(p *Peer, items []InvVect) {
	for i := 0; i < len(items); i += maxBlocksPerGetData {
		end := i + maxBlocksPerGetData
		if end > len(items) {
			end = len(items)
		}
		p.Send(&MsgGetData{Items: items[i:end]})
	}
}

// retryWanted (from the sync watchdog) forgets wanted bodies that arrived
// some other way or are no longer needed, and asks again for those not
// delivered within requestTimeout (or reported not found): of another peer
// than last time if there is one, up to maxWantedTries times in all.
func (s *Server) retryWanted() {
	var peers []*Peer
	for _, p := range s.peerList() {
		if p.isHandshakeDone() && !p.gone() {
			peers = append(peers, p)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].id < peers[j].id })
	s.sync.mu.Lock()
	now := time.Now()
	asks := map[*Peer][]InvVect{}
	for h, w := range s.sync.wanted {
		if st, ok := s.ch.Status(h); !ok || st != chain.StatusHeaderOnly {
			delete(s.sync.wanted, h) // arrived, or no longer in the index
			continue
		}
		if now.Sub(w.at) < s.cfg.wantedTimeout {
			continue
		}
		if w.tries >= maxWantedTries || len(peers) == 0 {
			delete(s.sync.wanted, h)
			continue
		}
		next := peers[0] // the peer after the one asked last time
		for i, q := range peers {
			if q.id > w.from.id {
				next = peers[i]
				break
			}
		}
		w.at, w.from = now, next
		w.tries++
		asks[next] = append(asks[next], InvVect{Type: InvBlock, Hash: h})
	}
	s.sync.mu.Unlock()
	for p, items := range asks {
		sendGetData(p, items)
	}
}

func (s *Server) cacheOrphan(p *Peer, blk *consensus.Block) {
	// A body that is not the one its header commits to is nobody's block:
	// it must not take the slot of the real one (orphans are keyed by
	// parent). Cheap, so checked first.
	if err := consensus.CheckBlockCommitments(blk); err != nil {
		p.misbehave(banThreshold, "orphan block "+blk.Header.Hash().String()+" with a body that does not match its header")
		return
	}
	// Only proof-of-work-valid blocks earn a cache slot.
	if err := s.cfg.checkHeader(&blk.Header); err != nil {
		p.misbehave(banThreshold, "orphan fails proof of work")
		return
	}
	s.sync.mu.Lock()
	syncing := s.sync.peer != nil
	if len(s.sync.orphans) >= orphanCacheSize {
		var oldestKey consensus.Hash
		var oldest time.Time
		first := true
		for k, o := range s.sync.orphans {
			if first || o.added.Before(oldest) {
				oldestKey, oldest, first = k, o.added, false
			}
		}
		delete(s.sync.orphans, oldestKey)
	}
	s.sync.orphans[blk.Header.PrevHash] = orphan{blk: blk, added: time.Now()}
	s.sync.mu.Unlock()
	if !syncing {
		p.Send(&MsgGetHeaders{Locator: s.locator(s.ch.Tip())})
	}
}

func (s *Server) expireOrphans() {
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	for k, o := range s.sync.orphans {
		if time.Since(o.added) > orphanMaxAge {
			delete(s.sync.orphans, k)
		}
	}
}

func (s *Server) heightOf(h consensus.Hash) uint32 {
	if n, ok := s.ch.Node(h); ok {
		return n.Height
	}
	return 0
}

func (s *Server) retryOrphans(p *Peer, parent consensus.Hash) {
	for {
		s.sync.mu.Lock()
		o, ok := s.sync.orphans[parent]
		if ok {
			delete(s.sync.orphans, parent)
		}
		s.sync.mu.Unlock()
		if !ok {
			return
		}
		if _, err := s.ch.ProcessBlock(o.blk, time.Now().Unix()); err != nil {
			return
		}
		parent = o.blk.Header.Hash()
	}
}

// relayInv announces an item to every peer that does not know it yet.
// Blocks go out at once; transactions join the next batched inv.
func (s *Server) relayInv(it InvVect, except *Peer) {
	for _, p := range s.peerList() {
		if p == except || !p.isHandshakeDone() || p.knows(it.Hash) {
			continue
		}
		if it.Type == InvTx {
			p.markAnnounced(it.Hash, false)
			p.queueInv(it)
			continue
		}
		p.markKnown(it.Hash)
		p.Send(&MsgInv{Items: []InvVect{it}})
	}
}

// onTip relays newly connected tips. Old blocks connected during initial
// sync are not announced.
func (s *Server) onTip(tip *chain.Node) {
	if s.Syncing() && time.Since(time.Unix(int64(tip.Header.Time), 0)) > 24*time.Hour {
		return
	}
	s.relayInv(InvVect{Type: InvBlock, Hash: tip.Hash}, nil)
}

// ---- address book ----

const (
	maxAddrs = 10_000
	// maxAddrsPerSource bounds the gossiped addresses one source (the host
	// that told us; all peers whose address says nothing about them share
	// one) holds in the book.
	maxAddrsPerSource = 500
	// addrHorizon: a gossiped address we never connected to, that nobody
	// has mentioned (by its timestamp) for this long, is dropped.
	addrHorizon = 30 * 24 * time.Hour
	// unknownAddrSource is the source of addresses gossiped by peers whose
	// own address does not identify them (behind a proxy or NAT).
	unknownAddrSource = "?unknown"
)

type addrEntry struct {
	addr  NetAddress
	tried bool
	fails int
	// gossip: learned from a peer (or from peers.json), so it can expire
	// and be displaced; operator-supplied addresses (seeds, -connect, DNS
	// seeds) are not.
	gossip bool
	source string    // who told us, for maxAddrsPerSource ("" if not a peer)
	seen   time.Time // the latest time anyone vouched for it, never in the future
}

type addrBook struct {
	mu         sync.Mutex
	path       string
	allowLocal bool
	addrs      map[string]*addrEntry // "host:port"
	bySource   map[string]int        // gossiped entries per source
}

func newAddrBook(dir string, allowLocal bool) *addrBook {
	b := &addrBook{addrs: map[string]*addrEntry{}, bySource: map[string]int{}, allowLocal: allowLocal}
	if dir != "" {
		b.path = filepath.Join(dir, "peers.json")
		b.load()
	}
	return b
}

func (a NetAddress) String() string {
	ip := net.IP(a.IP[:])
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(int(a.Port)))
}

func parseNetAddress(s string) (NetAddress, bool) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return NetAddress{}, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return NetAddress{}, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return NetAddress{}, false
		}
		ip = ips[0]
	}
	var a NetAddress
	copy(a.IP[:], ip.To16())
	a.Port = uint16(port)
	a.Services = ServiceNetwork
	a.Time = uint64(time.Now().Unix())
	return a, true
}

// routable reports whether ip is a globally routable unicast address.
func routable(ip net.IP) bool {
	return !(ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsPrivate() || ip.IsInterfaceLocalMulticast())
}

// addString stores an operator-supplied address (seeds, -connect): trusted
// enough to accept local addresses.
func (b *addrBook) addString(s string, trusted bool) {
	if a, ok := parseNetAddress(s); ok {
		b.add(a, trusted)
	}
}

// add stores an address: trusted for operator-supplied ones (seeds,
// -connect, DNS seeds), which never expire or get displaced; otherwise as
// gossip from no particular source. Gossiped (untrusted) addresses must be
// routable unless AllowLocalAddrs is set. Returns true if something new was
// stored.
func (b *addrBook) add(a NetAddress, trusted bool) bool {
	return b.addAt(a, trusted, "", time.Now())
}

// addFrom stores an address gossiped by source (see addrSource).
func (b *addrBook) addFrom(a NetAddress, source string) bool {
	return b.addAt(a, false, source, time.Now())
}

// addrSource names who gossiped to us, for maxAddrsPerSource: the peer's
// host, or one shared name for all peers whose address says nothing.
func addrSource(p *Peer) string {
	if p.unknownIP || p.host == "" {
		return unknownAddrSource
	}
	return p.host
}

func (b *addrBook) addAt(a NetAddress, trusted bool, source string, now time.Time) bool {
	if a.Port == 0 {
		return false
	}
	ip := net.IP(a.IP[:])
	if ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if !trusted && !b.allowLocal && !routable(ip) {
		return false
	}
	// A timestamp in the future is worth no more than now.
	if a.Time > uint64(now.Unix()) {
		a.Time = uint64(now.Unix())
	}
	seen := time.Unix(int64(a.Time), 0)
	if trusted {
		seen = now
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := a.String()
	if e, ok := b.addrs[key]; ok {
		if a.Time > e.addr.Time {
			e.addr.Time = a.Time
		}
		if seen.After(e.seen) {
			e.seen = seen
		}
		if trusted && e.gossip {
			b.forgetSourceLocked(e)
			e.gossip, e.source = false, ""
		}
		return false
	}
	if !trusted {
		if now.Sub(seen) > addrHorizon {
			return false // stale already
		}
		if source != "" && b.bySource[source] >= maxAddrsPerSource {
			return false
		}
	}
	if len(b.addrs) >= maxAddrs {
		b.expireLocked(now)
		if len(b.addrs) >= maxAddrs && !b.evictLocked() {
			return false
		}
	}
	e := &addrEntry{addr: a, seen: seen}
	if !trusted {
		e.gossip, e.source = true, source
		if source != "" {
			b.bySource[source]++
		}
	}
	b.addrs[key] = e
	return true
}

func (b *addrBook) forgetSourceLocked(e *addrEntry) {
	if !e.gossip || e.source == "" {
		return
	}
	if b.bySource[e.source]--; b.bySource[e.source] <= 0 {
		delete(b.bySource, e.source)
	}
}

func (b *addrBook) deleteLocked(key string, e *addrEntry) {
	b.forgetSourceLocked(e)
	delete(b.addrs, key)
}

// expireLocked drops gossiped addresses we never connected to and nobody
// has vouched for within addrHorizon.
func (b *addrBook) expireLocked(now time.Time) {
	for k, e := range b.addrs {
		if e.gossip && !e.tried && now.Sub(e.seen) > addrHorizon {
			b.deleteLocked(k, e)
		}
	}
}

func (b *addrBook) expire(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked(now)
}

// evictLocked makes room for one more address by dropping a gossiped one we
// never connected to: from the source holding the most, the one vouched
// for longest ago. Operator-supplied and tried addresses are never
// displaced. Returns false if nothing may go.
func (b *addrBook) evictLocked() bool {
	var victimKey string
	var victim *addrEntry
	held := func(e *addrEntry) int {
		if e.source == "" {
			return 0
		}
		return b.bySource[e.source]
	}
	for k, e := range b.addrs {
		if !e.gossip || e.tried {
			continue
		}
		if victim == nil {
			victimKey, victim = k, e
			continue
		}
		if ne, nv := held(e), held(victim); ne > nv || (ne == nv && e.seen.Before(victim.seen)) {
			victimKey, victim = k, e
		}
	}
	if victim == nil {
		return false
	}
	b.deleteLocked(victimKey, victim)
	return true
}

func (b *addrBook) tried(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.addrs[key]; ok {
		now := time.Now()
		e.tried = true
		e.fails = 0
		e.seen = now
		// We just reached it: that is what its gossiped timestamp vouches
		// for, so peers we pass it on to do not drop it as stale.
		e.addr.Time = uint64(now.Unix())
	}
}

func (b *addrBook) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.addrs)
}

func (b *addrBook) failed(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.addrs[key]; ok {
		e.fails++
		if e.fails >= 5 && !e.tried || e.fails >= 20 {
			b.deleteLocked(key, e)
		}
	}
}

// random picks up to n addresses to dial, preferring ones we have connected
// to before (an attacker who floods gossip cannot displace them).
func (b *addrBook) random(n int, exclude map[string]struct{}) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var tried, fresh []string
	for k, e := range b.addrs {
		if _, ex := exclude[k]; ex {
			continue
		}
		if e.tried {
			tried = append(tried, k)
		} else {
			fresh = append(fresh, k)
		}
	}
	shuffle(tried)
	shuffle(fresh)
	var out []string
	for len(out) < n && (len(tried) > 0 || len(fresh) > 0) {
		pickTried := len(tried) > 0 && (len(fresh) == 0 || len(out)%2 == 0)
		if pickTried {
			out = append(out, tried[0])
			tried = tried[1:]
		} else {
			out = append(out, fresh[0])
			fresh = fresh[1:]
		}
	}
	return out
}

func (b *addrBook) sample(n int) []NetAddress {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]NetAddress, 0, len(b.addrs))
	for _, e := range b.addrs {
		if b.allowLocal || routable(net.IP(e.addr.IP[:])) {
			out = append(out, e.addr)
		}
	}
	shuffleAddrs(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func shuffle(s []string) {
	for i := len(s) - 1; i > 0; i-- {
		var rb [8]byte
		rand.Read(rb[:])
		j := int(binary.LittleEndian.Uint64(rb[:]) % uint64(i+1))
		s[i], s[j] = s[j], s[i]
	}
}

func shuffleAddrs(s []NetAddress) {
	for i := len(s) - 1; i > 0; i-- {
		var rb [8]byte
		rand.Read(rb[:])
		j := int(binary.LittleEndian.Uint64(rb[:]) % uint64(i+1))
		s[i], s[j] = s[j], s[i]
	}
}

type addrFile struct {
	Addrs []string `json:"addrs"`
	Tried []string `json:"tried"`
}

func (b *addrBook) load() {
	data, err := os.ReadFile(b.path)
	if err != nil {
		return
	}
	var f addrFile
	if json.Unmarshal(data, &f) != nil {
		return
	}
	// Addresses saved from gossip stay gossip: they expire and can be
	// displaced like any other (seeds and -connect are re-added as trusted).
	now := time.Now()
	for _, s := range f.Addrs {
		if a, ok := parseNetAddress(s); ok {
			b.addAt(a, false, "", now)
		}
	}
	for _, s := range f.Tried {
		if a, ok := parseNetAddress(s); ok {
			b.addAt(a, false, "", now)
			b.tried(s)
		}
	}
}

func (b *addrBook) save() {
	if b.path == "" {
		return
	}
	b.mu.Lock()
	f := addrFile{}
	for k, e := range b.addrs {
		if e.tried {
			f.Tried = append(f.Tried, k)
		} else {
			f.Addrs = append(f.Addrs, k)
		}
	}
	b.mu.Unlock()
	sort.Strings(f.Addrs)
	sort.Strings(f.Tried)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(b.path, data, 0o600)
}
