package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/consensus"
	"argonmine/wallet"
)

// Indexing Argonmine (docs/BRIDGE.md, "Reorganisations").
//
// Every block the daemon reads is recorded with its hash, and every
// transaction that changes the bridge's state (a custody coin received or
// spent, a deposit settled, a burn recorded) is applied in one atomic write
// together with an undo record: the previous value of every key it wrote.
// When the node's chain no longer contains a recorded block, the daemon
// walks back to the last block both agree on and undoes exactly what the
// blocks above it did, newest first, then indexes the new branch. A failed
// call to the node is never taken for a reorganisation: the round stops and
// is tried again. Undo records are kept for the last l1UndoWindow blocks.

const l1UndoWindow = 1000

// L1Block is one indexed Argonmine block.
type L1Block struct {
	Hash  string   `json:"hash"`
	TxIDs []string `json:"txids,omitempty"` // the transactions that changed the state, in block order
}

// l1TxUndo is what indexing one transaction wrote.
type l1TxUndo struct {
	Height uint32   `json:"height"`
	Block  string   `json:"block"`
	Ops    []undoOp `json:"ops"`
}

// undoOp is one key a transaction wrote: what it held before (nil: absent)
// and what was written (nil: deleted). Undoing restores Prev only if the key
// still holds Written: anything changed since (a redemption released, a
// coin spent by the leader) is kept as it is.
type undoOp struct {
	Key     []byte `json:"k"`
	Prev    []byte `json:"p"`
	Written []byte `json:"w"`
}

func blockKey(h uint64) []byte     { return append([]byte(pfxBlock), be64(h)...) }
func txUndoKey(txid string) []byte { return append([]byte(pfxTxUndo), txid...) }

func (s *State) l1Block(h uint64) (*L1Block, bool) {
	var b L1Block
	if !s.get(blockKey(h), &b) {
		return nil, false
	}
	return &b, true
}

// scanL1 walks new blocks, recording custody coins, settled issuances and
// uARG burns, after undoing whatever a reorganisation took away.
func (s *Service) scanL1(ctx context.Context) error {
	info, err := s.cfg.Node.ChainInfo()
	if err != nil {
		return fmt.Errorf("node: %w", err)
	}
	if err := s.ensureEpochs(info.Blocks); err != nil {
		return err
	}
	height, hash, ok := s.st.Cursor("l1")
	if ok {
		fork, moved, err := s.l1Fork(height, hash, info.Blocks)
		if err != nil {
			return err
		}
		if moved {
			fh, err := s.cfg.Node.BlockHash(uint32(fork))
			if err != nil {
				return fmt.Errorf("node: %w", err)
			}
			s.cfg.Logf("bridge: Argonmine reorganised: the last block in common is %d (%s); undoing what the blocks above it did", fork, fh)
			if depth := height - fork; s.cfg.L1ReorgPause > 0 && depth > uint64(s.cfg.L1ReorgPause) {
				// Deeper than the guardians expect: stop paying anything until
				// a human has looked (docs/BRIDGE.md, "Pauses").
				if err := s.pauseAll(fmt.Sprintf("Argonmine reorganised %d blocks deep (blocks %d to %d undone, more than -l1-reorg-pause %d): issuance and releases are paused until an operator resumes them", depth, fork+1, height, s.cfg.L1ReorgPause)); err != nil {
					return err
				}
			}
			if err := s.undoL1Above(int64(fork)); err != nil {
				return err
			}
			if err := s.st.SetCursor("l1", fork, fh); err != nil {
				return err
			}
			height, hash = fork, fh
		}
	}
	// A block indexed only in part (the daemon stopped in its middle) is
	// undone and indexed again.
	above := int64(-1)
	if ok {
		above = int64(height)
	}
	if err := s.undoL1Above(above); err != nil {
		return err
	}
	start := uint64(0)
	if ok {
		start = height + 1
	}
	prev := hash
	for h := start; h <= uint64(info.Blocks); h++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		bh, err := s.cfg.Node.BlockHash(uint32(h))
		if err != nil {
			return fmt.Errorf("node: %w", err)
		}
		blk, err := s.cfg.Node.Block(bh)
		if err != nil {
			return fmt.Errorf("node: %w", err)
		}
		if h > 0 && prev != "" && blk.PrevHash != prev {
			return nil // the chain moved while it was read; the next round finds the fork
		}
		if err := s.indexL1Block(blk, h, bh); err != nil {
			return err
		}
		prev = bh
	}
	return nil
}

