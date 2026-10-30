// Package wallet implements addresses, keys and transaction building.
package wallet

import (
	"errors"
	"strings"

	"argonmine/consensus"
)

// Addresses are bech32m (BIP-350): hrp || "1" || data || 6-char checksum.
// data = [output type as one 5-bit group] || base32(payload).
// For P2PK: type 0, payload = 32-byte x-only key -> 'l1' + '1' + 'q' + 52 chars + 6 = 62 chars.

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

const bech32mConst = 0x2bc830a3

var (
	ErrAddressFormat   = errors.New("address: malformed")
	ErrAddressChecksum = errors.New("address: bad checksum")
	ErrAddressHRP      = errors.New("address: wrong network prefix")
	ErrAddressType     = errors.New("address: unsupported output type")
)

var bech32Rev = func() [128]int8 {
	var r [128]int8
	for i := range r {
		r[i] = -1
	}
	for i, c := range bech32Charset {
		r[c] = int8(i)
	}
	return r
}()

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, 2*len(hrp)+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func createChecksum(hrp string, data []byte) []byte {
	values := append(hrpExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(values) ^ bech32mConst
	out := make([]byte, 6)
	for i := 0; i < 6; i++ {
		out[i] = byte((mod >> uint(5*(5-i))) & 31)
	}
	return out
}

func verifyChecksum(hrp string, data []byte) bool {
	return bech32Polymod(append(hrpExpand(hrp), data...)) == bech32mConst
}

// convertBits regroups bits, from -> to bits per group. pad adds zero bits on
// encode; on decode (pad=false) it rejects non-zero or excessive padding.
func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	acc := uint32(0)
	bits := uint(0)
	var out []byte
	maxv := uint32(1<<to) - 1
	for _, v := range data {
		if uint32(v)>>from != 0 {
			return nil, ErrAddressFormat
		}
		acc = acc<<from | uint32(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte((acc<<(to-bits))&maxv))
		}
	} else if bits >= from || (acc<<(to-bits))&maxv != 0 {
		return nil, ErrAddressFormat
	}
	return out, nil
}

// EncodeAddress renders an output (type + payload) as a bech32m address for
// the network with the given HRP. Only P2PK outputs have addresses.
func EncodeAddress(hrp string, outType uint8, payload []byte) (string, error) {
	if outType != consensus.OutputTypeP2PK || len(payload) != consensus.P2PKPayloadLen {
		return "", ErrAddressType
	}
	data := []byte{outType}
	conv, err := convertBits(payload, 8, 5, true)
	if err != nil {
		return "", err
	}
	data = append(data, conv...)
	data = append(data, createChecksum(hrp, data)...)
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, d := range data {
		sb.WriteByte(bech32Charset[d])
	}
	return sb.String(), nil
}

// DecodeAddress parses an address and returns the output it pays to. The
// HRP must match the expected network exactly.
func DecodeAddress(hrp, addr string) (consensus.TxOut, error) {
	if len(addr) < 8 || len(addr) > 90 {
		return consensus.TxOut{}, ErrAddressFormat
	}
	// Reject mixed case; accept all-lower or all-upper.
	lower := strings.ToLower(addr)
	if addr != lower && addr != strings.ToUpper(addr) {
		return consensus.TxOut{}, ErrAddressFormat
	}
	addr = lower
	sep := strings.LastIndexByte(addr, '1')
	if sep < 1 || sep+7 > len(addr) {
		return consensus.TxOut{}, ErrAddressFormat
	}
	gotHRP := addr[:sep]
	for i := 0; i < len(gotHRP); i++ {
		if gotHRP[i] < 33 || gotHRP[i] > 126 {
			return consensus.TxOut{}, ErrAddressFormat
		}
	}
	data := make([]byte, 0, len(addr)-sep-1)
	for i := sep + 1; i < len(addr); i++ {
		c := addr[i]
		if c >= 128 || bech32Rev[c] == -1 {
			return consensus.TxOut{}, ErrAddressFormat
		}
		data = append(data, byte(bech32Rev[c]))
	}
	if !verifyChecksum(gotHRP, data) {
		return consensus.TxOut{}, ErrAddressChecksum
	}
	if gotHRP != hrp {
		return consensus.TxOut{}, ErrAddressHRP
	}
	data = data[:len(data)-6]
	if len(data) < 1 {
		return consensus.TxOut{}, ErrAddressFormat
	}
	outType := data[0]
	payload, err := convertBits(data[1:], 5, 8, false)
	if err != nil {
		return consensus.TxOut{}, err
	}
	if outType != consensus.OutputTypeP2PK || len(payload) != consensus.P2PKPayloadLen {
		return consensus.TxOut{}, ErrAddressType
	}
	return consensus.TxOut{Type: outType, Payload: payload}, nil
}
