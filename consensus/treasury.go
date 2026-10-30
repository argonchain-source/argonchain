package consensus

import (
	"bytes"
	"encoding/hex"
)

// The treasury (SPEC §3.3.1). From a network's TreasuryHeight on, every
// coinbase pays TreasuryBps of the block subsidy (not of the fees, which
// stay with the miner) to the treasury key, in ARG. Earlier blocks, and
// networks without a treasury, are unaffected. Nodes from before the rule
// accept such blocks, since a coinbase may have several outputs, so the rule
// is a soft fork: it only turns away blocks that leave the treasury out.

// TreasuryBps is the treasury's share of the block subsidy: 1%.
const TreasuryBps = 100

// The mainnet treasury, from block 300 (about two days after the rule was
// added on 2026-10-28): address
// arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2.
var (
	mainnetTreasuryHeight uint32 = 300
	mainnetTreasuryKey           = mustHex("7ad1a30d482a3fdb23efd2de4da69de2e9dc50d204d8cd82495183e6b3ffdf6c")
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// withTreasury sets a network's treasury.
func (p *Params) withTreasury(height uint32, key []byte) *Params {
	if len(key) != P2PKPayloadLen {
		panic("treasury key must be a 32-byte x-only key")
	}
	p.TreasuryHeight, p.TreasuryKey = height, key
	return p
}

// TreasuryAmount is what the coinbase at height must pay the treasury: 0
// before the network's treasury height, or on a network without one.
func TreasuryAmount(p *Params, height uint32) Amount {
	if p == nil || p.TreasuryHeight == 0 || height < p.TreasuryHeight || len(p.TreasuryKey) != P2PKPayloadLen {
		return 0
	}
	return Subsidy(height) * TreasuryBps / 10000
}

// TreasuryOutput is the output a coinbase at height adds for the treasury;
// ok is false when none is due.
func TreasuryOutput(p *Params, height uint32) (out TxOut, ok bool) {
	a := TreasuryAmount(p, height)
	if a == 0 {
		return TxOut{}, false
	}
	return TxOut{Amount: a, Type: OutputTypeP2PK, Payload: append([]byte(nil), p.TreasuryKey...)}, true
}

// CheckTreasury reports ErrBlockTreasury when the coinbase at height pays the
// treasury key less than TreasuryAmount in ARG, over all its outputs.
func CheckTreasury(p *Params, cb *Tx, height uint32) error {
	want := TreasuryAmount(p, height)
	if want == 0 {
		return nil
	}
	var got Amount
	for _, o := range cb.Outputs {
		if o.Type == OutputTypeP2PK && o.Asset == NativeAsset && bytes.Equal(o.Payload, p.TreasuryKey) {
			sum, err := AddAmounts(got, o.Amount)
			if err != nil {
				return err
			}
			got = sum
		}
	}
	if got < want {
		return ErrBlockTreasury
	}
	return nil
}
