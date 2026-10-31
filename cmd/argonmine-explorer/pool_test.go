package main

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"argonmine/consensus"
	"argonmine/miner"
	"argonmine/wallet"
	"argonmine/workclient"
)

// A hard template: shares are far easier than blocks, so a submission can
// be a share without being a block.
func hardTemplate(p *consensus.Params) *miner.TemplateJSON {
	tj := fullTemplate(p, 2000)
	tj.Bits = "1c0fffff"
	return tj
}

func enableTestPool(t *testing.T, ws *workServer, w *wallet.Wallet, dir string) string {
	t.Helper()
	addr := randomAddr(t, consensus.RegTest)
	if w != nil {
		var err error
		if addr, err = w.MiningAddress(); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.pool.enable(poolConfig{Address: addr, FeeBps: 100, MinPayout: 10_000_000, StatePath: filepath.Join(dir, "ledger.json"), Wallet: w}); err != nil {
		t.Fatal(err)
	}
	return addr
}

// hashAt makes powHash return a fixed value, so a submission's fate is chosen.
func hashAt(t *testing.T, v *big.Int) {
	t.Helper()
	old := powHash
	powHash = func([]byte) consensus.Hash {
		// The inverse of consensus.HashToBig: little-endian bytes.
		var h consensus.Hash
		b := v.Bytes()
		for i := range b {
			h[i] = b[len(b)-1-i]
		}
		return h
	}
	t.Cleanup(func() { powHash = old })
}

// TestPoolSharesAreCreditedByWeight: pool jobs pay the pool, serve the share
// target as the target and the block target beside it; a hash under the
// share target is a share worth the job's difficulty, one above is refused,
// duplicates are refused, and the ledger survives a restart.
func TestPoolSharesAreCreditedByWeight(t *testing.T) {
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	tmpl := fixedTemplate(t, ws, hardTemplate(consensus.RegTest))
	dir := t.TempDir()
	poolAddr := enableTestPool(t, ws, nil, dir)
	a, b := randomAddr(t, consensus.RegTest), randomAddr(t, consensus.RegTest)

	if _, err := ws.pool.Job("nope"); err == nil {
		t.Fatal("bad miner address accepted")
	}
	job, err := ws.pool.Job(a)
	if err != nil {
		t.Fatal(err)
	}
	if job.BlockTarget != tmpl.targetHex {
		t.Fatalf("block target %s, want %s", job.BlockTarget, tmpl.targetHex)
	}
	share, _ := new(big.Int).SetString(job.Target, 16)
	if share.Cmp(tmpl.target) <= 0 {
		t.Fatal("the share target is not easier than the block target")
	}
	if want := targetFor(poolStartDiff, tmpl.target); share.Cmp(want) != 0 {
		t.Fatalf("share target %x, want %x", share, want)
	}
	cb := checkJobProof(t, job, poolAddr, "", 0)
	if len(cb.Outputs) != 1 || cb.Outputs[0].Amount != consensus.Subsidy(1) || string(cb.Witnesses[0]) != poolTag {
		t.Fatalf("pool coinbase: %+v tag %q", cb.Outputs, cb.Witnesses[0])
	}

	// Under the share target, above the block target: a share, no block.
	hashAt(t, new(big.Int).Sub(share, big.NewInt(1)))
	r := ws.Submit(workclient.Submit{JobID: job.ID, Nonce: 1, Address: a})
	if !r.Share || r.Accepted || r.Stale || r.Reason != "" {
		t.Fatalf("share: %+v", r)
	}
	if r := ws.Submit(workclient.Submit{JobID: job.ID, Nonce: 1, Address: a}); r.Share || r.Reason != "duplicate submission" {
		t.Fatalf("duplicate: %+v", r)
	}
	if r := ws.Submit(workclient.Submit{JobID: job.ID, Nonce: 2, Address: b}); r.Share || !strings.Contains(r.Reason, "another address") {
		t.Fatalf("other miner on a's job: %+v", r)
	}
	// Above the share target: refused and counted.
	hashAt(t, new(big.Int).Add(share, big.NewInt(1)))
	if r := ws.Submit(workclient.Submit{JobID: job.ID, Nonce: 3, Address: a}); r.Share || r.Reason != "hash above share target" {
		t.Fatalf("bad share: %+v", r)
	}
	// b sends three shares on its own job.
	jb, _ := ws.pool.Job(b)
	hashAt(t, big.NewInt(1))
	ws.client = nil // a block cannot be handed in here; make the hash a share only
	hashAt(t, new(big.Int).Sub(share, big.NewInt(2)))
	for n := uint64(10); n < 13; n++ {
		if r := ws.Submit(workclient.Submit{JobID: jb.ID, Nonce: n, Address: b}); !r.Share {
			t.Fatalf("b share %d: %+v", n, r)
		}
	}
	st := ws.pool.stats()
	if st.SharesAccepted != 4 || st.SharesRejected != 1 || st.RoundShares != 4*poolStartDiff || st.Miners != 2 {
		t.Fatalf("stats: %+v", st)
	}
	ma, mb := ws.pool.miner(a), ws.pool.miner(b)
	if ma.RoundShares != poolStartDiff || mb.RoundShares != 3*poolStartDiff || ma.RoundTotal != 4*poolStartDiff {
		t.Fatalf("miner views: a %+v b %+v", ma, mb)
	}

	// The ledger is written within poolSaveEvery; a new pool on the same file
	// carries the round on.
	ws.pool.save()
	ws2 := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	if err := ws2.pool.enable(poolConfig{Address: poolAddr, FeeBps: 100, MinPayout: 1, StatePath: filepath.Join(dir, "ledger.json")}); err != nil {
		t.Fatal(err)
	}
	if st2 := ws2.pool.stats(); st2.RoundShares != 4*poolStartDiff || st2.SharesAccepted != 4 {
		t.Fatalf("reloaded: %+v", st2)
	}
}

// TestPoolVardiff: a miner sending shares too often gets a harder share
// target (and its weight), one sending none gets an easier one, within the
// floor and the block difficulty.
func TestPoolVardiff(t *testing.T) {
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	tmpl := fixedTemplate(t, ws, hardTemplate(consensus.RegTest))
	enableTestPool(t, ws, nil, t.TempDir())
	a := randomAddr(t, consensus.RegTest)
	p := ws.pool
	now := time.Now()
	p.mu.Lock()
	m := p.minerLocked(a, now)
	m.lastAdjust, m.sharesSince = now.Add(-poolAdjustEvery-time.Second), 40 // 40 shares in 31 s: far too many
	p.adjustLocked(m, now)
	fast := m.diff
	m.lastAdjust, m.sharesSince = now.Add(-poolAdjustEvery-time.Second), 0 // none in 31 s: not yet evidence
	p.adjustLocked(m, now)
	same := m.diff
	m.lastAdjust, m.sharesSince = now.Add(-4*poolShareInterval-time.Second), 0 // none in a minute: easier
	p.adjustLocked(m, now)
	slow := m.diff
	for i := 0; i < 20; i++ { // keeps easing down to the floor
		m.lastAdjust = now.Add(-4*poolShareInterval - time.Second)
		p.adjustLocked(m, now)
	}
	floor := m.diff
	p.mu.Unlock()
	if fast != 4*poolStartDiff || same != fast || slow != fast/2 || floor != poolMinDiff {
		t.Fatalf("diff after fast %d same %d slow %d floor %d", fast, same, slow, floor)
	}
	// A job carries the current difficulty; the share target follows it.
	job, _ := p.Job(a)
	share, _ := new(big.Int).SetString(job.Target, 16)
	if share.Cmp(targetFor(poolMinDiff, tmpl.target)) != 0 {
		t.Fatalf("share target %x for diff %d", share, poolMinDiff)
	}
	// Never harder than the block: a huge difficulty is clamped and the share
	// is then worth the block's difficulty.
	p.mu.Lock()
	m.diff = poolMaxDiff
	p.mu.Unlock()
	job, _ = p.Job(a)
	if job.Target != job.BlockTarget {
		t.Fatalf("share target %s not clamped to block target %s", job.Target, job.BlockTarget)
	}
	ws.mu.Lock()
	j := ws.jobs[job.ID]
	ws.mu.Unlock()
	if j.shareDiff != diffOf(tmpl.target) {
		t.Fatalf("clamped share weight %d, want the block difficulty %d", j.shareDiff, diffOf(tmpl.target))
	}
}

// TestPoolRoundsMatureAndPay runs the whole pool against a regtest node: a
// miner core mines pool jobs over HTTP (on regtest every share is a block),
// rounds mature after CoinbaseMaturity blocks, the pool wallet pays the
// miner, and an orphaned round's shares move to the next round.
func TestPoolRoundsMatureAndPay(t *testing.T) {
	node := newTestNode(t)
	client := node.client
	dir := t.TempDir()
	pw, _, err := wallet.Create(filepath.Join(dir, "pool-wallet.json"), consensus.RegTest, "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	pw.SetClient(client)
	ws := newWorkServer(client, consensus.RegTest, 5, t.Logf)
	poolAddr := enableTestPool(t, ws, pw, dir)
	s := newServer(consensus.RegTest, client, nil, ws, nil, "", "")
	web := httptest.NewServer(s.routes())
	defer web.Close()

	miner := testKeyAddr(t, 0)
	mc := workclient.New(web.URL+"/pool", miner, 2, t.Logf)
	mc.Start()
	waitFor(t, "three pool blocks", 60*time.Second, func() bool { return ws.pool.stats().BlocksFound >= 3 })
	mc.Stop()
	if mc.Shares() == 0 || mc.Blocks() == 0 {
		t.Fatalf("client counters: shares %d blocks %d", mc.Shares(), mc.Blocks())
	}
	st := ws.pool.stats()
	if st.Pending != st.BlocksFound || st.TotalPaid != 0 {
		t.Fatalf("before maturity: %+v", st)
	}
	for _, r := range st.Rounds {
		if r.Status != "pending" || r.Reward != consensus.Subsidy(r.Height) || r.FoundBy != miner || r.TotalShares == 0 {
			t.Fatalf("round: %+v", r)
		}
		h, _ := client.BlockHash(r.Height)
		if h != r.BlockHash {
			t.Fatalf("round %d hash %s, chain %s", r.Height, r.BlockHash, h)
		}
		blk, _ := client.Block(h)
		if blk.Tx[0].Outputs[0].Address != poolAddr {
			t.Fatalf("pool block %d pays %s", r.Height, blk.Tx[0].Outputs[0].Address)
		}
	}
	// Nothing matures before CoinbaseMaturity.
	ws.pool.tick()
	if st := ws.pool.stats(); st.Owed != 0 || st.TotalPaid != 0 {
		t.Fatalf("paid before maturity: %+v", st)
	}
	// A round whose block is not on the chain: orphaned once six blocks deep,
	// its shares carried over.
	ws.pool.mu.Lock()
	ws.pool.state.Rounds = append(ws.pool.state.Rounds, poolRound{Height: 2, BlockHash: strings.Repeat("ab", 32), Reward: consensus.Subsidy(2), Shares: map[string]uint64{miner: 7}, TotalShares: 7, Status: "pending"})
	before := ws.pool.state.Round[miner]
	ws.pool.mu.Unlock()

	other := testKeyAddr(t, 1)
	if _, err := client.Call("generatetoaddress", int(consensus.CoinbaseMaturity), other); err != nil {
		t.Fatal(err)
	}
	ws.pool.tick()
	st = ws.pool.stats()
	if st.Error != "" {
		t.Fatalf("tick: %s", st.Error)
	}
	var matured, orphaned int
	var fees consensus.Amount
	for _, r := range st.Rounds {
		switch r.Status {
		case "matured":
			matured++
			fees += r.Fee
			if r.Fee != r.Reward/100 {
				t.Fatalf("fee %d on reward %d", r.Fee, r.Reward)
			}
		case "orphaned":
			orphaned++
		}
	}
	if matured != 3 || orphaned != 1 || st.Fees != fees {
		t.Fatalf("after maturity: matured %d orphaned %d, %+v", matured, orphaned, st)
	}
	if got := ws.pool.miner(miner).RoundShares; got != before+7 {
		t.Fatalf("orphaned shares not carried: %d, want %d", got, before+7)
	}
	// The pool wallet paid the miner everything matured, in one transaction.
	want := 3 * (consensus.Subsidy(1) - consensus.Subsidy(1)/100)
	if st.TotalPaid != want || st.Owed != 0 || len(st.Payouts) != 1 || st.Payouts[0].Amounts[miner] != want {
		t.Fatalf("payout: %+v", st)
	}
	m := ws.pool.miner(miner)
	if m.Paid != want || m.Balance != 0 || m.Pending != 0 {
		t.Fatalf("miner view: %+v", m)
	}
	// Confirm it: the payout is in the next block, paying the miner.
	if _, err := client.Call("generatetoaddress", 1, other); err != nil {
		t.Fatal(err)
	}
	tipHash, _ := client.BlockHash(node.ch.Tip().Height)
	blk, _ := client.Block(tipHash)
	paid := false
	for _, tx := range blk.Tx {
		if tx.TxID == st.Payouts[0].TxID {
			for _, o := range tx.Outputs {
				if o.Address == miner && o.Amount == want {
					paid = true
				}
			}
		}
	}
	if !paid {
		t.Fatalf("payout %s not confirmed to %s", st.Payouts[0].TxID, miner)
	}
	// A second tick pays nothing more.
	ws.pool.tick()
	if st2 := ws.pool.stats(); st2.TotalPaid != want || len(st2.Payouts) != 1 {
		t.Fatalf("paid twice: %+v", st2)
	}

	// HTTP surface.
	get := func(path string) (int, []byte) {
		resp, err := http.Get(web.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf strings.Builder
		b := make([]byte, 1<<16)
		for {
			n, err := resp.Body.Read(b)
			buf.Write(b[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, []byte(buf.String())
	}
	if code, body := get("/pool/api/work?address=" + miner); code != 200 || !strings.Contains(string(body), `"block_target"`) {
		t.Fatalf("pool work: %d %s", code, body)
	}
	if code, _ := get("/pool/api/work?address=bad"); code != 400 {
		t.Fatalf("bad address: %d", code)
	}
	var ps poolStats
	if code, body := get("/api/pool/stats"); code != 200 || json.Unmarshal(body, &ps) != nil || !ps.Enabled || ps.Address != poolAddr || ps.FeePercent != 1 {
		t.Fatalf("stats: %d %s", code, body)
	}
	var pm poolMinerStats
	if code, body := get("/api/pool/miner/" + miner); code != 200 || json.Unmarshal(body, &pm) != nil || pm.Paid != want {
		t.Fatalf("miner: %d %s", code, body)
	}
	var led poolState
	if code, body := get("/api/pool/ledger"); code != 200 || json.Unmarshal(body, &led) != nil || led.TotalPaid != want || len(led.Rounds) != 4 {
		t.Fatalf("ledger: %d %s", code, body)
	}
}

// TestPoolDisabled: without a wallet the pool answers 503 and the solo
// endpoints are unchanged.
func TestPoolDisabled(t *testing.T) {
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 1000))
	s := newServer(consensus.RegTest, nil, nil, ws, nil, "", "")
	web := httptest.NewServer(s.routes())
	defer web.Close()
	a := randomAddr(t, consensus.RegTest)
	resp, err := http.Get(web.URL + "/pool/api/work?address=" + a)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("disabled pool: %d", resp.StatusCode)
	}
	resp, _ = http.Get(web.URL + "/api/pool/stats")
	var st poolStats
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Enabled {
		t.Fatal("stats say enabled")
	}
	resp, _ = http.Get(web.URL + "/api/work?address=" + a)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("solo work: %d", resp.StatusCode)
	}
	if job, err := ws.Job(a, ""); err != nil || job.BlockTarget != "" || job.Target != ws.tmpl.targetHex {
		t.Fatalf("solo job changed: %+v %v", job, err)
	}
}

// TestPoolDistributionRounding: the split over shares never exceeds the
// reward less the fee; dust from rounding stays with the pool.
func TestPoolDistributionRounding(t *testing.T) {
	node := newTestNode(t)
	ws := newWorkServer(node.client, consensus.RegTest, 5, t.Logf)
	enableTestPool(t, ws, nil, t.TempDir())
	hash, err := node.client.Call("generatetoaddress", 1, randomAddr(t, consensus.RegTest))
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	json.Unmarshal(hash, &hashes)
	shares := map[string]uint64{}
	var total uint64
	for i := 0; i < 7; i++ {
		w := uint64(1000 + 37*i)
		shares[randomAddr(t, consensus.RegTest)] = w
		total += w
	}
	ws.pool.mu.Lock()
	ws.pool.state.Rounds = []poolRound{{Height: 1, BlockHash: hashes[0], Reward: 4_950_000_001, Shares: shares, TotalShares: total, Status: "pending"}}
	ws.pool.mu.Unlock()
	if _, err := node.client.Call("generatetoaddress", int(consensus.CoinbaseMaturity), randomAddr(t, consensus.RegTest)); err != nil {
		t.Fatal(err)
	}
	if err := ws.pool.mature(); err != nil {
		t.Fatal(err)
	}
	st := ws.pool.stats()
	var given consensus.Amount
	ws.pool.mu.Lock()
	for _, b := range ws.pool.state.Balances {
		given += b
	}
	r := ws.pool.state.Rounds[0]
	ws.pool.mu.Unlock()
	net := r.Reward - r.Reward/100
	if r.Status != "matured" || given > net || net-given > 7 || given+r.Fee != r.Reward || st.Fees != r.Fee {
		t.Fatalf("given %d of %d, fee %d, reward %d (%s)", given, net, r.Fee, r.Reward, r.Status)
	}
}
