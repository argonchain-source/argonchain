package chain

import (
	"testing"

	"argonmine/consensus"
)

// Coins carrying a non-ARG asset round-trip through the on-disk encoding,
// and ARG coins keep the pre-asset encoding (older databases stay readable).
func TestCoinEncodingWithAsset(t *testing.T) {
	native := consensus.Coin{Out: consensus.TxOut{Amount: 7, Type: consensus.OutputTypeP2PK, Payload: make([]byte, 32)}, Height: 3, Coinbase: true}
	nb := encodeCoin(native)
	if len(nb) != 8+1+1+32+4+1 {
		t.Fatalf("native coin encoding grew to %d bytes", len(nb))
	}
	back, err := decodeCoin(nb)
	if err != nil || back.Out.Asset != consensus.NativeAsset || back.Out.Amount != 7 || !back.Coinbase {
		t.Fatalf("native round trip: %+v %v", back, err)
	}
	var issuer [32]byte
	issuer[0] = 2
	asset := consensus.AssetID(issuer, "UARG")
	ac := consensus.Coin{Out: consensus.TxOut{Amount: 1000, Type: consensus.OutputTypeP2PK, Payload: make([]byte, 32), Asset: asset}, Height: 9}
	ab := encodeCoin(ac)
	if len(ab) != len(nb)+32 {
		t.Fatalf("asset coin encoding: %d bytes", len(ab))
	}
	back, err = decodeCoin(ab)
	if err != nil || back.Out.Asset != asset || back.Out.Amount != 1000 || back.Height != 9 || back.Coinbase {
		t.Fatalf("asset round trip: %+v %v", back, err)
	}
	if _, err := decodeCoin(append(nb, 1, 2, 3)); err == nil {
		t.Fatal("trailing garbage accepted")
	}
}
