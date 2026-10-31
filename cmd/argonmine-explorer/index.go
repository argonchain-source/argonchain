package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/consensus"
	"argonmine/wallet"
)

// The explorer keeps its own small index, built from the node over RPC:
// per-block summaries, per-address history (ARG and every other asset) and
// an asset registry. Block and transaction details are fetched live from
// the node, which has its own tx index.
//
// Keys:
//
//	s                      -> {height, hash} sync point
//	h || height(BE32)      -> BlockSummary
//	a || addr || 0 || height(BE32) || txid -> AddrEntry
//	A || addr              -> AddrTotals
//	r || asset             -> AssetInfo (registry; asset is the 64-hex id)
//	t || asset || 0 || height(BE32) || txid -> AssetTx
//	c || addr || 0 || height(BE32) || txid -> coinbaseEntry (mining rewards, for the maturing sum)
//	o || addr || 0 || txid || index(BE32) -> coinEntry (unspent outputs, for wallets that spend without a node)
//	u || height(BE32)      -> blockUndo (to rewind a block on reorg)
//	n, N, e, R, x, C       -> names (names_index.go)
//
// Lifetime totals use saturating arithmetic: asset amounts go up to 2^62 per
// output, so plain sums of them could overflow int64. A total that reached
// the limit stays there (display only; consensus never reads the index).

// BlockSummary is what the block lists show.
type BlockSummary struct {
	Hash    string           `json:"hash"`
	Height  uint32           `json:"height"`
	Time    uint64           `json:"time"`
	TxCount int              `json:"txs"`
	Size    int              `json:"size"`
	Miner   string           `json:"miner"`
	Reward  consensus.Amount `json:"reward"`
	Fees    consensus.Amount `json:"fees"`
	Bits    string           `json:"bits"`
	Work    string           `json:"work"` // hex, 2^256/(target+1)
}

// AssetMove is what one transaction did to one address's holding of one
// non-ARG asset. Display is derived from Symbol when the entry is read.
type AssetMove struct {
	Asset      string           `json:"asset"`
	Symbol     string           `json:"symbol"`
	Display    string           `json:"display,omitempty"`
	Unverified bool             `json:"unverified,omitempty"` // a reserved symbol (UARG, WARG, ARG) on an asset that is not the genuine one
	Received   consensus.Amount `json:"received"`
	Sent       consensus.Amount `json:"sent"`
}

// AddrEntry is one transaction touching an address. Received and Sent are
// ARG; Assets lists every other asset the transaction moved for it.
type AddrEntry struct {
	Height   uint32           `json:"height"`
	Time     uint64           `json:"time"`
	TxID     string           `json:"txid"`
	Received consensus.Amount `json:"received"`
	Sent     consensus.Amount `json:"sent"`
	Coinbase bool             `json:"coinbase,omitempty"` // a mining reward, spendable after CoinbaseMaturity
	Referral bool             `json:"referral,omitempty"` // the referral output of a work-server coinbase (see referralOutput); also Coinbase
	Assets   []AssetMove      `json:"assets,omitempty"`
}

// indexVersion is bumped whenever the stored shapes change; a mismatch
// rebuilds the index from genesis on start.
const indexVersion = "7"

// AssetTotals are an address's lifetime totals for one non-ARG asset.
type AssetTotals struct {
	Symbol   string           `json:"symbol"`
	Received consensus.Amount `json:"received"`
	Sent     consensus.Amount `json:"sent"`
}

// AddrTotals are the lifetime totals for an address. Received and Sent are
// ARG; Assets holds the same per asset id. TxCount counts every transaction
// that touched the address, whatever it moved. The mining, referral and
// first/last-seen figures are kept up to date block by block, so no page
// scans an address's history for them.
type AddrTotals struct {
	Received      consensus.Amount       `json:"received"`
	Sent          consensus.Amount       `json:"sent"`
	TxCount       int                    `json:"txcount"`
	Assets        map[string]AssetTotals `json:"assets,omitempty"`
	Mined         int                    `json:"mined,omitempty"`      // coinbase entries that are not referral shares
	MinedTotal    consensus.Amount       `json:"minedTotal,omitempty"` // ARG received in them
	ReferralCount int                    `json:"refCount,omitempty"`
	ReferralTotal consensus.Amount       `json:"refTotal,omitempty"`
	FirstHeight   uint32                 `json:"firstHeight,omitempty"`
	FirstTime     uint64                 `json:"firstTime,omitempty"`
	LastHeight    uint32                 `json:"lastHeight,omitempty"`
	LastTime      uint64                 `json:"lastTime,omitempty"`
}

// AssetInfo is the registry entry of a non-ARG asset. Supply and Display
// are derived when the entry is read.
type AssetInfo struct {
	Asset       string           `json:"asset"`
	Symbol      string           `json:"symbol"`
	Display     string           `json:"display,omitempty"`
	Unverified  bool             `json:"unverified,omitempty"` // reserved symbol, not the genuine asset: show the issuer, no logo
	Logo        string           `json:"logo,omitempty"`       // logo URL: the genuine uARG only
	Issuer      string           `json:"issuer"`               // 32-byte x-only public key, hex
	Issued      consensus.Amount `json:"issued"`
	Burned      consensus.Amount `json:"burned"`
	Supply      consensus.Amount `json:"supply"`
	TxCount     int              `json:"txcount"`
	FirstHeight uint32           `json:"firstHeight"`
	FirstTime   uint64           `json:"firstTime"`
	FirstTxID   string           `json:"firstTxid"`
}

