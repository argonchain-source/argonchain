package consensus

import (
	"math"
	"math/big"
	"testing"
)

// A synthetic non-regtest params for ASERT tests: anchor target well below
// powLimit so both directions have room.
func asertParams(t *testing.T) *Params {
	t.Helper()
	p := *MainNet
	p.ConstantDifficulty = false
	p.GenesisTime = 1_000_000
	p.PowLimit, _ = CompactToTarget(0x1F0FFFFF)
	p.AnchorTarget, _ = CompactToTarget(0x1B0FFFFF) // 2^32 harder than limit
	return &p
}

func TestASERTOnScheduleIsStable(t *testing.T) {
	p := asertParams(t)
	// Parent at height h-1 arrived exactly at genesis + 600*(h-1).
	for _, h := range []uint32{1, 2, 10, 1000, 100_000, 10_000_000} {
		parentHeight := h - 1
		parentTime := p.GenesisTime + uint64(TargetSpacing)*uint64(parentHeight)
		got := NextTarget(p, parentTime, parentHeight)
		if got.Cmp(p.AnchorTarget) != 0 {
			t.Fatalf("h=%d: on-schedule target drifted: %x vs %x", h, got, p.AnchorTarget)
		}
	}
}

func TestASERTHalfLifeDoublesAndHalves(t *testing.T) {
	p := asertParams(t)
	parentHeight := uint32(5000)
	onTime := p.GenesisTime + uint64(TargetSpacing)*uint64(parentHeight)

	double := new(big.Int).Lsh(p.AnchorTarget, 1)
	half := new(big.Int).Rsh(p.AnchorTarget, 1)

	// One half-life behind schedule: target exactly doubles (frac = 0, exact path).
	if got := NextTarget(p, onTime+uint64(ASERTHalfLife), parentHeight); got.Cmp(double) != 0 {
		t.Fatalf("1 half-life late: got %x want %x", got, double)
	}
	// One half-life ahead: exactly halves.
	if got := NextTarget(p, onTime-uint64(ASERTHalfLife), parentHeight); got.Cmp(half) != 0 {
		t.Fatalf("1 half-life early: got %x want %x", got, half)
	}
	// Three half-lives late: ×8.
	x8 := new(big.Int).Lsh(p.AnchorTarget, 3)
	if got := NextTarget(p, onTime+3*uint64(ASERTHalfLife), parentHeight); got.Cmp(x8) != 0 {
		t.Fatalf("3 half-lives late: got %x want %x", got, x8)
	}
}

func TestASERTFractionalApproximation(t *testing.T) {
	// For frac in [0, 65536) the cubic approximation must be within 0.013% of 2^(frac/65536).
	for frac := uint64(0); frac < 65536; frac += 37 {
		factor := 65536 + ((195766423245049*frac + 971821376*frac*frac + 5127*frac*frac*frac + (1 << 47)) >> 48)
		exact := 65536 * math.Pow(2, float64(frac)/65536)
		relErr := math.Abs(float64(factor)-exact) / exact
		if relErr > 0.00013 {
			t.Fatalf("frac=%d: factor %d vs exact %.2f, rel err %.6f", frac, factor, exact, relErr)
		}
	}
}

func TestASERTMonotonicInTime(t *testing.T) {
	p := asertParams(t)
	parentHeight := uint32(777)
	base := p.GenesisTime + uint64(TargetSpacing)*uint64(parentHeight)
	prev := NextTarget(p, base-50_000, parentHeight)
	for dt := int64(-50_000); dt <= 50_000; dt += 613 {
		cur := NextTarget(p, uint64(int64(base)+dt), parentHeight)
		if cur.Cmp(prev) < 0 {
			t.Fatalf("target decreased when time increased at dt=%d", dt)
		}
		prev = cur
	}
}

func TestASERTClamps(t *testing.T) {
	p := asertParams(t)
	parentHeight := uint32(10)
	onTime := p.GenesisTime + uint64(TargetSpacing)*uint64(parentHeight)

	// Absurdly late (1000 years): clamps at powLimit.
	if got := NextTarget(p, onTime+1000*365*86400, parentHeight); got.Cmp(p.PowLimit) != 0 {
		t.Fatalf("late clamp: got %x", got)
	}
	// Parent timestamp far in the "past" relative to schedule: clamps to >= 1.
	// (Use a parent height so large that ideal time dwarfs actual time.)
	if got := NextTarget(p, p.GenesisTime+1, 4_000_000_000); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("early clamp: got %x", got)
	}
	// Extreme u64 timestamp must not panic and must clamp at powLimit.
	if got := NextTarget(p, MaxBlockTime, parentHeight); got.Cmp(p.PowLimit) != 0 {
		t.Fatalf("u64 max clamp: got %x", got)
	}
	// Parent time before genesis (cannot happen on a valid chain, must still be total).
	if got := NextTarget(p, 0, parentHeight); got.Sign() <= 0 {
		t.Fatalf("time 0 gave non-positive target")
	}
}

func TestASERTHeightOneEqualsAnchor(t *testing.T) {
	p := asertParams(t)
	if got := NextTarget(p, p.GenesisTime, 0); got.Cmp(p.AnchorTarget) != 0 {
		t.Fatalf("height 1 target must equal anchor; got %x", got)
	}
}

func TestASERTRegtestConstant(t *testing.T) {
	for _, pt := range []uint64{0, RegTest.GenesisTime, RegTest.GenesisTime + 1<<40} {
		if got := NextTarget(RegTest, pt, 123); got.Cmp(RegTest.PowLimit) != 0 {
			t.Fatal("regtest difficulty must be constant")
		}
	}
	if NextBits(RegTest, 0, 0) != RegTest.PowLimitBits {
		t.Fatal("regtest bits")
	}
}

func TestASERTReferenceVectorShape(t *testing.T) {
	// Reference identity: with heightDiff = n and timeDiff = 600*(n+1) the
	// exponent is zero and the target is unchanged, for any refTarget.
	ref, _ := CompactToTarget(0x1C0FFFFF)
	limit, _ := CompactToTarget(0x1F0FFFFF)
	for _, n := range []int64{0, 1, 2, 100, 1 << 20} {
		got := asertTarget(ref, 600*(n+1), n, limit, 2*86400)
		if got.Cmp(ref) != 0 {
			t.Fatalf("n=%d: %x", n, got)
		}
	}
	// 2 days late with the BCH half-life of 2 days doubles exactly.
	got := asertTarget(ref, 600+2*86400, 0, limit, 2*86400)
	if got.Cmp(new(big.Int).Lsh(ref, 1)) != 0 {
		t.Fatalf("doubling: %x", got)
	}
}

func TestMedianTimePast(t *testing.T) {
	if MedianTimePast([]uint64{5}) != 5 {
		t.Fatal("single")
	}
	if MedianTimePast([]uint64{3, 1, 2}) != 2 {
		t.Fatal("three")
	}
	// Only the last 11 count; the median is element 5 of the sorted last 11.
	times := []uint64{100, 100, 100, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if got := MedianTimePast(times); got != 6 {
		t.Fatalf("got %d", got)
	}
	// Input must not be mutated.
	in := []uint64{9, 8, 7}
	MedianTimePast(in)
	if in[0] != 9 || in[1] != 8 || in[2] != 7 {
		t.Fatal("input mutated")
	}
}