// l1Fork checks the cursor's block against the node. It returns the height
// of the last block both agree on, and whether that is below the cursor's
// block (or the cursor's hash was wrong). Only a hash the node returns
// that differs from the recorded one counts; an error is returned as such.
func (s *Service) l1Fork(height uint64, hash string, tip uint32) (uint64, bool, error) {
	if height <= uint64(tip) {
		h, err := s.cfg.Node.BlockHash(uint32(height))
		if err != nil {
			return 0, false, fmt.Errorf("node: %w", err)
		}
		if h == hash {
			return height, false, nil
		}
	}
	h := height
	if h > uint64(tip) {
		h = uint64(tip) // the node's chain is shorter: the blocks above its tip are gone
	}
	for {
		rec, ok := s.st.l1Block(h)
		if !ok {
			return 0, false, fmt.Errorf("Argonmine reorganised below block %d, deeper than the %d blocks this guardian can undo: restore the bridge state from a backup", h, l1UndoWindow)
		}
		nh, err := s.cfg.Node.BlockHash(uint32(h))
		if err != nil {
			return 0, false, fmt.Errorf("node: %w", err)
		}
		if nh == rec.Hash {
			return h, true, nil
		}
		if h == 0 {
			return 0, false, errors.New("the node's genesis block is not the one indexed: wrong network or data directory")
		}
		h--
	}
}

// undoL1Above undoes every recorded block above height, newest first.
func (s *Service) undoL1Above(above int64) error {
	var heights []uint64
	iter := s.st.db.NewIterator(&util.Range{Start: blockKey(uint64(above + 1)), Limit: util.BytesPrefix([]byte(pfxBlock)).Limit}, nil)
	for iter.Next() {
		heights = append(heights, binary.BigEndian.Uint64(iter.Key()[len(pfxBlock):]))
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return err
	}
	for i := len(heights) - 1; i >= 0; i-- {
		if err := s.undoL1Block(heights[i]); err != nil {
			return err
		}
	}
	return nil
}

// undoL1Block reverses what indexing one block did, in one atomic write.
func (s *Service) undoL1Block(h uint64) error {
	rec, ok := s.st.l1Block(h)
	if !ok {
		return nil
	}
	type held struct{ v []byte }
	overlay := map[string]held{}
	current := func(k []byte) ([]byte, error) {
		if v, ok := overlay[string(k)]; ok {
			return v.v, nil
		}
		return s.st.getRaw(k)
	}
	var ops []kvOp
	for i := len(rec.TxIDs) - 1; i >= 0; i-- {
		txid := rec.TxIDs[i]
		var u l1TxUndo
		if !s.st.get(txUndoKey(txid), &u) {
			continue
		}
		for j := len(u.Ops) - 1; j >= 0; j-- {
			op := u.Ops[j]
			cur, err := current(op.Key)
			if err != nil {
				return err
			}
			if !bytes.Equal(cur, op.Written) {
				s.cfg.Logf("bridge: undoing %s (Argonmine block %d): record %q changed since it was indexed and is kept", txid, h, printableKey(op.Key))
				continue
			}
			overlay[string(op.Key)] = held{op.Prev}
			ops = append(ops, kvOp{key: op.Key, value: op.Prev})
		}
		ops = append(ops, kvOp{key: txUndoKey(txid)})
	}
	ops = append(ops, kvOp{key: blockKey(h)})
	if len(rec.TxIDs) > 0 {
		s.cfg.Logf("bridge: Argonmine block %d (%s) left the chain: undid %d transactions", h, rec.Hash, len(rec.TxIDs))
	}
	return s.st.write(ops, true)
}

func assetName(id string) string {
	if id == "" {
		return "ARG"
	}
	return "asset " + id
}

func printableKey(k []byte) string {
	if len(k) > 2 && k[1] == '/' {
		return string(k[:2]) + hex.EncodeToString(k[2:])
	}
	return string(k[:1]) + hex.EncodeToString(k[1:])
}

