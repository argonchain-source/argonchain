package wallet

import (
	"strings"
	"testing"

	"argonmine/consensus"
)

func FuzzDecodeAddress(f *testing.F) {
	key := make([]byte, 32)
	good, _ := EncodeAddress("arg", consensus.OutputTypeP2PK, key)
	f.Add(good)
	f.Add(strings.ToUpper(good))
	f.Add("arg1")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		out, err := DecodeAddress("arg", s)
		if err != nil {
			return
		}
		back, err := EncodeAddress("arg", out.Type, out.Payload)
		if err != nil {
			t.Fatalf("decoded address cannot be re-encoded: %v", err)
		}
		if back != strings.ToLower(s) {
			t.Fatalf("address round trip: %q -> %q", s, back)
		}
	})
}

func FuzzParseAmount(f *testing.F) {
	f.Add("1.5")
	f.Add("0")
	f.Add("21000000")
	f.Add("-0.00000001")
	f.Fuzz(func(t *testing.T, s string) {
		a, err := ParseAmount(s)
		if err != nil {
			return
		}
		if !consensus.MoneyRange(a) {
			t.Fatalf("%q parsed to out-of-range %d", s, a)
		}
		again, err := ParseAmount(FormatAmount(a))
		if err != nil || again != a {
			t.Fatalf("format/parse round trip: %q -> %d -> %q -> %d (%v)", s, a, FormatAmount(a), again, err)
		}
	})
}
