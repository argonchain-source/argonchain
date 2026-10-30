package miner

import (
	"encoding/hex"
	"fmt"

	"argonmine/consensus"
)

// TemplateJSON mirrors the node's getblocktemplate result.
type TemplateJSON struct {
	Version           uint32           `json:"version"`
	PreviousBlockHash string           `json:"previousblockhash"`
	Height            uint32           `json:"height"`
	Bits              string           `json:"bits"`
	CurTime           uint64           `json:"curtime"`
	MinTime           uint64           `json:"mintime"`
	CoinbaseValue     consensus.Amount `json:"coinbasevalue"`
	Transactions      []struct {
		Data string `json:"data"`
		TxID string `json:"txid"`
	} `json:"transactions"`
}

// AssembleFromTemplate builds a complete block (nonce 0) from a node's
// template, paying the whole coinbase value to payout, less the treasury's
// share from the network's treasury height (consensus/treasury.go), which
// it pays the treasury. Remote miners and pools use this; the in-process
// miner uses BuildTemplate.
func AssembleFromTemplate(p *consensus.Params, t *TemplateJSON, payout consensus.TxOut, tag []byte, now int64) (*consensus.Block, error) {
	if payout.Type != consensus.OutputTypeP2PK || len(payout.Payload) != consensus.P2PKPayloadLen {
		return nil, ErrNoPayout
	}
	value := t.CoinbaseValue - consensus.TreasuryAmount(p, t.Height)
	if value < 0 {
		return nil, fmt.Errorf("template: coinbase value %d is below the treasury's share", t.CoinbaseValue)
	}
	return AssembleFromTemplateOutputs(p, t, []consensus.TxOut{{Amount: value, Type: payout.Type, Payload: append([]byte{}, payout.Payload...)}}, tag, now)
}

// AssembleFromTemplateOutputs is AssembleFromTemplate with an explicit
// coinbase output list, for example a miner share and a referral share. The
// amounts must be P2PK ARG outputs summing to at most the template's
// coinbase value less the treasury's share, which is appended as the last
// output when the height pays one.
func AssembleFromTemplateOutputs(p *consensus.Params, t *TemplateJSON, outputs []consensus.TxOut, tag []byte, now int64) (*consensus.Block, error) {
	if len(outputs) == 0 {
		return nil, ErrNoPayout
	}
	budget := t.CoinbaseValue
	treasury, payTreasury := consensus.TreasuryOutput(p, t.Height)
	if payTreasury {
		budget -= treasury.Amount
	}
	var total consensus.Amount
	for i := range outputs {
		o := &outputs[i]
		if o.Type != consensus.OutputTypeP2PK || len(o.Payload) != consensus.P2PKPayloadLen || o.Asset != consensus.NativeAsset || o.Amount < 0 {
			return nil, ErrNoPayout
		}
		var err error
		if total, err = consensus.AddAmounts(total, o.Amount); err != nil {
			return nil, err
		}
	}
	if total > budget {
		return nil, fmt.Errorf("template: coinbase outputs exceed the coinbase value")
	}
	prev, err := consensus.HashFromHex(t.PreviousBlockHash)
	if err != nil {
		return nil, fmt.Errorf("template: previousblockhash: %w", err)
	}
	var bits uint32
	if _, err := fmt.Sscanf(t.Bits, "%x", &bits); err != nil {
		return nil, fmt.Errorf("template: bits: %w", err)
	}
	if _, err := consensus.CompactToTarget(bits); err != nil {
		return nil, fmt.Errorf("template: bits: %w", err)
	}
	if len(tag) > consensus.MaxCoinbaseTagLen {
		tag = tag[:consensus.MaxCoinbaseTagLen]
	}
	outs := make([]consensus.TxOut, 0, len(outputs)+1)
	for _, o := range outputs {
		outs = append(outs, consensus.TxOut{Amount: o.Amount, Type: o.Type, Payload: append([]byte{}, o.Payload...)})
	}
	if payTreasury {
		outs = append(outs, treasury)
	}
	cb := &consensus.Tx{
		Version:    consensus.TxVersion,
		LockHeight: t.Height,
		Inputs:     []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs:    outs,
		Witnesses:  [][]byte{append([]byte{}, tag...)},
	}
	txs := []*consensus.Tx{cb}
	for i, raw := range t.Transactions {
		b, err := hex.DecodeString(raw.Data)
		if err != nil {
			return nil, fmt.Errorf("template: tx %d: %w", i, err)
		}
		tx, err := consensus.DecodeTx(b)
		if err != nil {
			return nil, fmt.Errorf("template: tx %d: %w", i, err)
		}
		txs = append(txs, tx)
	}
	blk := &consensus.Block{Txs: txs}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		return nil, err
	}
	tm := uint64(now)
	if tm < t.MinTime {
		tm = t.MinTime
	}
	if tm < t.CurTime {
		tm = t.CurTime
	}
	blk.Header = consensus.Header{
		Version:     t.Version,
		PrevHash:    prev,
		TxRoot:      txRoot,
		WitnessRoot: wRoot,
		Time:        tm,
		Bits:        bits,
	}
	return blk, nil
}
