package swap

import (
	"context"
	"path/filepath"
	"testing"

	"argonmine/consensus"
	"argonmine/wallet"
)

// Swap fees are booked per swap, earned at 6 confirmations, kept out of
// the free balance, and paid to the treasury in one payment per coin.
func TestSwapFeesGoToTreasury(t *testing.T) {
	e := newEnv(t)
	tw, _, err := wallet.Create(filepath.Join(t.TempDir(), "treasury.wallet"), consensus.RegTest, "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	tw.SetClient(e.node)
	treasury, _ := tw.ReceiveAddress()
	e.m.cfg.Treasury, e.m.cfg.FeeSweepMin = treasury, 1_000_000 // 0.01
	if e.m.fees, err = openFeeBook(filepath.Join(t.TempDir(), "fees.json")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	assets := e.assets()
	swap := func(from, to string, in consensus.Amount) {
		t.Helper()
		p, err := Prepare(ctx, e.user, e.c, assets, from, to, in, 0)
		if err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
		if _, err := p.Execute(ctx); err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
		e.mine(1, e.sink)
		e.sync()
	}
	argFreeBefore, _, _ := e.m.freeLocked()
	swap(ARG, UARG, 10*unit) // 1% of 10 ARG: 0.1 ARG
	swap(UARG, ARG, 5*unit)  // 1% of 5 uARG: 0.05 uARG
	swap(ARG, WARG, 20*unit) // no wrap fee
	swap(WARG, UARG, 2*unit) // 1% of 2 wARG: 0.02 ARG
	wantARG, wantUARG := consensus.Amount(12_000_000), consensus.Amount(5_000_000)
	if got := e.m.fees.owed(ARG); got != wantARG {
		t.Fatalf("booked ARG fees %d, want %d", got, wantARG)
	}
	if got := e.m.fees.owed(UARG); got != wantUARG {
		t.Fatalf("booked uARG fees %d, want %d", got, wantUARG)
	}
	// Not yet earned: under 6 confirmations nothing is paid.
	e.m.reconcileFees()
	e.m.sweepFees()
	if ids, _ := e.node.RawMempool(); len(ids) != 0 {
		t.Fatalf("paid before 6 confirmations: %v", ids)
	}
	if free, _, _ := e.m.freeLocked(); free > argFreeBefore+10*unit {
		t.Fatalf("the booked fee counts as free: %d", free)
	}

	e.mine(6, e.sink)
	e.sync()
	e.m.reconcileFees()
	e.m.sweepFees()
	ids, _ := e.node.RawMempool()
	if len(ids) != 2 {
		t.Fatalf("sweeps in the mempool: %v, want one ARG and one uARG payment", ids)
	}
	e.mine(1, e.sink)
	e.sync()
	if _, err := tw.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := e.bal(tw, ARG); got != wantARG {
		t.Fatalf("the treasury got %d ARG, want %d", got, wantARG)
	}
	if got := e.bal(tw, UARG); got != wantUARG {
		t.Fatalf("the treasury got %d uARG, want %d", got, wantUARG)
	}
	if e.m.fees.owed(ARG) != 0 || e.m.fees.owed(UARG) != 0 {
		t.Fatal("fees still owed after the sweep")
	}
	// Nothing paid twice, and the wARG stays backed.
	e.m.sweepFees()
	if ids, _ := e.node.RawMempool(); len(ids) != 0 {
		t.Fatalf("paid again: %v", ids)
	}
	if r, s := e.bal(e.maker, ARG), e.m.wargSupply(); r < s {
		t.Fatalf("maker holds %d ARG behind %d wARG", r, s)
	}
}

func TestSwapFee(t *testing.T) {
	for _, c := range []struct {
		from string
		in   consensus.Amount
		bps  uint32
		coin string
		fee  consensus.Amount
	}{
		{ARG, 10 * unit, 100, ARG, unit / 10},
		{WARG, 3 * unit, 100, ARG, 3_000_000},
		{UARG, 5 * unit, 100, UARG, 5_000_000},
		{ARG, 99, 100, ARG, 0},                                        // rounds down
		{ARG, consensus.MaxMoney, 100, ARG, consensus.MaxMoney / 100}, // no overflow
	} {
		coin, fee := swapFee(c.from, c.in, c.bps)
		if coin != c.coin || fee != c.fee {
			t.Errorf("%s %d at %d bps: %s %d, want %s %d", c.from, c.in, c.bps, coin, fee, c.coin, c.fee)
		}
	}
}