// AssetTx is one transaction touching an asset: what it issued, burned and
// moved (the sum of its outputs in that asset).
type AssetTx struct {
	Height uint32           `json:"height"`
	Time   uint64           `json:"time"`
	TxID   string           `json:"txid"`
	Issued consensus.Amount `json:"issued"`
	Burned consensus.Amount `json:"burned"`
	Out    consensus.Amount `json:"out"`
}

type undoEntry struct {
	Addr     string           `json:"a"`
	TxID     string           `json:"t"`
	Received consensus.Amount `json:"r"`
	Sent     consensus.Amount `json:"s"`
	Assets   []AssetMove      `json:"m,omitempty"`
	Coinbase bool             `json:"c,omitempty"`
	Referral bool             `json:"f,omitempty"`
}

// coinbaseEntry is what a mining reward paid an address (key 'c').
type coinbaseEntry struct {
	Received consensus.Amount `json:"r"`
	Sent     consensus.Amount `json:"s"`
}

// assetUndo reverses one transaction's effect on one registry entry.
type assetUndo struct {
	Asset  string           `json:"x"`
	TxID   string           `json:"t"`
	Issued consensus.Amount `json:"i,omitempty"`
	Burned consensus.Amount `json:"b,omitempty"`
	First  bool             `json:"f,omitempty"` // this tx created the entry: drop it
}

// coinEntry is one unspent output of an address (key 'o').
type coinEntry struct {
	Amount   consensus.Amount `json:"v"`
	Asset    string           `json:"a,omitempty"`
	Height   uint32           `json:"h"`
	Coinbase bool             `json:"c,omitempty"`
}

// coinRef names a coin of an address; spentCoin is one a block spent,
// kept to put it back when the block is rewound.
type coinRef struct {
	Addr  string `json:"a"`
	TxID  string `json:"t"`
	Index uint32 `json:"i"`
}

type spentCoin struct {
	coinRef
	Coin coinEntry `json:"c"`
}

type blockUndo struct {
	Addrs   []undoEntry `json:"addrs"`
	Assets  []assetUndo `json:"assets,omitempty"`
	Spent   []spentCoin `json:"spent,omitempty"`
	Created []coinRef   `json:"created,omitempty"`
	Names   []nameUndo  `json:"names,omitempty"`
}

type syncPoint struct {
	Height int64  `json:"height"`
	Hash   string `json:"hash"`
}

// issuanceJSON is one entry of a version-2 transaction's "issuances".
type issuanceJSON struct {
	Asset  string           `json:"asset"`
	Issuer string           `json:"issuer"`
	Symbol string           `json:"symbol"`
	Amount consensus.Amount `json:"amount"`
}

// txJSON is the node's transaction shape with the asset fields the wallet
// client does not model.
type txJSON struct {
	wallet.TxJSON
	Issuances []issuanceJSON `json:"issuances,omitempty"`
}

// blockJSON is the node's getblock verbosity-2 shape (superset of wallet.BlockJSON).
type blockJSON struct {
	Hash     string   `json:"hash"`
	Height   uint32   `json:"height"`
	PrevHash string   `json:"prevhash"`
	Time     uint64   `json:"time"`
	Bits     string   `json:"bits"`
	Size     int      `json:"size"`
	Tx       []txJSON `json:"tx"`
}

// Indexer syncs the index with the node.
type Indexer struct {
	db       *leveldb.DB
	client   *wallet.Client
	params   *consensus.Params
	treasury string // the treasury's address, or "" on a network without one
	logf     func(string, ...any)

	onRewind func() // called after a block is rewound (reorg), to drop caches

	mu      sync.RWMutex
	height  int64
	hash    string
	tip     uint32
	lastErr string // internal detail: logged, never shown publicly
}

func be32(h uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, h)
	return b
}

func keyBlock(h uint32) []byte     { return append([]byte{'h'}, be32(h)...) }
func keyUndo(h uint32) []byte      { return append([]byte{'u'}, be32(h)...) }
func keyTotals(addr string) []byte { return append([]byte{'A'}, addr...) }
func keyAsset(id string) []byte    { return append([]byte{'r'}, id...) }
func keyAddrPrefix(addr string) []byte {
	k := append([]byte{'a'}, addr...)
	return append(k, 0)
}
func keyAddr(addr string, h uint32, txid string) []byte {
	k := keyAddrPrefix(addr)
	k = append(k, be32(h)...)
	return append(k, txid...)
}
func keyCoinbasePrefix(addr string) []byte {
	k := append([]byte{'c'}, addr...)
	return append(k, 0)
}
func keyCoinbase(addr string, h uint32, txid string) []byte {
	k := keyCoinbasePrefix(addr)
	k = append(k, be32(h)...)
	return append(k, txid...)
}
func keyCoinPrefix(addr string) []byte {
	k := append([]byte{'o'}, addr...)
	return append(k, 0)
}
func keyCoin(addr, txid string, index uint32) []byte {
	k := keyCoinPrefix(addr)
	k = append(k, txid...)
	return append(k, be32(index)...)
}
func keyAssetTxPrefix(id string) []byte {
	k := append([]byte{'t'}, id...)
	return append(k, 0)
}
func keyAssetTx(id string, h uint32, txid string) []byte {
	k := keyAssetTxPrefix(id)
	k = append(k, be32(h)...)
	return append(k, txid...)
}

