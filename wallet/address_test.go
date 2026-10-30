package wallet

import (
	"errors"
	"strings"
	"testing"

	"argonmine/consensus"
)

func TestAddressRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	for _, hrp := range []string{"arg", "targ", "rarg"} {
		addr, err := EncodeAddress(hrp, consensus.OutputTypeP2PK, key)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(addr, hrp+"1q") {
			t.Fatalf("prefix: %s", addr)
		}
		if len(addr) != len(hrp)+1+1+52+6 {
			t.Fatalf("length %d: %s", len(addr), addr)
		}
		out, err := DecodeAddress(hrp, addr)
		if err != nil {
			t.Fatal(err)
		}
		if out.Type != consensus.OutputTypeP2PK || string(out.Payload) != string(key) {
			t.Fatal("payload mismatch")
		}
		// Upper-case form decodes too; mixed case does not.
		if _, err := DecodeAddress(hrp, strings.ToUpper(addr)); err != nil {
			t.Fatal("upper case rejected")
		}
		mixed := strings.ToUpper(addr[:5]) + addr[5:]
		if _, err := DecodeAddress(hrp, mixed); !errors.Is(err, ErrAddressFormat) {
			t.Fatal("mixed case accepted")
		}
	}
}

func TestAddressErrors(t *testing.T) {
	key := make([]byte, 32)
	addr, _ := EncodeAddress("arg", consensus.OutputTypeP2PK, key)

	// Any single-character change must fail the checksum.
	for i := 3; i < len(addr); i++ {
		b := []byte(addr)
		if b[i] == 'q' {
			b[i] = 'p'
		} else {
			b[i] = 'q'
		}
		if _, err := DecodeAddress("arg", string(b)); err == nil {
			t.Fatalf("corruption at %d accepted", i)
		}
	}
	// Wrong network.
	if _, err := DecodeAddress("targ", addr); !errors.Is(err, ErrAddressHRP) {
		t.Fatalf("hrp: %v", err)
	}
	// Non-address types cannot be encoded.
	if _, err := EncodeAddress("arg", consensus.OutputTypeNullData, []byte("x")); !errors.Is(err, ErrAddressType) {
		t.Fatal("nulldata address")
	}
	if _, err := EncodeAddress("arg", consensus.OutputTypeP2PK, key[:31]); !errors.Is(err, ErrAddressType) {
		t.Fatal("short key address")
	}
	if _, err := DecodeAddress("arg", "arg"); !errors.Is(err, ErrAddressFormat) {
		t.Fatal("tiny")
	}
	if _, err := DecodeAddress("arg", "arg1b"+addr[5:]); err == nil { // 'b' not in charset
		t.Fatal("bad char")
	}
}

// BIP-350 test vector: bech32m checksum of a known string must verify.
func TestBech32mVector(t *testing.T) {
	// From BIP-350 valid bech32m strings.
	valid := []string{
		"A1LQFN3A",
		"a1lqfn3a",
		"abcdef1l7aum6echk45nj3s0wdvt2fg8x9yrzpqzd3ryx",
		"split1checkupstagehandshakeupstreamerranterredcaperredlc445v",
		"?1v759aa",
	}
	for _, s := range valid {
		s = strings.ToLower(s)
		sep := strings.LastIndexByte(s, '1')
		hrp := s[:sep]
		data := make([]byte, 0)
		for i := sep + 1; i < len(s); i++ {
			data = append(data, byte(bech32Rev[s[i]]))
		}
		if !verifyChecksum(hrp, data) {
			t.Fatalf("vector %q failed", s)
		}
	}
	// A bech32 (non-m) string must NOT verify under bech32m.
	s := "a12uel5l"
	sep := strings.LastIndexByte(s, '1')
	data := make([]byte, 0)
	for i := sep + 1; i < len(s); i++ {
		data = append(data, byte(bech32Rev[s[i]]))
	}
	if verifyChecksum(s[:sep], data) {
		t.Fatal("bech32 checksum accepted as bech32m")
	}
}

// The mainnet treasury key in consensus is the treasury wallet's address.
func TestMainnetTreasuryAddress(t *testing.T) {
	out, err := DecodeAddress("arg", "arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2")
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != consensus.OutputTypeP2PK || string(out.Payload) != string(consensus.MainNet.TreasuryKey) {
		t.Fatalf("the treasury address decodes to %x, consensus has %x", out.Payload, consensus.MainNet.TreasuryKey)
	}
}
