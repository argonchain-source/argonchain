package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"argonmine/consensus"
)

// The work server's jobs pay the treasury from the treasury height, and
// the browser miner (miner-worker.js) accepts exactly that: the treasury's
// share as the last output, to the treasury key, and nothing else.
func TestTreasuryJobs(t *testing.T) {
	p := consensus.MainNet
	h := p.TreasuryHeight
	job := func(params *consensus.Params, height uint32, addr, ref string) *workJobJSON {
		t.Helper()
		ws := newWorkServer(nil, params, 5, t.Logf)
		tj := fullTemplate(params, 2000)
		tj.Height, tj.CoinbaseValue = height, consensus.Subsidy(height)+1234
		fixedTemplate(t, ws, tj)
		j, err := ws.Job(addr, ref)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	me, ref := randomAddr(t, p), randomAddr(t, p)

	// The coinbase the server builds at the treasury height.
	j := job(p, h, me, ref)
	raw, _ := hex.DecodeString(j.Coinbase)
	cb, err := consensus.DecodeTx(raw)
	if err != nil {
		t.Fatal(err)
	}
	share := consensus.TreasuryAmount(p, h)
	last := cb.Outputs[len(cb.Outputs)-1]
	if len(cb.Outputs) != 3 || last.Amount != share || string(last.Payload) != string(p.TreasuryKey) {
		t.Fatalf("coinbase outputs %+v, want miner, referrer, treasury %d", cb.Outputs, share)
	}
	var total consensus.Amount
	for _, o := range cb.Outputs {
		total += o.Amount
	}
	if total != consensus.Subsidy(h)+1234 || consensus.CheckTreasury(p, cb, h) != nil {
		t.Fatalf("total %d", total)
	}
	if cb.Outputs[1].Amount != (total-share)/100*5 {
		t.Fatalf("referral share %d is not 5%% of what the treasury leaves", cb.Outputs[1].Amount)
	}
	if j0 := job(p, h-1, me, ""); strings.Count(j0.Coinbase, hex.EncodeToString(p.TreasuryKey)) != 0 {
		t.Fatal("a job before the treasury height pays the treasury")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: the browser check is not run")
	}
	worker, _ := filepath.Abs(filepath.Join("..", "argonmine-web", "site", "static", "miner-worker.js"))
	noTreasury := *p
	noTreasury.TreasuryHeight = 0
	otherKey := *p
	otherKey.TreasuryKey = make([]byte, 32)
	otherKey.TreasuryKey[0] = 1
	early := *p
	early.TreasuryHeight = 1
	type jcase struct {
		Name   string       `json:"name"`
		Job    *workJobJSON `json:"job"`
		Addr   string       `json:"address"`
		Ref    string       `json:"ref,omitempty"`
		RefPct int          `json:"refPct,omitempty"`
		Want   string       `json:"-"`
	}
	cases := []jcase{
		{Name: "treasury height", Job: job(p, h, me, ""), Addr: me},
		{Name: "treasury height with referral", Job: job(p, h, me, ref), Addr: me, Ref: ref, RefPct: 5},
		{Name: "before the treasury height", Job: job(p, h-1, me, ""), Addr: me},
		{Name: "treasury left out", Job: job(&noTreasury, h, me, ""), Addr: me, Want: "treasury"},
		{Name: "treasury left out, referral kept", Job: job(&noTreasury, h, me, ref), Addr: me, Ref: ref, RefPct: 5, Want: "treasury"},
		{Name: "another treasury key", Job: job(&otherKey, h, me, ""), Addr: me, Want: "treasury"},
		{Name: "treasury paid before its height", Job: job(&early, h-1, me, ""), Addr: me, Want: "you chose no referrer"},
	}
	in, _ := json.Marshal(map[string]any{"cases": cases, "hashes": []string{}})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "in.json"), in, 0o600)
	os.WriteFile(filepath.Join(dir, "harness.js"), []byte(jsHarness), 0o600)
	outb, err := exec.Command(node, filepath.Join(dir, "harness.js"), worker, filepath.Join(dir, "in.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outb)
	}
	var out struct {
		Cases []struct {
			Name string `json:"name"`
			Why  string `json:"why"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(outb, &out); err != nil {
		t.Fatalf("node output: %v\n%s", err, outb)
	}
	for i, c := range cases {
		why := out.Cases[i].Why
		if c.Want == "" && why != "" {
			t.Errorf("%s: refused: %s", c.Name, why)
		}
		if c.Want != "" && !strings.Contains(why, c.Want) {
			t.Errorf("%s: %q, want a reason with %q", c.Name, why, c.Want)
		}
	}
}

// The browser's copy of the treasury rule equals the consensus one.
func TestBrowserTreasuryConstants(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "argonmine-web", "site", "static", "miner-worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`arg: \{ height: (\d+), key: '([0-9a-f]{64})', bps: (\d+) \}`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no mainnet treasury in miner-worker.js")
	}
	height, _ := strconv.Atoi(string(m[1]))
	bps, _ := strconv.Atoi(string(m[3]))
	if uint32(height) != consensus.MainNet.TreasuryHeight || string(m[2]) != hex.EncodeToString(consensus.MainNet.TreasuryKey) || bps != consensus.TreasuryBps {
		t.Fatalf("miner-worker.js has height %d key %s bps %d; consensus has %d %x %d", height, m[2], bps, consensus.MainNet.TreasuryHeight, consensus.MainNet.TreasuryKey, consensus.TreasuryBps)
	}
}