func newIndexer(path string, client *wallet.Client, params *consensus.Params, logf func(string, ...any)) (*Indexer, error) {
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, err
	}
	ix := &Indexer{db: db, client: client, params: params, logf: logf, height: -1}
	if len(params.TreasuryKey) == consensus.P2PKPayloadLen {
		if a, err := wallet.EncodeAddress(params.HRP, consensus.OutputTypeP2PK, params.TreasuryKey); err == nil {
			ix.treasury = a
		}
	}
	if v, err := db.Get([]byte("v"), nil); err != nil || string(v) != indexVersion {
		iter := db.NewIterator(nil, nil)
		batch := new(leveldb.Batch)
		for iter.Next() {
			batch.Delete(append([]byte{}, iter.Key()...))
		}
		iter.Release()
		batch.Put([]byte("v"), []byte(indexVersion))
		if err := db.Write(batch, nil); err != nil {
			db.Close()
			return nil, err
		}
		logf("index: format %s, rebuilding from genesis", indexVersion)
	}
	if b, err := db.Get([]byte("s"), nil); err == nil {
		var sp syncPoint
		if json.Unmarshal(b, &sp) == nil {
			ix.height, ix.hash = sp.Height, sp.Hash
		}
	}
	return ix, nil
}

func (ix *Indexer) close() { ix.db.Close() }

// run keeps the index in sync forever.
func (ix *Indexer) run() {
	for {
		if err := ix.syncOnce(); err != nil {
			ix.mu.Lock()
			repeat := ix.lastErr == err.Error()
			ix.lastErr = err.Error()
			ix.mu.Unlock()
			if !repeat {
				ix.logf("index: %v", err)
			}
			time.Sleep(5 * time.Second)
			continue
		}
		time.Sleep(2 * time.Second)
	}
}

func (ix *Indexer) syncOnce() error {
	info, err := ix.client.ChainInfo()
	if err != nil {
		return err
	}
	if info.Chain != ix.params.Name {
		return fmt.Errorf("node is on %s, explorer configured for %s", info.Chain, ix.params.Name)
	}
	ix.mu.Lock()
	ix.tip = info.Blocks
	ix.lastErr = ""
	ix.mu.Unlock()

	for {
		ix.mu.RLock()
		h, hash := ix.height, ix.hash
		ix.mu.RUnlock()
		// Reorg check: is our last indexed block still on the node's chain?
		if h >= 0 {
			nodeHash, err := ix.client.BlockHash(uint32(h))
			if err != nil || nodeHash != hash {
				if err := ix.rewind(uint32(h)); err != nil {
					return err
				}
				continue
			}
		}
		if h >= int64(info.Blocks) {
			return nil
		}
		next := uint32(h + 1)
		bh, err := ix.client.BlockHash(next)
		if err != nil {
			return err
		}
		raw, err := ix.client.Call("getblock", bh, 2)
		if err != nil {
			return err
		}
		var blk blockJSON
		if err := json.Unmarshal(raw, &blk); err != nil {
			return err
		}
		if h >= 0 && blk.PrevHash != hash {
			// Chain moved under us between the two calls; loop re-checks.
			continue
		}
		if err := ix.apply(&blk); err != nil {
			return err
		}
	}
}

// blockState accumulates the totals and registry entries a block touches so
// that several transactions of one block touching the same address or asset
// compose correctly before the single batch write.
type blockState struct {
	ix     *Indexer
	totals map[string]*AddrTotals
	assets map[string]*AssetInfo // nil value: entry to delete
}

func (bs *blockState) addr(addr string) *AddrTotals {
	if t, ok := bs.totals[addr]; ok {
		return t
	}
	t := bs.ix.totals(addr)
	bs.totals[addr] = &t
	return &t
}

func (bs *blockState) asset(id string) (*AssetInfo, bool) {
	if a, ok := bs.assets[id]; ok {
		return a, a != nil
	}
	a, ok := bs.ix.assetRaw(id)
	if !ok {
		bs.assets[id] = nil
		return nil, false
	}
	bs.assets[id] = &a
	return &a, true
}

func (bs *blockState) write(batch *leveldb.Batch) {
	for addr, t := range bs.totals {
		if t.TxCount <= 0 {
			batch.Delete(keyTotals(addr))
			continue
		}
		tb, _ := json.Marshal(t)
		batch.Put(keyTotals(addr), tb)
	}
	for id, a := range bs.assets {
		if a == nil {
			batch.Delete(keyAsset(id))
			continue
		}
		ab, _ := json.Marshal(a)
		batch.Put(keyAsset(id), ab)
	}
}

// assetFlow is what one transaction did with one asset.
type assetFlow struct {
	in, out, issued consensus.Amount
}

