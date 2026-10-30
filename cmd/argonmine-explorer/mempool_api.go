package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"argonmine/consensus"
)

// GET /api/mempool feeds the mempool page: the waiting transactions packed
// into the blocks that would mine them (best fee rate first, each block up
// to the size limit), the fee rates that reach each of the next blocks, the
// latest mined blocks, and the best-paying transactions. A projection, not a
// promise: it ignores transaction chains and assumes no new arrivals.

const (
	mempoolProjected = 6   // projected blocks shown; the last one holds the rest
	mempoolAPITxs    = 50  // transactions listed
	mempoolRecent    = 8   // mined blocks shown
	minRelayRate     = 1.0 // units per byte: the node's default relay floor (mempool.DefaultConfig)
)

type projBlock struct {
	Txs    int              `json:"txs"`
	Bytes  float64          `json:"bytes"`
	Fees   consensus.Amount `json:"fees"`
	MinFee float64          `json:"min_rate"`
	MedFee float64          `json:"median_rate"`
	MaxFee float64          `json:"max_rate"`
	Rest   bool             `json:"rest,omitempty"` // holds everything past the shown blocks
}

// projectBlocks packs rows (best fee rate first) into blocks of at most
// maxBytes, at most n of them; the last takes whatever is left.
func projectBlocks(rows []mempoolRow, maxBytes float64, n int) []projBlock {
	var out []projBlock
	var rates []float64
	cur := projBlock{}
	flush := func() {
		if cur.Txs == 0 {
			return
		}
		sort.Float64s(rates)
		cur.MinFee, cur.MaxFee, cur.MedFee = rates[0], rates[len(rates)-1], rates[len(rates)/2]
		out = append(out, cur)
		cur, rates = projBlock{}, nil
	}
	for _, r := range rows {
		last := len(out) == n-1
		if !last && cur.Txs > 0 && cur.Bytes+r.Size > maxBytes {
			flush()
			last = len(out) == n-1
		}
		cur.Txs++
		cur.Bytes += r.Size
		cur.Fees = satAdd(cur.Fees, r.Fee)
		rates = append(rates, r.FeeRate)
		if last && cur.Bytes > maxBytes {
			cur.Rest = true
		}
	}
	flush()
	return out
}

// feeEstimates gives the rate that gets a transaction into the next block
// (high), the one after (medium) and the third (low): the lowest rate in
// that projected block when it is full, else the relay floor.
func feeEstimates(blocks []projBlock, maxBytes float64) map[string]float64 {
	cut := func(i int) float64 {
		if i < len(blocks) && blocks[i].Bytes >= maxBytes*0.95 {
			if blocks[i].MinFee > minRelayRate {
				return blocks[i].MinFee
			}
		}
		return minRelayRate
	}
	high, medium, low := cut(0), cut(1), cut(2)
	if medium > high {
		medium = high
	}
	if low > medium {
		low = medium
	}
	return map[string]float64{"none": minRelayRate, "low": low, "medium": medium, "high": high}
}

type recentBlock struct {
	Height uint32           `json:"height"`
	Time   uint64           `json:"time"`
	Txs    int              `json:"txs"`
	Size   int              `json:"size"`
	Fees   consensus.Amount `json:"fees"`
	Reward consensus.Amount `json:"reward"`
}

type mempoolTx struct {
	TxID  string           `json:"txid"`
	Size  float64          `json:"size"`
	Fee   consensus.Amount `json:"fee"`
	Rate  float64          `json:"rate"`
	Value float64          `json:"value"`
}

func (s *server) apiMempool(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	v := s.mempoolView()
	maxBytes := float64(consensus.MaxBlockSize)
	proj := projectBlocks(v.All, maxBytes, mempoolProjected)
	var fees consensus.Amount
	for _, b := range proj {
		fees = satAdd(fees, b.Fees)
	}
	txs := []mempoolTx{}
	for i, rw := range v.All {
		if i >= mempoolAPITxs {
			break
		}
		txs = append(txs, mempoolTx{rw.TxID, rw.Size, rw.Fee, rw.FeeRate, rw.Out})
	}
	recent := []recentBlock{}
	tip := int64(-1)
	if s.ix != nil {
		h, _, _ := s.ix.status()
		tip = h
		if h >= 0 {
			for _, b := range s.ix.latestBlocks(h, mempoolRecent) {
				recent = append(recent, recentBlock{b.Height, b.Time, b.TxCount, b.Size, b.Fees, b.Reward})
			}
		}
	}
	if proj == nil {
		proj = []projBlock{}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"count": v.Count, "bytes": v.Bytes, "fees": fees, "complete": v.Complete, "error": v.Error,
		"max_block_bytes": consensus.MaxBlockSize, "min_rate": minRelayRate,
		"projected": proj, "estimates": feeEstimates(proj, maxBytes),
		"recent": recent, "txs": txs, "tip": tip, "updated": time.Now().Unix(),
		"block_seconds": int(consensus.TargetSpacing),
	})
}
