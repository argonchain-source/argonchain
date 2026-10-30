package consensus

// HeaderSize is the fixed serialized size of a block header (SPEC §3.1).
const HeaderSize = 4 + 32 + 32 + 32 + 8 + 4 + 8 // = 120

// HeaderVersion is the only valid header version in v1.
const HeaderVersion uint32 = 1

// Header is the block header. Field order is the wire order.
type Header struct {
	Version     uint32
	PrevHash    Hash
	TxRoot      Hash
	WitnessRoot Hash
	Time        uint64
	Bits        uint32
	Nonce       uint64
}

func (h *Header) write(w *Writer) {
	w.U32(h.Version)
	w.Hash(h.PrevHash)
	w.Hash(h.TxRoot)
	w.Hash(h.WitnessRoot)
	w.U64(h.Time)
	w.U32(h.Bits)
	w.U64(h.Nonce)
}

// Serialize returns the 120-byte canonical encoding.
func (h *Header) Serialize() []byte {
	var w Writer
	h.write(&w)
	return w.Bytes()
}

// Hash is the block identifier: H("Argonmine/block", header). Cheap; not the PoW.
func (h *Header) Hash() Hash { return TaggedHash(TagBlock, h.Serialize()) }

func readHeader(r *Reader) (Header, error) {
	var h Header
	var err error
	if h.Version, err = r.U32(); err != nil {
		return h, err
	}
	if h.PrevHash, err = r.Hash(); err != nil {
		return h, err
	}
	if h.TxRoot, err = r.Hash(); err != nil {
		return h, err
	}
	if h.WitnessRoot, err = r.Hash(); err != nil {
		return h, err
	}
	if h.Time, err = r.U64(); err != nil {
		return h, err
	}
	if h.Bits, err = r.U32(); err != nil {
		return h, err
	}
	if h.Nonce, err = r.U64(); err != nil {
		return h, err
	}
	return h, nil
}

// DecodeHeader parses exactly 120 bytes.
func DecodeHeader(b []byte) (*Header, error) {
	if len(b) != HeaderSize {
		return nil, ErrBadHeaderLength
	}
	r := NewReader(b)
	h, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// CheckHeaderStateless enforces the rules that need no chain context:
// version, timestamp bound, and canonical bits not easier than powLimit, plus
// the proof of work itself (SPEC §3.1, §4, §5.1).
func CheckHeaderStateless(p *Params, h *Header) error {
	if h.Version != HeaderVersion {
		return ErrBadHeaderVersion
	}
	if h.Time > MaxBlockTime {
		return ErrTimeOverflow
	}
	return CheckProofOfWork(h, p.PowLimit)
}

// CheckHeaderContext enforces the rules that need the parent and recent
// timestamps (SPEC §5.2, §5.3, §7.1). adjustedNow is the node's
// network-adjusted clock; pass a huge value to skip the future check when
// validating historical blocks that are already buried.
func CheckHeaderContext(p *Params, h *Header, parentHash Hash, parentTime uint64, parentHeight uint32, medianTimePast uint64, adjustedNow int64) error {
	if h.PrevHash != parentHash {
		return ErrBadPrevHash
	}
	if want := NextBits(p, parentTime, parentHeight); h.Bits != want {
		return ErrBadBits
	}
	if h.Time <= medianTimePast {
		return ErrTimeTooOld
	}
	if adjustedNow >= 0 && h.Time > uint64(adjustedNow)+uint64(MaxFutureBlockTime) {
		return ErrTimeTooNew
	}
	return nil
}