func (ix *Indexer) apply(blk *blockJSON) error {
	batch := new(leveldb.Batch)
	summary := BlockSummary{Hash: blk.Hash, Height: blk.Height, Time: blk.Time, TxCount: len(blk.Tx), Size: blk.Size, Bits: blk.Bits}
	if bits, err := parseBits(blk.Bits); err == nil {
		if target, err := consensus.CompactToTarget(bits); err == nil {
			summary.Work = fmt.Sprintf("%x", consensus.BlockWork(target))
		}
	}
	if len(blk.Tx) > 0 {
		cb := blk.Tx[0]
		for _, o := range cb.Outputs {
			summary.Reward += o.Amount
		}
		if len(cb.Outputs) > 0 {
			summary.Miner = cb.Outputs[0].Address
		}
		if sub := consensus.Subsidy(blk.Height); summary.Reward > sub {
			summary.Fees = summary.Reward - sub
		}
	}
	sb, _ := json.Marshal(summary)
	batch.Put(keyBlock(blk.Height), sb)

	state := &blockState{ix: ix, totals: map[string]*AddrTotals{}, assets: map[string]*AssetInfo{}}
	var undo blockUndo
	// The coin set, transaction by transaction: a coin created and spent in
	// the same block is put and deleted in that order in the batch.
	fresh := map[string]coinEntry{}
	for ti, tx := range blk.Tx {
		for _, in := range tx.Inputs {
			if in.Prevout == nil || in.Prevout.Address == "" {
				continue
			}
			ref := coinRef{Addr: in.Prevout.Address, TxID: in.TxID, Index: in.Index}
			k := keyCoin(ref.Addr, ref.TxID, ref.Index)
			c, ok := fresh[string(k)]
			if !ok {
				b, err := ix.db.Get(k, nil)
				if err != nil || json.Unmarshal(b, &c) != nil {
					continue
				}
			}
			delete(fresh, string(k))
			batch.Delete(k)
			undo.Spent = append(undo.Spent, spentCoin{coinRef: ref, Coin: c})
		}
		for n, o := range tx.Outputs {
			if o.Address == "" {
				continue
			}
			ref := coinRef{Addr: o.Address, TxID: tx.TxID, Index: uint32(n)} // outputs are listed in order
			c := coinEntry{Amount: o.Amount, Asset: o.Asset, Height: blk.Height, Coinbase: ti == 0}
			k := keyCoin(ref.Addr, ref.TxID, ref.Index)
			cb, _ := json.Marshal(c)
			batch.Put(k, cb)
			fresh[string(k)] = c
			undo.Created = append(undo.Created, ref)
		}
	}
	for ti, tx := range blk.Tx {
		// Registry: an issuance names the asset; a transfer of an asset the
		// index has never seen (impossible on a consistent chain) still gets
		// a nameless entry so its pages work.
		flows := map[string]*assetFlow{}
		var flowOrder []string
		flow := func(id string) *assetFlow {
			f, ok := flows[id]
			if !ok {
				f = &assetFlow{}
				flows[id] = f
				flowOrder = append(flowOrder, id)
			}
			return f
		}
		created := map[string]bool{}
		ensure := func(id string) *AssetInfo {
			a, ok := state.asset(id)
			if !ok {
				a = &AssetInfo{Asset: id, FirstHeight: blk.Height, FirstTime: blk.Time, FirstTxID: tx.TxID}
				state.assets[id] = a
				created[id] = true
			}
			return a
		}
		for _, is := range tx.Issuances {
			a := ensure(is.Asset)
			if a.Symbol == "" {
				a.Symbol, a.Issuer = is.Symbol, is.Issuer
			}
			f := flow(is.Asset)
			f.issued = satAdd(f.issued, is.Amount)
		}

		// Address deltas: ARG in Received/Sent, everything else per asset.
		type agg struct {
			recv, sent consensus.Amount
			assets     map[string]*AssetMove
			order      []string
		}
		per := map[string]*agg{}
		var order []string
		touch := func(addr string) *agg {
			if addr == "" {
				return nil
			}
			a, ok := per[addr]
			if !ok {
				a = &agg{assets: map[string]*AssetMove{}}
				per[addr] = a
				order = append(order, addr)
			}
			return a
		}
		move := func(a *agg, id string) *AssetMove {
			m, ok := a.assets[id]
			if !ok {
				m = &AssetMove{Asset: id}
				a.assets[id] = m
				a.order = append(a.order, id)
			}
			return m
		}
		for _, o := range tx.Outputs {
			if o.Asset != "" {
				f := flow(o.Asset)
				f.out = satAdd(f.out, o.Amount)
				if a := touch(o.Address); a != nil {
					m := move(a, o.Asset)
					m.Received = satAdd(m.Received, o.Amount)
				}
				continue
			}
			if a := touch(o.Address); a != nil {
				a.recv = satAdd(a.recv, o.Amount)
			}
		}
		for _, in := range tx.Inputs {
			if in.Prevout == nil {
				continue
			}
			if in.Prevout.Asset != "" {
				f := flow(in.Prevout.Asset)
				f.in = satAdd(f.in, in.Prevout.Amount)
				if a := touch(in.Prevout.Address); a != nil {
					m := move(a, in.Prevout.Asset)
					m.Sent = satAdd(m.Sent, in.Prevout.Amount)
				}
				continue
			}
			if a := touch(in.Prevout.Address); a != nil {
				a.sent = satAdd(a.sent, in.Prevout.Amount)
			}
		}

		for _, id := range flowOrder {
			f := flows[id]
			a := ensure(id)
			burned := satSub(satAdd(f.in, f.issued), f.out)
			if burned < 0 {
				burned = 0 // the node served a transfer without its prevouts; never negative
			}
			a.Issued = satAdd(a.Issued, f.issued)
			a.Burned = satAdd(a.Burned, burned)
			a.TxCount++
			at := AssetTx{Height: blk.Height, Time: blk.Time, TxID: tx.TxID, Issued: f.issued, Burned: burned, Out: f.out}
			atb, _ := json.Marshal(at)
			batch.Put(keyAssetTx(id, blk.Height, tx.TxID), atb)
			undo.Assets = append(undo.Assets, assetUndo{Asset: id, TxID: tx.TxID, Issued: f.issued, Burned: burned, First: created[id]})
		}

		refAddr := ""
		if ti == 0 {
			if i := ix.referralOutput(tx, blk.Height); i >= 0 {
				refAddr = tx.Outputs[i].Address
			}
		}
		for _, addr := range order {
			a := per[addr]
			e := AddrEntry{Height: blk.Height, Time: blk.Time, TxID: tx.TxID, Received: a.recv, Sent: a.sent, Coinbase: ti == 0, Referral: ti == 0 && addr == refAddr}
			t := state.addr(addr)
			t.Received = satAdd(t.Received, a.recv)
			t.Sent = satAdd(t.Sent, a.sent)
			if t.TxCount == 0 {
				t.FirstHeight, t.FirstTime = blk.Height, blk.Time
			}
			t.LastHeight, t.LastTime = blk.Height, blk.Time
			t.TxCount++
			switch {
			case e.Referral:
				t.ReferralCount++
				t.ReferralTotal = satAdd(t.ReferralTotal, a.recv)
			case e.Coinbase:
				t.Mined++
				t.MinedTotal = satAdd(t.MinedTotal, a.recv)
			}
			if e.Coinbase {
				cb, _ := json.Marshal(coinbaseEntry{Received: a.recv, Sent: a.sent})
				batch.Put(keyCoinbase(addr, blk.Height, tx.TxID), cb)
			}
			for _, id := range a.order {
				m := *a.assets[id]
				if info, ok := state.asset(id); ok {
					m.Symbol = info.Symbol
				}
				e.Assets = append(e.Assets, m)
				if t.Assets == nil {
					t.Assets = map[string]AssetTotals{}
				}
				at := t.Assets[id]
				at.Symbol = m.Symbol
				at.Received = satAdd(at.Received, m.Received)
				at.Sent = satAdd(at.Sent, m.Sent)
				t.Assets[id] = at
			}
			sortMoves(e.Assets)
			eb, _ := json.Marshal(e)
			batch.Put(keyAddr(addr, blk.Height, tx.TxID), eb)
			undo.Addrs = append(undo.Addrs, undoEntry{Addr: addr, TxID: tx.TxID, Received: a.recv, Sent: a.sent, Assets: e.Assets, Coinbase: e.Coinbase, Referral: e.Referral})
		}
	}
	nstate := newNameState(ix)
	for ti := range blk.Tx {
		ix.applyNames(blk, ti, &blk.Tx[ti], batch, &undo, nstate)
	}
	nstate.flush(batch)
	state.write(batch)
	ub, _ := json.Marshal(undo)
	batch.Put(keyUndo(blk.Height), ub)
	spb, _ := json.Marshal(syncPoint{Height: int64(blk.Height), Hash: blk.Hash})
	batch.Put([]byte("s"), spb)
	if err := ix.db.Write(batch, nil); err != nil {
		return err
	}
	ix.mu.Lock()
	ix.height, ix.hash = int64(blk.Height), blk.Hash
	ix.mu.Unlock()
	return nil
}

