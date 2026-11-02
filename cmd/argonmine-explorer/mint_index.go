package main

import (
	"encoding/json"
	"strings"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/consensus"
	"argonmine/wallet"
)

// The mint window (consensus/mint.go): between Params.Mint.Start and End a
// block's coinbase creates ARG for the mint requests it includes. The index
// keeps one record per request, by block and by the address that made it,
// and a running total, so that the supply counts what the window created.
// Every record is undone with its block on a reorg.
//
// Keys:
//
//	m || height(BE32) || tx(BE32)               -> mintRecord (requests, by block)
//	b || buyer || 0 || height(BE32) || tx(BE32) -> mintRecord (the requests of an address)
//	M                                           -> mintTotals
//
// A confirmed request is a valid one: a block with an invalid request is
// not a block, so the index applies the same tests as consensus (the
// window, the per-request cap) and nothing more.

// mintRecord is one mint request as the API lists it: what the buyer paid
// in the mint asset and what the block's coinbase paid the buyer in ARG.
type mintRecord struct {
	Height uint32           `json:"height"`
	Time   uint64           `json:"time"`
	TxID   string           `json:"txid"`
	Buyer  string           `json:"buyer"`
	Paid   consensus.Amount `json:"paid"`
	Minted consensus.Amount `json:"minted"`
	ti     uint32
}

// mintTotals is the running total of the window (key M).
type mintTotals struct {
	Minted   consensus.Amount `json:"minted"`
	Requests int64            `json:"requests"`
}

// mintUndo reverses one request of a block.
type mintUndo struct {
	TI     uint32           `json:"i"`
	Buyer  string           `json:"b"`
	Minted consensus.Amount `json:"m"`
}

func keyMint(h, ti uint32) []byte {
	k := append([]byte{'m'}, be32(h)...)
	return append(k, be32(ti)...)
}
func keyMintBuyerPrefix(addr string) []byte {
	k := append([]byte{'b'}, addr...)
	return append(k, 0)
}
func keyMintBuyer(addr string, h, ti uint32) []byte {
	k := append(keyMintBuyerPrefix(addr), be32(h)...)
	return append(k, be32(ti)...)
}

var keyMintTotals = []byte("M")

// mintAddress is the address of the network's mint key, "" without a window.
func mintAddress(p *consensus.Params) string {
	if p == nil || p.Mint == nil {
		return ""
	}
	a, err := wallet.EncodeAddress(p.HRP, consensus.OutputTypeP2PK, p.Mint.Key)
	if err != nil {
		return ""
	}
	return a
}

// mintRequestOf reads tx as a mint request: the buyer (the owner of the
// coin its first input spends), what it pays the mint key in the mint
// asset, and the ARG that buys under the rule. ok is false for any other
// transaction, for one that pays nothing, and for one that asks for more
// than a block may mint; the window is the caller's test (it depends on
// the height).
func mintRequestOf(p *consensus.Params, tx *txJSON) (buyer string, paid, minted consensus.Amount, ok bool) {
	if p == nil || p.Mint == nil || tx.Coinbase {
		return "", 0, 0, false
	}
	memo := false
	for _, b := range nullDataPayloads(tx) {
		if string(b) == consensus.MintMemo {
			memo = true
			break
		}
	}
	if !memo {
		return "", 0, 0, false
	}
	addr, asset := mintAddress(p), p.Mint.Asset.String()
	if addr == "" {
		return "", 0, 0, false
	}
	for _, o := range tx.Outputs {
		if o.Asset == asset && strings.EqualFold(o.Address, addr) {
			paid = satAdd(paid, o.Amount)
		}
	}
	if paid <= 0 {
		return "", 0, 0, false
	}
	minted, err := consensus.MintValue(p.Mint, paid)
	if err != nil || minted > p.Mint.PerBlock {
		return "", 0, 0, false
	}
	if len(tx.Inputs) == 0 || tx.Inputs[0].Prevout == nil || tx.Inputs[0].Prevout.Address == "" {
		return "", 0, 0, false
	}
	return tx.Inputs[0].Prevout.Address, paid, minted, true
}

// blockMints lists the mint requests of blk, in transaction order.
func (ix *Indexer) blockMints(blk *blockJSON) []mintRecord {
	if !consensus.MintOpen(ix.params, blk.Height) {
		return nil
	}
	var out []mintRecord
	for ti := range blk.Tx {
		if ti == 0 {
			continue
		}
		tx := &blk.Tx[ti]
		buyer, paid, minted, ok := mintRequestOf(ix.params, tx)
		if !ok {
			continue
		}
		out = append(out, mintRecord{Height: blk.Height, Time: blk.Time, TxID: tx.TxID, Buyer: buyer, Paid: paid, Minted: minted, ti: uint32(ti)})
	}
	return out
}

