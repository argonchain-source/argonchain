package main

import (
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/consensus"
	"argonmine/names"
)

// Names (docs/NAMES.md) are derived from confirmed transactions in block
// order, then transaction order, with the rules of package names. The index
// keeps one record per label, the events that led to it, and the lookups
// the pages need. Every change is undone with its block on a reorg.
//
// Keys:
//
//	n || label                                    -> names.Record
//	N || owner || 0 || label                      -> "" (the labels an address owns)
//	e || label || 0 || height(BE32) || tx(BE32)   -> nameEvent (history)
//	R || height(BE32) || tx(BE32)                 -> nameReg (registrations, by block)
//	x || expires(BE32) || label                   -> "" (records by expiry, for the active count)
//	C                                             -> number of records, decimal
//
// A record is never deleted once written (a dropped name keeps its last
// record, which reads as available), except when the block that created it
// is rewound.

// nameEvent is one operation that changed a name.
type nameEvent struct {
	Op     string `json:"op"`
	Height uint32 `json:"height"`
	Time   uint64 `json:"time"`
	TxID   string `json:"txid"`
	From   string `json:"from"` // the previous owner of a transfer; the signer otherwise
	To     string `json:"to"`   // the new owner of a transfer; the signer otherwise
	Years  int    `json:"years,omitempty"`
}

// nameReg is one registration, as /api/names/recent lists it.
type nameReg struct {
	Label   string `json:"name"`
	Owner   string `json:"owner"`
	Height  uint32 `json:"height"`
	Time    uint64 `json:"time"`
	TxID    string `json:"txid"`
	Expires uint32 `json:"expires_height"`
}

// nameUndo reverses one touch of a name by a block: the record before it
// (nil: the name did not exist), and whether the touch wrote an event or a
// registration row at (height, tx index).
type nameUndo struct {
	Label string        `json:"l"`
	TI    uint32        `json:"i"`
	Prev  *names.Record `json:"p,omitempty"`
	Event bool          `json:"e,omitempty"`
	Reg   bool          `json:"r,omitempty"`
}

func keyName(label string) []byte { return append([]byte{'n'}, label...) }
func keyOwnerPrefix(owner string) []byte {
	k := append([]byte{'N'}, owner...)
	return append(k, 0)
}
func keyOwnerName(owner, label string) []byte { return append(keyOwnerPrefix(owner), label...) }
func keyNameEventPrefix(label string) []byte {
	k := append([]byte{'e'}, label...)
	return append(k, 0)
}
func keyNameEvent(label string, h, ti uint32) []byte {
	k := append(keyNameEventPrefix(label), be32(h)...)
	return append(k, be32(ti)...)
}
func keyNameReg(h, ti uint32) []byte {
	k := append([]byte{'R'}, be32(h)...)
	return append(k, be32(ti)...)
}
func keyNameExpiry(expires uint32, label string) []byte {
	k := append([]byte{'x'}, be32(expires)...)
	return append(k, label...)
}

var keyNameCount = []byte("C")

// nameState is the names a block has touched so far, so that a second
// transaction of the block sees the first one's effect before the batch is
// written. A nil value means the name has no record.
type nameState struct {
	ix    *Indexer
	cur   map[string]*names.Record
	count int64 // change to the record count
}

func newNameState(ix *Indexer) *nameState {
	return &nameState{ix: ix, cur: map[string]*names.Record{}}
}

func (st *nameState) get(label string) *names.Record {
	if r, ok := st.cur[label]; ok {
		return r
	}
	r, ok := st.ix.nameRecord(label)
	if !ok {
		st.cur[label] = nil
		return nil
	}
	st.cur[label] = &r
	return &r
}

func (st *nameState) set(label string, r *names.Record) { st.cur[label] = r }

