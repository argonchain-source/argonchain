package consensus

import (
	"bytes"
	"errors"
	"testing"
)

func TestVarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 255, 256, 16383, 16384, 1<<32 - 1, 1 << 32, 1<<56 - 1, 1 << 56, MaxVarint}
	for _, v := range cases {
		var w Writer
		w.Varint(v)
		r := NewReader(w.Bytes())
		got, err := r.Varint()
		if err != nil {
			t.Fatalf("varint %d: decode error %v", v, err)
		}
		if got != v {
			t.Fatalf("varint %d: got %d", v, got)
		}
		if err := r.ExpectEOF(); err != nil {
			t.Fatalf("varint %d: trailing", v)
		}
	}
}

func TestVarintMinimalLength(t *testing.T) {
	var w Writer
	w.Varint(MaxVarint)
	if w.Len() != 9 {
		t.Fatalf("MaxVarint should be 9 bytes, got %d", w.Len())
	}
	w = Writer{}
	w.Varint(127)
	if w.Len() != 1 {
		t.Fatalf("127 should be 1 byte")
	}
	w = Writer{}
	w.Varint(128)
	if !bytes.Equal(w.Bytes(), []byte{0x80, 0x01}) {
		t.Fatalf("128 encoding wrong: %x", w.Bytes())
	}
}

func TestVarintRejectsNonCanonical(t *testing.T) {
	bad := map[string]struct {
		in  []byte
		err error
	}{
		"zero with continuation":       {[]byte{0x80, 0x00}, ErrVarintNonCanonical},
		"127 padded":                   {[]byte{0xFF, 0x00}, ErrVarintNonCanonical},
		"128 padded":                   {[]byte{0x80, 0x81, 0x00}, ErrVarintNonCanonical},
		"ten bytes":                    {[]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}, ErrVarintTooLong},
		"ninth byte with continuation": {[]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}, ErrVarintTooLong},
		"truncated":                    {[]byte{0x80}, ErrUnexpectedEOF},
		"empty":                        {[]byte{}, ErrUnexpectedEOF},
	}
	for name, c := range bad {
		_, err := NewReader(c.in).Varint()
		if !errors.Is(err, c.err) {
			t.Errorf("%s: got %v, want %v", name, err, c.err)
		}
	}
}

func TestVarBytesMax(t *testing.T) {
	var w Writer
	w.VarBytes(make([]byte, 101))
	_, err := NewReader(w.Bytes()).VarBytes(100)
	if !errors.Is(err, ErrLengthExceedsMax) {
		t.Fatalf("expected ErrLengthExceedsMax, got %v", err)
	}
	// Length claims more than available: must be EOF, never a huge alloc.
	_, err = NewReader([]byte{0xFF, 0xFF, 0x03}).VarBytes(1 << 20)
	if !errors.Is(err, ErrUnexpectedEOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestFixedWidthLittleEndian(t *testing.T) {
	var w Writer
	w.U32(0x01020304)
	w.U64(0x0102030405060708)
	w.I64(-1)
	want := []byte{4, 3, 2, 1, 8, 7, 6, 5, 4, 3, 2, 1, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("got %x", w.Bytes())
	}
	r := NewReader(w.Bytes())
	a, _ := r.U32()
	b, _ := r.U64()
	c, _ := r.I64()
	if a != 0x01020304 || b != 0x0102030405060708 || c != -1 {
		t.Fatal("round trip mismatch")
	}
}

func TestTaggedHashKnownAnswer(t *testing.T) {
	// Tagged hash is BIP-340's: SHA256(SHA256(tag)||SHA256(tag)||msg).
	// Cross-check against an independent computation.
	got := TaggedHash("Argonmine/test", []byte("abc"))
	// Independent: compute with crypto/sha256 inline.
	th := sha256Sum([]byte("Argonmine/test"))
	want := sha256Sum(append(append(append([]byte{}, th[:]...), th[:]...), []byte("abc")...))
	if got != Hash(want) {
		t.Fatal("tagged hash mismatch")
	}
	// Different tags, same data, different hashes.
	if TaggedHash(TagTxID, []byte("x")) == TaggedHash(TagWTxID, []byte("x")) {
		t.Fatal("domain separation failed")
	}
	// Concatenation of data slices is equivalent to one slice.
	if TaggedHash("t", []byte("ab"), []byte("c")) != TaggedHash("t", []byte("abc")) {
		t.Fatal("multi-slice hashing differs")
	}
}

func TestHashHex(t *testing.T) {
	h := TaggedHash("t", []byte("x"))
	back, err := HashFromHex(h.String())
	if err != nil || back != h {
		t.Fatal("hex round trip failed")
	}
	if _, err := HashFromHex("abcd"); !errors.Is(err, ErrBadHashLength) {
		t.Fatal("short hex accepted")
	}
}
