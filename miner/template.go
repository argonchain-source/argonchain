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
// template, paying the whole coinbase value to payout. Remote miners and
// pools use this; the in-process miner uses BuildTemplate.
func AssembleFromTemplate(t *TemplateJSON, payout consensus.TxOut, tag []byte, now int64) (*consensus.Block, error) {
	if payout.Type != consensus.OutputTypeP2PK || len(payout.Payload) != consensus.P2PKPayloadLen {
		return nil, ErrNoPayout
	}
	return AssembleFromTemplateOutputs(t, []consensus.TxOut{{Amount: t.CoinbaseValue, Type: payout.Type, Payload: append([]byte{}, payout.Payload...)}}, tag, now)
}

// AssembleFromTemplateOutputs is AssembleFromTemplate with an explicit
// coinbase output list, for example a miner share and a referral share. The
// amounts must be P2PK ARG outputs summing to at most the template's
// coinbase value.
func AssembleFromTemplateOutputs(t *TemplateJSON, outputs []consensus.TxOut, tag []byte, now int64) (*consensus.Block, error) {
	if len(outputs) == 0 {
		return nil, ErrNoPayout
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
	if total > t.CoinbaseValue {
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
	outs := make([]consensus.TxOut, len(outputs))
	for i, o := range outputs {
		outs[i] = consensus.TxOut{Amount: o.Amount, Type: o.Type, Payload: append([]byte{}, o.Payload...)}
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
