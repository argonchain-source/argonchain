package consensus

import (
	"errors"
	"testing"
)

func TestMoneyRange(t *testing.T) {
	if !MoneyRange(0) || !MoneyRange(MaxMoney) || !MoneyRange(1) {
		t.Fatal("valid amounts rejected")
	}
	if MoneyRange(-1) || MoneyRange(MaxMoney+1) || MoneyRange(Amount(1<<63-1)) || MoneyRange(Amount(-1<<63)) {
		t.Fatal("invalid amounts accepted")
	}
}

// CVE-2010-5139 class: sums that wrap or exceed the cap must be rejected
// before any comparison with inputs happens.
func TestAddAmountsChecked(t *testing.T) {
	if _, err := AddAmounts(MaxMoney, 1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatal("cap exceeded not detected")
	}
	if _, err := AddAmounts(MaxMoney/2+1, MaxMoney/2+1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatal("cap exceeded not detected")
	}
	if _, err := AddAmounts(-1, 1); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatal("negative operand accepted")
	}
	if _, err := AddAmounts(1, Amount(1<<63-1)); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatal("huge operand accepted")
	}
	// The classic attack: two outputs that individually look fine but sum to a negative int64.
	huge := Amount(1<<63 - 1)
	if _, err := SumAmounts([]Amount{huge, huge, 1}); err == nil {
		t.Fatal("wrapping sum accepted")
	}
	s, err := SumAmounts([]Amount{1, 2, 3})
	if err != nil || s != 6 {
		t.Fatal("simple sum wrong")
	}
	s, err = SumAmounts(nil)
	if err != nil || s != 0 {
		t.Fatal("empty sum wrong")
	}
}

func TestSubsidySchedule(t *testing.T) {
	if Subsidy(0) != 50*UnitsPerCoin {
		t.Fatal("genesis subsidy")
	}
	if Subsidy(209_999) != 50*UnitsPerCoin {
		t.Fatal("last block before halving")
	}
	if Subsidy(210_000) != 25*UnitsPerCoin {
		t.Fatal("first halving")
	}
	if Subsidy(420_000) != 1_250_000_000 {
		t.Fatal("second halving")
	}
	if Subsidy(6_930_000) != 0 { // 33rd halving: 50e8 >> 33 = 0
		t.Fatalf("subsidy should reach 0 at 33 halvings, got %d", Subsidy(6_930_000))
	}
	if Subsidy(64*HalvingInterval) != 0 || Subsidy(100*HalvingInterval) != 0 || Subsidy(1<<32-1) != 0 {
		t.Fatal("subsidy must be exactly 0 at >= 64 halvings")
	}
}

func TestTotalSupplyBelowCap(t *testing.T) {
	// Sum over every halving period: period subsidy × 210000, with checked adds.
	var total Amount
	for halvings := uint32(0); halvings < 64; halvings++ {
		per := Subsidy(halvings * HalvingInterval)
		if per == 0 {
			break
		}
		period := per * Amount(HalvingInterval) // 50e8*210000 ≈ 1.05e15 fits easily
		var err error
		if total, err = AddAmounts(total, period); err != nil {
			t.Fatal(err)
		}
	}
	// Exactly Bitcoin's figure: 20,999,999.9769 ARG.
	if total != 2_099_999_997_690_000 {
		t.Fatalf("total emission %d", total)
	}
	if total >= MaxMoney {
		t.Fatal("emission must stay strictly below MAX_MONEY")
	}
}
