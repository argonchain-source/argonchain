package rpc

import (
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/miner"
)

// solve finds a nonce meeting the block's target (regtest: a few tries).
func solve(t *testing.T, blk *consensus.Block) {
	t.Helper()
	for n := uint64(0); n < 1000; n++ {
		blk.Header.Nonce = n
		if consensus.CheckProofOfWork(&blk.Header, consensus.RegTest.PowLimit) == nil {
			return
		}
	}
	t.Fatal("no nonce found")
}

// TestSubmitBlockSideChain: a valid block whose parent is no longer the tip
// (another block at its height came first) is stored but does not become the
// tip, and submitblock says so instead of returning null.
func TestSubmitBlockSideChain(t *testing.T) {
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	srv := New(Config{Listen: "127.0.0.1:0", Token: "s"}, Deps{Params: consensus.RegTest, Chain: ch, Pool: pool, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	c := &client{t: t, url: "http://" + srv.Addr(), token: "s"}

	var tmpl miner.TemplateJSON
	if err := json.Unmarshal(c.must("getblocktemplate"), &tmpl); err != nil {
		t.Fatal(err)
	}
	build := func(fill byte) *consensus.Block {
		payload := make([]byte, 32)
		for i := range payload {
			payload[i] = fill
		}
		blk, err := miner.AssembleFromTemplate(&tmpl, consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: payload}, []byte("t"), time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		solve(t, blk)
		return blk
	}
	a, b := build(1), build(2)
	submit := func(blk *consensus.Block) *string {
		var v *string
		json.Unmarshal(c.must("submitblock", hex.EncodeToString(blk.Serialize())), &v)
		return v
	}
	if v := submit(a); v != nil {
		t.Fatalf("first block: verdict %q, want null (new tip)", *v)
	}
	if v := submit(b); v == nil || *v != SubmitSideChain {
		t.Fatalf("competing block at the same height: verdict %v, want %q", v, SubmitSideChain)
	}
	if ch.Tip().Hash != a.Header.Hash() {
		t.Fatal("the tip moved to the side-branch block")
	}
	if v := submit(a); v == nil || *v != "duplicate" {
		t.Fatalf("resubmission: verdict %v, want duplicate", v)
	}
}
