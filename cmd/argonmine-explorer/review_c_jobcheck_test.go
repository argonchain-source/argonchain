package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"argonmine/consensus"
	"argonmine/workclient"
)

// Security review C (2026-10-28): the worker checks a job with JSON.parse
// (keys are case sensitive) and must hand the Go miner exactly what it
// checked. Go's encoding/json matches keys in any case and keeps the last,
// so forwarding the server's own text would let a work server send the
// genuine job and, after it, "HEADER_PREFIX" for a header whose coinbase
// pays someone else. This runs the real worker's guardedFetch in Node.
const jobCheckHarness = `
globalThis.self = globalThis;
globalThis.postMessage = function () {};
const text = require('fs').readFileSync(process.argv[3], 'utf8');
globalThis.fetch = async function () { return new Response(text, { status: 200, headers: { 'Content-Type': 'application/json' } }); };
require(process.argv[2]);
const C = self.argonmineJobCheck;
const why = C.setExpect(process.argv[4], '', 0);
C.guardedFetch('https://explorer.example/api/work?address=x').then(function (r) { return r.text().then(function (body) { process.stdout.write(JSON.stringify({ why: why, status: r.status, body: body })); }); });
`

func TestJobCheckForwardsWhatItChecked(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	worker, _ := filepath.Abs(filepath.Join("..", "argonmine-web", "site", "static", "miner-worker.js"))
	ws := newWorkServer(nil, consensus.RegTest, 5, t.Logf)
	fixedTemplate(t, ws, fullTemplate(consensus.RegTest, 20_000))
	me, attacker := randomAddr(t, consensus.RegTest), randomAddr(t, consensus.RegTest)
	mine, err := ws.Job(me, "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := ws.Job(attacker, "")
	if err != nil {
		t.Fatal(err)
	}
	genuine, _ := json.Marshal(mine)
	// The genuine job, then the attacker's header and job id under upper-case keys.
	text := strings.TrimSuffix(string(genuine), "}") + `,"HEADER_PREFIX":"` + theirs.HeaderPrefix + `","JOB_ID":"` + theirs.ID + `"}`

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "job.json"), []byte(text), 0o600)
	os.WriteFile(filepath.Join(dir, "h.js"), []byte(jobCheckHarness), 0o600)
	out, err := exec.Command(node, filepath.Join(dir, "h.js"), worker, filepath.Join(dir, "job.json"), me).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	var res struct {
		Why, Body string
		Status    int
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("%s", out)
	}
	if res.Why != "" || res.Status != 200 {
		t.Fatalf("the genuine job was refused: %q %d %s", res.Why, res.Status, res.Body)
	}
	var job workclient.Job // what the Go miner decodes from what the worker forwards
	if err := json.Unmarshal([]byte(res.Body), &job); err != nil {
		t.Fatal(err)
	}
	if job.HeaderPrefix != mine.HeaderPrefix || job.ID != mine.ID {
		t.Fatalf("the miner would hash header %s… (job %s), not the checked one", job.HeaderPrefix[:24], job.ID)
	}
	if strings.Contains(res.Body, "HEADER_PREFIX") {
		t.Fatalf("the server's extra fields reached the miner: %s", res.Body)
	}
}
