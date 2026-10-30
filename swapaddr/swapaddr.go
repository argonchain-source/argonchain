// Package swapaddr encodes and parses swap addresses (docs/BRIDGE.md,
// "Swap addresses"). A swap address is an Argonmine address that names a
// payout: uARG sent to it from any wallet is paid out by the bridge as the
// named token on the named chain to the named EVM address, with no memo.
// Its 32-byte P2PK payload is not anyone's key; it spells the destination:
//
//	bytes 0-2   magic: every swap address reads "<hrp>1qswap…"; the low
//	            4 bits of byte 2 are the layout version, 0
//	byte 3      the smallest value that makes the payload a valid x coordinate
//	bytes 4-7   chain id, big-endian
//	bytes 8-11  token symbol, ASCII upper case, zero-padded (USDC, USDT)
//	bytes 12-31 the EVM recipient
//
// Nobody has the private key of such a point: finding one is the discrete
// logarithm problem, and a key made at random matches the 248 fixed bits
// with no real chance. Whatever is sent to a swap address can therefore
// never move again; uARG sent there is burned, and every guardian reads the
// payout from the address itself, so there is nothing to register and no
// server can redirect it. ARG or any other asset sent there is lost, which
// is why wallets refuse to send anything but uARG to one.
//
// Byte 3 makes every swap address a valid public key, so wallets that
// check keys accept it, and it must be the smallest such value, so each
// payout has exactly one address.
//
// The package depends only on consensus and wallet encoding, so the
// browser wallet (WebAssembly) parses swap addresses with the same code as
// the guardians.
package swapaddr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
	"argonmine/wallet"
)

// Dest is the payout a swap address names.
type Dest struct {
	ChainID uint64   // EVM chain id
	Symbol  string   // token symbol, upper case, 1 to 4 characters
	To      [20]byte // EVM recipient
}

// magic is "swap" in bech32 after the type character, then version 0.
var magic = [3]byte{0x83, 0xba, 0x10}

var errDest = errors.New("a swap address needs a chain id up to 4294967295, a token symbol of 1 to 4 letters or digits and a recipient")

func normalSymbol(s string) (string, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) < 1 || len(s) > 4 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", false
		}
	}
	return s, true
}

// Payload returns the P2PK payload of the swap address for d.
func Payload(d Dest) ([32]byte, error) {
	var p [32]byte
	sym, ok := normalSymbol(d.Symbol)
	if !ok || d.ChainID == 0 || d.ChainID > 0xffffffff || d.To == ([20]byte{}) {
		return p, errDest
	}
	copy(p[0:3], magic[:])
	binary.BigEndian.PutUint32(p[4:8], uint32(d.ChainID))
	copy(p[8:12], sym)
	copy(p[12:32], d.To[:])
	for c := 0; c < 256; c++ {
		p[3] = byte(c)
		if _, err := schnorr.ParsePubKey(p[:]); err == nil {
			return p, nil
		}
	}
	return p, fmt.Errorf("no valid key for this payout") // about 2^-256
}

// Address returns the swap address for d on the network with this HRP.
func Address(hrp string, d Dest) (string, error) {
	p, err := Payload(d)
	if err != nil {
		return "", err
	}
	return wallet.EncodeAddress(hrp, consensus.OutputTypeP2PK, p[:])
}

// ParsePayload reports the payout a P2PK payload names, if it is the
// canonical payload of a swap address.
func ParsePayload(p []byte) (Dest, bool) {
	var d Dest
	if len(p) != 32 || p[0] != magic[0] || p[1] != magic[1] || p[2] != magic[2] {
		return d, false
	}
	sym := strings.TrimRight(string(p[8:12]), "\x00")
	if strings.IndexByte(sym, 0) >= 0 {
		return d, false
	}
	n, ok := normalSymbol(sym)
	if !ok || n != sym {
		return d, false
	}
	d.ChainID = uint64(binary.BigEndian.Uint32(p[4:8]))
	d.Symbol = sym
	copy(d.To[:], p[12:32])
	want, err := Payload(d)
	if err != nil || string(want[:]) != string(p) {
		return Dest{}, false
	}
	return d, true
}

// Parse reports the payout an address names, if it is a swap address of
// the network with this HRP.
func Parse(hrp, addr string) (Dest, bool) {
	out, err := wallet.DecodeAddress(hrp, strings.TrimSpace(addr))
	if err != nil || out.Type != consensus.OutputTypeP2PK {
		return Dest{}, false
	}
	return ParsePayload(out.Payload)
}

// Is reports whether addr is a swap address of the network.
func Is(hrp, addr string) bool {
	_, ok := Parse(hrp, addr)
	return ok
}

// LooksLike reports whether a P2PK payload starts like a swap address of
// any layout version, valid or not. Wallets refuse to pay anything but
// uARG to such a payload, and uARG only to a canonical one (ParsePayload):
// coins sent there can never move again.
func LooksLike(p []byte) bool {
	return len(p) == 32 && p[0] == magic[0] && p[1] == magic[1] && p[2]&0xf0 == magic[2]&0xf0
}