// indexL1Block records one block and applies its transactions, then moves
// the cursor onto it.
func (s *Service) indexL1Block(blk *wallet.BlockJSON, h uint64, bh string) error {
	rec := &L1Block{Hash: bh}
	if err := s.st.put(blockKey(h), rec); err != nil {
		return err
	}
	for i := range blk.Tx {
		tx := &blk.Tx[i]
		if s.st.has(txUndoKey(tx.TxID)) {
			continue // already applied (a transaction is in one block of a chain)
		}
		w := &l1Writer{st: s.st, pos: map[string]int{}, blockTime: int64(blk.Time)}
		s.indexTx(w, tx, uint32(h))
		if len(w.ops) == 0 {
			w.flushLogs(s.cfg.Logf)
			continue
		}
		rec.TxIDs = append(rec.TxIDs, tx.TxID)
		if err := w.commit(tx.TxID, uint32(h), bh, rec); err != nil {
			return err
		}
		w.flushLogs(s.cfg.Logf)
	}
	if err := s.st.SetCursor("l1", h, bh); err != nil {
		return err
	}
	if h%100 == 0 && h > l1UndoWindow {
		return s.pruneL1(h - l1UndoWindow)
	}
	return nil
}

// pruneL1 drops the undo records of blocks below height.
func (s *Service) pruneL1(below uint64) error {
	var ops []kvOp
	iter := s.st.db.NewIterator(&util.Range{Start: blockKey(0), Limit: blockKey(below)}, nil)
	for iter.Next() {
		var rec L1Block
		if json.Unmarshal(iter.Value(), &rec) == nil {
			for _, txid := range rec.TxIDs {
				ops = append(ops, kvOp{key: txUndoKey(txid)})
			}
		}
		ops = append(ops, kvOp{key: append([]byte{}, iter.Key()...)})
	}
	iter.Release()
	if len(ops) == 0 {
		return nil
	}
	return s.st.write(ops, false)
}

// l1Writer collects what indexing one transaction writes, reading its own
// writes back, and commits them with their undo record.
type l1Writer struct {
	st   *State
	ops  []undoOp
	pos  map[string]int
	logs []string
	// blockTime is the block's timestamp: when a payment to a swap address
	// the mempool never showed this guardian was first seen.
	blockTime int64
}

func (w *l1Writer) raw(key []byte) []byte {
	if i, ok := w.pos[string(key)]; ok {
		return w.ops[i].Written
	}
	b, _ := w.st.getRaw(key)
	return b
}

func (w *l1Writer) get(key []byte, v any) bool {
	b := w.raw(key)
	return b != nil && json.Unmarshal(b, v) == nil
}

func (w *l1Writer) has(key []byte) bool { return w.raw(key) != nil }

func (w *l1Writer) set(key, value []byte) {
	if i, ok := w.pos[string(key)]; ok {
		w.ops[i].Written = value
		return
	}
	prev, _ := w.st.getRaw(key)
	if bytes.Equal(prev, value) {
		return // nothing changes
	}
	w.pos[string(key)] = len(w.ops)
	w.ops = append(w.ops, undoOp{Key: append([]byte{}, key...), Prev: prev, Written: value})
}

func (w *l1Writer) put(key []byte, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.set(key, b)
}

func (w *l1Writer) del(key []byte) { w.set(key, nil) }

func (w *l1Writer) logf(format string, args ...any) {
	w.logs = append(w.logs, fmt.Sprintf(format, args...))
}

func (w *l1Writer) flushLogs(logf func(string, ...any)) {
	for _, l := range w.logs {
		logf("bridge: %s", l)
	}
}

func (w *l1Writer) commit(txid string, height uint32, block string, rec *L1Block) error {
	undo, err := json.Marshal(l1TxUndo{Height: height, Block: block, Ops: w.ops})
	if err != nil {
		return err
	}
	recb, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ops := make([]kvOp, 0, len(w.ops)+2)
	for _, op := range w.ops {
		ops = append(ops, kvOp{key: op.Key, value: op.Written})
	}
	ops = append(ops, kvOp{key: txUndoKey(txid), value: undo}, kvOp{key: blockKey(uint64(height)), value: recb})
	return w.st.write(ops, false)
}