// rewind removes block h from the index (reorg).
func (ix *Indexer) rewind(h uint32) error {
	ix.logf("index: rewinding block %d (reorg)", h)
	batch := new(leveldb.Batch)
	state := &blockState{ix: ix, totals: map[string]*AddrTotals{}, assets: map[string]*AssetInfo{}}
	if ub, err := ix.db.Get(keyUndo(h), nil); err == nil {
		var undo blockUndo
		if json.Unmarshal(ub, &undo) == nil {
			for _, u := range undo.Addrs {
				batch.Delete(keyAddr(u.Addr, h, u.TxID))
				t := state.addr(u.Addr)
				t.Received = satSub(t.Received, u.Received)
				t.Sent = satSub(t.Sent, u.Sent)
				t.TxCount--
				switch {
				case u.Referral:
					t.ReferralCount--
					t.ReferralTotal = satSub(t.ReferralTotal, u.Received)
				case u.Coinbase:
					t.Mined--
					t.MinedTotal = satSub(t.MinedTotal, u.Received)
				}
				if u.Coinbase {
					batch.Delete(keyCoinbase(u.Addr, h, u.TxID))
				}
				// The newest entry left below this block is the last seen.
				if t.TxCount > 0 && t.LastHeight >= h {
					if e, ok := ix.lastEntryBefore(u.Addr, h); ok {
						t.LastHeight, t.LastTime = e.Height, e.Time
					}
				}
				for _, m := range u.Assets {
					at := t.Assets[m.Asset]
					at.Received = satSub(at.Received, m.Received)
					at.Sent = satSub(at.Sent, m.Sent)
					if at.Received == 0 && at.Sent == 0 {
						delete(t.Assets, m.Asset)
						continue
					}
					if t.Assets == nil {
						t.Assets = map[string]AssetTotals{}
					}
					t.Assets[m.Asset] = at
				}
				if len(t.Assets) == 0 {
					t.Assets = nil
				}
			}
			// Coins: put back what the block spent, then drop what it
			// created (a coin both created and spent in it ends dropped).
			for _, c := range undo.Spent {
				cb, _ := json.Marshal(c.Coin)
				batch.Put(keyCoin(c.Addr, c.TxID, c.Index), cb)
			}
			for _, c := range undo.Created {
				batch.Delete(keyCoin(c.Addr, c.TxID, c.Index))
			}
			// Undo in reverse so a transaction that created an entry is
			// undone last and drops it.
			for i := len(undo.Assets) - 1; i >= 0; i-- {
				u := undo.Assets[i]
				batch.Delete(keyAssetTx(u.Asset, h, u.TxID))
				if u.First {
					state.assets[u.Asset] = nil
					continue
				}
				if a, ok := state.asset(u.Asset); ok {
					a.Issued = satSub(a.Issued, u.Issued)
					a.Burned = satSub(a.Burned, u.Burned)
					a.TxCount--
				}
			}
			nstate := newNameState(ix)
			ix.rewindNames(h, &undo, batch, nstate)
			nstate.flush(batch)
		}
	}
	state.write(batch)
	batch.Delete(keyUndo(h))
	batch.Delete(keyBlock(h))
	var sp syncPoint
	if h == 0 {
		sp = syncPoint{Height: -1}
	} else {
		prev, ok := ix.block(h - 1)
		if !ok {
			return fmt.Errorf("index: missing block %d during rewind", h-1)
		}
		sp = syncPoint{Height: int64(h - 1), Hash: prev.Hash}
	}
	spb, _ := json.Marshal(sp)
	batch.Put([]byte("s"), spb)
	if err := ix.db.Write(batch, nil); err != nil {
		return err
	}
	ix.mu.Lock()
	ix.height, ix.hash = sp.Height, sp.Hash
	ix.mu.Unlock()
	if ix.onRewind != nil {
		ix.onRewind()
	}
	return nil
}

