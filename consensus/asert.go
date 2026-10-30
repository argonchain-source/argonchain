package consensus

import "math/big"

// ASERT difficulty adjustment (SPEC §5.2). This is a faithful port of the
// aserti3-2d reference algorithm (Bitcoin Cash, Nov 2020) with these
// differences, both deliberate:
//   - the anchor is the genesis block, whose "parent time" is defined as
//     genesis.time - TargetSpacing so the schedule is exactly on time at
//     height 1 (SPEC §5.2);
//   - all intermediate arithmetic is done in big.Int so there is no 256-bit
//     overflow path; the exponent is saturated at ±(2^47-1) which the
//     reference asserts instead.

const (
	// TargetSpacing is the ideal number of seconds between blocks.
	TargetSpacing int64 = 600

	// ASERTHalfLife: the target doubles for every half-life the chain falls
	// behind schedule, and halves for every half-life it runs ahead.
	ASERTHalfLife int64 = 86_400

	// MedianTimeSpan is the window for median-time-past.
	MedianTimeSpan = 11

	// MaxFutureBlockTime is how far ahead of adjusted local time a block
	// timestamp may be (policy-ish but universally enforced; SPEC §5.3).
	MaxFutureBlockTime int64 = 2 * 60 * 60

	// MaxBlockTime bounds the u64 timestamp so that all int64 arithmetic on
	// times is safe (SPEC §5.3 / header rules).
	MaxBlockTime uint64 = 1<<63 - 1
)

var (
	asertExpLimit = new(big.Int).Sub(new(big.Int).Lsh(bigOne, 47), bigOne)
	asertNegLimit = new(big.Int).Neg(asertExpLimit)
	big65536      = big.NewInt(65536)
)

// asertTarget implements CalculateASERT from the reference exactly, over
// big.Int. refTarget must satisfy 0 < refTarget <= powLimit, powLimit < 2^256.
//
//	timeDiff   = parentTime - anchorParentTime
//	heightDiff = parentHeight - anchorHeight   (>= 0)
func asertTarget(refTarget *big.Int, timeDiff, heightDiff int64, powLimit *big.Int, halfLife int64) *big.Int {
	if refTarget.Sign() <= 0 || refTarget.Cmp(powLimit) > 0 || heightDiff < 0 || halfLife <= 0 {
		panic("consensus: asertTarget precondition violated")
	}

	// numerator = timeDiff - spacing*(heightDiff+1), in big.Int, then saturated.
	num := big.NewInt(timeDiff)
	ideal := new(big.Int).Mul(big.NewInt(TargetSpacing), big.NewInt(heightDiff+1))
	num.Sub(num, ideal)
	if num.Cmp(asertExpLimit) > 0 {
		num.Set(asertExpLimit)
	} else if num.Cmp(asertNegLimit) < 0 {
		num.Set(asertNegLimit)
	}
	// exponent = (num * 65536) / halfLife  with truncation toward zero, as in C++.
	exp := new(big.Int).Mul(num, big65536)
	exp.Quo(exp, big.NewInt(halfLife)) // Quo truncates toward zero
	exponent := exp.Int64()            // |num| < 2^47 so this fits comfortably

	// Decompose: shifts = floor(exponent / 65536), frac = exponent mod 65536 in [0, 65536).
	shifts := exponent >> 16 // arithmetic shift = floor division
	frac := uint64(uint16(exponent))

	// 2^x ≈ 1 + 0.695502049·x + 0.2262698·x² + 0.0782318·x³ for x in [0,1),
	// in 16.48 fixed point. Max error vs true 2^x is < 0.013%.
	factor := 65536 + ((195766423245049*frac +
		971821376*frac*frac +
		5127*frac*frac*frac +
		(1 << 47)) >> 48)

	next := new(big.Int).Mul(refTarget, new(big.Int).SetUint64(factor))

	shifts -= 16
	switch {
	case shifts < 0:
		if -shifts > 300 {
			// refTarget*factor < 2^(256+17); shifting right by >300 yields 0.
			return big.NewInt(1)
		}
		next.Rsh(next, uint(-shifts))
	case shifts > 0:
		if shifts > 300 {
			// next >= 65536, so shifting left by >300 exceeds any powLimit.
			return new(big.Int).Set(powLimit)
		}
		next.Lsh(next, uint(shifts))
	}

	if next.Sign() == 0 {
		return big.NewInt(1)
	}
	if next.Cmp(powLimit) > 0 {
		return new(big.Int).Set(powLimit)
	}
	return next
}

// NextTarget returns the exact target the block at height h must use, given
// its parent's timestamp and height and the genesis anchor (SPEC §5.2).
//
//	anchorTarget = TargetFromCompact(genesis.bits)
//	anchorTime   = genesis.time
func NextTarget(p *Params, parentTime uint64, parentHeight uint32) *big.Int {
	if p.ConstantDifficulty {
		return new(big.Int).Set(p.PowLimit)
	}
	// Reference uses the anchor's parent time; we define it as genesis.time - spacing.
	// timeDiff = parentTime - (genesisTime - spacing) = parentTime - genesisTime + spacing.
	// Both times are <= MaxBlockTime (2^63-1) so int64 conversion is exact, and
	// the subtraction is done in big.Int inside asertTarget's saturation path
	// via int64 that cannot overflow because |difference| < 2^63 + 600 would...
	// To stay provably safe we compute the difference in big.Int here.
	diff := new(big.Int).SetUint64(parentTime)
	diff.Sub(diff, new(big.Int).SetUint64(p.GenesisTime))
	diff.Add(diff, big.NewInt(TargetSpacing))
	var timeDiff int64
	if diff.Cmp(asertExpLimit) > 0 {
		timeDiff = asertExpLimit.Int64()
	} else if diff.Cmp(asertNegLimit) < 0 {
		timeDiff = asertNegLimit.Int64()
	} else {
		timeDiff = diff.Int64()
	}
	return asertTarget(p.AnchorTarget, timeDiff, int64(parentHeight), p.PowLimit, ASERTHalfLife)
}

// NextBits is NextTarget encoded in compact form; this is the value a header
// at height parentHeight+1 must carry in Bits.
func NextBits(p *Params, parentTime uint64, parentHeight uint32) uint32 {
	return TargetToCompact(NextTarget(p, parentTime, parentHeight))
}

// MedianTimePast returns the median of the given timestamps (the last
// MedianTimeSpan block times, fewer near genesis). Caller passes at least one.
func MedianTimePast(times []uint64) uint64 {
	n := len(times)
	if n == 0 {
		panic("consensus: MedianTimePast of empty slice")
	}
	if n > MedianTimeSpan {
		times = times[n-MedianTimeSpan:]
		n = MedianTimeSpan
	}
	sorted := make([]uint64, n)
	copy(sorted, times)
	// insertion sort: n <= 11
	for i := 1; i < n; i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	return sorted[n/2]
}
