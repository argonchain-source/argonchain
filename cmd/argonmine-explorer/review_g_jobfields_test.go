package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"argonmine/consensus"
	"argonmine/workclient"
)

// Security review G (2026-10-28): the worker re-encodes a checked job
// (jobFields) for the Go miner. Whatever JSON types the work server uses
// for the fields the check reads (arrays, numbers, strings with odd
// spellings, repeated keys), the Go miner must either refuse the job or
// hash exactly the header the check verified, with the target it checked.
func TestReviewG_JobFieldsTypes(t *testing.T) {
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
	base := func() map[string]any {
		b, _ := json.Marshal(mine)
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	enc := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }

	cases := map[string]string{}
	m := base()
	m["header_prefix"] = []string{mine.HeaderPrefix}
	cases["header as a one-element array"] = enc(m)
	m = base()
	m["target"] = []string{mine.Target}
	cases["target as an array"] = enc(m)
	m = base()
	m["height"] = "  " + strconv.Itoa(int(mine.Height))
	cases["height as a padded string"] = enc(m)
	m = base()
	m["job_id"] = 12345
	cases["job id as a number"] = enc(m)
	m = base()
	m["job_id"] = strings.Repeat("j", 2<<20)
	cases["job id of 2 MiB"] = enc(m)
	m = base()
	m["referral_pct"] = 5.5
	cases["referral share as a fraction"] = enc(m)
	m = base()
	m["referrer"] = attacker
	m["referral_pct"] = 50
	cases["display-only referrer fields"] = enc(m)
	// Repeated keys: JSON.parse keeps the last, and so must the forwarded job.
	cases["repeated header key"] = strings.TrimSuffix(enc(base()), "}") + `,"header_prefix":"` + theirs.HeaderPrefix + `"}`
	// Keys spelled with escapes are the same key to JSON.parse.
	cases["escaped header key"] = strings.TrimSuffix(enc(base()), "}") + `,"header` + string(rune(92)) + `u005fprefix":"` + theirs.HeaderPrefix + `"}`

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "h.js"), []byte(jobCheckHarness), 0o600)
	for name, text := range cases {
		os.WriteFile(filepath.Join(dir, "job.json"), []byte(text), 0o600)
		out, err := exec.Command(node, filepath.Join(dir, "h.js"), worker, filepath.Join(dir, "job.json"), me).CombinedOutput()
		if err != nil {
			t.Logf("%s: the worker failed (the miner gets no job): %v", name, err)
			continue
		}
		var res struct {
			Why, Body string
			Status    int
		}
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("%s: %s", name, out)
		}
		if res.Status != 200 {
			t.Logf("%s: refused (%d %.80s)", name, res.Status, res.Body)
			continue
		}
		// What the Go miner does with it (workclient.fetchJob reads at most 1 MiB).
		body := res.Body
		if len(body) > 1<<20 {
			body = body[:1<<20]
		}
		var job workclient.Job
		if err := json.Unmarshal([]byte(body), &job); err != nil {
			t.Logf("%s: forwarded, and the Go miner cannot decode it (mines nothing): %v", name, err)
			continue
		}
		if job.HeaderPrefix != mine.HeaderPrefix || job.Target != mine.Target {
			t.Fatalf("%s: the miner would hash header %.24s… target %.16s…, not the checked ones", name, job.HeaderPrefix, job.Target)
		}
		t.Logf("%s: forwarded; the miner hashes the checked header (job %.16s, height %d, ref %q %d%%)", name, job.ID, job.Height, job.Referrer, job.ReferralPct)
	}
}
