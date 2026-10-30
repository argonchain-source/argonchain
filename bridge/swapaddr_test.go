package bridge

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
	"argonmine/wallet"
)

func TestSwapAddressRoundTrip(t *testing.T) {
	to, _ := ParseAddress("0x1111111111111111111111111111111111111111")
	for _, hrp := range []string{"arg", "targ", "rarg"} {
		for _, d := range []SwapDest{
			{ChainID: 11155111, Symbol: "USDC", To: to},
			{ChainID: 97, Symbol: "usdt", To: to},
			{ChainID: 1, Symbol: "DAI", To: to},
			{ChainID: 0xffffffff, Symbol: "A", To: to},
		} {
			a, err := SwapAddress(hrp, d)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(a, hrp+"1qswap") {
				t.Fatalf("%s does not read %s1qswap", a, hrp)
			}
			got, ok := ParseSwapAddress(hrp, a)
			want := d
			want.Symbol = strings.ToUpper(d.Symbol)
			if !ok || got != want {
				t.Fatalf("%s parsed as %+v %v, want %+v", a, got, ok, want)
			}
			if _, ok := ParseSwapAddress("x"+hrp, a); ok {
				t.Fatal("parsed on another network")
			}
			if strings.ToUpper(a) != a {
				if _, ok := ParseSwapAddress(hrp, strings.ToUpper(a)); !ok {
					t.Fatal("upper-case form refused")
				}
			}
		}
	}
}

func TestSwapAddressRejects(t *testing.T) {
	to, _ := ParseAddress("0x2222222222222222222222222222222222222222")
	for _, d := range []SwapDest{
		{ChainID: 0, Symbol: "USDC", To: to},
		{ChainID: 1 << 32, Symbol: "USDC", To: to},
		{ChainID: 1, Symbol: "", To: to},
		{ChainID: 1, Symbol: "USDCX", To: to},
		{ChainID: 1, Symbol: "US-C", To: to},
		{ChainID: 1, Symbol: "USDC"},
	} {
		if _, err := SwapAddress("targ", d); err == nil {
			t.Fatalf("%+v accepted", d)
		}
	}
	p, err := SwapPayload(SwapDest{ChainID: 5, Symbol: "USDT", To: to})
	if err != nil {
		t.Fatal(err)
	}
	// Every other value of the counter byte, and any change of version,
	// symbol padding or case, is not a swap address.
	for c := 0; c < 256; c++ {
		if byte(c) == p[3] {
			continue
		}
		q := p
		q[3] = byte(c)
		if _, ok := ParseSwapPayload(q[:]); ok {
			t.Fatalf("counter %d accepted beside %d", c, p[3])
		}
	}
	for _, mut := range []func(q *[32]byte){
		func(q *[32]byte) { q[2] |= 1 },                             // version 1
		func(q *[32]byte) { q[8] = 'u' },                            // lower case
		func(q *[32]byte) { q[9] = 0; q[10] = 'D' },                 // a hole in the symbol
		func(q *[32]byte) { q[8], q[9], q[10], q[11] = 0, 0, 0, 0 }, // no symbol
		func(q *[32]byte) { q[0] ^= 0x80 },
	} {
		q := p
		mut(&q)
		if _, ok := ParseSwapPayload(q[:]); ok {
			t.Fatalf("mutated payload %x accepted", q)
		}
	}
	// Ordinary keys are not swap addresses.
	for i := 0; i < 200; i++ {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		a, err := wallet.EncodeAddress("targ", consensus.OutputTypeP2PK, schnorr.SerializePubKey(k.PubKey()))
		if err != nil {
			t.Fatal(err)
		}
		if IsSwapAddress("targ", a) {
			t.Fatalf("key address %s reads as a swap address", a)
		}
	}
}