// indexTx records what a confirmed transaction means for the bridge.
func (s *Service) indexTx(w *l1Writer, tx *wallet.TxJSON, height uint32) {
	custody := s.cfg.Custody.Address()
	assetHex := s.asset.String()
	var inAsset, outAsset consensus.Amount
	for _, in := range tx.Inputs {
		if in.Prevout == nil {
			continue
		}
		if in.Prevout.Asset == "" && in.Prevout.Address == custody {
			w.del(utxoKey(in.TxID, in.Index))
			w.put(spendKey(in.TxID, in.Index), CustodySpend{TxID: tx.TxID, Height: height})
		}
		if in.Prevout.Asset == assetHex {
			inAsset += in.Prevout.Amount
		}
	}
	var memos [][]byte
	type swapOut struct {
		o *wallet.TxOutJSON
		d SwapDest
	}
	var swaps, lost []swapOut
	for i := range tx.Outputs {
		o := &tx.Outputs[i]
		if o.Type == consensus.OutputTypeNullData {
			if b, err := hex.DecodeString(o.Payload); err == nil {
				memos = append(memos, b)
			}
			continue
		}
		if o.Asset == assetHex {
			outAsset += o.Amount
			if o.Type == consensus.OutputTypeP2PK {
				if d, ok := ParseSwapAddress(s.cfg.Params.HRP, o.Address); ok {
					swaps = append(swaps, swapOut{o, d})
				}
			}
			continue
		}
		if o.Type == consensus.OutputTypeP2PK {
			if d, ok := ParseSwapAddress(s.cfg.Params.HRP, o.Address); ok {
				lost = append(lost, swapOut{o, d}) // not uARG: it can never move again
				continue
			}
		}
		if o.Asset == "" && o.Address == custody {
			w.put(utxoKey(tx.TxID, uint32(o.N)), &UTXO{TxID: tx.TxID, Index: uint32(o.N), Amount: o.Amount, Height: height, Coinbase: tx.Coinbase})
		}
	}
	// What the whole transaction pays out of the bridge: its payout depth
	// and the hold above the last tier follow this total, not one output.
	payout := s.txPayout(tx)
	// Payments to swap addresses, with the withdrawal memo the transaction
	// carries (withdraw.go): it matches them to a withdrawal on the page and
	// changes nothing about the payout.
	memo := withdrawMemo(memos)
	for _, x := range swaps {
		s.recordSwap(w, tx, height, x.o, x.d, memo, payout)
	}
	for _, x := range lost {
		id := tx.TxID + ":" + strconv.Itoa(x.o.N)
		if !w.has(swapPaymentKey(x.o.Address, id)) {
			recordSwapRef(w, x.o.Address, swapPaymentRef{ID: id, Amount: x.o.Amount, Memo: memo, Lost: true, Asset: x.o.Asset, Height: height,
				Created: time.Now().Unix(), Seen: s.paymentSeen(id, w.blockTime)})
			w.logf("%s sent %d units of %s to the swap address %s: not uARG, so it is never paid out", id, x.o.Amount, assetName(x.o.Asset), x.o.Address)
		}
	}
	// uARG appeared: only the guardians' joint key can issue it, so this is
	// one of their issuances, and it says which deposits it pays.
	if outAsset > inAsset {
		s.settleIssuance(w, tx, height, memos)
	}
	// A burn: uARG went in and less (or none) came out.
	if inAsset > outAsset {
		var memo []byte
		if len(memos) > 0 {
			memo = memos[len(memos)-1]
		}
		s.recordBurn(w, tx, height, inAsset-outAsset, memo, payout)
	}
}

// txPayout is the bridge payout of a whole transaction: every output of the
// bridge's uARG to a swap address, and what it burns (its uARG in less its
// uARG out). A reorganisation undoes all of a transaction's payouts
// together, so the payout depth of each of them, and the hold above the
// last tier, are chosen for this total (docs/BRIDGE.md, "Payout depth").
// Every guardian computes it from the transaction as its own node has it.
func (s *Service) txPayout(tx *wallet.TxJSON) consensus.Amount {
	assetHex := s.asset.String()
	var in, out, swaps consensus.Amount
	for _, i := range tx.Inputs {
		if i.Prevout != nil && i.Prevout.Asset == assetHex {
			in += i.Prevout.Amount
		}
	}
	for _, o := range tx.Outputs {
		if o.Asset != assetHex || o.Type == consensus.OutputTypeNullData {
			continue
		}
		out += o.Amount
		if o.Type == consensus.OutputTypeP2PK {
			if _, ok := ParseSwapAddress(s.cfg.Params.HRP, o.Address); ok {
				swaps += o.Amount
			}
		}
	}
	if in > out {
		swaps += in - out
	}
	return swaps
}

