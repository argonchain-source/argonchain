package main

import (
	"container/list"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"argonmine/consensus"
)

// byteLRU is a least-recently-used cache of byte slices bounded by their
// total size. Entries larger than a quarter of the budget are not kept.
type byteLRU struct {
	mu    sync.Mutex
	max   int
	size  int
	ll    *list.List
	items map[string]*list.Element
}

type lruEntry struct {
	key string
	val []byte
	aux int64
}

func newByteLRU(maxBytes int) *byteLRU {
	return &byteLRU{max: maxBytes, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *byteLRU) get(key string) ([]byte, int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil, 0, false
	}
	c.ll.MoveToFront(e)
	en := e.Value.(*lruEntry)
	return en.val, en.aux, true
}

func (c *byteLRU) put(key string, val []byte, aux int64) {
	if len(val) > c.max/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		en := e.Value.(*lruEntry)
		c.size += len(val) - len(en.val)
		en.val, en.aux = val, aux
		c.ll.MoveToFront(e)
	} else {
		c.items[key] = c.ll.PushFront(&lruEntry{key: key, val: val, aux: aux})
		c.size += len(val)
	}
	for c.size > c.max {
		e := c.ll.Back()
		en := e.Value.(*lruEntry)
		c.ll.Remove(e)
		delete(c.items, en.key)
		c.size -= len(en.val)
	}
}

func (c *byteLRU) purge() {
	c.mu.Lock()
	c.ll.Init()
	c.items = map[string]*list.Element{}
	c.size = 0
	c.mu.Unlock()
}

// Read caches. Confirmed blocks and transactions do not change while they
// stay on the main chain, so the node's JSON for them is kept by hash and
// only the confirmation count is recomputed; a reorg seen by the index
// empties both caches.
const (
	blockCacheBytes = 48 << 20
	txCacheBytes    = 16 << 20
	nodeInfoTTL     = 5 * time.Second
	mempoolTTL      = 8 * time.Second
	mempoolMaxRows  = 200  // rows on the mempool page
	mempoolMaxScan  = 2000 // transactions read for the wallets' coin view
	chartsTTL       = 30 * time.Second
)

// cachedCall returns the node's answer to method(id, extra...) from cache,
// fetching and caching it when keep(answer) says it is final (a confirmed
// block or transaction). aux carries the height stored with it.
func (s *server) cachedCall(c *byteLRU, keep func(json.RawMessage) (int64, bool), method, id string, extra ...any) (json.RawMessage, error) {
	if b, _, ok := c.get(id); ok {
		return b, nil
	}
	raw, err := s.client.Call(method, append([]any{id}, extra...)...)
	if err != nil {
		return nil, err
	}
	if h, ok := keep(raw); ok {
		c.put(id, append([]byte(nil), raw...), h)
	}
	return raw, nil
}

// confirmedBlock reports whether a getblock answer is a main-chain block
// with a successor (so its "nextblockhash" is final too).
func confirmedBlock(raw json.RawMessage) (int64, bool) {
	var b struct {
		Height      int64  `json:"height"`
		OnMain      bool   `json:"onmainchain"`
		NextBlock   string `json:"nextblockhash"`
		Transaction []any  `json:"-"`
	}
	if json.Unmarshal(raw, &b) != nil || !b.OnMain || b.NextBlock == "" {
		return 0, false
	}
	return b.Height, true
}

// confirmedTx reports whether a getrawtransaction answer is in a block.
func confirmedTx(raw json.RawMessage) (int64, bool) {
	var t struct {
		BlockHash string `json:"blockhash"`
		Height    int64  `json:"height"`
	}
	if json.Unmarshal(raw, &t) != nil || t.BlockHash == "" {
		return 0, false
	}
	return t.Height, true
}

// confirmations of a main-chain item at height h, from the node tip the
// index last saw.
func (s *server) confirmations(h float64) uint32 {
	_, tip, _ := s.ix.status()
	if h < 0 || uint32(h) > tip {
		return 1
	}
	return tip - uint32(h) + 1
}

// nodeInfo is what the home page asks the node besides the index.
type nodeInfo struct {
	Difficulty float64
	Mempool    int
	MempoolB   float64
	Peers      int
}

type timedCache struct {
	mu  sync.Mutex
	at  time.Time
	val any
}

// get returns the cached value while fresh, otherwise rebuilds it (one
// caller at a time; the others wait and share the result).
func (c *timedCache) get(ttl time.Duration, build func() any) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.val != nil && time.Since(c.at) < ttl {
		return c.val
	}
	c.val, c.at = build(), time.Now()
	return c.val
}

// expire makes the next get rebuild.
func (c *timedCache) expire() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