// applyMints writes a block's requests and moves the totals.
func (ix *Indexer) applyMints(mints []mintRecord, batch *leveldb.Batch, undo *blockUndo) {
	if len(mints) == 0 {
		return
	}
	t := ix.mintTotals()
	for _, m := range mints {
		b, _ := json.Marshal(m)
		batch.Put(keyMint(m.Height, m.ti), b)
		batch.Put(keyMintBuyer(m.Buyer, m.Height, m.ti), b)
		undo.Mints = append(undo.Mints, mintUndo{TI: m.ti, Buyer: m.Buyer, Minted: m.Minted})
		t.Minted = satAdd(t.Minted, m.Minted)
		t.Requests++
	}
	tb, _ := json.Marshal(t)
	batch.Put(keyMintTotals, tb)
}

// rewindMints undoes a block's requests.
func (ix *Indexer) rewindMints(h uint32, undo *blockUndo, batch *leveldb.Batch) {
	if len(undo.Mints) == 0 {
		return
	}
	t := ix.mintTotals()
	for _, u := range undo.Mints {
		batch.Delete(keyMint(h, u.TI))
		batch.Delete(keyMintBuyer(u.Buyer, h, u.TI))
		t.Minted = satSub(t.Minted, u.Minted)
		t.Requests--
	}
	if t.Requests <= 0 {
		batch.Delete(keyMintTotals)
		return
	}
	tb, _ := json.Marshal(t)
	batch.Put(keyMintTotals, tb)
}

// withoutMintOutputs returns the coinbase outputs less the ones that pay
// the block's buyers (one output per buyer, after the miner's), so that
// the referral test sees the coinbase as it was before the mint.
func withoutMintOutputs(outs []wallet.TxOutJSON, mints []mintRecord) []wallet.TxOutJSON {
	if len(mints) == 0 || len(outs) < 2 {
		return outs
	}
	due := map[string]consensus.Amount{}
	for _, m := range mints {
		due[m.Buyer] = satAdd(due[m.Buyer], m.Minted)
	}
	kept := []wallet.TxOutJSON{outs[0]}
	for _, o := range outs[1:] {
		if amt, ok := due[o.Address]; ok && o.Asset == "" && o.Amount == amt {
			delete(due, o.Address)
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

// ---- reads ----

// mintTotals is what the window has created so far.
func (ix *Indexer) mintTotals() mintTotals {
	var t mintTotals
	if b, err := ix.db.Get(keyMintTotals, nil); err == nil {
		json.Unmarshal(b, &t)
	}
	return t
}

// mintedAt is the ARG the window had created by block h: the running
// total kept in the block's summary.
func (ix *Indexer) mintedAt(h int64) consensus.Amount {
	if h < 0 {
		return 0
	}
	if b, ok := ix.block(uint32(h)); ok {
		return b.MintedTotal
	}
	return 0
}

// recentMints lists the newest requests (at most limit). Never nil.
func (ix *Indexer) recentMints(limit int) []mintRecord {
	iter := ix.db.NewIterator(util.BytesPrefix([]byte{'m'}), nil)
	defer iter.Release()
	out := []mintRecord{}
	for ok := iter.Last(); ok && len(out) < limit; ok = iter.Prev() {
		var m mintRecord
		if json.Unmarshal(iter.Value(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// mintsAt lists the requests of block h, in transaction order.
func (ix *Indexer) mintsAt(h uint32) []mintRecord {
	iter := ix.db.NewIterator(util.BytesPrefix(append([]byte{'m'}, be32(h)...)), nil)
	defer iter.Release()
	var out []mintRecord
	for iter.Next() {
		var m mintRecord
		if json.Unmarshal(iter.Value(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// mintRecordOf finds the request txid made in block h.
func (ix *Indexer) mintRecordOf(h uint32, txid string) (mintRecord, bool) {
	for _, m := range ix.mintsAt(h) {
		if m.TxID == txid {
			return m, true
		}
	}
	return mintRecord{}, false
}

// mintBuyersAt is what block h's coinbase owes each buyer, by address.
func (ix *Indexer) mintBuyersAt(h uint32) map[string]consensus.Amount {
	out := map[string]consensus.Amount{}
	for _, m := range ix.mintsAt(h) {
		out[m.Buyer] = satAdd(out[m.Buyer], m.Minted)
	}
	return out
}

// mintsOf lists an address's requests, newest first (at most limit), with
// its totals over all of them. The slice is never nil.
func (ix *Indexer) mintsOf(addr string, limit int) (recent []mintRecord, paid, minted consensus.Amount, count int64) {
	iter := ix.db.NewIterator(util.BytesPrefix(keyMintBuyerPrefix(addr)), nil)
	defer iter.Release()
	recent = []mintRecord{}
	for ok := iter.Last(); ok; ok = iter.Prev() {
		var m mintRecord
		if json.Unmarshal(iter.Value(), &m) != nil {
			continue
		}
		count++
		paid = satAdd(paid, m.Paid)
		minted = satAdd(minted, m.Minted)
		if len(recent) < limit {
			recent = append(recent, m)
		}
	}
	return recent, paid, minted, count
}
