package main

import (
	"bytes"
	"container/list"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"argonmine/consensus"
	"argonmine/miner"
	"argonmine/wallet"
	"argonmine/workclient"
)

// The explorer doubles as the work server for browser and remote miners:
// it hands out block templates from its node that pay the requesting
// address, and submits the blocks they solve. No shares, no pool, no custody.
//
// Each node template is decoded once and shared by every job built from it:
// the template keeps its transactions serialized back to back plus the
// merkle branches of position 0 (txid and wtxid trees). A job is then only
// its coinbase and header: building one hashes the coinbase and folds the
// two branches, O(log n) whatever the size of the block.
//
// A job as served (GET /api/work) is workclient.Job plus the proof that it
// pays the miner:
//
//	coinbase        hex of the full coinbase transaction (with its witness)
//	merkle_branch   hex hashes; folding H(leaf, txid(coinbase)) with each
//	                (H(node, acc || sibling)) gives the header's tx root
//	witness_branch  the same over wtxids, giving the header's witness root
//
// The browser miner checks all of it before mining (site/static/miner-worker.js).

const (
	workTemplateTTL  = 10 * time.Second // reuse the node's template this long
	workJobMaxAge    = 2 * time.Minute  // forget jobs older than this (miners refetch every 20 s)
	workMaxJobs      = 20000            // jobs remembered at most; the oldest go first
	workJobsPerAddr  = 8                // jobs remembered per payout address; its oldest go first
	workMaxSubmits   = 3                // solutions checked per job
	workTick         = 3 * time.Second  // expiry and tip poll interval
	workCoinbaseTag  = "argonmine-web"  // coinbase witness of every job
	workJobIDBytes   = 16               // random job id: 128 bits
	workMaxJobIDHex  = 2 * workJobIDBytes
	workReasonBusy   = "the work server is busy checking other solutions; try again in a moment"
	workReasonLimit  = "too many requests; slow down"
	workMaxRefShare  = 50
	workNodeDownText = "node unavailable"
	// verdictSideChain is the node's submitblock answer for a valid block
	// that did not become the tip (rpc.SubmitSideChain; the explorer does
	// not link the node's packages).
	verdictSideChain = "inconclusive-side-chain"
)

// workBusyWait is how long a submission waits for a free hashing slot.
var workBusyWait = 2 * time.Second

// powHash is the proof-of-work hash (a variable so tests can watch it).
var powHash = consensus.PowHash

// workTemplate is one node template, decoded once.
type workTemplate struct {
	prev      consensus.Hash
	height    uint32
	version   uint32
	bits      uint32
	target    *big.Int
	targetHex string
	curTime   uint64
	minTime   uint64
	value     consensus.Amount // coinbase value: subsidy plus fees
	txBranch  []consensus.Hash
	wBranch   []consensus.Hash
	txHex     []string // txBranch as hex, served with every job
	wHex      []string
	body      []byte // the template's transactions, serialized back to back in block order
	ntx       int    // transactions in a block built from it, coinbase included
	digest    [32]byte
	fetched   time.Time
	stale     bool // its parent is no longer the tip
}

// workJob is what the server remembers of a job: about half a kilobyte.
type workJob struct {
	id       string
	address  string
	tmpl     *workTemplate
	header   consensus.Header // nonce 0
	coinbase []byte           // full serialization
	created  time.Time
	nonces   []uint64 // submitted so far (at most workMaxSubmits)
	elem     *list.Element
}

// workJobJSON is the job as served: the miner protocol's job plus the
// coinbase and merkle branches that let the miner check that it is paid.
type workJobJSON struct {
	workclient.Job
	Coinbase      string   `json:"coinbase"`
	MerkleBranch  []string `json:"merkle_branch"`
	WitnessBranch []string `json:"witness_branch"`
}

type tmplCall struct {
	done chan struct{}
	t    *workTemplate
	err  error
}

