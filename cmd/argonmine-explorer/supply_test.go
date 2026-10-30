package main

import (
	"testing"

	"argonmine/consensus"
)

// The genesis block pays nothing, so the supply at height h is the subsidy
// of blocks 1 to h.
func TestSupply(t *testing.T) {
	coin := consensus.Subsidy(1)
	hv := int64(consensus.HalvingInterval)
	for _, c := range []struct {
		h    int64
		want consensus.Amount
	}{
		{0, 0},
		{1, coin},
		{62, 62 * coin},
		{hv - 1, consensus.Amount(hv-1) * coin},
		{hv, consensus.Amount(hv-1)*coin + coin/2},
	} {
		if got := supply(c.h); got != c.want {
			t.Errorf("supply(%d) = %d, want %d", c.h, got, c.want)
		}
	}
	if got := supply(64 * hv); got > consensus.MaxMoney || got < consensus.MaxMoney-100*coin {
		t.Errorf("every coin ever mined: %d, want just under %d", got, consensus.MaxMoney)
	}
}
