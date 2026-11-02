package main

import (
	"fmt"
	"net/http"
	"strings"

	"argonmine/consensus"
	"argonmine/ui"
	"argonmine/wallet"
)

// The mint window on the explorer (consensus/mint.go): the JSON API the app
// reads, the /early page, and the lines the block, transaction and address
// pages add for a request.

const mintRecentDefault = 20

// Statuses of the window, as /api/early says them.
const (
	mintNone     = "none"     // no window on this network
	mintUpcoming = "upcoming" // before the first block of the window
	mintOpen     = "open"     // the index height is in the window
	mintClosed   = "closed"   // past the last block of it
)

// mintStatus judges the window at index height h, with the blocks that
// remain: until the window opens, or until it closes, else 0.
func mintStatus(p *consensus.Params, h int64) (status string, remaining int64) {
	if p == nil || p.Mint == nil {
		return mintNone, 0
	}
	switch {
	case h < int64(p.Mint.Start):
		return mintUpcoming, int64(p.Mint.Start) - h
	case h <= int64(p.Mint.End):
		return mintOpen, int64(p.Mint.End) - h + 1
	}
	return mintClosed, 0
}

// mintCap is the most the window can create: the per-block amount over
// every block of it.
func mintCap(r *consensus.MintRule) consensus.Amount {
	blocks := consensus.Amount(r.End-r.Start) + 1
	if r.PerBlock > consensus.MaxMoney/blocks {
		return consensus.MaxMoney
	}
	return r.PerBlock * blocks
}

// mintAssetName names the mint asset as the pages do: uARG for the
// bridge's asset, the registry's symbol otherwise, else a short id.
func (s *server) mintAssetName() string {
	if s.params.Mint == nil {
		return ""
	}
	id := s.params.Mint.Asset.String()
	if ui.CanonicalAsset("UARG") == id {
		return "uARG"
	}
	if info, ok := s.ix.asset(id); ok && info.Display != "" {
		return info.Display
	}
	return shorten(id, 8, 4)
}

// mintPending is the ARG the mempool's requests ask the next block to
// mint, and how many of them there are.
func (s *server) mintPending() (consensus.Amount, int) {
	if s.client == nil || s.params.Mint == nil {
		return 0, 0
	}
	var sum consensus.Amount
	n := 0
	for _, rw := range s.mempoolView().All {
		if rw.Mint > 0 {
			sum = satAdd(sum, rw.Mint)
			n++
		}
	}
	return sum, n
}

// meanInterval is the mean seconds per block over the last n indexed
// blocks, or the target spacing while there are too few.
func (ix *Indexer) meanInterval(n int) float64 {
	h := ix.indexedHeight()
	if recent := ix.latestBlocks(h, n+1); len(recent) > 1 {
		if span := int64(recent[0].Time) - int64(recent[len(recent)-1].Time); span > 0 {
			return float64(span) / float64(len(recent)-1)
		}
	}
	return float64(consensus.TargetSpacing)
}

// aboutTime says how long n blocks take at secs a block: "about 3 days".
func aboutTime(n int64, secs float64) string {
	if n <= 0 {
		return ""
	}
	t := float64(n) * secs
	switch {
	case t < 3600:
		m := int(t/60 + 0.5)
		if m < 1 {
			m = 1
		}
		return fmt.Sprintf("about %d minute%s", m, plural(m))
	case t < 48*3600:
		h := int(t/3600 + 0.5)
		return fmt.Sprintf("about %d hour%s", h, plural(h))
	}
	d := int(t/86400 + 0.5)
	return fmt.Sprintf("about %d day%s", d, plural(d))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fmtCoins prints units as whole coins with separators, with the fraction
// only when there is one: "1,000,000", "0.5".
func fmtCoins(a consensus.Amount) string {
	neg := a < 0
	if neg {
		a = -a
	}
	s := numFmt(a / consensus.UnitsPerCoin)
	if frac := a % consensus.UnitsPerCoin; frac != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%08d", frac), "0")
	}
	if neg {
		return "-" + s
	}
	return s
}