type workServer struct {
	client      *wallet.Client
	params      *consensus.Params
	logf        func(string, ...any)
	referralPct int // percent of a block reward paid to a referrer when a miner names one

	sem chan struct{} // one slot per CPU for the Argon2id check of a submission

	mu       sync.Mutex
	tmpl     *workTemplate
	fetching *tmplCall
	jobs     map[string]*workJob
	order    *list.List            // jobs, oldest first
	byAddr   map[string][]*workJob // jobs per payout address, oldest first
	waiting  int                   // submissions queued for a hashing slot
	found    uint64
	lastErr  string // internal: logged and counted, never shown publicly
	lastSeen time.Time
}

func newWorkServer(client *wallet.Client, params *consensus.Params, referralPct int, logf func(string, ...any)) *workServer {
	if referralPct < 0 {
		referralPct = 0
	}
	if referralPct > workMaxRefShare {
		referralPct = workMaxRefShare
	}
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return &workServer{client: client, params: params, referralPct: referralPct, logf: logf,
		sem: make(chan struct{}, n), jobs: map[string]*workJob{}, order: list.New(), byAddr: map[string][]*workJob{}}
}

// run expires old jobs and follows the node's tip until stop is closed:
// jobs whose parent is no longer the tip are dropped, so their miners get a
// "stale" answer and fetch new work.
func (ws *workServer) run(stop <-chan struct{}) {
	t := time.NewTicker(workTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		ws.mu.Lock()
		ws.expireLocked(time.Now())
		live := len(ws.jobs) > 0 || ws.tmpl != nil
		ws.mu.Unlock()
		if !live || ws.client == nil {
			continue
		}
		raw, err := ws.client.Call("getbestblockhash")
		if err != nil {
			ws.noteErr("getbestblockhash", err)
			continue
		}
		var hs string
		if json.Unmarshal(raw, &hs) != nil {
			continue
		}
		if tip, err := consensus.HashFromHex(hs); err == nil {
			ws.tipIs(tip)
			ws.mu.Lock()
			ws.lastErr = ""
			ws.mu.Unlock()
		}
	}
}

func (ws *workServer) noteErr(what string, err error) {
	ws.mu.Lock()
	changed := ws.lastErr == ""
	ws.lastErr = what + ": " + err.Error()
	ws.mu.Unlock()
	if changed {
		ws.logf("work: %s: %v", what, err)
	}
}

// tipIs drops every job built on another parent and the cached template if
// it is one of them.
func (ws *workServer) tipIs(tip consensus.Hash) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.tipIsLocked(tip)
}

func (ws *workServer) tipIsLocked(tip consensus.Hash) {
	if ws.tmpl != nil && ws.tmpl.prev != tip {
		ws.tmpl.stale = true
		ws.tmpl = nil
	}
	for e := ws.order.Front(); e != nil; {
		next := e.Next()
		j := e.Value.(*workJob)
		if j.tmpl.prev != tip {
			j.tmpl.stale = true
			ws.removeLocked(j)
		}
		e = next
	}
}

// expireLocked drops jobs older than workJobMaxAge (oldest first, so it
// stops at the first young one).
func (ws *workServer) expireLocked(now time.Time) {
	for e := ws.order.Front(); e != nil; e = ws.order.Front() {
		j := e.Value.(*workJob)
		if now.Sub(j.created) <= workJobMaxAge {
			return
		}
		ws.removeLocked(j)
	}
}

