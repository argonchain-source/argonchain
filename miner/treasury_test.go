package miner

import (
	"bytes"
	"context"
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
)

// From the treasury height the node's own templates pay the treasury its
// share as the coinbase's last output, and the chain accepts the blocks.
func TestTemplatePaysTreasury(t *testing.T) {
	p := *consensus.RegTest
	treasury := payoutOut(t)
	p.TreasuryHeight, p.TreasuryKey = 2, treasury.Payload
	ch, err := chain.Open(&p, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	payout := payoutOut(t)
	for height := uint32(1); height <= 3; height++ {
		blk, err := BuildTemplate(ch, pool, payout, []byte("t"), time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		outs := blk.Txs[0].Outputs
		share := consensus.TreasuryAmount(&p, height)
		if height < p.TreasuryHeight {
			if len(outs) != 1 || outs[0].Amount != consensus.Subsidy(height) {
				t.Fatalf("height %d: %+v", height, outs)
			}
		} else if len(outs) != 2 || outs[0].Amount != consensus.Subsidy(height)-share || outs[1].Amount != share || !bytes.Equal(outs[1].Payload, treasury.Payload) {
			t.Fatalf("height %d: %+v, want the miner and the treasury's %d", height, outs, share)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ok := Solve(ctx, &p, blk, 2, nil)
		cancel()
		if !ok {
			t.Fatal("solve failed")
		}
		if _, err := ch.ProcessBlock(blk, time.Now().Unix()); err != nil {
			t.Fatalf("height %d rejected: %v", height, err)
		}
	}
}
