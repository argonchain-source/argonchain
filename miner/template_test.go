package miner_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/miner"
	"argonmine/rpc"
	"argonmine/wallet"
)

func TestAssembleFromNodeTemplate(t *testing.T) {
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	srv := rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: "t"}, rpc.Deps{Params: consensus.RegTest, Chain: ch, Pool: pool, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	client := wallet.NewClient("http://"+srv.Addr(), "t")

	priv, _ := btcec.NewPrivateKey()
	payout := consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: schnorr.SerializePubKey(priv.PubKey())}

	for i := 0; i < 3; i++ {
		raw, err := client.Call("getblocktemplate")
		if err != nil {
			t.Fatal(err)
		}
		var tmpl miner.TemplateJSON
		if err := json.Unmarshal(raw, &tmpl); err != nil {
			t.Fatal(err)
		}
		blk, err := miner.AssembleFromTemplate(consensus.RegTest, &tmpl, payout, []byte("remote"), time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		if blk.Txs[0].LockHeight != uint32(i+1) || blk.Txs[0].Outputs[0].Amount != consensus.Subsidy(uint32(i+1)) {
			t.Fatalf("coinbase shape at height %d", i+1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ok := miner.Solve(ctx, consensus.RegTest, blk, 2, nil)
		cancel()
		if !ok {
			t.Fatal("solve")
		}
		// Submit through the node like a remote miner would.
		var verdict *string
		res, err := client.Call("submitblock", hexOf(blk.Serialize()))
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(res, &verdict)
		if verdict != nil {
			t.Fatalf("block rejected: %s", *verdict)
		}
	}
	if ch.Tip().Height != 3 {
		t.Fatalf("height %d", ch.Tip().Height)
	}
	// Bad payout.
	if _, err := miner.AssembleFromTemplate(consensus.RegTest, &miner.TemplateJSON{}, consensus.TxOut{}, nil, 0); err != miner.ErrNoPayout {
		t.Fatal("payout check")
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i] = digits[c>>4]
		out[2*i+1] = digits[c&15]
	}
	return string(out)
}
