package consensus

// MaxBlockSize is the maximum serialized block size including witnesses
// (SPEC §3.4). There is no weight concept; one byte limit.
const MaxBlockSize = 1_000_000

// Block is a header plus its transactions.
type Block struct {
	Header Header
	Txs    []*Tx
}

// Serialize returns the canonical block encoding.
func (b *Block) Serialize() []byte {
	var w Writer
	b.Header.write(&w)
	w.Varint(uint64(len(b.Txs)))
	for _, tx := range b.Txs {
		tx.write(&w, true)
	}
	return w.Bytes()
}

// SerializedSize is the full encoded size in bytes.
func (b *Block) SerializedSize() int { return len(b.Serialize()) }

// TxIDs returns every transaction's txid in block order.
func (b *Block) TxIDs() []Hash {
	ids := make([]Hash, len(b.Txs))
	for i, tx := range b.Txs {
		ids[i] = tx.TxID()
	}
	return ids
}

// WTxIDs returns every transaction's wtxid in block order.
func (b *Block) WTxIDs() []Hash {
	ids := make([]Hash, len(b.Txs))
	for i, tx := range b.Txs {
		ids[i] = tx.WTxID()
	}
	return ids
}

// ComputeRoots returns (tx_root, witness_root) for the block's transactions.
func (b *Block) ComputeRoots() (Hash, Hash, error) {
	txRoot, err := MerkleRoot(b.TxIDs())
	if err != nil {
		return Hash{}, Hash{}, err
	}
	wRoot, err := MerkleRoot(b.WTxIDs())
	if err != nil {
		return Hash{}, Hash{}, err
	}
	return txRoot, wRoot, nil
}

// DecodeBlock parses exactly one block. Enforces MaxBlockSize before parsing
// and rejects trailing bytes. Each transaction is additionally bounded by
// MaxTxSize.
func DecodeBlock(b []byte) (*Block, error) {
	if len(b) > MaxBlockSize {
		return nil, ErrBlockTooLarge
	}
	r := NewReader(b)
	hdr, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	// Count is bounded by the byte budget; do not preallocate from it.
	n, err := r.VarintMax(MaxBlockSize)
	if err != nil {
		return nil, err
	}
	blk := &Block{Header: hdr}
	for i := uint64(0); i < n; i++ {
		start := r.Remaining()
		tx, err := readTx(r)
		if err != nil {
			return nil, err
		}
		if start-r.Remaining() > MaxTxSize {
			return nil, ErrTxTooLarge
		}
		blk.Txs = append(blk.Txs, tx)
	}
	if err := r.ExpectEOF(); err != nil {
		return nil, err
	}
	return blk, nil
}
