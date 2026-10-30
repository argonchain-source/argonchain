package consensus

// SigHashCache precomputes the per-transaction parts of the signature
// message (SPEC §6.2) so that signing or verifying n inputs costs O(n) hashes
// instead of O(n²).
type SigHashCache struct {
	chainTag     Hash
	version      uint32
	lockHeight   uint32
	hashPrevouts Hash
	hashAmounts  Hash
	hashScripts  Hash
	hashOutputs  Hash
	hashAssets   Hash // version 2: spent asset ids
	hashIssuance Hash // version 2: the issuances
}

// NewSigHashCache builds the cache. spent[i] is the output being spent by
// input i; len(spent) must equal len(tx.Inputs).
func NewSigHashCache(chainTag Hash, tx *Tx, spent []TxOut) *SigHashCache {
	if len(spent) != len(tx.Inputs) {
		panic("consensus: spent outputs count != input count")
	}
	assets := tx.HasAssets()
	var wPrev, wAmt, wScr, wOut, wAss, wIss Writer
	for i, in := range tx.Inputs {
		wPrev.Hash(in.PrevOut.TxID)
		wPrev.U32(in.PrevOut.Index)
		wAmt.I64(int64(spent[i].Amount))
		wScr.U8(spent[i].Type)
		wScr.VarBytes(spent[i].Payload)
		wAss.Hash(spent[i].Asset)
	}
	for i := range tx.Outputs {
		tx.Outputs[i].write(&wOut, assets)
	}
	c := &SigHashCache{
		chainTag:     chainTag,
		version:      tx.Version,
		lockHeight:   tx.LockHeight,
		hashPrevouts: TaggedHash(TagPrevouts, wPrev.Bytes()),
		hashAmounts:  TaggedHash(TagAmounts, wAmt.Bytes()),
		hashScripts:  TaggedHash(TagScripts, wScr.Bytes()),
		hashOutputs:  TaggedHash(TagOutputs, wOut.Bytes()),
	}
	if assets {
		wIss.Varint(uint64(len(tx.Issuances)))
		for i := range tx.Issuances {
			tx.Issuances[i].write(&wIss)
		}
		c.hashAssets = TaggedHash(TagAssets, wAss.Bytes())
		c.hashIssuance = TaggedHash(TagIssuance, wIss.Bytes())
	}
	return c
}

// Hash returns the 32-byte message that input i must sign. Version-2
// transactions additionally commit to the spent asset ids and the issuances,
// so a signer cannot be misled about which asset it spends or creates.
func (c *SigHashCache) Hash(inputIndex uint32) Hash {
	var w Writer
	w.Hash(c.chainTag)
	w.U32(c.version)
	w.U32(c.lockHeight)
	w.Hash(c.hashPrevouts)
	w.Hash(c.hashAmounts)
	w.Hash(c.hashScripts)
	w.Hash(c.hashOutputs)
	if c.version >= TxVersionAssets {
		w.Hash(c.hashAssets)
		w.Hash(c.hashIssuance)
	}
	w.U32(inputIndex)
	return TaggedHash(TagSighash, w.Bytes())
}

// IssuanceHash is the message the issuer signs for the transaction's
// issuance (witness index len(inputs)).
func (c *SigHashCache) IssuanceHash() Hash { return c.Hash(IssuanceSigIndex) }

// SigHash is the one-shot form of NewSigHashCache(...).Hash(i).
func SigHash(chainTag Hash, tx *Tx, spent []TxOut, inputIndex uint32) Hash {
	return NewSigHashCache(chainTag, tx, spent).Hash(inputIndex)
}
