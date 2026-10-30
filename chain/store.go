// Package chain maintains the block index, the best chain, the UTXO set and
// undo data on disk, and applies the consensus rules to incoming blocks,
// including reorganisations.
package chain

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"argonmine/consensus"
)

// Key prefixes. Every key is prefix || payload.
const (
	prefixNode   = 'n' // hash -> header || height u32 || status u8
	prefixBlock  = 'b' // hash -> raw block
	prefixUndo   = 'u' // hash -> undo data
	prefixCoin   = 'c' // outpoint (36 bytes) -> coin
	prefixHeight = 'm' // height u32 big-endian -> hash (main chain only)
	prefixTxIdx  = 'x' // txid -> block hash || index u32 (main chain only)
	keyTip       = "t" // -> tip hash
	keyTxIndexed = "X" // present once the transaction index is complete
)

var errCorrupt = errors.New("chain: corrupt database record")

func nodeKey(h consensus.Hash) []byte  { return append([]byte{prefixNode}, h[:]...) }
func blockKey(h consensus.Hash) []byte { return append([]byte{prefixBlock}, h[:]...) }
func undoKey(h consensus.Hash) []byte  { return append([]byte{prefixUndo}, h[:]...) }
func heightKey(height uint32) []byte {
	k := make([]byte, 5)
	k[0] = prefixHeight
	binary.BigEndian.PutUint32(k[1:], height)
	return k
}
func txKey(txid consensus.Hash) []byte { return append([]byte{prefixTxIdx}, txid[:]...) }

func txLocValue(block consensus.Hash, index uint32) []byte {
	v := make([]byte, 36)
	copy(v, block[:])
	binary.LittleEndian.PutUint32(v[32:], index)
	return v
}

func coinKey(op consensus.OutPoint) []byte {
	k := make([]byte, 1+32+4)
	k[0] = prefixCoin
	copy(k[1:33], op.TxID[:])
	binary.LittleEndian.PutUint32(k[33:], op.Index)
	return k
}

func encodeCoin(c consensus.Coin) []byte {
	var w consensus.Writer
	w.I64(int64(c.Out.Amount))
	w.U8(c.Out.Type)
	w.VarBytes(c.Out.Payload)
	w.U32(c.Height)
	if c.Coinbase {
		w.U8(1)
	} else {
		w.U8(0)
	}
	// Asset id only for non-ARG coins, so existing databases stay readable.
	if c.Out.Asset != consensus.NativeAsset {
		w.Hash(c.Out.Asset)
	}
	return w.Bytes()
}

func decodeCoin(b []byte) (consensus.Coin, error) {
	var c consensus.Coin
	r := consensus.NewReader(b)
	a, err := r.I64()
	if err != nil {
		return c, errCorrupt
	}
	c.Out.Amount = consensus.Amount(a)
	if c.Out.Type, err = r.U8(); err != nil {
		return c, errCorrupt
	}
	if c.Out.Payload, err = r.VarBytes(1 << 16); err != nil {
		return c, errCorrupt
	}
	if c.Height, err = r.U32(); err != nil {
		return c, errCorrupt
	}
	cb, err := r.U8()
	if err != nil {
		return c, errCorrupt
	}
	c.Coinbase = cb == 1
	if r.Remaining() == consensus.HashSize {
		if c.Out.Asset, err = r.Hash(); err != nil {
			return c, errCorrupt
		}
	}
	if err := r.ExpectEOF(); err != nil {
		return c, errCorrupt
	}
	return c, nil
}

// undo data: varint n, then n × (outpoint || coin-bytes-with-length)
func encodeUndo(spent map[consensus.OutPoint]consensus.Coin) []byte {
	var w consensus.Writer
	w.Varint(uint64(len(spent)))
	for op, c := range spent {
		w.Hash(op.TxID)
		w.U32(op.Index)
		w.VarBytes(encodeCoin(c))
	}
	return w.Bytes()
}

func decodeUndo(b []byte) (map[consensus.OutPoint]consensus.Coin, error) {
	r := consensus.NewReader(b)
	n, err := r.VarintMax(1 << 24)
	if err != nil {
		return nil, errCorrupt
	}
	out := make(map[consensus.OutPoint]consensus.Coin, n)
	for i := uint64(0); i < n; i++ {
		var op consensus.OutPoint
		if op.TxID, err = r.Hash(); err != nil {
			return nil, errCorrupt
		}
		if op.Index, err = r.U32(); err != nil {
			return nil, errCorrupt
		}
		cb, err := r.VarBytes(1 << 16)
		if err != nil {
			return nil, errCorrupt
		}
		c, err := decodeCoin(cb)
		if err != nil {
			return nil, err
		}
		out[op] = c
	}
	if err := r.ExpectEOF(); err != nil {
		return nil, errCorrupt
	}
	return out, nil
}

func encodeNode(n *Node) []byte {
	var w consensus.Writer
	w.Raw(n.Header.Serialize())
	w.U32(n.Height)
	w.U8(uint8(n.Status))
	return w.Bytes()
}

func decodeNode(b []byte) (*Node, error) {
	if len(b) != consensus.HeaderSize+4+1 {
		return nil, errCorrupt
	}
	hdr, err := consensus.DecodeHeader(b[:consensus.HeaderSize])
	if err != nil {
		return nil, errCorrupt
	}
	n := &Node{Header: *hdr, Hash: hdr.Hash()}
	n.Height = binary.LittleEndian.Uint32(b[consensus.HeaderSize:])
	n.Status = Status(b[consensus.HeaderSize+4])
	return n, nil
}

// dbView reads coins straight from the database. It is the base of every
// overlay used to connect a block.
type dbView struct{ db *leveldb.DB }

func (v dbView) Lookup(op consensus.OutPoint) (consensus.Coin, bool) {
	b, err := v.db.Get(coinKey(op), nil)
	if err != nil {
		if errors.Is(err, leveldb.ErrNotFound) {
			return consensus.Coin{}, false
		}
		// A failing disk must never be mistaken for a missing coin (that
		// would turn an I/O error into a consensus verdict). Abort loudly.
		panic(fmt.Sprintf("chain: database read failed for %s: %v", op, err))
	}
	c, err := decodeCoin(b)
	if err != nil {
		// A corrupt coin record is fatal for a node; surface loudly.
		panic(fmt.Sprintf("chain: corrupt coin %s: %v", op, err))
	}
	return c, true
}

func openDB(path string) (*leveldb.DB, error) {
	return leveldb.OpenFile(path, &opt.Options{
		// Sync on every batch so a connected block is durable before we
		// announce it. Throughput is not the bottleneck at 1 block / 10 min.
		NoSync: false,
	})
}
