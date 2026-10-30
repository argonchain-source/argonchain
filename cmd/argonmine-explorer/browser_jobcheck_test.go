package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"argonmine/consensus"
)

// C12/I1: the browser miner refuses jobs that do not pay it. This runs the
// job check of the web app's worker (cmd/argonmine-web/site/static/
// miner-worker.js) under Node against jobs built by this work server, both
// genuine and tampered. Skipped when node is not installed.

const jsHarness = `
globalThis.self = globalThis;
require(process.argv[2]);
const C = self.argonmineJobCheck;
const input = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const hex = (b) => Buffer.from(b).toString('hex');
const out = { cases: [], hashes: [] };
// Every case: decode the miner's address (and referrer) as the worker does
// on start, then check the job.
out.cases = input.cases.map((c) => {
  const a = C.decodeAddress(c.address);
  const exp = { hrp: a.hrp, payload: a.payload, refPayload: c.ref ? C.decodeAddress(c.ref).payload : null, refPct: c.refPct || 0 };
  return { name: c.name, why: C.checkJob(c.job, exp), addr: C.encodeAddress(a.hrp, 0, a.payload), setup: C.setExpect(c.address, c.ref || '', c.refPct || 0) };
});
for (const d of input.hashes) out.hashes.push({ sha: hex(C.sha256(Buffer.from(d, 'hex'))), tagged: hex(C.taggedHash('Argonmine/txid', Buffer.from(d, 'hex'))) });
process.stdout.write(JSON.stringify(out));
`

