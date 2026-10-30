package main

import "testing"

func TestProjectBlocks(t *testing.T) {
	var rows []mempoolRow
	// 30 transactions of 100 kB at rates 30, 29, ... 1 (sorted best first).
	for i := 30; i >= 1; i-- {
		rows = append(rows, mempoolRow{TxID: "t", Size: 100_000, Fee: 100_000 * 1, FeeRate: float64(i)})
	}
	b := projectBlocks(rows, 1_000_000, 3)
	if len(b) != 3 || b[0].Txs != 10 || b[1].Txs != 10 || b[2].Txs != 10 || b[2].Rest {
		t.Fatalf("blocks %+v", b)
	}
	if b[0].MaxFee != 30 || b[0].MinFee != 21 || b[1].MinFee != 11 {
		t.Fatalf("rates %+v", b)
	}
	e := feeEstimates(b, 1_000_000)
	if e["high"] != 21 || e["medium"] != 11 || e["low"] != 1 || e["none"] != 1 {
		t.Fatalf("estimates %v", e)
	}
	// More than the shown blocks: the last holds the rest.
	b = projectBlocks(rows, 1_000_000, 2)
	if len(b) != 2 || b[1].Txs != 20 || !b[1].Rest {
		t.Fatalf("rest %+v", b)
	}
	// A block that is not full: everything gets in at the floor.
	b = projectBlocks(rows[:3], 1_000_000, 6)
	if len(b) != 1 || feeEstimates(b, 1_000_000)["high"] != 1 {
		t.Fatalf("not full %+v", b)
	}
	if len(projectBlocks(nil, 1_000_000, 6)) != 0 {
		t.Fatal("empty mempool")
	}
}