// labels lists the labels owner holds as the block has left them so far.
func (st *nameState) labels(owner string) []string {
	set := map[string]bool{}
	for _, l := range st.ix.ownerLabels(owner) {
		set[l] = true
	}
	for l, r := range st.cur {
		if r != nil && r.Owner == owner {
			set[l] = true
		} else {
			delete(set, l)
		}
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// write puts next in place of prev in the batch: the record and its owner
// and expiry lookups. Batch operations apply in order, so deleting a key
// and putting it back in the same batch leaves it present.
func (st *nameState) write(batch *leveldb.Batch, prev, next *names.Record) {
	if prev != nil {
		batch.Delete(keyOwnerName(prev.Owner, prev.Label))
		batch.Delete(keyNameExpiry(prev.Expires, prev.Label))
	}
	if next == nil {
		batch.Delete(keyName(prev.Label))
		return
	}
	b, _ := json.Marshal(next)
	batch.Put(keyName(next.Label), b)
	batch.Put(keyOwnerName(next.Owner, next.Label), nil)
	batch.Put(keyNameExpiry(next.Expires, next.Label), nil)
}

func (st *nameState) flush(batch *leveldb.Batch) {
	if st.count == 0 {
		return
	}
	batch.Put(keyNameCount, []byte(strconv.FormatInt(st.ix.nameCount()+st.count, 10)))
}

// nullDataPayloads returns the NULLDATA payloads of tx, in output order.
func nullDataPayloads(tx *txJSON) [][]byte {
	var out [][]byte
	for _, o := range tx.Outputs {
		if o.Type != consensus.OutputTypeNullData || o.Address != "" {
			continue
		}
		if b, err := hex.DecodeString(o.Payload); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// nameTxOf is what names.Apply needs to know about tx: its signer (the
// owner of the coin input 0 spends), output 0, and what it paid the
// treasury in ARG.
func nameTxOf(p *consensus.Params, tx *txJSON) names.Tx {
	ntx := names.Tx{TxID: tx.TxID}
	if len(tx.Inputs) > 0 && tx.Inputs[0].Prevout != nil {
		ntx.Signer = tx.Inputs[0].Prevout.Address
	}
	if len(tx.Outputs) > 0 {
		ntx.Out0 = tx.Outputs[0].Address
	}
	if treasury := names.TreasuryAddress(p); treasury != "" {
		for _, o := range tx.Outputs {
			if o.Asset == "" && strings.EqualFold(o.Address, treasury) {
				ntx.Paid = satAdd(ntx.Paid, o.Amount)
			}
		}
	}
	return ntx
}

// applyNames indexes the name operation tx carries, if any and if valid.
func (ix *Indexer) applyNames(blk *blockJSON, ti int, tx *txJSON, batch *leveldb.Batch, undo *blockUndo, st *nameState) {
	if ti == 0 || tx.Coinbase {
		return
	}
	op, ok := names.FindMemo(nullDataPayloads(tx))
	if !ok {
		return
	}
	ntx := nameTxOf(ix.params, tx)
	cur := st.get(op.Label)
	next, ok := names.Apply(ix.params, cur, op, ntx, blk.Height)
	if !ok {
		return
	}
	u := nameUndo{Label: op.Label, TI: uint32(ti), Prev: cur, Event: true}
	st.write(batch, cur, &next)
	st.set(op.Label, &next)
	if cur == nil {
		st.count++
	}
	ev := nameEvent{Op: op.Kind, Height: blk.Height, Time: blk.Time, TxID: tx.TxID, From: ntx.Signer, To: ntx.Signer, Years: op.Years}
	if op.Kind == names.OpTransfer {
		ev.From, ev.To = cur.Owner, next.Owner
	}
	eb, _ := json.Marshal(ev)
	batch.Put(keyNameEvent(op.Label, blk.Height, uint32(ti)), eb)
	if op.Kind == names.OpRegister {
		u.Reg = true
		rb, _ := json.Marshal(nameReg{Label: op.Label, Owner: next.Owner, Height: blk.Height, Time: blk.Time, TxID: tx.TxID, Expires: next.Expires})
		batch.Put(keyNameReg(blk.Height, uint32(ti)), rb)
	}
	undo.Names = append(undo.Names, u)
	if op.Kind == names.OpPrimary {
		// One name is shown for an address: the owner's other primary
		// flags come off, each as its own undoable touch.
		for _, l := range st.labels(next.Owner) {
			if l == op.Label {
				continue
			}
			r := st.get(l)
			if r == nil || !r.Primary {
				continue
			}
			prev := *r
			n := *r
			n.Primary = false
			st.write(batch, &prev, &n)
			st.set(l, &n)
			undo.Names = append(undo.Names, nameUndo{Label: l, TI: uint32(ti), Prev: &prev})
		}
	}
}

// rewindNames undoes a block's name touches, newest first.
func (ix *Indexer) rewindNames(h uint32, undo *blockUndo, batch *leveldb.Batch, st *nameState) {
	for i := len(undo.Names) - 1; i >= 0; i-- {
		u := undo.Names[i]
		if u.Event {
			batch.Delete(keyNameEvent(u.Label, h, u.TI))
		}
		if u.Reg {
			batch.Delete(keyNameReg(h, u.TI))
		}
		cur := st.get(u.Label)
		switch {
		case cur == nil && u.Prev == nil:
			continue
		case cur != nil && u.Prev == nil:
			st.count--
		case cur == nil && u.Prev != nil:
			st.count++
		}
		if cur == nil {
			// The record was dropped by a later touch's undo; put the
			// earlier state back without a stale delete.
			st.write(batch, nil, u.Prev)
		} else {
			st.write(batch, cur, u.Prev)
		}
		st.set(u.Label, u.Prev)
	}
}

// ---- reads ----

func (ix *Indexer) nameRecord(label string) (names.Record, bool) {
	var r names.Record
	b, err := ix.db.Get(keyName(label), nil)
	if err != nil {
		return r, false
	}
	return r, json.Unmarshal(b, &r) == nil
}

// hasName reports whether label has a record (current or dropped).
func (ix *Indexer) hasName(label string) bool {
	_, ok := ix.nameRecord(label)
	return ok
}

// nameCount is the number of records (names registered at some point and
// not rewound).
func (ix *Indexer) nameCount() int64 {
	b, err := ix.db.Get(keyNameCount, nil)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(string(b), 10, 64)
	return n
}

// activeNames counts the names that resolve at height h: every record
// less those whose expiry is at or below h.
func (ix *Indexer) activeNames(h uint32) int64 {
	total := ix.nameCount()
	if total == 0 {
		return 0
	}
	iter := ix.db.NewIterator(&util.Range{Start: []byte{'x'}, Limit: keyNameExpiry(h+1, "")}, nil)
	defer iter.Release()
	var expired int64
	for iter.Next() {
		expired++
	}
	if expired > total {
		return 0
	}
	return total - expired
}

// ownerLabels lists the labels owner holds, sorted.
func (ix *Indexer) ownerLabels(owner string) []string {
	if owner == "" {
		return nil
	}
	p := keyOwnerPrefix(owner)
	iter := ix.db.NewIterator(util.BytesPrefix(p), nil)
	defer iter.Release()
	var out []string
	for iter.Next() {
		out = append(out, string(iter.Key()[len(p):]))
	}
	return out
}

// indexedHeight is the height the index is at, or -1.
func (ix *Indexer) indexedHeight() int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.height
}

// ownerNames lists the records owner holds that are registered or in
// grace at the index height, oldest registration first, then by label.
func (ix *Indexer) ownerNames(owner string) []names.Record {
	h := ix.indexedHeight()
	out := []names.Record{}
	for _, l := range ix.ownerLabels(owner) {
		r, ok := ix.nameRecord(l)
		if !ok || h < 0 || names.Status(&r, uint32(h)) == names.StatusAvailable {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Registered != out[j].Registered {
			return out[i].Registered < out[j].Registered
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// displayName is the name shown for addr: its primary active name, else
// its oldest active name; "" when it has none.
func (ix *Indexer) displayName(addr string) string {
	h := ix.indexedHeight()
	if h < 0 {
		return ""
	}
	best := ""
	var bestReg uint32
	for _, l := range ix.ownerLabels(addr) {
		r, ok := ix.nameRecord(l)
		if !ok || !names.Active(&r, uint32(h)) {
			continue
		}
		if r.Primary {
			return l
		}
		if best == "" || r.Registered < bestReg || (r.Registered == bestReg && l < best) {
			best, bestReg = l, r.Registered
		}
	}
	return best
}

// nameHistory lists a name's events, newest first (at most limit).
func (ix *Indexer) nameHistory(label string, limit int) []nameEvent {
	iter := ix.db.NewIterator(util.BytesPrefix(keyNameEventPrefix(label)), nil)
	defer iter.Release()
	out := []nameEvent{}
	for ok := iter.Last(); ok && len(out) < limit; ok = iter.Prev() {
		var e nameEvent
		if json.Unmarshal(iter.Value(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// nameEventOf finds the event tx wrote for label at height h, if the
// operation it carried was applied.
func (ix *Indexer) nameEventOf(label string, h uint32, txid string) (nameEvent, bool) {
	iter := ix.db.NewIterator(util.BytesPrefix(append(keyNameEventPrefix(label), be32(h)...)), nil)
	defer iter.Release()
	for iter.Next() {
		var e nameEvent
		if json.Unmarshal(iter.Value(), &e) == nil && e.TxID == txid {
			return e, true
		}
	}
	return nameEvent{}, false
}

// recentNames lists the newest registrations (at most limit).
func (ix *Indexer) recentNames(limit int) []nameReg {
	iter := ix.db.NewIterator(util.BytesPrefix([]byte{'R'}), nil)
	defer iter.Release()
	out := []nameReg{}
	for ok := iter.Last(); ok && len(out) < limit; ok = iter.Prev() {
		var r nameReg
		if json.Unmarshal(iter.Value(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// namesSince counts the registrations in blocks timed at or after t.
func (ix *Indexer) namesSince(t uint64) int {
	iter := ix.db.NewIterator(util.BytesPrefix([]byte{'R'}), nil)
	defer iter.Release()
	n := 0
	for ok := iter.Last(); ok; ok = iter.Prev() {
		var r nameReg
		if json.Unmarshal(iter.Value(), &r) != nil {
			continue
		}
		if r.Time < t {
			break
		}
		n++
	}
	return n
}

// timeAt is the time of block h: from the index for blocks it has, else
// an estimate of ten minutes a block past the newest indexed block.
func (ix *Indexer) timeAt(h uint32) uint64 {
	tip := ix.indexedHeight()
	if tip < 0 {
		return uint64(time.Now().Unix()) + uint64(h)*uint64(consensus.TargetSpacing)
	}
	if int64(h) <= tip {
		if b, ok := ix.block(h); ok {
			return b.Time
		}
		return 0
	}
	b, ok := ix.block(uint32(tip))
	if !ok {
		return 0
	}
	return b.Time + (uint64(h)-uint64(tip))*uint64(consensus.TargetSpacing)
}