func (ws *workServer) removeLocked(j *workJob) {
	if _, ok := ws.jobs[j.id]; !ok {
		return
	}
	delete(ws.jobs, j.id)
	ws.order.Remove(j.elem)
	list := ws.byAddr[j.address]
	for i, x := range list {
		if x == j {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(ws.byAddr, j.address)
	} else {
		ws.byAddr[j.address] = list
	}
}

func (ws *workServer) addLocked(j *workJob) {
	if old := ws.byAddr[j.address]; len(old) >= workJobsPerAddr {
		ws.removeLocked(old[0])
	}
	for len(ws.jobs) >= workMaxJobs {
		ws.removeLocked(ws.order.Front().Value.(*workJob))
	}
	j.elem = ws.order.PushBack(j)
	ws.jobs[j.id] = j
	ws.byAddr[j.address] = append(ws.byAddr[j.address], j)
}

// template returns the node's current template, cached briefly. Concurrent
// callers share one fetch.
func (ws *workServer) template() (*workTemplate, error) {
	ws.mu.Lock()
	if t := ws.tmpl; t != nil && time.Since(t.fetched) < workTemplateTTL {
		ws.mu.Unlock()
		return t, nil
	}
	if c := ws.fetching; c != nil {
		ws.mu.Unlock()
		<-c.done
		return c.t, c.err
	}
	c := &tmplCall{done: make(chan struct{})}
	ws.fetching = c
	ws.mu.Unlock()

	c.t, c.err = ws.fetchTemplate()

	ws.mu.Lock()
	ws.fetching = nil
	if c.err == nil {
		// An unchanged template (same parent, transactions and value) keeps
		// the object the existing jobs share.
		if old := ws.tmpl; old != nil && old.digest == c.t.digest {
			old.fetched = c.t.fetched
			c.t = old
		} else {
			if old != nil && old.prev != c.t.prev {
				ws.tipIsLocked(c.t.prev)
			}
			ws.tmpl = c.t
		}
		ws.lastErr = ""
	}
	ws.mu.Unlock()
	close(c.done)
	return c.t, c.err
}

func (ws *workServer) fetchTemplate() (*workTemplate, error) {
	if ws.client == nil {
		return nil, errors.New("no node")
	}
	raw, err := ws.client.Call("getblocktemplate")
	if err != nil {
		return nil, err
	}
	var t miner.TemplateJSON
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return newWorkTemplate(&t, time.Now())
}

// newWorkTemplate decodes a node template once: every transaction is parsed
// (so a bad template fails here, not in a miner's block), its ids feed the
// merkle branches, and its bytes are kept for the blocks submitted from it.
func newWorkTemplate(t *miner.TemplateJSON, now time.Time) (*workTemplate, error) {
	prev, err := consensus.HashFromHex(t.PreviousBlockHash)
	if err != nil {
		return nil, fmt.Errorf("template: previousblockhash: %w", err)
	}
	var bits uint32
	if _, err := fmt.Sscanf(t.Bits, "%x", &bits); err != nil {
		return nil, fmt.Errorf("template: bits: %w", err)
	}
	target, err := consensus.CompactToTarget(bits)
	if err != nil {
		return nil, fmt.Errorf("template: bits: %w", err)
	}
	if t.CoinbaseValue < 0 || t.CoinbaseValue > consensus.MaxMoney {
		return nil, fmt.Errorf("template: coinbase value out of range")
	}
	wt := &workTemplate{prev: prev, height: t.Height, version: t.Version, bits: bits, target: target, targetHex: fmt.Sprintf("%064x", target),
		curTime: t.CurTime, minTime: t.MinTime, value: t.CoinbaseValue, ntx: 1 + len(t.Transactions), fetched: now}
	txids := make([]consensus.Hash, 0, len(t.Transactions))
	wtxids := make([]consensus.Hash, 0, len(t.Transactions))
	var body bytes.Buffer
	d := sha256.New()
	fmt.Fprintf(d, "%s|%d|%d|%d|%d|", t.PreviousBlockHash, t.Height, t.Version, bits, t.CoinbaseValue)
	for i, tx := range t.Transactions {
		b, err := hex.DecodeString(tx.Data)
		if err != nil {
			return nil, fmt.Errorf("template: tx %d: %w", i, err)
		}
		dtx, err := consensus.DecodeTx(b)
		if err != nil {
			return nil, fmt.Errorf("template: tx %d: %w", i, err)
		}
		id := dtx.TxID()
		txids = append(txids, id)
		wtxids = append(wtxids, dtx.WTxID())
		d.Write(id[:])
		body.Write(b)
	}
	copy(wt.digest[:], d.Sum(nil))
	wt.body = body.Bytes()
	wt.txBranch, wt.wBranch = merkleBranch0(txids), merkleBranch0(wtxids)
	wt.txHex, wt.wHex = hashesHex(wt.txBranch), hashesHex(wt.wBranch)
	return wt, nil
}

func hashesHex(hs []consensus.Hash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.String()
	}
	return out
}