// ---- JSON API ----

// mintRecordJSON is one request as the API lists it, with the amounts
// also as decimal strings.
type mintRecordJSON struct {
	Height     uint32           `json:"height"`
	Time       uint64           `json:"time"`
	TxID       string           `json:"txid"`
	Buyer      string           `json:"buyer"`
	Paid       consensus.Amount `json:"paid"`
	PaidCoin   string           `json:"paidCoin"`
	Minted     consensus.Amount `json:"minted"`
	MintedCoin string           `json:"mintedCoin"`
}

func mintRecordsJSON(rs []mintRecord) []mintRecordJSON {
	out := make([]mintRecordJSON, 0, len(rs))
	for _, r := range rs {
		out = append(out, mintRecordJSON{Height: r.Height, Time: r.Time, TxID: r.TxID, Buyer: r.Buyer,
			Paid: r.Paid, PaidCoin: wallet.FormatAmount(r.Paid), Minted: r.Minted, MintedCoin: wallet.FormatAmount(r.Minted)})
	}
	return out
}

// apiMint serves GET /api/early: the window, its rule and what it has done.
func (s *server) apiMint(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/early" {
		writeNamesJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	h := s.ix.indexedHeight()
	status, remaining := mintStatus(s.params, h)
	t := s.ix.mintTotals()
	pending, _ := s.mintPending()
	out := map[string]any{
		"window": s.params.Mint != nil, "start": 0, "end": 0, "height": h, "status": status,
		"perBlock": 0, "perBlockCoin": wallet.FormatAmount(0), "rate": 0, "maxPaid": 0, "maxPaidCoin": wallet.FormatAmount(0),
		"asset": "", "address": "", "minted": t.Minted, "mintedCoin": wallet.FormatAmount(t.Minted), "requests": t.Requests,
		"cap": 0, "capCoin": wallet.FormatAmount(0), "remaining": remaining, "pending": pending, "pendingCoin": wallet.FormatAmount(pending),
		"maturity": consensus.CoinbaseMaturity, "recent": mintRecordsJSON(s.ix.recentMints(mintRecentDefault)),
	}
	if m := s.params.Mint; m != nil {
		maxPaid := m.PerBlock / m.Rate
		cap := mintCap(m)
		out["start"], out["end"] = m.Start, m.End
		out["perBlock"], out["perBlockCoin"] = m.PerBlock, wallet.FormatAmount(m.PerBlock)
		out["rate"] = m.Rate
		out["maxPaid"], out["maxPaidCoin"] = maxPaid, wallet.FormatAmount(maxPaid)
		out["asset"], out["address"] = m.Asset.String(), mintAddress(s.params)
		out["cap"], out["capCoin"] = cap, wallet.FormatAmount(cap)
	}
	writeNamesJSON(w, http.StatusOK, out)
}

// apiMintAddress serves GET /api/early/{address}: the address's requests
// and their totals.
func (s *server) apiMintAddress(w http.ResponseWriter, r *http.Request) {
	addr := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/early/")))
	if _, err := wallet.DecodeAddress(s.params.HRP, addr); err != nil {
		writeNamesJSON(w, http.StatusNotFound, map[string]any{"error": "not a valid address for this network"})
		return
	}
	recent, paid, minted, count := s.ix.mintsOf(addr, mintRecentDefault)
	writeNamesJSON(w, http.StatusOK, map[string]any{
		"address": addr, "count": count, "paid": paid, "paidCoin": wallet.FormatAmount(paid),
		"minted": minted, "mintedCoin": wallet.FormatAmount(minted), "requests": mintRecordsJSON(recent),
	})
}

// ---- pages ----