// lastEntryBefore returns the newest history entry of addr below height h.
func (ix *Indexer) lastEntryBefore(addr string, h uint32) (AddrEntry, bool) {
	iter := ix.db.NewIterator(util.BytesPrefix(keyAddrPrefix(addr)), nil)
	defer iter.Release()
	ok := iter.Seek(keyAddr(addr, h, ""))
	if ok {
		ok = iter.Prev()
	} else {
		ok = iter.Last()
	}
	for ; ok; ok = iter.Prev() {
		var e AddrEntry
		if json.Unmarshal(iter.Value(), &e) == nil && e.Height < h {
			return e, true
		}
	}
	return AddrEntry{}, false
}

// referralOutput returns the index of the referral output of a coinbase
// built by a work server (cmd/argonmine-explorer/work.go): after the
// treasury's output, if the height pays one, exactly two ARG outputs to two
// different addresses, the second being p percent of their sum for a whole
// p from 1 to 50, computed as the server does (total/100*p). Any other
// coinbase has no referral output (-1); its outputs are all mining rewards.
func (ix *Indexer) referralOutput(cb txJSON, height uint32) int {
	outs := cb.Outputs
	// From the treasury height the coinbase ends with the treasury's output
	// (consensus/treasury.go; the explorer's own jobs put it last). It is
	// not a referral, so set it aside before looking for one.
	if want := consensus.TreasuryAmount(ix.params, height); want > 0 && ix.treasury != "" && len(outs) >= 2 {
		if last := outs[len(outs)-1]; last.Address == ix.treasury && last.Asset == "" && last.Amount >= want {
			outs = outs[:len(outs)-1]
		}
	}
	if len(outs) != 2 {
		return -1
	}
	a, b := outs[0], outs[1]
	if a.Asset != "" || b.Asset != "" || a.Address == "" || b.Address == "" || a.Address == b.Address || a.Amount < 0 || b.Amount <= 0 {
		return -1
	}
	total := satAdd(a.Amount, b.Amount)
	for p := consensus.Amount(1); p <= workMaxRefShare; p++ {
		if b.Amount == total/100*p {
			return 1
		}
	}
	return -1
}

// satAdd adds two amounts, stopping at the int64 limits instead of wrapping.
func satAdd(a, b consensus.Amount) consensus.Amount {
	s := a + b
	switch {
	case b > 0 && s < a:
		return math.MaxInt64
	case b < 0 && s > a:
		return math.MinInt64
	}
	return s
}

// satSub subtracts with the same saturation.
func satSub(a, b consensus.Amount) consensus.Amount {
	if b == math.MinInt64 {
		return satAdd(satAdd(a, math.MaxInt64), 1)
	}
	return satAdd(a, -b)
}

func sortMoves(ms []AssetMove) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Symbol != ms[j].Symbol {
			return ms[i].Symbol < ms[j].Symbol
		}
		return ms[i].Asset < ms[j].Asset
	})
}

// fillMoves derives the display symbols of an entry's asset movements.
func fillMoves(ms []AssetMove) {
	for i := range ms {
		ms[i].Display, ms[i].Unverified, _ = assetLabel(ms[i].Symbol, ms[i].Asset)
	}
}

func (ix *Indexer) totals(addr string) AddrTotals {
	var t AddrTotals
	if b, err := ix.db.Get(keyTotals(addr), nil); err == nil {
		json.Unmarshal(b, &t)
	}
	return t
}

