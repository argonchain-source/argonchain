package wallet

import (
	"errors"
	"path/filepath"
	"testing"

	"argonmine/consensus"
)

func TestMnemonicAndDeterministicKeys(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidateMnemonic(m) {
		t.Fatal("generated mnemonic invalid")
	}
	if ValidateMnemonic("abandon abandon abandon") {
		t.Fatal("bad mnemonic accepted")
	}
	seed1, _ := SeedFromMnemonic(m, "")
	seed2, _ := SeedFromMnemonic(m, "")
	seedP, _ := SeedFromMnemonic(m, "pass")
	if string(seed1) != string(seed2) || string(seed1) == string(seedP) {
		t.Fatal("seed derivation")
	}
	kc1, _ := NewKeyChain(seed1, CoinTypeTest, 0)
	kc2, _ := NewKeyChain(seed2, CoinTypeTest, 0)
	a1, _ := kc1.Derive(false, 0)
	a2, _ := kc2.Derive(false, 0)
	if string(a1.XOnly) != string(a2.XOnly) {
		t.Fatal("same seed must give same key")
	}
	b, _ := kc1.Derive(false, 1)
	c, _ := kc1.Derive(true, 0)
	if string(a1.XOnly) == string(b.XOnly) || string(a1.XOnly) == string(c.XOnly) {
		t.Fatal("different paths must give different keys")
	}
	kcMain, _ := NewKeyChain(seed1, CoinTypeMain, 0)
	d, _ := kcMain.Derive(false, 0)
	if string(a1.XOnly) == string(d.XOnly) {
		t.Fatal("coin types must separate keys")
	}
	addr := a1.Address("rarg")
	out, err := DecodeAddress("rarg", addr)
	if err != nil || string(out.Payload) != string(a1.XOnly) {
		t.Fatal("address round trip")
	}
}

func TestKeySignVerifies(t *testing.T) {
	m, _ := NewMnemonic()
	seed, _ := SeedFromMnemonic(m, "")
	kc, _ := NewKeyChain(seed, CoinTypeTest, 0)
	k, _ := kc.Derive(false, 3)
	spent := []consensus.TxOut{k.Output(1000)}
	tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: consensus.OutPoint{Index: 1}}}, Outputs: []consensus.TxOut{k.Output(900)}, Witnesses: make([][]byte, 1)}
	if err := k.Sign(consensus.RegTest.ChainTag, tx, spent, 0); err != nil {
		t.Fatal(err)
	}
	if err := consensus.VerifyTxSignatures(consensus.RegTest.ChainTag, tx, spent); err != nil {
		t.Fatal(err)
	}
	if err := consensus.VerifyTxSignatures(consensus.MainNet.ChainTag, tx, spent); err == nil {
		t.Fatal("signature valid on another chain")
	}
}

func TestStoreEncryption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.json")
	m, _ := NewMnemonic()
	st, err := CreateStore(path, "regtest", m, "", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mnemonic() != m {
		t.Fatal("mnemonic")
	}
	if _, err := CreateStore(path, "regtest", m, "", "pw"); !errors.Is(err, ErrWalletExists) {
		t.Fatal("overwrite allowed")
	}
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Locked() || st2.Seed() != nil || st2.Mnemonic() != "" {
		t.Fatal("should be locked")
	}
	if err := st2.Unlock("wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := st2.Unlock("pw"); err != nil {
		t.Fatal(err)
	}
	if st2.Mnemonic() != m || string(st2.Seed()) != string(st.Seed()) {
		t.Fatal("decrypted secret differs")
	}
	// State persists.
	st2.State().NextReceive = 7
	if err := st2.Save(); err != nil {
		t.Fatal(err)
	}
	st3, _ := OpenStore(path)
	if st3.State().NextReceive != 7 {
		t.Fatal("state not saved")
	}
}

func TestParseFormatAmount(t *testing.T) {
	cases := map[string]consensus.Amount{
		"0": 0, "1": 100000000, "1.5": 150000000, "0.00000001": 1, "21000000": consensus.MaxMoney, ".5": 50000000, "12.34567891": 1234567891,
	}
	for in, want := range cases {
		got, err := ParseAmount(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d err %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "1.123456789", "21000001", "-1", "1.2.3"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if FormatAmount(150000000) != "1.50000000" || FormatAmount(-1) != "-0.00000001" {
		t.Fatal("format")
	}
}