type mintPageData struct {
	Window       bool
	Status       string
	Start, End   uint32
	Blocks       uint32 // of the window
	Height       int64
	Remaining    int64
	ETA          string // how long the remaining blocks take
	Rate         consensus.Amount
	PerBlock     consensus.Amount
	MaxPaid      consensus.Amount
	Cap          consensus.Amount
	Asset        string
	AssetName    string
	Address      string
	Minted       consensus.Amount
	Requests     int64
	Pct          float64 // minted, of the cap
	Pending      consensus.Amount
	PendingCount int
	Maturity     uint32
	MaturityTime string
	Memo         string // the memo a request carries (consensus.MintMemo)
	Recent       []mintRecord
}

func (s *server) mintPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/early" {
		s.notFound(w, r.URL.Path)
		return
	}
	s.render(w, "mint", "Early blocks", s.mintData())
}

// mintData builds the /early page.
func (s *server) mintData() mintPageData {
	h := s.ix.indexedHeight()
	t := s.ix.mintTotals()
	d := mintPageData{Height: h, Minted: t.Minted, Requests: t.Requests, Maturity: consensus.CoinbaseMaturity, Memo: consensus.MintMemo, Recent: s.ix.recentMints(mintRecentDefault)}
	per := s.ix.meanInterval(120)
	d.MaturityTime = aboutTime(int64(consensus.CoinbaseMaturity), per)
	m := s.params.Mint
	if m == nil {
		d.Status = mintNone
		return d
	}
	d.Window = true
	d.Status, d.Remaining = mintStatus(s.params, h)
	d.Start, d.End, d.Blocks = m.Start, m.End, m.End-m.Start+1
	d.ETA = aboutTime(d.Remaining, per)
	d.Rate, d.PerBlock, d.MaxPaid, d.Cap = m.Rate, m.PerBlock, m.PerBlock/m.Rate, mintCap(m)
	d.Asset, d.AssetName, d.Address = m.Asset.String(), s.mintAssetName(), mintAddress(s.params)
	if d.Cap > 0 {
		d.Pct = float64(d.Minted) / float64(d.Cap) * 100
	}
	d.Pending, d.PendingCount = s.mintPending()
	return d
}

// mintReqView is a mint request as the transaction page says it.
type mintReqView struct {
	Buyer     string
	Paid      consensus.Amount
	Minted    consensus.Amount
	AssetName string
	Confirmed bool
	Height    uint32
}

// mintReqOf reads a transaction page's data as a mint request, if it is
// one. A confirmed request's figures come from the index.
func (s *server) mintReqOf(tx map[string]any, typed *txJSON) *mintReqView {
	buyer, paid, minted, ok := mintRequestOf(s.params, typed)
	if !ok {
		return nil
	}
	v := &mintReqView{Buyer: buyer, Paid: paid, Minted: minted, AssetName: s.mintAssetName()}
	if bh, _ := tx["blockhash"].(string); bh != "" {
		if hf, ok := tx["height"].(float64); ok {
			v.Confirmed, v.Height = true, uint32(hf)
			if rec, ok := s.ix.mintRecordOf(uint32(hf), typed.TxID); ok {
				v.Buyer, v.Paid, v.Minted = rec.Buyer, rec.Paid, rec.Minted
			}
		}
	}
	return v
}

// labelMintOutputs marks the outputs of a confirmed coinbase that pay the
// block's buyers.
func (s *server) labelMintOutputs(tx map[string]any) {
	if tx["coinbase"] != true || s.params.Mint == nil {
		return
	}
	hf, ok := tx["height"].(float64)
	if !ok {
		return
	}
	buyers := s.ix.mintBuyersAt(uint32(hf))
	if len(buyers) == 0 {
		return
	}
	outs, _ := tx["outputs"].([]any)
	for i, o := range outs {
		om, _ := o.(map[string]any)
		if om == nil || i == 0 || om["asset"] != nil {
			continue
		}
		if addr, _ := om["address"].(string); addr != "" {
			if _, ok := buyers[addr]; ok {
				om["mint"] = true
			}
		}
	}
}