// Coin is an unspent output of an address, as /api/utxos lists it.
type Coin struct {
	TxID     string           `json:"txid"`
	Index    uint32           `json:"index"`
	Amount   consensus.Amount `json:"amount"`
	Asset    string           `json:"asset,omitempty"`
	Height   uint32           `json:"height"`
	Coinbase bool             `json:"coinbase,omitempty"`
	SpentBy  string           `json:"spentBy,omitempty"` // a mempool transaction spending it
}

// coins returns at most limit unspent outputs of addr, and whether that
// is all of them.
func (ix *Indexer) coins(addr string, limit int) ([]Coin, bool) {
	iter := ix.db.NewIterator(util.BytesPrefix(keyCoinPrefix(addr)), nil)
	defer iter.Release()
	pre := len(keyCoinPrefix(addr))
	out := []Coin{}
	for iter.Next() {
		k := iter.Key()
		if len(k) < pre+1+4 {
			continue
		}
		if len(out) >= limit {
			return out, false
		}
		var c coinEntry
		if json.Unmarshal(iter.Value(), &c) != nil {
			continue
		}
		n := len(k) - 4
		out = append(out, Coin{TxID: string(k[pre:n]), Index: binary.BigEndian.Uint32(k[n:]), Amount: c.Amount, Asset: c.Asset, Height: c.Height, Coinbase: c.Coinbase})
	}
	return out, true
}

// AssetHolding is one row of an address's asset list.
type AssetHolding struct {
	Asset      string           `json:"asset"`
	Symbol     string           `json:"symbol"`
	Display    string           `json:"display"`
	Unverified bool             `json:"unverified,omitempty"`
	Issuer     string           `json:"issuer,omitempty"` // for an unverified look-alike: who issued it
	Logo       string           `json:"logo,omitempty"`
	Balance    consensus.Amount `json:"balance"`
	Received   consensus.Amount `json:"received"`
	Sent       consensus.Amount `json:"sent"`
}

