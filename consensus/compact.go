package consensus

import "math/big"

// Compact target encoding (SPEC §5.1): 1-byte exponent, 3-byte mantissa,
//   target = mantissa * 256^(exponent-3)
// identical to Bitcoin nBits but strictly canonical: the sign bit is
// rejected, zero is rejected, and the encoding must round-trip exactly.

var (
	bigOne   = big.NewInt(1)
	big2_256 = new(big.Int).Lsh(bigOne, 256)
)

// CompactToTarget decodes bits into a positive target < 2^256.
func CompactToTarget(bits uint32) (*big.Int, error) {
	if bits&0x00800000 != 0 {
		return nil, ErrCompactSignBit
	}
	exp := bits >> 24
	mant := uint64(bits & 0x007fffff)
	t := new(big.Int)
	if exp <= 3 {
		t.SetUint64(mant >> (8 * (3 - exp)))
	} else {
		t.SetUint64(mant)
		t.Lsh(t, uint(8*(exp-3)))
	}
	if t.Sign() == 0 {
		return nil, ErrCompactZero
	}
	if t.Cmp(big2_256) >= 0 {
		return nil, ErrCompactOverflow
	}
	if TargetToCompact(t) != bits {
		return nil, ErrCompactNonCanon
	}
	return t, nil
}

// TargetToCompact encodes a positive target. The result is the unique
// canonical encoding; feeding it back to CompactToTarget yields a target that
// is t with all but the top 23-24 bits of precision dropped (same as Bitcoin).
// t must satisfy 0 < t < 2^256.
func TargetToCompact(t *big.Int) uint32 {
	if t.Sign() <= 0 {
		panic("consensus: TargetToCompact of non-positive target")
	}
	size := uint32((t.BitLen() + 7) / 8)
	var mant uint64
	if size <= 3 {
		mant = t.Uint64() << (8 * (3 - size))
	} else {
		tmp := new(big.Int).Rsh(t, uint(8*(size-3)))
		mant = tmp.Uint64()
	}
	if mant&0x00800000 != 0 {
		mant >>= 8
		size++
	}
	return size<<24 | uint32(mant)
}

// BlockWork returns 2^256 / (target+1), the expected number of hash attempts
// to find a block at this target. Used for fork choice (SPEC §7).
func BlockWork(target *big.Int) *big.Int {
	den := new(big.Int).Add(target, bigOne)
	return new(big.Int).Div(big2_256, den)
}
