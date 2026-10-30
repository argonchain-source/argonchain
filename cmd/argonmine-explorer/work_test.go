package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/rpc"
	"argonmine/wallet"
	"argonmine/workclient"
)

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// testNode is a regtest chain with its RPC server.
type testNode struct {
	ch     *chain.Chain
	client *wallet.Client
}

func newTestNode(t *testing.T) *testNode {
	t.Helper()
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Close() })
	mp := mempool.New(ch, mempool.DefaultConfig())
	srv := rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: "t"}, rpc.Deps{Params: consensus.RegTest, Chain: ch, Pool: mp, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return &testNode{ch: ch, client: wallet.NewClient("http://"+srv.Addr(), "t")}
}

func testKeyAddr(t *testing.T, i uint32) string {
	t.Helper()
	m, err := wallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := wallet.SeedFromMnemonic(m, "")
	kc, _ := wallet.NewKeyChain(seed, wallet.CoinTypeFor(consensus.RegTest), 0)
	k, _ := kc.Derive(false, i)
	return k.Address(consensus.RegTest.HRP)
}

// checkJobProof is the miner's check in Go (the browser does the same in
// site/static/miner-worker.js): the coinbase pays the address (and the
// referrer, within pct), and hashing it up the branches gives the header's
// roots.
func checkJobProof(t *testing.T, j *workJobJSON, addr, ref string, pct int) *consensus.Tx {
	t.Helper()
	cbRaw, err := hex.DecodeString(j.Coinbase)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := consensus.DecodeTx(cbRaw)
	if err != nil {
		t.Fatalf("coinbase: %v", err)
	}
	pay, _ := wallet.DecodeAddress(consensus.RegTest.HRP, addr)
	if !cb.IsCoinbase() || len(cb.Outputs) == 0 || !bytes.Equal(cb.Outputs[0].Payload, pay.Payload) {
		t.Fatalf("coinbase does not pay %s", addr)
	}
	if ref != "" && len(cb.Outputs) == 2 {
		rp, _ := wallet.DecodeAddress(consensus.RegTest.HRP, ref)
		total := cb.Outputs[0].Amount + cb.Outputs[1].Amount
		if !bytes.Equal(cb.Outputs[1].Payload, rp.Payload) || cb.Outputs[1].Amount*100 > total*consensus.Amount(pct) {
			t.Fatalf("referral output wrong: %+v", cb.Outputs[1])
		}
	}
	prefix, _ := hex.DecodeString(j.HeaderPrefix)
	hdr, err := consensus.DecodeHeader(append(prefix, make([]byte, 8)...))
	if err != nil {
		t.Fatal(err)
	}
	branch := func(hs []string) []consensus.Hash {
		var out []consensus.Hash
		for _, h := range hs {
			x, err := consensus.HashFromHex(h)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, x)
		}
		return out
	}
	if merkleFold(cb.TxID(), branch(j.MerkleBranch)) != hdr.TxRoot {
		t.Fatal("tx root does not commit to the coinbase")
	}
	if merkleFold(cb.WTxID(), branch(j.WitnessBranch)) != hdr.WitnessRoot {
		t.Fatal("witness root does not commit to the coinbase")
	}
	return cb
}