func TestBrowserJobCheck(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	worker, err := filepath.Abs(filepath.Join("..", "argonmine-web", "site", "static", "miner-worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 20_000)) // a few dozen transactions: real branches
	me, ref, other := randomAddr(t, consensus.RegTest), randomAddr(t, consensus.RegTest), randomAddr(t, consensus.RegTest)
	mine, err := ws.Job(me, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine.MerkleBranch) < 4 {
		t.Fatalf("branch too short to mean much: %d", len(mine.MerkleBranch))
	}
	referred, _ := ws.Job(me, ref)
	theirs, _ := ws.Job(other, "")
	theirsRef, _ := ws.Job(other, me) // pays me only the referral share

	type jcase struct {
		Name   string       `json:"name"`
		Job    *workJobJSON `json:"job"`
		Addr   string       `json:"address"`
		Ref    string       `json:"ref,omitempty"`
		RefPct int          `json:"refPct,omitempty"`
		Want   string       `json:"-"` // "" = accepted, else a substring of the reason
	}
	clone := func(j *workJobJSON, f func(*workJobJSON)) *workJobJSON {
		c := *j
		c.MerkleBranch = append([]string(nil), j.MerkleBranch...)
		c.WitnessBranch = append([]string(nil), j.WitnessBranch...)
		f(&c)
		return &c
	}
	flipHex := func(s string, i int) string {
		b := []byte(s)
		if b[i] == '0' {
			b[i] = '1'
		} else {
			b[i] = '0'
		}
		return string(b)
	}
	// A job whose coinbase pays someone else but is otherwise consistent:
	// the server rebuilt the header around the other coinbase.
	swapped := clone(mine, func(c *workJobJSON) { c.Coinbase, c.HeaderPrefix = theirs.Coinbase, theirs.HeaderPrefix })
	// Coinbase paying someone else, header left as it was.
	swappedCB := clone(mine, func(c *workJobJSON) { c.Coinbase = theirs.Coinbase })
	cases := []jcase{
		{Name: "genuine", Job: mine, Addr: me},
		{Name: "genuine with referral", Job: referred, Addr: me, Ref: ref, RefPct: 5},
		{Name: "referral share above what was accepted", Job: referred, Addr: me, Ref: ref, RefPct: 4, Want: "more than the 4%"},
		{Name: "referral nobody chose", Job: referred, Addr: me, Want: "you chose no referrer"},
		{Name: "referral to someone else", Job: referred, Addr: me, Ref: other, RefPct: 5, Want: "not to the referrer you accepted"},
		{Name: "pays another address", Job: swapped, Addr: me, Want: "goes to " + other},
		{Name: "pays another address, header untouched", Job: swappedCB, Addr: me, Want: "goes to " + other},
		{Name: "me as the referrer only", Job: theirsRef, Addr: me, Want: "goes to " + other},
		{Name: "branch tampered", Job: clone(mine, func(c *workJobJSON) { c.MerkleBranch[2] = flipHex(c.MerkleBranch[2], 5) }), Addr: me, Want: "transaction root"},
		{Name: "branch shortened", Job: clone(mine, func(c *workJobJSON) { c.MerkleBranch = c.MerkleBranch[1:] }), Addr: me, Want: "transaction root"},
		{Name: "witness branch tampered", Job: clone(mine, func(c *workJobJSON) { c.WitnessBranch[0] = flipHex(c.WitnessBranch[0], 0) }), Addr: me, Want: "witness root"},
		{Name: "header root tampered", Job: clone(mine, func(c *workJobJSON) { c.HeaderPrefix = flipHex(c.HeaderPrefix, 2*40) }), Addr: me, Want: "transaction root"},
		{Name: "target not the header's", Job: clone(mine, func(c *workJobJSON) { c.Target = strings.Repeat("f", 64) }), Addr: me, Want: "target"},
		{Name: "no proof (old server)", Job: clone(mine, func(c *workJobJSON) { c.Coinbase = "" }), Addr: me, Want: "no proof"},
		{Name: "trailing bytes", Job: clone(mine, func(c *workJobJSON) { c.Coinbase += "00" }), Addr: me, Want: "malformed"},
		{Name: "garbage coinbase", Job: clone(mine, func(c *workJobJSON) { c.Coinbase = "zz" }), Addr: me, Want: "malformed"},
	}
	var hashes []string
	for _, n := range []int{0, 1, 3, 55, 56, 63, 64, 65, 119, 120, 200} {
		b := make([]byte, n)
		rand.Read(b)
		hashes = append(hashes, hex.EncodeToString(b))
	}
	in, _ := json.Marshal(map[string]any{"cases": cases, "hashes": hashes})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "in.json"), in, 0o600)
	os.WriteFile(filepath.Join(dir, "harness.js"), []byte(jsHarness), 0o600)
	cmd := exec.Command(node, filepath.Join(dir, "harness.js"), worker, filepath.Join(dir, "in.json"))
	outb, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outb)
	}
	var out struct {
		Cases []struct {
			Name  string `json:"name"`
			Why   string `json:"why"`
			Addr  string `json:"addr"`
			Setup string `json:"setup"`
		} `json:"cases"`
		Hashes []struct {
			Sha    string `json:"sha"`
			Tagged string `json:"tagged"`
		} `json:"hashes"`
	}
	if err := json.Unmarshal(outb, &out); err != nil {
		t.Fatalf("node output: %v\n%s", err, outb)
	}
	for i, c := range cases {
		got := out.Cases[i]
		if got.Addr != c.Addr || got.Setup != "" {
			t.Errorf("%s: JS re-encodes the address as %s (setup %q)", c.Name, got.Addr, got.Setup)
		}
		switch {
		case c.Want == "" && got.Why != "":
			t.Errorf("%s: refused: %s", c.Name, got.Why)
		case c.Want != "" && !strings.Contains(got.Why, c.Want):
			t.Errorf("%s: reason %q, want it to say %q", c.Name, got.Why, c.Want)
		default:
			t.Logf("%-42s %s", c.Name, orDefault(got.Why, "mined"))
		}
	}
	for i, h := range hashes {
		b, _ := hex.DecodeString(h)
		if sum := sha256.Sum256(b); out.Hashes[i].Sha != hex.EncodeToString(sum[:]) {
			t.Errorf("sha256 of %d bytes: JS %s", len(b), out.Hashes[i].Sha)
		}
		if want := consensus.TaggedHash(consensus.TagTxID, b).String(); out.Hashes[i].Tagged != want {
			t.Errorf("tagged hash of %d bytes: JS %s, Go %s", len(b), out.Hashes[i].Tagged, want)
		}
	}
}
