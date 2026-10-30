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
		blk, err := miner.AssembleFromTemplate(consensus.RegTest, &tmpl, consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: payload}, []byte("t"), time.Now().Unix())
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

// From the treasury height, getblocktemplate's coinbasevalue is still the
// whole coinbase (subsidy plus fees), and a block assembled from it by a
// remote miner pays the treasury its share and is accepted. Before this
// test the node reported the miner's share only, and the explorer, which
// takes the share out itself, paid every browser block short by it.
func TestTemplateCoinbaseValueWithTreasury(t *testing.T) {
	p := *consensus.RegTest
	p.TreasuryHeight = 1
	p.TreasuryKey = make([]byte, consensus.P2PKPayloadLen)
	p.TreasuryKey[0] = 7
	ch, err := chain.Open(&p, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	srv := New(Config{Listen: "127.0.0.1:0", Token: "s"}, Deps{Params: &p, Chain: ch, Pool: pool, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	c := &client{t: t, url: "http://" + srv.Addr(), token: "s"}
	var tmpl miner.TemplateJSON
	if err := json.Unmarshal(c.must("getblocktemplate"), &tmpl); err != nil {
		t.Fatal(err)
	}
	share := consensus.TreasuryAmount(&p, 1)
	if share == 0 || tmpl.Height != 1 || tmpl.CoinbaseValue != consensus.Subsidy(1) {
		t.Fatalf("height %d coinbasevalue %d, want the whole subsidy %d (treasury share %d)", tmpl.Height, tmpl.CoinbaseValue, consensus.Subsidy(1), share)
	}
	payload := make([]byte, 32)
	payload[0] = 1
	blk, err := miner.AssembleFromTemplate(&p, &tmpl, consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: payload}, []byte("t"), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	outs := blk.Txs[0].Outputs
	if len(outs) != 2 || outs[0].Amount != consensus.Subsidy(1)-share || outs[1].Amount != share || string(outs[1].Payload) != string(p.TreasuryKey) {
		t.Fatalf("coinbase outputs %+v, want miner %d and treasury %d", outs, consensus.Subsidy(1)-share, share)
	}
	solve(t, blk)
	var verdict *string
	json.Unmarshal(c.must("submitblock", hexOf(blk.Serialize())), &verdict)
	if verdict != nil {
		t.Fatalf("block rejected: %s", *verdict)
	}
	if ch.Tip().Height != 1 {
		t.Fatalf("height %d", ch.Tip().Height)
	}
}