// TestWorkServerEndToEnd runs a regtest node, serves work from it the way the
// explorer does, and lets a workclient (the browser miner core) solve blocks
// that pay its own address.
func TestWorkServerEndToEnd(t *testing.T) {
	node := newTestNode(t)
	client := node.client
	ws := newWorkServer(client, consensus.RegTest, 5, t.Logf)
	s := newServer(consensus.RegTest, client, nil, ws, nil, "", "")
	web := httptest.NewServer(s.routes())
	defer web.Close()

	// Validation paths.
	if _, err := ws.Job("bad", ""); err == nil {
		t.Fatal("bad address accepted")
	}
	if r := ws.Submit(workclient.Submit{JobID: "nope", Nonce: 1, Address: "bad"}); r.Accepted || r.Reason == "" {
		t.Fatalf("bad address accepted: %+v", r)
	}
	addr := testKeyAddr(t, 0)
	if r := ws.Submit(workclient.Submit{JobID: "nope", Nonce: 1, Address: addr}); !r.Stale {
		t.Fatalf("unknown job should be stale: %+v", r)
	}
	job, err := ws.Job(addr, "")
	if err != nil {
		t.Fatal(err)
	}
	if cb := checkJobProof(t, job, addr, "", 0); len(cb.Outputs) != 1 || cb.Outputs[0].Amount != consensus.Subsidy(1) {
		t.Fatalf("coinbase outputs %+v", cb.Outputs)
	}
	// Referral split: 5% of the reward to the referrer, the rest to the miner.
	refAddr := testKeyAddr(t, 1)
	if _, err := ws.Job(addr, addr); err == nil {
		t.Fatal("self-referral accepted")
	}
	if _, err := ws.Job(addr, "bad"); err == nil {
		t.Fatal("invalid referrer accepted")
	}
	rj, err := ws.Job(addr, refAddr)
	if err != nil {
		t.Fatal(err)
	}
	if rj.ReferralPct != 5 || rj.Referrer != refAddr {
		t.Fatalf("referral fields: %+v", rj.Job)
	}
	cb := checkJobProof(t, rj, addr, refAddr, 5)
	if len(cb.Outputs) != 2 || cb.Outputs[1].Amount != consensus.Subsidy(1)/100*5 || cb.Outputs[0].Amount+cb.Outputs[1].Amount != consensus.Subsidy(1) {
		t.Fatalf("referral coinbase split wrong: %+v", cb.Outputs)
	}
	if job.Height != 1 || job.Network != "regtest" || len(job.HeaderPrefix) != 2*workclient.HeaderPrefixLen {
		t.Fatalf("bad job: %+v", job.Job)
	}
	// Regtest accepts almost any nonce, so a submission is either a block or a
	// "hash above target" verdict; both are correct, anything else is a bug.
	if r := ws.Submit(workclient.Submit{JobID: job.ID, Nonce: 0, Address: addr}); !r.Accepted && r.Reason != "hash above target" {
		t.Fatalf("unexpected verdict: %+v", r)
	} else if r.Accepted {
		// The block moved the tip: the other job (same parent) is stale now.
		if r := ws.Submit(workclient.Submit{JobID: rj.ID, Nonce: 0, Address: addr}); !r.Stale {
			t.Fatalf("job on the old tip after a found block: %+v", r)
		}
	} else if r := ws.Submit(workclient.Submit{JobID: job.ID, Nonce: 0, Address: addr}); r.Accepted || r.Reason != "duplicate submission" {
		t.Fatalf("duplicate not detected: %+v", r)
	}

	// The miner core solves blocks against the HTTP interface.
	start := node.ch.Tip().Height
	mc := workclient.New(web.URL, addr, 2, t.Logf)
	mc.Start()
	waitFor(t, "three blocks mined through the work server", 60*time.Second, func() bool { return node.ch.Tip().Height >= start+3 })
	mc.Stop()
	if mc.Blocks() == 0 || mc.Hashes() == 0 {
		t.Fatalf("client counters: blocks %d hashes %d", mc.Blocks(), mc.Hashes())
	}
	found, down := ws.status()
	if found == 0 || down {
		t.Fatalf("server status: found %d node down %v", found, down)
	}
	// Every block's coinbase pays the miner.
	for h := uint32(1); h <= node.ch.Tip().Height; h++ {
		hash, err := client.BlockHash(h)
		if err != nil {
			t.Fatal(err)
		}
		blk, err := client.Block(hash)
		if err != nil {
			t.Fatal(err)
		}
		if len(blk.Tx) == 0 || len(blk.Tx[0].Outputs) == 0 || blk.Tx[0].Outputs[0].Address != addr {
			t.Fatalf("block %d coinbase does not pay the miner", h)
		}
	}
	// The served JSON carries the proof fields.
	resp, err := http.Get(web.URL + "/api/work?address=" + addr)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	for _, k := range []string{"job_id", "header_prefix", "target", "coinbase", "merkle_branch", "witness_branch"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("job JSON lacks %q: %v", k, raw)
		}
	}
}

// TestMerkleBranch0: folding the coinbase id with the branch gives the
// consensus merkle root for every tree size and shape.
func TestMerkleBranch0(t *testing.T) {
	for n := 1; n <= 40; n++ {
		ids := make([]consensus.Hash, n)
		for i := range ids {
			ids[i] = consensus.TaggedHash("test", []byte{byte(n), byte(i)})
		}
		want, err := consensus.MerkleRoot(ids)
		if err != nil {
			t.Fatal(err)
		}
		if got := merkleFold(ids[0], merkleBranch0(ids[1:])); got != want {
			t.Fatalf("n=%d: folded root %s, merkle root %s", n, got, want)
		}
	}
}