// merkleBranch0 returns the siblings on the path from position 0 to the root
// of the tagged, promote-odd tree (consensus.MerkleRoot) over a list whose
// position 0 is the coinbase and whose other positions are ids. Position 0
// always pairs with position 1, so no sibling depends on the coinbase.
func merkleBranch0(ids []consensus.Hash) []consensus.Hash {
	level := make([]consensus.Hash, 1+len(ids))
	for i, id := range ids {
		level[i+1] = consensus.TaggedHash(consensus.TagMerkleLeaf, id[:])
	}
	var branch []consensus.Hash
	for len(level) > 1 {
		branch = append(branch, level[1])
		next := make([]consensus.Hash, 0, (len(level)+1)/2)
		for i := 0; i+1 < len(level); i += 2 {
			if i == 0 {
				next = append(next, consensus.Hash{}) // the coinbase's side, unknown here
				continue
			}
			next = append(next, consensus.TaggedHash(consensus.TagMerkleNode, level[i][:], level[i+1][:]))
		}
		if len(level)%2 == 1 {
			next = append(next, level[len(level)-1])
		}
		level = next
	}
	return branch
}

// merkleFold computes the root from a position-0 id and its branch.
func merkleFold(id consensus.Hash, branch []consensus.Hash) consensus.Hash {
	acc := consensus.TaggedHash(consensus.TagMerkleLeaf, id[:])
	for _, s := range branch {
		acc = consensus.TaggedHash(consensus.TagMerkleNode, acc[:], s[:])
	}
	return acc
}

// errWorkInput is a request the miner must fix; anything else is the node's fault.
type errWorkInput struct{ msg string }

func (e errWorkInput) Error() string { return e.msg }

