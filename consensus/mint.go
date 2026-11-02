package consensus

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Early blocks (SPEC §3.3.2): the mint window. Between a network's
// MintStart and MintEnd, a block's coinbase may create ARG for the mint
// requests the block includes: transactions that pay the network's mint
// asset to the mint key and carry the memo EARLY1. The coins come out of the coinbase like every
// other ARG, at MintRate ARG units per unit paid, and a block creates at
// most MintPerBlock this way. After the window a mint request is invalid,
// so nobody pays for coins that cannot be made. Before it, and on a network
// without a window, the memo means nothing: the transaction is an ordinary
// payment, as it was to every node before the rule (relay policy refuses
// it, mempool.go).
//
// Nodes from before the rule reject a block whose coinbase exceeds subsidy
// plus fees, so the rule is a hard fork: every node must run it before the
// window opens.

// MintMemo is the NULLDATA payload that marks a mint request.
const MintMemo = "EARLY1"

// MintRule is one network's mint window. Start and End are inclusive
// heights; PerBlock is the most ARG one block may mint; Rate is ARG units
// created per unit of Asset paid to Key (a P2PK x-only key).
type MintRule struct {
	Start, End uint32
	PerBlock   Amount
	Rate       Amount
	Asset      Hash
	Key        []byte
}

var (
	ErrTxMintClosed    = errors.New("tx: mint request outside the mint window")
	ErrTxMintUnpaid    = errors.New("tx: mint request pays nothing to the mint key")
	ErrTxMintTooLarge  = errors.New("tx: mint request exceeds the per-block mint")
	ErrTxMintBuyer     = errors.New("tx: mint request's first input is not a key")
	ErrBlockMintCap    = errors.New("block: mint requests exceed the per-block mint")
	ErrBlockMintUnpaid = errors.New("block: coinbase pays a buyer less than its mint")
)

// Mainnet's mint window: 2,000 blocks (about a week) at 500 ARG a block,
// 20 ARG per uARG (the bridge's dollar token, 1e8 units like ARG), paid to
// the treasury key. At most 1,000,000 ARG over the window.
var (
	mainnetMintStart    uint32 = 2_925
	mainnetMintEnd      uint32 = 4_924
	mainnetMintPerBlock Amount = 500 * UnitsPerCoin
	mainnetMintRate     Amount = 20
	mainnetMintAsset           = mustHash("15772bdf3353c217b21394e9db1f5023986b51bd36fa2d41144cb1e898f8a595")
)

func mustHash(s string) Hash {
	h, err := HashFromHex(s)
	if err != nil {
		panic(err)
	}
	return h
}

// withMint sets a network's mint window.
func (p *Params) withMint(r MintRule) *Params {
	if err := r.check(); err != nil {
		panic("consensus: mint rule: " + err.Error())
	}
	p.Mint = &r
	return p
}

func (r *MintRule) check() error {
	switch {
	case r.End < r.Start:
		return errors.New("end before start")
	case r.PerBlock <= 0 || r.PerBlock > MaxMoney:
		return errors.New("per-block amount out of range")
	case r.Rate <= 0 || r.Rate > MaxMoney:
		return errors.New("rate out of range")
	case r.Asset == NativeAsset:
		return errors.New("the asset cannot be ARG")
	case len(r.Key) != P2PKPayloadLen:
		return errors.New("the key must be a 32-byte x-only key")
	}
	return nil
}

// SetMint sets or clears (nil) the mint window of a test network, for a
// node or explorer flag. Mainnet's window is fixed in this file.
func SetMint(p *Params, r *MintRule) error {
	if p.Name == "mainnet" {
		return errors.New("mainnet's mint window is fixed")
	}
	if r == nil {
		p.Mint = nil
		return nil
	}
	if err := r.check(); err != nil {
		return err
	}
	c := *r
	c.Key = append([]byte(nil), r.Key...)
	p.Mint = &c
	return nil
}

// ParseMintRule reads a rule from "start:end:perBlockARG:rateARG:asset:key"
// (amounts in whole ARG, asset and key in hex), the form of the -mint flag.
func ParseMintRule(s string) (*MintRule, error) {
	f := strings.Split(strings.TrimSpace(s), ":")
	if len(f) != 6 {
		return nil, errors.New("want start:end:perBlock:rate:asset:key")
	}
	start, err1 := strconv.ParseUint(f[0], 10, 32)
	end, err2 := strconv.ParseUint(f[1], 10, 32)
	per, err3 := strconv.ParseInt(f[2], 10, 64)
	rate, err4 := strconv.ParseInt(f[3], 10, 64)
	asset, err5 := HashFromHex(f[4])
	key, err6 := hexBytes(f[5])
	for _, err := range []error{err1, err2, err3, err4, err5, err6} {
		if err != nil {
			return nil, err
		}
	}
	r := &MintRule{Start: uint32(start), End: uint32(end), PerBlock: Amount(per) * UnitsPerCoin, Rate: Amount(rate), Asset: asset, Key: key}
	if per <= 0 || Amount(per) > MaxMoney/UnitsPerCoin {
		return nil, errors.New("per-block amount out of range")
	}
	return r, r.check()
}

