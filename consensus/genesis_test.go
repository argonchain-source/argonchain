package consensus

import "testing"

// Every network's genesis block must be a valid block under its own rules.
// Run cmd/genesis to produce the nonces if this fails with ErrHighHash.
func TestGenesisBlocksValid(t *testing.T) {
	for _, p := range []*Params{MainNet, TestNet, RegTest} {
		g := p.Genesis
		if err := CheckHeaderStateless(p, &g.Header); err != nil {
			t.Fatalf("%s genesis header: %v (nonce=%d)", p.Name, err, g.Header.Nonce)
		}
		if g.Header.Bits != p.PowLimitBits {
			t.Fatalf("%s genesis bits should equal pow limit", p.Name)
		}
		ov := NewOverlay(MemView{})
		if err := ConnectBlock(p, g, 0, ov); err != nil {
			t.Fatalf("%s genesis connect: %v", p.Name, err)
		}
		if len(ov.Added()) != 0 {
			t.Fatalf("%s genesis must add no coins", p.Name)
		}
		if p.ChainTag != g.Header.Hash() || p.GenesisHash != g.Header.Hash() {
			t.Fatalf("%s chain tag mismatch", p.Name)
		}
		if p.GenesisTime != g.Header.Time {
			t.Fatalf("%s genesis time mismatch", p.Name)
		}
		// The block must round-trip through the wire format.
		back, err := DecodeBlock(g.Serialize())
		if err != nil || back.Header.Hash() != g.Header.Hash() {
			t.Fatalf("%s genesis does not round trip", p.Name)
		}
	}
}

func TestNetworksDistinct(t *testing.T) {
	nets := []*Params{MainNet, TestNet, RegTest}
	for i := range nets {
		for j := i + 1; j < len(nets); j++ {
			a, b := nets[i], nets[j]
			if a.ChainTag == b.ChainTag {
				t.Fatalf("%s and %s share a chain tag", a.Name, b.Name)
			}
			if a.Magic == b.Magic || a.HRP == b.HRP || a.P2PPort == b.P2PPort || a.RPCPort == b.RPCPort {
				t.Fatalf("%s and %s share a network identifier", a.Name, b.Name)
			}
		}
	}
}