// holdings lists an address's non-ARG assets, sorted by symbol then id.
// The slice is never nil so it encodes as [] rather than null.
func (ix *Indexer) holdings(t AddrTotals) []AssetHolding {
	out := make([]AssetHolding, 0, len(t.Assets))
	for id, at := range t.Assets {
		h := AssetHolding{Asset: id, Symbol: at.Symbol, Balance: satSub(at.Received, at.Sent), Received: at.Received, Sent: at.Sent}
		h.Display, h.Unverified, h.Logo = assetLabel(at.Symbol, id)
		if h.Unverified {
			if a, ok := ix.assetRaw(id); ok {
				h.Issuer = a.Issuer
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Asset < out[j].Asset
	})
	return out
}

// assetRaw reads a registry entry as stored.
func (ix *Indexer) assetRaw(id string) (AssetInfo, bool) {
	var a AssetInfo
	b, err := ix.db.Get(keyAsset(id), nil)
	if err != nil {
		return a, false
	}
	return a, json.Unmarshal(b, &a) == nil
}

func finishAsset(a *AssetInfo) {
	a.Display, a.Unverified, a.Logo = assetLabel(a.Symbol, a.Asset)
	a.Supply = satSub(a.Issued, a.Burned)
}

// asset returns a registry entry with its derived fields filled.
func (ix *Indexer) asset(id string) (AssetInfo, bool) {
	a, ok := ix.assetRaw(id)
	if ok {
		finishAsset(&a)
	}
	return a, ok
}

// assets lists the whole registry, sorted by symbol then id. The slice is
// never nil so it encodes as [] rather than null.
func (ix *Indexer) assets() []AssetInfo {
	iter := ix.db.NewIterator(util.BytesPrefix([]byte{'r'}), nil)
	defer iter.Release()
	out := []AssetInfo{}
	for iter.Next() {
		var a AssetInfo
		if json.Unmarshal(iter.Value(), &a) == nil {
			finishAsset(&a)
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Asset < out[j].Asset
	})
	return out
}

// assetHistory returns the newest transactions of an asset (at most limit),
// or those below height "before" when it is non-zero. Never nil.
func (ix *Indexer) assetHistory(id string, before uint32, limit int) []AssetTx {
	iter := ix.db.NewIterator(util.BytesPrefix(keyAssetTxPrefix(id)), nil)
	defer iter.Release()
	out := []AssetTx{}
	var ok bool
	if before == 0 {
		ok = iter.Last()
	} else if ok = iter.Seek(keyAssetTx(id, before, "")); ok {
		ok = iter.Prev() // the first key at or above "before" is too new
	} else {
		ok = iter.Last()
	}
	for ; ok && len(out) < limit; ok = iter.Prev() {
		var e AssetTx
		if json.Unmarshal(iter.Value(), &e) == nil && (before == 0 || e.Height < before) {
			out = append(out, e)
		}
	}
	return out
}

func (ix *Indexer) block(h uint32) (BlockSummary, bool) {
	var s BlockSummary
	b, err := ix.db.Get(keyBlock(h), nil)
	if err != nil {
		return s, false
	}
	return s, json.Unmarshal(b, &s) == nil
}

// latestBlocks returns up to n summaries at or below height "from", newest first.
func (ix *Indexer) latestBlocks(from int64, n int) []BlockSummary {
	var out []BlockSummary
	for h := from; h >= 0 && len(out) < n; h-- {
		if s, ok := ix.block(uint32(h)); ok {
			out = append(out, s)
		}
	}
	return out
}

// addressHistory returns the newest entries for addr (at most limit).
func (ix *Indexer) addressHistory(addr string, limit int) []AddrEntry {
	iter := ix.db.NewIterator(util.BytesPrefix(keyAddrPrefix(addr)), nil)
	defer iter.Release()
	var out []AddrEntry
	for ok := iter.Last(); ok && len(out) < limit; ok = iter.Prev() {
		var e AddrEntry
		if json.Unmarshal(iter.Value(), &e) == nil {
			fillMoves(e.Assets)
			out = append(out, e)
		}
	}
	return out
}

// addressHistoryBefore returns up to limit entries with height < before,
// newest first (paging cursor for the address page).
func (ix *Indexer) addressHistoryBefore(addr string, before uint32, limit int) []AddrEntry {
	iter := ix.db.NewIterator(util.BytesPrefix(keyAddrPrefix(addr)), nil)
	defer iter.Release()
	var out []AddrEntry
	// Position at the first key of height "before"; everything before it is older.
	ok := iter.Seek(keyAddr(addr, before, ""))
	if !ok {
		ok = iter.Last()
		if ok {
			var e AddrEntry
			if json.Unmarshal(iter.Value(), &e) == nil && e.Height < before {
				fillMoves(e.Assets)
				out = append(out, e)
			}
		}
	}
	for ok && len(out) < limit {
		ok = iter.Prev()
		if !ok {
			break
		}
		var e AddrEntry
		if json.Unmarshal(iter.Value(), &e) == nil && e.Height < before {
			fillMoves(e.Assets)
			out = append(out, e)
		}
	}
	return out
}

// AddrStats are derived figures for the address page.
type AddrStats struct {
	Mined         int // blocks whose reward the address received (referral shares excluded)
	FirstHeight   uint32
	FirstTime     uint64
	LastHeight    uint32
	LastTime      uint64
	MinedTotal    consensus.Amount
	ReferralCount int
	ReferralTotal consensus.Amount
}

// addressStats reads the figures kept in the address's totals.
func addressStats(t AddrTotals) AddrStats {
	return AddrStats{Mined: t.Mined, MinedTotal: t.MinedTotal, ReferralCount: t.ReferralCount, ReferralTotal: t.ReferralTotal,
		FirstHeight: t.FirstHeight, FirstTime: t.FirstTime, LastHeight: t.LastHeight, LastTime: t.LastTime}
}

// maturing sums the mining rewards of addr that are not yet spendable at
// index height h (younger than CoinbaseMaturity blocks). It reads only the
// address's newest coinbase entries.
func (ix *Indexer) maturing(addr string, h int64) consensus.Amount {
	p := keyCoinbasePrefix(addr)
	iter := ix.db.NewIterator(util.BytesPrefix(p), nil)
	defer iter.Release()
	var sum consensus.Amount
	for ok := iter.Last(); ok; ok = iter.Prev() {
		k := iter.Key()
		if len(k) < len(p)+4 {
			continue
		}
		height := int64(binary.BigEndian.Uint32(k[len(p):]))
		if height+int64(consensus.CoinbaseMaturity) <= h {
			break
		}
		var e coinbaseEntry
		if json.Unmarshal(iter.Value(), &e) == nil {
			sum = satAdd(sum, satSub(e.Received, e.Sent))
		}
	}
	return sum
}

// nodeUnavailable is all the public pages and API say about a failure to
// reach or use the node; the details (host, port, DNS, RPC errors) go to
// the log only.
const nodeUnavailable = "Node unavailable"

// status returns the index sync state. publicErr is nodeUnavailable when the
// last sync attempt failed, "" otherwise.
func (ix *Indexer) status() (height int64, tip uint32, publicErr string) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.lastErr != "" {
		publicErr = nodeUnavailable
	}
	return ix.height, ix.tip, publicErr
}

// hashrate estimates network hash rate over the last n blocks.
func (ix *Indexer) hashrate(n int) float64 {
	ix.mu.RLock()
	h := ix.height
	ix.mu.RUnlock()
	blocks := ix.latestBlocks(h, n+1)
	if len(blocks) < 2 {
		return 0
	}
	work := new(big.Int)
	for _, b := range blocks[:len(blocks)-1] {
		w, ok := new(big.Int).SetString(b.Work, 16)
		if ok {
			work.Add(work, w)
		}
	}
	span := int64(blocks[0].Time) - int64(blocks[len(blocks)-1].Time)
	if span <= 0 {
		return 0
	}
	f, _ := new(big.Float).SetInt(work).Float64()
	return f / float64(span)
}

// supply is the total subsidy issued by blocks 1 to h. The genesis block
// pays nothing: its coinbase never enters the UTXO set.
func supply(h int64) consensus.Amount {
	var total consensus.Amount
	for start := int64(0); start <= h; start += int64(consensus.HalvingInterval) {
		end := start + int64(consensus.HalvingInterval) - 1
		if end > h {
			end = h
		}
		total += consensus.Subsidy(uint32(start)) * consensus.Amount(end-start+1)
	}
	if h >= 0 {
		total -= consensus.Subsidy(0)
	}
	return total
}

func parseBits(s string) (uint32, error) {
	var v uint32
	_, err := fmt.Sscanf(s, "%x", &v)
	return v, err
}
