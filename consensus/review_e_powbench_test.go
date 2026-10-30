package consensus

import (
	"testing"
	"time"
)

// TestReviewE_PowCost reports the cost of one Argon2id evaluation here and
// what filling chain.MaxSideHeaders (100,000) with minimum-difficulty mainnet
// headers (2^18 expected evaluations each) costs an attacker.
func TestReviewE_PowCost(t *testing.T) {
	h := MainNet.Genesis.Header
	const n = 20
	start := time.Now()
	for i := 0; i < n; i++ {
		h.Nonce = uint64(i)
		PowHash(h.Serialize())
	}
	per := time.Since(start) / n
	evals := 100_000.0 * float64(uint64(1)<<18)
	coreHours := evals * per.Seconds() / 3600
	t.Logf("one Argon2id: %s; mainnet powLimit header: 2^18 evals; 100,000 side headers: %.3g evals = %.0f core-hours at this speed",
		per, evals, coreHours)
}
