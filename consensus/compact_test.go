package consensus

import (
	"errors"
	"math/big"
	"testing"
)

func TestCompactRoundTrip(t *testing.T) {
	cases := []uint32{
		0x1F0FFFFF, 0x1F7FFFFF, 0x207FFFFF, // our pow limits
		0x1d00ffff, // bitcoin genesis
		0x1b0404cb, // bitcoin block 100k-ish
		0x01010000, // target 1
		0x02010000, // target 256
		0x03010000, // target 65536
		0x04010000, // target 2^24
		0x0300ffff, // target 0xffff
	}
	for _, bits := range cases {
		tgt, err := CompactToTarget(bits)
		if err != nil {
			t.Fatalf("%08x: %v", bits, err)
		}
		if back := TargetToCompact(tgt); back != bits {
			t.Fatalf("%08x: round trip gave %08x", bits, back)
		}
	}
}

func TestCompactKnownTargets(t *testing.T) {
	// Bitcoin's genesis nBits 0x1d00ffff = 0x00000000FFFF0000...0000 (26 zero bytes after ffff).
	tgt, err := CompactToTarget(0x1d00ffff)
	if err != nil {
		t.Fatal(err)
	}
	want := bigFromHex(t, "00000000ffff0000000000000000000000000000000000000000000000000000")
	if tgt.Cmp(want) != 0 {
		t.Fatalf("got %x", tgt)
	}
	one, _ := CompactToTarget(0x01010000)
	if one.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("0x01010000 should be 1, got %v", one)
	}
}

func TestCompactRejects(t *testing.T) {
	cases := map[uint32]error{
		0x1d80ffff: ErrCompactSignBit,  // sign bit
		0x00000000: ErrCompactZero,     // zero
		0x1d000000: ErrCompactZero,     // zero mantissa
		0x01000001: ErrCompactZero,     // mantissa shifted out entirely -> 0
		0x020001ff: ErrCompactNonCanon, // low byte lost on decode, re-encodes as 0x01010000
		0x1d0000ff: ErrCompactNonCanon, // not normalized; canonical form is 0x1c00ff00
		0x22000001: ErrCompactNonCanon, // 2^248; canonical form is 0x20010000
		0x23000001: ErrCompactOverflow, // 2^256
		0xff7fffff: ErrCompactOverflow,
	}
	for bits, want := range cases {
		_, err := CompactToTarget(bits)
		if !errors.Is(err, want) {
			t.Errorf("%08x: got %v want %v", bits, err, want)
		}
	}
}

func TestCompactNoNegativeTargetsPossible(t *testing.T) {
	// Every accepted encoding must decode to a strictly positive target.
	for exp := uint32(0); exp < 0x40; exp++ {
		for _, mant := range []uint32{1, 0x7f, 0x80, 0xff, 0x100, 0x7fff, 0x8000, 0xffff, 0x10000, 0x7fffff} {
			bits := exp<<24 | mant
			tgt, err := CompactToTarget(bits)
			if err == nil && tgt.Sign() <= 0 {
				t.Fatalf("%08x decoded to non-positive", bits)
			}
		}
	}
}

func TestBlockWork(t *testing.T) {
	// target = 2^255 - 1  ->  work = 2^256 / 2^255 = 2
	tgt := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1))
	if BlockWork(tgt).Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("work = %v", BlockWork(tgt))
	}
	// smaller target -> more work
	a := BlockWork(big.NewInt(1000))
	b := BlockWork(big.NewInt(999))
	if b.Cmp(a) <= 0 {
		t.Fatal("work must increase as target decreases")
	}
}
