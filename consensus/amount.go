package consensus

// Amount is a quantity of the native coin in indivisible units (SPEC §2.2).
type Amount int64

const (
	// UnitsPerCoin is the number of indivisible units in one ARG.
	UnitsPerCoin Amount = 100_000_000

	// MaxMoney is the hard supply cap. No single amount and no sum of amounts
	// may ever exceed it.
	MaxMoney Amount = 21_000_000 * UnitsPerCoin
)

// MoneyRange reports whether a is a legal amount: 0 <= a <= MaxMoney.
func MoneyRange(a Amount) bool { return a >= 0 && a <= MaxMoney }

// AddAmounts returns a+b, requiring both operands and the result to be in
// MoneyRange. Because both operands are <= MaxMoney (< 2^51), the addition
// itself cannot overflow int64; the check is on the result.
func AddAmounts(a, b Amount) (Amount, error) {
	if !MoneyRange(a) || !MoneyRange(b) {
		return 0, ErrAmountOutOfRange
	}
	s := a + b
	if !MoneyRange(s) {
		return 0, ErrAmountOverflow
	}
	return s, nil
}

// SumAmounts adds every amount with checking. An empty slice sums to 0.
func SumAmounts(xs []Amount) (Amount, error) {
	var total Amount
	for _, x := range xs {
		var err error
		if total, err = AddAmounts(total, x); err != nil {
			return 0, err
		}
	}
	return total, nil
}