// settleIssuance marks the deposits a mined issuance pays as confirmed, by
// the deposit references it carries (never by address and amount), so that
// no deposit can be paid twice whoever coordinates the next batch. A
// reference to a deposit this guardian has not seen yet (a rebuilt state,
// an EVM scan behind) is kept, with the issuance's height, and applied when
// the deposit is seen. A reference that may name a deposit of an earlier
// vault of the chain as well as one of the current vault is never applied
// (refPredatesVault): the chain's issuance is paused for an operator.
func (s *Service) settleIssuance(w *l1Writer, tx *wallet.TxJSON, height uint32, memos [][]byte) {
	var refs []InboundRef
	for _, m := range memos {
		if r, ok := DecodeIssueMemo(m); ok {
			refs = r
			break
		}
	}
	var is Issue
	if w.get(issueKey(tx.TxID), &is) && !is.Confirmed {
		is.Confirmed = true
		w.put(issueKey(tx.TxID), &is)
	}
	legacy := false
	if refs == nil {
		// An issuance from before deposit references: only the leader knows
		// what it paid, by its own record of the transaction id.
		if is.TxID == "" {
			w.logf("issuance %s carries no deposit references; nothing is settled from it", tx.TxID)
			return
		}
		refs, legacy = is.Inbound, true
	}
	now := time.Now().Unix()
	for _, ref := range refs {
		if !legacy {
			if why := s.refPredatesVault(ref, tx.TxID, height); why != "" {
				if c := s.byID[ref.Chain]; c != nil {
					s.pauseChain(c, why)
				}
				w.logf("issuance %s: deposit reference %s not applied: %s", tx.TxID, ref, why)
				continue
			}
		}
		var d Inbound
		if !w.get(inboundKey(ref.Chain, ref.ID), &d) {
			if !legacy {
				w.put(settledKey(ref), &SettledRef{TxID: tx.TxID, Height: height})
				w.logf("issuance %s pays deposit %s, not on record here yet; it is settled when the deposit is seen", tx.TxID, ref)
			}
			continue
		}
		switch d.Status {
		case InboundPending, InboundIssued:
			if legacy && d.IssueTx != tx.TxID {
				continue
			}
			d.Status, d.IssueTx, d.Error, d.Updated = InboundConfirmed, tx.TxID, "", now
			w.put(inboundKey(ref.Chain, ref.ID), &d)
		case InboundConfirmed:
			if d.IssueTx != tx.TxID {
				w.logf("ALERT: deposit %s was issued twice, in %s and in %s", ref, d.IssueTx, tx.TxID)
			}
		default:
			w.logf("ALERT: issuance %s pays deposit %s, which is %s here", tx.TxID, ref, d.Status)
		}
	}
}

// refPredatesVault is why an issuance's deposit reference must not be
// applied to the chain's current vault ("" when it may be): the issuance was
// mined below the height the current vault pays from (its epoch or the
// chain's l1_start_height), so it was signed for a deposit of an earlier
// vault, yet the reference's id is one the current vault numbers its own
// deposits with (at or above its first deposit id). That happens only when
// the vault was deployed with a first deposit id too low, or l1_start_height
// is wrong; the deposit cannot be told apart and needs an operator. Before
// the vault's ids are checked (no deposit of it is on record yet) and for a
// height that is not known, it is "".
func (s *Service) refPredatesVault(ref InboundRef, txid string, height uint32) string {
	c, ok := s.byID[ref.Chain]
	if !ok || height == 0 {
		return ""
	}
	first := s.vaultFirstID(c)
	if first == 0 || ref.ID < first {
		return "" // not checked yet, or a deposit of an earlier vault: no deposit of this vault has the id
	}
	start, err := s.payableFrom(c)
	if err != nil || height >= start {
		return ""
	}
	return fmt.Sprintf("issuance %s, mined at Argonmine height %d before the current vault %s of %s (which pays from height %d), pays deposit %s, an id that vault numbers its own deposits with (from %d): it paid a deposit of an earlier vault, and is never applied to this vault's deposit %s. Issuance on %s is paused until an operator resumes it; a vault that replaces another must be deployed with a firstDepositId above every earlier deposit id (docs/BRIDGE.md, \"Replacing a vault\")",
		txid, height, HexAddress(c.Vault), c.Name, start, ref, first, ref, c.Name)
}