func newJobID() string {
	var b [workJobIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("work: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Job builds a block template paying address. With a referrer, the coinbase
// is split: the referral share goes to the referrer, the rest to the miner.
// From the treasury height the coinbase's last output pays the treasury.
func (ws *workServer) Job(address, referrer string) (*workJobJSON, error) {
	payout, err := wallet.DecodeAddress(ws.params.HRP, address)
	if err != nil {
		return nil, errWorkInput{"invalid payout address"}
	}
	var refOut *consensus.TxOut
	if referrer != "" {
		if referrer == address {
			return nil, errWorkInput{"the referrer cannot be your own address"}
		}
		ro, err := wallet.DecodeAddress(ws.params.HRP, referrer)
		if err != nil {
			return nil, errWorkInput{"invalid referrer address"}
		}
		refOut = &ro
	}
	t, err := ws.template()
	if err != nil {
		ws.noteErr("getblocktemplate", err)
		return nil, errors.New(workNodeDownText)
	}
	// From the treasury height the treasury's 1% of the subsidy comes off
	// first (consensus/treasury.go); the referral share is of what remains.
	value := t.value
	treasury, payTreasury := consensus.TreasuryOutput(ws.params, t.height)
	if payTreasury {
		if treasury.Amount > value {
			return nil, errors.New(workNodeDownText)
		}
		value -= treasury.Amount
	}
	outs := []consensus.TxOut{{Amount: value, Type: payout.Type, Payload: payout.Payload}}
	pct := 0
	if refOut != nil && ws.referralPct > 0 {
		share := value / 100 * consensus.Amount(ws.referralPct)
		if share > 0 {
			outs = []consensus.TxOut{{Amount: value - share, Type: payout.Type, Payload: payout.Payload}, {Amount: share, Type: refOut.Type, Payload: refOut.Payload}}
			pct = ws.referralPct
		}
	}
	if payTreasury {
		outs = append(outs, treasury)
	}
	cb := &consensus.Tx{
		Version:    consensus.TxVersion,
		LockHeight: t.height,
		Inputs:     []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs:    outs,
		Witnesses:  [][]byte{[]byte(workCoinbaseTag)},
	}
	now := time.Now()
	tm := uint64(now.Unix())
	if tm < t.minTime {
		tm = t.minTime
	}
	if tm < t.curTime {
		tm = t.curTime
	}
	j := &workJob{
		id:      newJobID(),
		address: address,
		tmpl:    t,
		header: consensus.Header{
			Version:     t.version,
			PrevHash:    t.prev,
			TxRoot:      merkleFold(cb.TxID(), t.txBranch),
			WitnessRoot: merkleFold(cb.WTxID(), t.wBranch),
			Time:        tm,
			Bits:        t.bits,
		},
		coinbase: cb.Serialize(),
		created:  now,
	}
	hdr := j.header.Serialize()

	ws.mu.Lock()
	if t.stale {
		ws.mu.Unlock()
		return nil, errors.New(workNodeDownText) // the tip moved during the build; the miner retries
	}
	ws.expireLocked(now)
	ws.addLocked(j)
	ws.lastSeen = now
	ws.mu.Unlock()

	job := &workJobJSON{
		Job: workclient.Job{
			ID:           j.id,
			HeaderPrefix: hex.EncodeToString(hdr[:workclient.HeaderPrefixLen]),
			Target:       t.targetHex,
			Height:       t.height,
			Network:      ws.params.Name,
			Algorithm:    "argon2id",
		},
		Coinbase:      hex.EncodeToString(j.coinbase),
		MerkleBranch:  t.txHex,
		WitnessBranch: t.wHex,
	}
	if pct > 0 {
		job.Referrer, job.ReferralPct = referrer, pct
	}
	return job, nil
}

// Submit verifies a solution and hands the block to the node.
func (ws *workServer) Submit(s workclient.Submit) workclient.Result {
	r, _ := ws.submit(s)
	return r
}

// submit is Submit that also reports whether the answer is "busy" (no
// hashing slot freed up in time), which the handler sends as 429.
func (ws *workServer) submit(s workclient.Submit) (workclient.Result, bool) {
	if _, err := wallet.DecodeAddress(ws.params.HRP, s.Address); err != nil {
		return workclient.Result{Reason: "invalid payout address"}, false
	}
	ws.mu.Lock()
	j, ok := ws.jobs[s.JobID]
	switch {
	case !ok:
		ws.mu.Unlock()
		return workclient.Result{Stale: true, Reason: "unknown or expired job; fetch new work"}, false
	case j.address != s.Address:
		ws.mu.Unlock()
		return workclient.Result{Reason: "this job was issued for another address"}, false
	case j.tmpl.stale:
		ws.removeLocked(j)
		ws.mu.Unlock()
		return workclient.Result{Stale: true, Reason: "the chain moved on; fetch new work"}, false
	}
	for _, n := range j.nonces {
		if n == s.Nonce {
			ws.mu.Unlock()
			return workclient.Result{Reason: "duplicate submission"}, false
		}
	}
	if len(j.nonces) >= workMaxSubmits {
		ws.mu.Unlock()
		return workclient.Result{Stale: true, Reason: "too many submissions for this job; fetch new work"}, false
	}
	j.nonces = append(j.nonces, s.Nonce)
	ws.mu.Unlock()

	// One Argon2id evaluation (8 MiB), outside the lock, at most one per CPU
	// at a time. A submission waits briefly for a slot; when none frees up it
	// is answered "busy" and not counted against the job, so the miner can
	// send the same nonce again.
	if !ws.acquire() {
		ws.mu.Lock()
		for i, n := range j.nonces {
			if n == s.Nonce {
				j.nonces = append(j.nonces[:i:i], j.nonces[i+1:]...)
				break
			}
		}
		ws.mu.Unlock()
		return workclient.Result{Reason: workReasonBusy}, true
	}
	hdr := j.header
	hdr.Nonce = s.Nonce
	above := consensus.HashToBig(powHash(hdr.Serialize())).Cmp(j.tmpl.target) > 0
	<-ws.sem
	if above {
		return workclient.Result{Reason: "hash above target"}, false
	}

	var w consensus.Writer
	w.Raw(hdr.Serialize())
	w.Varint(uint64(j.tmpl.ntx))
	w.Raw(j.coinbase)
	w.Raw(j.tmpl.body)
	hash := hdr.Hash()
	raw, err := ws.client.Call("submitblock", hex.EncodeToString(w.Bytes()))
	if err != nil {
		ws.noteErr("submitblock", err)
		ws.logf("work: block %s at height %d could not be submitted: %v", hash, j.tmpl.height, err)
		return workclient.Result{Reason: workNodeDownText}, false
	}
	var verdict *string
	json.Unmarshal(raw, &verdict)
	if verdict != nil {
		v := *verdict
		switch {
		case v == verdictSideChain:
			ws.logf("work: block %s at height %d by %s is valid but on a side branch (the chain moved on)", hash, j.tmpl.height, s.Address)
			return workclient.Result{Stale: true, Hash: hash.String(), Reason: "stale: the chain moved on before the block arrived; fetch new work"}, false
		case v == "duplicate":
			return workclient.Result{Stale: true, Hash: hash.String(), Reason: "duplicate: this block was already submitted"}, false
		case strings.HasPrefix(v, "inconclusive") || strings.Contains(v, "stale") || strings.Contains(v, "prev"):
			ws.logf("work: block %s at height %d is stale: %s", hash, j.tmpl.height, v)
			return workclient.Result{Stale: true, Reason: "stale: " + v + "; fetch new work"}, false
		default:
			ws.logf("work: block %s at height %d rejected: %s", hash, j.tmpl.height, v)
			return workclient.Result{Reason: v}, false
		}
	}
	ws.mu.Lock()
	ws.found++
	ws.mu.Unlock()
	ws.tipIs(hash) // every other job is now stale
	ws.logf("work: block %s at height %d found by %s", hash, j.tmpl.height, s.Address)
	return workclient.Result{Accepted: true, Hash: hash.String()}, false
}

// acquire takes a hashing slot, waiting up to workBusyWait. At most a few
// submissions per slot wait at all; the rest are told the server is busy.
func (ws *workServer) acquire() bool {
	select {
	case ws.sem <- struct{}{}:
		return true
	default:
	}
	ws.mu.Lock()
	if ws.waiting >= 4*cap(ws.sem) {
		ws.mu.Unlock()
		return false
	}
	ws.waiting++
	ws.mu.Unlock()
	defer func() {
		ws.mu.Lock()
		ws.waiting--
		ws.mu.Unlock()
	}()
	t := time.NewTimer(workBusyWait)
	defer t.Stop()
	select {
	case ws.sem <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

// status is included in /api/stats: blocks found, and whether the last
// contact with the node failed (details are in the log only).
func (ws *workServer) status() (found uint64, nodeDown bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.found, ws.lastErr != ""
}

func (s *server) apiWork(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	payout := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("address")))
	job, err := s.work.Job(payout, strings.ToLower(strings.TrimSpace(r.URL.Query().Get("ref"))))
	if err != nil {
		code := http.StatusServiceUnavailable
		var in errWorkInput
		if errors.As(err, &in) {
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}
	if s.miners != nil {
		s.miners.note(payout, s.clientIP(r), time.Now())
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(job)
}

// apiWorkSubmit takes a solution. Browsers may call it only from the
// configured web app origins (-allow-origin), only as application/json (a
// form or text/plain post from another site is refused before any work).
func (s *server) apiWorkSubmit(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	w.Header().Add("Vary", "Origin")
	if origin != "" {
		if !s.origins.allowed(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var sub workclient.Submit
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&sub); err != nil || len(sub.JobID) != workMaxJobIDHex {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sub.Address = strings.ToLower(strings.TrimSpace(sub.Address))
	res, busy := s.work.submit(sub)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if busy {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	json.NewEncoder(w).Encode(res)
}

// isJSON reports whether a Content-Type is application/json (parameters
// such as charset allowed).
func isJSON(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.EqualFold(strings.TrimSpace(mt), "application/json")
}
