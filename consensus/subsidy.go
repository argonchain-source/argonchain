package consensus

const (
	// HalvingInterval is the number of blocks between subsidy halvings.
	HalvingInterval uint32 = 210_000

	// InitialSubsidy is the block reward at height 0.
	InitialSubsidy Amount = 50 * UnitsPerCoin
)

// Subsidy returns the block reward at the given height (SPEC §8).
// After 64 halvings the result is exactly 0; the shift is never performed
// with a count >= 64.
func Subsidy(height uint32) Amount {
	halvings := height / HalvingInterval
	if halvings >= 64 {
		return 0
	}
	return InitialSubsidy >> halvings
}
