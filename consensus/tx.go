package consensus

import "fmt"

// Transaction constants (SPEC §1, §3.2, §3.3).
const (
	TxVersion uint32 = 1

	OutputTypeP2PK     uint8 = 0x00
	OutputTypeNullData uint8 = 0x01

	P2PKPayloadLen = 32
	MaxNullDataLen = 80
	// maxPayloadLen is the structural cap used by the decoder; every type's
	// payload limit is <= this, so a longer payload is invalid regardless.
	maxPayloadLen = 80

	SchnorrSigLen     = 64
	MaxCoinbaseTagLen = 100
	// maxWitnessLen is the structural cap for a single witness: P2PK needs
	// exactly 64 bytes, a coinbase tag at most 100.
	maxWitnessLen = 100

	MaxTxInputs  = 10_000
	MaxTxOutputs = 10_000
	MaxTxSize    = 100_000

	CoinbaseMaturity uint32 = 100
)

// OutPoint identifies a transaction output.
type OutPoint struct {
	TxID  Hash
	Index uint32
}

// NullOutPoint is the prevout a coinbase input must carry.
var NullOutPoint = OutPoint{Index: 0xFFFFFFFF}

func (o OutPoint) String() string { return fmt.Sprintf("%s:%d", o.TxID, o.Index) }

// TxIn is a transaction input. There is no script and no sequence field.
type TxIn struct {
	PrevOut OutPoint
}

// TxOut is a transaction output. Asset is NativeAsset (all zero) for ARG;
// other assets exist only in version-2 transactions (SPEC §3.5).
type TxOut struct {
	Amount  Amount
	Type    uint8
	Payload []byte
	Asset   Hash
}

// Tx is a transaction. Witnesses has exactly one entry per input, followed
// by one per issuance.
type Tx struct {
	Version    uint32
	LockHeight uint32
	Inputs     []TxIn
	Outputs    []TxOut
	Issuances  []Issuance // version 2 only, at most MaxTxIssuances
	Witnesses  [][]byte
}

// HasAssets reports whether the transaction uses the version-2 asset fields.
func (tx *Tx) HasAssets() bool { return tx.Version >= TxVersionAssets }

// IsCoinbase reports whether the tx has the coinbase shape: exactly one
// input whose prevout is null. Shape only; see CheckCoinbase for the rules.
func (tx *Tx) IsCoinbase() bool {
	return len(tx.Inputs) == 1 && tx.Inputs[0].PrevOut == NullOutPoint
}

func (o *TxOut) write(w *Writer, withAsset bool) {
	w.I64(int64(o.Amount))
	w.U8(o.Type)
	w.VarBytes(o.Payload)
	if withAsset {
		w.Hash(o.Asset)
	}
}

func (tx *Tx) write(w *Writer, withWitness bool) {
	assets := tx.HasAssets()
	w.U32(tx.Version)
	w.U32(tx.LockHeight)
	w.Varint(uint64(len(tx.Inputs)))
	for _, in := range tx.Inputs {
		w.Hash(in.PrevOut.TxID)
		w.U32(in.PrevOut.Index)
	}
	w.Varint(uint64(len(tx.Outputs)))
	for i := range tx.Outputs {
		tx.Outputs[i].write(w, assets)
	}
	if assets {
		w.Varint(uint64(len(tx.Issuances)))
		for i := range tx.Issuances {
			tx.Issuances[i].write(w)
		}
	}
	if withWitness {
		for _, wit := range tx.Witnesses {
			w.VarBytes(wit)
		}
	}
}

// Serialize returns the full canonical encoding including witnesses.
func (tx *Tx) Serialize() []byte {
	var w Writer
	tx.write(&w, true)
	return w.Bytes()
}

// SerializeNoWitness returns the encoding that txid commits to.
func (tx *Tx) SerializeNoWitness() []byte {
	var w Writer
	tx.write(&w, false)
	return w.Bytes()
}

// TxID = H("Argonmine/txid", no-witness serialization). Immune to signature malleability.
func (tx *Tx) TxID() Hash { return TaggedHash(TagTxID, tx.SerializeNoWitness()) }

// WTxID = H("Argonmine/wtxid", full serialization).
func (tx *Tx) WTxID() Hash { return TaggedHash(TagWTxID, tx.Serialize()) }

// SerializedSize is the size of the full encoding in bytes.
func (tx *Tx) SerializedSize() int { return len(tx.Serialize()) }

func readTxOut(r *Reader, withAsset bool) (TxOut, error) {
	var o TxOut
	a, err := r.I64()
	if err != nil {
		return o, err
	}
	o.Amount = Amount(a)
	if o.Type, err = r.U8(); err != nil {
		return o, err
	}
	if o.Payload, err = r.VarBytes(maxPayloadLen); err != nil {
		return o, err
	}
	if withAsset {
		if o.Asset, err = r.Hash(); err != nil {
			return o, err
		}
	}
	return o, nil
}

func readTx(r *Reader) (*Tx, error) {
	tx := &Tx{}
	var err error
	if tx.Version, err = r.U32(); err != nil {
		return nil, err
	}
	if tx.LockHeight, err = r.U32(); err != nil {
		return nil, err
	}
	nIn, err := r.VarintMax(MaxTxInputs)
	if err != nil {
		return nil, err
	}
	tx.Inputs = make([]TxIn, nIn)
	for i := range tx.Inputs {
		if tx.Inputs[i].PrevOut.TxID, err = r.Hash(); err != nil {
			return nil, err
		}
		if tx.Inputs[i].PrevOut.Index, err = r.U32(); err != nil {
			return nil, err
		}
	}
	nOut, err := r.VarintMax(MaxTxOutputs)
	if err != nil {
		return nil, err
	}
	assets := tx.HasAssets()
	tx.Outputs = make([]TxOut, nOut)
	for i := range tx.Outputs {
		if tx.Outputs[i], err = readTxOut(r, assets); err != nil {
			return nil, err
		}
	}
	var nIss uint64
	if assets {
		if nIss, err = r.VarintMax(MaxTxIssuances); err != nil {
			return nil, err
		}
		tx.Issuances = make([]Issuance, nIss)
		for i := range tx.Issuances {
			if tx.Issuances[i], err = readIssuance(r); err != nil {
				return nil, err
			}
		}
	}
	tx.Witnesses = make([][]byte, nIn+nIss)
	for i := range tx.Witnesses {
		if tx.Witnesses[i], err = r.VarBytes(maxWitnessLen); err != nil {
			return nil, err
		}
	}
	return tx, nil
}

// DecodeTx parses exactly one transaction from b. Enforces MaxTxSize before
// parsing and rejects trailing bytes.
func DecodeTx(b []byte) (*Tx, error) {
	if len(b) > MaxTxSize {
		return nil, ErrTxTooLarge
	}
	r := NewReader(b)
	tx, err := readTx(r)
	if err != nil {
		return nil, err
	}
	if err := r.ExpectEOF(); err != nil {
		return nil, err
	}
	return tx, nil
}
