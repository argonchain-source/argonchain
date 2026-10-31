package names

import (
	"testing"

	"argonmine/consensus"
)

func TestCheck(t *testing.T) {
	good := []string{"abc", "alice", "a-b", "x1y", "satoshi-nakamoto-2009", "abcdefghijklmnopqrstuvwxyz012345"}
	for _, g := range good[1:] {
		if err := Check(g); err != nil {
			t.Errorf("%q: %v", g, err)
		}
	}
	bad := map[string]error{"ab": ErrLength, "abcdefghijklmnopqrstuvwxyz0123456": ErrLength, "Alice": ErrUpper, "al ice": ErrChars, "-abc": ErrHyphen, "abc-": ErrHyphen, "alice.arg": ErrSuffix, "a_b": ErrChars, "ümlaut": ErrChars}
	for b, want := range bad {
		if err := Check(b); err != want {
			t.Errorf("%q: %v, want %v", b, err, want)
		}
	}
	if Normalize("  Alice.ARG ") != "alice" {
		t.Fatal("normalize")
	}
}

func TestMemoRoundTrip(t *testing.T) {
	ops := []Op{{OpRegister, "alice", 2}, {OpRenew, "bob", 1}, {OpTransfer, "carol", 0}, {OpPrimary, "dave", 0}}
	for _, o := range ops {
		got, ok := Parse([]byte(o.Memo()))
		if !ok || got != o {
			t.Errorf("%v -> %q -> %v %v", o, o.Memo(), got, ok)
		}
		if len(o.Memo()) > consensus.MaxNullDataLen {
			t.Errorf("memo too long")
		}
	}
	long := Op{OpRegister, "abcdefghijklmnopqrstuvwxyz012345", 5}
	if len(long.Memo()) > consensus.MaxNullDataLen {
		t.Fatalf("longest memo %d bytes", len(long.Memo()))
	}
	for _, bad := range []string{"", "NMS1", "NMS1 register", "NMS1 register alice", "NMS1 register alice 0", "NMS1 register alice 6", "NMS1 register alice 01", "NMS1 register alice x", "NMS1 renew alice", "NMS1 transfer alice 1", "NMS1 primary Alice", "NMS1 sell alice", "NMS2 register alice 1", "NMS1  register alice 1", "BRG1 whatever"} {
		if _, ok := Parse([]byte(bad)); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	if op, ok := FindMemo([][]byte{[]byte("hello"), []byte("NMS1 primary bob"), []byte("NMS1 register x 1")}); !ok || op.Kind != OpPrimary || op.Label != "bob" {
		t.Fatalf("find: %v %v", op, ok)
	}
}

func TestPrices(t *testing.T) {
	p := consensus.MainNet
	if PricePerYear(p, "abc") != 250*consensus.UnitsPerCoin || PricePerYear(p, "abcd") != 50*consensus.UnitsPerCoin || PricePerYear(p, "abcde") != 10*consensus.UnitsPerCoin || PricePerYear(p, "a-very-long-name") != 10*consensus.UnitsPerCoin {
		t.Fatal("prices")
	}
	if Price(p, "abc", 3) != 750*consensus.UnitsPerCoin || Price(p, "abc", 0) != 0 {
		t.Fatal("price by years")
	}
	if Price(consensus.RegTest, "abc", 3) != 0 || TreasuryAddress(consensus.RegTest) != "" {
		t.Fatal("regtest without a stand-in: no treasury, no price")
	}
	// A stand-in makes a test network charge like mainnet.
	if err := SetStandInTreasury(consensus.RegTest, "rarg1qnkvwnnl9rpy4sujx4rv8ayk05un6svdnwptzpl2jfw582642uupqeuhtdg"); err != nil {
		t.Fatal(err)
	}
	defer SetStandInTreasury(consensus.RegTest, "")
	if !HasTreasury(consensus.RegTest) || Price(consensus.RegTest, "abc", 1) != 250*consensus.UnitsPerCoin || TreasuryAddress(consensus.RegTest)[:5] != "rarg1" {
		t.Fatal("stand-in treasury")
	}
	if TreasuryAddress(consensus.MainNet) == TreasuryAddress(consensus.RegTest) {
		t.Fatal("the stand-in must not replace a real treasury")
	}
	if err := SetStandInTreasury(consensus.RegTest, "arg1notregtest"); err == nil {
		t.Fatal("wrong-network stand-in accepted")
	}
	if a := TreasuryAddress(p); a == "" || a[:4] != "arg1" {
		t.Fatalf("treasury %q", a)
	}
	if pr := Prices(p); len(pr) != 3 || pr["5"] != 10*consensus.UnitsPerCoin {
		t.Fatalf("prices %v", pr)
	}
}

func TestApply(t *testing.T) {
	p := consensus.MainNet
	alice, bob := "arg1alice", "arg1bob"
	reg := Op{OpRegister, "alice", 2}
	// Underpaid: nothing.
	if _, ok := Apply(p, nil, reg, Tx{Signer: alice, Paid: 19 * consensus.UnitsPerCoin}, 100); ok {
		t.Fatal("underpaid register accepted")
	}
	r, ok := Apply(p, nil, reg, Tx{Signer: alice, Paid: 20 * consensus.UnitsPerCoin, TxID: "t1"}, 100)
	if !ok || r.Owner != alice || r.Expires != 100+2*BlocksPerYear || r.Registered != 100 || r.RegTxID != "t1" || r.Primary {
		t.Fatalf("register: %+v %v", r, ok)
	}
	// Taken: a second register fails, even overpaid.
	if _, ok := Apply(p, &r, reg, Tx{Signer: bob, Paid: 1000 * consensus.UnitsPerCoin}, 101); ok {
		t.Fatal("taken name registered")
	}
	// Renew by a stranger: no. By the owner, active: extends from the expiry.
	if _, ok := Apply(p, &r, Op{OpRenew, "alice", 1}, Tx{Signer: bob, Paid: 10 * consensus.UnitsPerCoin}, 101); ok {
		t.Fatal("stranger renewed")
	}
	r2, ok := Apply(p, &r, Op{OpRenew, "alice", 1}, Tx{Signer: alice, Paid: 10 * consensus.UnitsPerCoin}, 101)
	if !ok || r2.Expires != r.Expires+BlocksPerYear {
		t.Fatalf("renew: %+v %v", r2, ok)
	}
	// Primary, then transfer clears it; the transfer goes to output 0.
	r3, ok := Apply(p, &r2, Op{OpPrimary, "alice", 0}, Tx{Signer: alice}, 102)
	if !ok || !r3.Primary {
		t.Fatal("primary")
	}
	if _, ok := Apply(p, &r3, Op{OpTransfer, "alice", 0}, Tx{Signer: alice}, 103); ok {
		t.Fatal("transfer without a recipient")
	}
	r4, ok := Apply(p, &r3, Op{OpTransfer, "alice", 0}, Tx{Signer: alice, Out0: bob}, 103)
	if !ok || r4.Owner != bob || r4.Primary || r4.Expires != r3.Expires {
		t.Fatalf("transfer: %+v", r4)
	}
	// Grace: only the owner may renew, from the renewing block; after grace
	// anyone may register.
	exp := r4.Expires
	if Status(&r4, exp-1) != StatusRegistered || Status(&r4, exp) != StatusGrace || Status(&r4, exp+GraceBlocks) != StatusAvailable {
		t.Fatal("status")
	}
	if _, ok := Apply(p, &r4, reg, Tx{Signer: alice, Paid: 20 * consensus.UnitsPerCoin}, exp+1); ok {
		t.Fatal("registered during grace")
	}
	r5, ok := Apply(p, &r4, Op{OpRenew, "alice", 1}, Tx{Signer: bob, Paid: 10 * consensus.UnitsPerCoin}, exp+10)
	if !ok || r5.Expires != exp+10+BlocksPerYear {
		t.Fatalf("grace renew: %+v %v", r5, ok)
	}
	r6, ok := Apply(p, &r4, reg, Tx{Signer: alice, Paid: 20 * consensus.UnitsPerCoin, TxID: "t9"}, exp+GraceBlocks)
	if !ok || r6.Owner != alice || r6.Registered != exp+GraceBlocks || r6.RegTxID != "t9" {
		t.Fatalf("re-register: %+v %v", r6, ok)
	}
	// Reserved: only the treasury.
	if _, ok := Apply(p, nil, Op{OpRegister, "pool", 1}, Tx{Signer: alice, Paid: 50 * consensus.UnitsPerCoin}, 1); ok {
		t.Fatal("reserved registered by a stranger")
	}
	if _, ok := Apply(p, nil, Op{OpRegister, "pool", 1}, Tx{Signer: TreasuryAddress(p), Paid: 50 * consensus.UnitsPerCoin}, 1); !ok {
		t.Fatal("treasury refused a reserved name")
	}
	// Nothing is free: a network with no treasury and no stand-in registers nothing.
	if _, ok := Apply(consensus.RegTest, nil, reg, Tx{Signer: alice, Paid: 1000 * consensus.UnitsPerCoin}, 1); ok {
		t.Fatal("registered with nowhere to pay")
	}
	if _, ok := Apply(p, nil, reg, Tx{Paid: 1000 * consensus.UnitsPerCoin}, 1); ok {
		t.Fatal("no signer")
	}
}