// vaultFirstID is the first deposit id of c's current vault as this guardian
// checked it (0: not checked yet; see checkVaultIdentity).
func (s *Service) vaultFirstID(c *chainState) uint64 {
	ep, ok := s.st.GetEpoch(c.ID)
	if !ok || !strings.EqualFold(ep.Vault, HexAddress(c.Vault)) {
		return 0
	}
	return ep.FirstID
}

// recordBurn records a redemption for a burn, once. payout is what the whole
// transaction pays out of the bridge (txPayout).
func (s *Service) recordBurn(w *l1Writer, tx *wallet.TxJSON, height uint32, burned consensus.Amount, memo []byte, payout consensus.Amount) {
	if w.has(redemptionKey(tx.TxID)) {
		return
	}
	now := time.Now().Unix()
	r := &Redemption{TxID: tx.TxID, Height: height, Amount: burned, TxPayout: payout, Created: now, Updated: now}
	s.classifyRedemption(r, memo)
	if old, ok := s.st.ArchivedRedemption(tx.TxID); ok && r.Status == RedemptionPending {
		switch old.Status {
		case RedemptionConfirmed:
			r.Status, r.EVMTx, r.Error = RedemptionConfirmed, old.EVMTx, "paid before the bridge state was migrated (archived record)"
		case RedemptionReleased:
			r.Status, r.EVMTx, r.Error = RedemptionUnreleasable, old.EVMTx, "a release was sent before the bridge state was migrated (archived record); check it by hand"
		}
	}
	w.put(redemptionKey(tx.TxID), r)
	w.logf("burn %s of %d units -> %s %s on %s to %s (%s)", tx.TxID, burned, r.TokenAmount, r.TokenSymbol, r.ChainName, r.To, r.Status)
}

// recordSwap records a redemption for uARG paid to a swap address, once
// per output: the address names the payout, and nothing can spend the
// output again. The payment's ref (withdraw.go, for the page only) is
// written whenever it is missing: a reorganisation that takes the block
// away deletes the ref but keeps a redemption that changed since (released,
// paid), and when the same payment is mined again only the ref comes back.
func (s *Service) recordSwap(w *l1Writer, tx *wallet.TxJSON, height uint32, o *wallet.TxOutJSON, d SwapDest, memo string, payout consensus.Amount) {
	vout := uint32(o.N)
	now := time.Now().Unix()
	r := &Redemption{TxID: tx.TxID, Vout: &vout, Address: o.Address, Height: height, Amount: o.Amount, TxPayout: payout, Memo: memo, Created: now, Updated: now}
	ref := swapPaymentRef{ID: r.ID(), Amount: o.Amount, Memo: memo, Height: height, Created: now, Seen: s.paymentSeen(r.ID(), w.blockTime)}
	if w.has(redemptionKey(r.ID())) {
		if !w.has(swapPaymentKey(o.Address, r.ID())) {
			recordSwapRef(w, o.Address, ref)
		}
		return
	}
	s.classifySwap(r, d)
	w.put(redemptionKey(r.ID()), r)
	recordSwapRef(w, o.Address, ref)
	w.logf("swap payment %s of %d units -> %s %s on %s to %s (%s)", r.ID(), r.Amount, r.TokenAmount, r.TokenSymbol, r.ChainName, r.To, r.Status)
}

// classifyRedemption fills a redemption's destination from the burn's memo:
// the chain, the token and the recipient, and the token amount to pay, or
// why it cannot be paid.
func (s *Service) classifyRedemption(r *Redemption, memo []byte) {
	m, ok := DecodeRedeemMemo(memo)
	if !ok {
		r.Status, r.Error = RedemptionUnreleasable, "no valid bridge memo"
		return
	}
	s.classifyPayout(r, m)
}

// classifySwap fills a swap payment's destination from its address.
func (s *Service) classifySwap(r *Redemption, d SwapDest) {
	r.To = HexAddress(d.To)
	m, err := s.swapRoute(d)
	if err != nil {
		r.Chain, r.TokenSymbol = d.ChainID, d.Symbol
		r.Status, r.Error = RedemptionUnreleasable, err.Error()
		return
	}
	s.classifyPayout(r, m)
}

