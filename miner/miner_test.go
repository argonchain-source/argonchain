package miner

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
)

func payoutOut(t *testing.T) consensus.TxOut {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: schnorr.SerializePubKey(priv.PubKey())}
}

func TestTemplateAndSolve(t *testing.T) {
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	payout := payoutOut(t)

	blk, err := BuildTemplate(ch, pool, payout, []byte("t"), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if blk.Txs[0].LockHeight != 1 || blk.Txs[0].Outputs[0].Amount != consensus.Subsidy(1) {
		t.Fatal("coinbase")
	}
	if blk.Header.Time <= ch.Tip().Header.Time && blk.Header.Time <= chain.MedianTimePast(ch.Tip()) {
		t.Fatal("template time not above MTP")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !Solve(ctx, consensus.RegTest, blk, 2, nil) {
		t.Fatal("solve failed")
	}
	if _, err := ch.ProcessBlock(blk, time.Now().Unix()); err != nil {
		t.Fatalf("mined block rejected: %v", err)
	}
	// Bad payout rejected.
	if _, err := BuildTemplate(ch, pool, consensus.TxOut{Type: 1}, nil, 0); err != ErrNoPayout {
		t.Fatal("payout check")
	}
	// Cancelled context returns false.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	blk2, _ := BuildTemplate(ch, pool, payout, nil, time.Now().Unix())
	if Solve(cctx, consensus.RegTest, blk2, 1, nil) {
		t.Fatal("solve with cancelled ctx should fail")
	}
}

func TestMinerLoopFindsBlocks(t *testing.T) {
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	m := New(ch, pool, payoutOut(t), 2, t.Logf)
	var broadcast int
	m.SetBroadcast(func(*consensus.Block) { broadcast++ })
	m.Start()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && ch.Tip().Height < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	m.Stop()
	if ch.Tip().Height < 3 {
		t.Fatalf("mined only %d blocks", ch.Tip().Height)
	}
	if m.BlocksFound() < 3 || broadcast < 3 || m.Hashes() == 0 {
		t.Fatal("counters")
	}
	if m.Running() {
		t.Fatal("still running")
	}
	// MineOne works too.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := ch.Tip().Height
	if _, err := MineOne(ctx, ch, pool, payoutOut(t), 1); err != nil {
		t.Fatal(err)
	}
	if ch.Tip().Height != before+1 {
		t.Fatal("MineOne did not extend chain")
	}
}