func (s *server) nodeInfo() nodeInfo {
	return s.infoCache.get(nodeInfoTTL, func() any {
		var d nodeInfo
		var info struct {
			Difficulty float64 `json:"difficulty"`
		}
		if raw, err := s.client.Call("getblockchaininfo"); err == nil {
			json.Unmarshal(raw, &info)
			d.Difficulty = info.Difficulty
		}
		var mp struct {
			Size  int     `json:"size"`
			Bytes float64 `json:"bytes"`
		}
		if raw, err := s.client.Call("getmempoolinfo"); err == nil {
			json.Unmarshal(raw, &mp)
			d.Mempool, d.MempoolB = mp.Size, mp.Bytes
		}
		var ni struct {
			Connections int `json:"connections"`
		}
		if raw, err := s.client.Call("getnetworkinfo"); err == nil {
			json.Unmarshal(raw, &ni)
			d.Peers = ni.Connections
		}
		return d
	}).(nodeInfo)
}

// mempoolRow is one line of the mempool page, with what the transaction
// spends and pays for the wallets' coin view (/api/utxos). A transaction's
// row never changes while it waits, so rows are kept by txid between
// rebuilds.
type mempoolRow struct {
	TxID    string
	Size    float64
	In, Out float64
	Fee     consensus.Amount
	FeeRate float64
	Outputs int
	Spends  []coinRef // the coins it spends, with their owners
	Pays    []mpOut   // its outputs to addresses
}

// mpOut is an unconfirmed output to an address.
type mpOut struct {
	Index  uint32
	Addr   string
	Amount consensus.Amount
	Asset  string
}

type mempoolView struct {
	Rows     []mempoolRow // the best-paying mempoolMaxRows
	Count    int
	Bytes    float64
	Error    string
	All      []mempoolRow      // every transaction read (at most mempoolMaxScan)
	SpentBy  map[string]string // "txid:index" of a coin -> the transaction spending it
	Complete bool              // All is the whole mempool
}

type mempoolCache struct {
	timedCache
	rows map[string]mempoolRow
}

// mempoolView lists unconfirmed transactions with their fees (at most 200),
// rebuilt at most every mempoolTTL: one getrawmempool, plus one
// getrawtransaction per transaction not seen in an earlier build.
func (s *server) mempoolView() mempoolView {
	return s.mpCache.get(mempoolTTL, func() any {
		ids, err := s.client.RawMempool()
		if err != nil {
			s.logNode("mempool", err)
			return mempoolView{Error: nodeUnavailable}
		}
		old := s.mpCache.rows
		rows := make(map[string]mempoolRow, len(ids))
		var out []mempoolRow
		var totalBytes float64
		for i, id := range ids {
			if i >= mempoolMaxScan {
				break
			}
			rw, ok := old[id]
			if !ok {
				raw, err := s.client.Call("getrawtransaction", id)
				if err != nil {
					continue
				}
				rw = mempoolRowOf(id, raw)
			}
			rows[id] = rw
			totalBytes += rw.Size
			out = append(out, rw)
		}
		s.mpCache.rows = rows
		sort.Slice(out, func(i, j int) bool { return out[i].FeeRate > out[j].FeeRate })
		v := mempoolView{All: out, Count: len(ids), Bytes: totalBytes, SpentBy: map[string]string{}, Complete: len(out) == len(ids)}
		for _, rw := range out {
			for _, c := range rw.Spends {
				v.SpentBy[outpointKey(c.TxID, c.Index)] = rw.TxID
			}
		}
		v.Rows = out
		if len(v.Rows) > mempoolMaxRows {
			v.Rows = v.Rows[:mempoolMaxRows]
		}
		return v
	}).(mempoolView)
}

func outpointKey(txid string, index uint32) string { return fmt.Sprintf("%s:%d", txid, index) }

func mempoolRowOf(id string, raw json.RawMessage) mempoolRow {
	var tx txJSON
	json.Unmarshal(raw, &tx)
	var size struct {
		Size float64 `json:"size"`
	}
	json.Unmarshal(raw, &size)
	rw := mempoolRow{TxID: id, Size: size.Size, Outputs: len(tx.Outputs)}
	// ARG only: asset units never pay fees and are not value in ARG.
	var in, out consensus.Amount
	for _, i := range tx.Inputs {
		if i.Prevout != nil {
			rw.Spends = append(rw.Spends, coinRef{Addr: i.Prevout.Address, TxID: i.TxID, Index: i.Index})
		}
		if i.Prevout != nil && i.Prevout.Asset == "" {
			in = satAdd(in, i.Prevout.Amount)
		}
	}
	for n, o := range tx.Outputs {
		if o.Address != "" {
			rw.Pays = append(rw.Pays, mpOut{Index: uint32(n), Addr: o.Address, Amount: o.Amount, Asset: o.Asset})
		}
		if o.Asset == "" {
			out = satAdd(out, o.Amount)
		}
	}
	rw.In, rw.Out = float64(in), float64(out)
	if in >= out {
		rw.Fee = in - out
	}
	if rw.Size > 0 {
		rw.FeeRate = float64(rw.Fee) / rw.Size
	}
	return rw
}

// chartsEntry is the last /api/charts answer: the series change only with
// the height.
type chartsEntry struct {
	n    int
	h    int64
	body []byte
}