// swapRoute maps a swap address's chain and token symbol to a configured
// chain and token, as a memo would name them.
func (s *Service) swapRoute(d SwapDest) (RedeemMemo, error) {
	c, ok := s.byID[d.ChainID]
	if !ok {
		return RedeemMemo{}, fmt.Errorf("chain %d is not served by this bridge", d.ChainID)
	}
	t, err := c.tokenBySymbol(d.Symbol)
	if err != nil {
		return RedeemMemo{}, err
	}
	return RedeemMemo{ChainID: c.ID, Token: t.Address, To: d.To}, nil
}

// tokenBySymbol is the one token of the chain with this symbol.
func (c *chainState) tokenBySymbol(sym string) (*Token, error) {
	var found *Token
	for i := range c.Tokens {
		if strings.EqualFold(c.Tokens[i].Symbol, sym) {
			if found != nil {
				return nil, fmt.Errorf("%s lists two tokens called %s, so a swap address cannot name one", c.Name, strings.ToUpper(sym))
			}
			found = &c.Tokens[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%s is not paid out on %s", strings.ToUpper(sym), c.Name)
	}
	return found, nil
}

// classifyPayout fills a redemption from the payout it names.
func (s *Service) classifyPayout(r *Redemption, m RedeemMemo) {
	r.To = HexAddress(m.To)
	c, t, err := s.resolve(m)
	if err != nil {
		r.Status, r.Error = RedemptionUnreleasable, err.Error()
		return
	}
	r.Chain, r.ChainName, r.Token, r.TokenSymbol, r.Decimals = c.ID, c.Name, HexAddress(t.Address), t.Symbol, t.Decimals
	amt := TokenFromUnits(r.Amount, t.Decimals)
	r.TokenAmount = amt.String()
	if amt.Sign() == 0 {
		r.Status, r.Error = RedemptionUnreleasable, "burn too small to pay out"
		return
	}
	if belowMinRelease(t, amt) {
		r.Status, r.Error = RedemptionUnreleasable, minReleaseReason(c, t)
		return
	}
	if start, err := s.payableFrom(c); err == nil && r.Height < start {
		r.Status, r.Error = RedemptionUnreleasable, predatesVault(c, r.Height, start)
		return
	}
	if need, ok, _, _ := s.payoutDepth(r); ok {
		r.Confirmations = need // as far as it is known now; the leader's round keeps it current
	}
	r.applyReleaseFee(c)
	r.Status, r.Error = RedemptionPending, ""
}

// belowMinRelease reports whether a payout of gross token units of t is
// below the token's minimum release (chains file, min_release): such a
// payout is never sent, so dust cannot make the gas key pay a release
// transaction per output.
func belowMinRelease(t *Token, gross *big.Int) bool {
	return t.MinRelease != nil && gross.Cmp(t.MinRelease) < 0
}

func minReleaseReason(c *chainState, t *Token) string {
	return fmt.Sprintf("below the minimum withdrawal of %s %s on %s: not paid out (it needs the guardians)", FormatBigTokenAmount(t.MinRelease, t.Decimals), t.Symbol, c.Name)
}

func predatesVault(c *chainState, height, start uint32) string {
	return fmt.Sprintf("burn at height %d predates the current vault of %s, which pays burns from Argonmine height %d", height, c.Name, start)
}

// resolve maps a memo to a configured chain and token. A BRG1 memo (no
// chain, no token) means the first chain's first token.
func (s *Service) resolve(m RedeemMemo) (*chainState, *Token, error) {
	if m.IsDefault() {
		return s.chains[0], &s.chains[0].Tokens[0], nil
	}
	c, ok := s.byID[m.ChainID]
	if !ok {
		return nil, nil, fmt.Errorf("chain %d is not served by this bridge", m.ChainID)
	}
	t, ok := c.Token(m.Token)
	if !ok {
		return nil, nil, fmt.Errorf("token %s is not accepted on %s", HexAddress(m.Token), c.Name)
	}
	return c, t, nil
}

// ---- vault epochs ----

// ensureEpochs makes sure every chain whose configured vault is not the one
// in use has its candidate epoch (vaultCandidate), built before this scan
// indexes anything: a replacement pays no burn mined below tip + 1. Nothing
// is recorded here; the epoch on record (and its L1Start) changes only when
// checkVaultIdentity passes the vault.
func (s *Service) ensureEpochs(tip uint32) error {
	for _, c := range s.chains {
		if _, err := s.vaultCandidate(c, func() (uint32, error) { return tip, nil }); err != nil {
			return err
		}
	}
	return nil
}

// vaultCandidate returns the epoch c's configured vault starts once it
// passes checkVaultIdentity, or nil when that vault is the one in use (its
// epoch is on record and checked). It is built once, from whichever scan
// needs it first, and kept in memory until it passes, never recorded before
// (a guardian restarted meanwhile builds it again):
//
//   - the epoch on record, when it names this vault but was recorded before
//     its check (by the build of 2026-10-28): checked as it is;
//   - an earlier epoch of the chain whose vault this is, when every vault
//     recorded after it never passed the check (Epoch.restorable): taken
//     back as it was;
//   - the chain's first epoch, when none is on record: burns from any
//     height (l1_start_height aside);
//   - a replacement of the vault in use: burns mined below Argonmine height
//     tip + 1 are never paid from it, whatever l1_start_height says, so
//     replacing a vault cannot pay old burns again, and its deposit ids
//     must lie above AboveID, the highest on record for the chain.
//
// A vault that is refused, mistyped or not deployed yet so never takes the
// place of the vault in use: going back to that vault needs nothing, and
// the reset-vault operator command drops a candidate to build it again.
func (s *Service) vaultCandidate(c *chainState, tip func() (uint32, error)) (*Epoch, error) {
	cur := HexAddress(c.Vault)
	ep, onRecord := s.st.GetEpoch(c.ID)
	if onRecord && strings.EqualFold(ep.Vault, cur) && ep.FirstID != 0 && !ep.Rescan {
		c.mu.Lock()
		c.cand = nil
		c.mu.Unlock()
		return nil, nil // in use
	}
	c.mu.Lock()
	cand := c.cand
	c.mu.Unlock()
	if cand != nil && strings.EqualFold(cand.Vault, cur) {
		return cand, nil
	}
	now := time.Now().Unix()
	switch {
	case onRecord && strings.EqualFold(ep.Vault, cur):
		cand = ep
	case onRecord:
		if back, ok := ep.restorable(cur); ok {
			s.cfg.Logf("bridge: %s: vault %s was in use before %s, which never passed its check: its epoch is taken back as it was (burns from Argonmine height %d, deposits from id %d)", c.Name, cur, ep.Vault, back.L1Start, back.FirstID)
			cand = back
			break
		}
		t, err := tip()
		if err != nil {
			return nil, err
		}
		cand = &Epoch{Vault: cur, L1Start: t + 1, Previous: ep.Vault, Since: now, AboveID: s.st.MaxDepositID(c.ID), Rescan: true, History: ep.succeeded()}
		s.cfg.Logf("bridge: the vault of %s changed from %s to %s: it is used once its deposit ids are checked above %d, the highest on record for the chain; it will pay no burn below Argonmine height %d (until then %s stays the chain's vault on record)", c.Name, ep.Vault, cur, cand.AboveID, cand.L1Start, ep.Vault)
	default:
		if c.L1StartHeight == 0 {
			s.cfg.Logf("bridge: %s has no l1_start_height: burns naming it from any Argonmine height are paid from vault %s", c.Name, cur)
		}
		cand = &Epoch{Vault: cur, Since: now}
	}
	c.mu.Lock()
	c.cand = cand
	c.mu.Unlock()
	return cand, nil
}

// payableFrom is the first Argonmine height whose burns the chain's current
// vault pays: the larger of its l1_start_height and the start of the epoch
// this guardian recorded for the vault. A vault not in use yet (its
// candidate not passed, checkVaultIdentity) pays nothing.
func (s *Service) payableFrom(c *chainState) (uint32, error) {
	ep, ok := s.st.GetEpoch(c.ID)
	if !ok || !strings.EqualFold(ep.Vault, HexAddress(c.Vault)) {
		why := "its first scan checks it"
		if e := c.identityErr(); e != "" {
			why = e
		}
		return 0, fmt.Errorf("the vault %s of %s is not in use yet (%s); payouts on it wait", HexAddress(c.Vault), c.Name, why)
	}
	start := c.L1StartHeight
	if ep.L1Start > start {
		start = ep.L1Start
	}
	return start, nil
}