func hexBytes(s string) ([]byte, error) {
	if len(s) != 2*P2PKPayloadLen {
		return nil, errors.New("the key must be 64 hex characters")
	}
	b := make([]byte, P2PKPayloadLen)
	for i := range b {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, errors.New("the key must be hex")
		}
		b[i] = byte(v)
	}
	return b, nil
}

// MintOpen reports whether blocks at height may mint on this network.
func MintOpen(p *Params, height uint32) bool {
	return p != nil && p.Mint != nil && height >= p.Mint.Start && height <= p.Mint.End
}

// IsMintRequest reports whether tx carries the mint memo. Whether it is a
// valid request is MintAmount's answer.
func IsMintRequest(tx *Tx) bool {
	for i := range tx.Outputs {
		if tx.Outputs[i].Type == OutputTypeNullData && string(tx.Outputs[i].Payload) == MintMemo {
			return true
		}
	}
	return false
}

// MintPaid is what tx pays the mint key in the mint asset, over all its
// outputs. 0 when the network has no mint window.
func MintPaid(p *Params, tx *Tx) (Amount, error) {
	if p == nil || p.Mint == nil {
		return 0, nil
	}
	var paid Amount
	for i := range tx.Outputs {
		o := &tx.Outputs[i]
		if o.Type == OutputTypeP2PK && o.Asset == p.Mint.Asset && bytes.Equal(o.Payload, p.Mint.Key) {
			var err error
			if paid, err = AddAssetAmounts(paid, o.Amount); err != nil {
				return 0, err
			}
		}
	}
	return paid, nil
}

// MintValue is the ARG that paid units buy under the rule: paid × rate,
// checked against MaxMoney.
func MintValue(r *MintRule, paid Amount) (Amount, error) {
	if paid <= 0 {
		return 0, nil
	}
	if paid > MaxMoney/r.Rate {
		return 0, ErrAmountOverflow
	}
	return paid * r.Rate, nil
}

// MintAmount checks a mint request against the rule at height and returns
// the buyer's key (the key of the output its first input spends) and the
// ARG the coinbase must pay it. For a transaction without the memo it
// returns nil, 0, nil. spent are the outputs tx spends, in input order.
func MintAmount(p *Params, tx *Tx, spent []TxOut, height uint32) (buyer []byte, amount Amount, err error) {
	if !IsMintRequest(tx) || p == nil || p.Mint == nil || height < p.Mint.Start {
		return nil, 0, nil
	}
	if height > p.Mint.End {
		return nil, 0, ErrTxMintClosed
	}
	paid, err := MintPaid(p, tx)
	if err != nil {
		return nil, 0, err
	}
	if paid == 0 {
		return nil, 0, ErrTxMintUnpaid
	}
	amount, err = MintValue(p.Mint, paid)
	if err != nil {
		return nil, 0, err
	}
	if amount > p.Mint.PerBlock {
		return nil, 0, ErrTxMintTooLarge
	}
	if len(spent) == 0 || spent[0].Type != OutputTypeP2PK || len(spent[0].Payload) != P2PKPayloadLen {
		return nil, 0, ErrTxMintBuyer
	}
	return append([]byte(nil), spent[0].Payload...), amount, nil
}

// MintPayout is one buyer's due from a block's coinbase.
type MintPayout struct {
	Key    []byte
	Amount Amount
}

// Mints accumulates the buyers of a block in order of first appearance,
// one entry per key, and the total.
type Mints struct {
	Payouts []MintPayout
	Total   Amount
}

// Add records one request's due. It returns ErrBlockMintCap when the block
// would mint more than the rule allows.
func (m *Mints) Add(r *MintRule, key []byte, amount Amount) error {
	if amount == 0 {
		return nil
	}
	total, err := AddAmounts(m.Total, amount)
	if err != nil {
		return err
	}
	if total > r.PerBlock {
		return ErrBlockMintCap
	}
	m.Total = total
	for i := range m.Payouts {
		if bytes.Equal(m.Payouts[i].Key, key) {
			m.Payouts[i].Amount += amount
			return nil
		}
	}
	m.Payouts = append(m.Payouts, MintPayout{Key: append([]byte(nil), key...), Amount: amount})
	return nil
}

// Outputs are the coinbase outputs that pay the buyers, in order.
func (m *Mints) Outputs() []TxOut {
	outs := make([]TxOut, 0, len(m.Payouts))
	for _, p := range m.Payouts {
		outs = append(outs, TxOut{Amount: p.Amount, Type: OutputTypeP2PK, Payload: append([]byte(nil), p.Key...)})
	}
	return outs
}

// CheckMintPaid reports ErrBlockMintUnpaid when the coinbase pays any buyer
// less than its due in ARG, over all its P2PK outputs to that key.
func CheckMintPaid(cb *Tx, m *Mints) error {
	for _, p := range m.Payouts {
		var got Amount
		for i := range cb.Outputs {
			o := &cb.Outputs[i]
			if o.Type == OutputTypeP2PK && o.Asset == NativeAsset && bytes.Equal(o.Payload, p.Key) {
				var err error
				if got, err = AddAmounts(got, o.Amount); err != nil {
					return err
				}
			}
		}
		if got < p.Amount {
			return fmt.Errorf("%w: %x", ErrBlockMintUnpaid, p.Key[:4])
		}
	}
	return nil
}
