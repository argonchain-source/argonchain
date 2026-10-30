package consensus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxVarint is the largest value a protocol varint may carry (SPEC §2.2).
const MaxVarint = 1<<63 - 1

// maxVarintBytes: 9 groups × 7 bits = 63 bits.
const maxVarintBytes = 9

// Writer accumulates canonical serialization. Writes never fail.
type Writer struct{ buf bytes.Buffer }

func (w *Writer) Bytes() []byte { return w.buf.Bytes() }
func (w *Writer) Len() int      { return w.buf.Len() }

func (w *Writer) U8(v uint8) { w.buf.WriteByte(v) }

func (w *Writer) U32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf.Write(b[:])
}

func (w *Writer) U64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf.Write(b[:])
}

func (w *Writer) I64(v int64) { w.U64(uint64(v)) }

func (w *Writer) Hash(h Hash) { w.buf.Write(h[:]) }

func (w *Writer) Raw(b []byte) { w.buf.Write(b) }

// Varint writes the minimal LEB128 encoding of v. Panics if v > MaxVarint,
// which can only happen through a programming error (lengths and counts are
// bounded far below that).
func (w *Writer) Varint(v uint64) {
	if v > MaxVarint {
		panic("consensus: varint value out of range")
	}
	for v >= 0x80 {
		w.buf.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	w.buf.WriteByte(byte(v))
}

// VarBytes writes varint(len) || bytes.
func (w *Writer) VarBytes(b []byte) {
	w.Varint(uint64(len(b)))
	w.buf.Write(b)
}

// Reader parses canonical serialization strictly. Any deviation from the
// minimal encoding is an error.
type Reader struct{ r *bytes.Reader }

func NewReader(b []byte) *Reader { return &Reader{r: bytes.NewReader(b)} }

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return r.r.Len() }

// ExpectEOF fails if any bytes remain.
func (r *Reader) ExpectEOF() error {
	if r.r.Len() != 0 {
		return ErrTrailingBytes
	}
	return nil
}

func (r *Reader) U8() (uint8, error) {
	b, err := r.r.ReadByte()
	if err != nil {
		return 0, ErrUnexpectedEOF
	}
	return b, nil
}

func (r *Reader) U32() (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r.r, b[:]); err != nil {
		return 0, ErrUnexpectedEOF
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func (r *Reader) U64() (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r.r, b[:]); err != nil {
		return 0, ErrUnexpectedEOF
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

func (r *Reader) I64() (int64, error) {
	v, err := r.U64()
	return int64(v), err
}

func (r *Reader) Hash() (Hash, error) {
	var h Hash
	if _, err := io.ReadFull(r.r, h[:]); err != nil {
		return h, ErrUnexpectedEOF
	}
	return h, nil
}

// Raw reads exactly n bytes.
func (r *Reader) Raw(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r.r, b); err != nil {
		return nil, ErrUnexpectedEOF
	}
	return b, nil
}

// Varint reads a minimal LEB128 value (SPEC §2.2).
//
// Rejected: more than 9 bytes; a final group equal to 0x00 when the value has
// more than one group (non-minimal); any 9th byte with the continuation bit
// set (would exceed 63 bits).
func (r *Reader) Varint() (uint64, error) {
	var v uint64
	var shift uint
	for i := 0; i < maxVarintBytes; i++ {
		b, err := r.r.ReadByte()
		if err != nil {
			return 0, ErrUnexpectedEOF
		}
		if i == maxVarintBytes-1 && b&0x80 != 0 {
			return 0, ErrVarintTooLong
		}
		if i > 0 && b == 0x00 {
			// A zero continuation group as the last group means the
			// previous byte's continuation bit was unnecessary.
			return 0, ErrVarintNonCanonical
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			if v > MaxVarint {
				return 0, ErrVarintTooLarge
			}
			return v, nil
		}
		shift += 7
	}
	return 0, ErrVarintTooLong
}

// VarintMax reads a varint and rejects values above max. Use for every count
// and length so that no allocation is driven by untrusted data.
func (r *Reader) VarintMax(max uint64) (uint64, error) {
	v, err := r.Varint()
	if err != nil {
		return 0, err
	}
	if v > max {
		return 0, fmt.Errorf("%w: %d > %d", ErrLengthExceedsMax, v, max)
	}
	return v, nil
}

// VarBytes reads varint(len) || bytes, rejecting len > max before allocating.
func (r *Reader) VarBytes(max uint64) ([]byte, error) {
	n, err := r.VarintMax(max)
	if err != nil {
		return nil, err
	}
	return r.Raw(int(n))
}
