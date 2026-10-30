package consensus

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// CheckOutput enforces SPEC §3.2 for a single ARG-only (version 1) output.
func CheckOutput(o *TxOut) error { return CheckOutputV(o, TxVersion) }

// CheckOutputV enforces SPEC §3.2 and §3.5 for an output of a transaction
// with the given version.
func CheckOutputV(o *TxOut, version uint32) error {
	if o.Asset != NativeAsset && version < TxVersionAssets {
		return ErrTxAssetVersion
	}
	switch o.Type {
	case OutputTypeP2PK:
		if len(o.Payload) != P2PKPayloadLen {
			return ErrTxBadPayloadLen
		}
		if o.Asset == NativeAsset {
			if !MoneyRange(o.Amount) {
				return ErrAmountOutOfRange
			}
		} else if !AssetRange(o.Amount) {
			return ErrTxAssetAmount
		}
	case OutputTypeNullData:
		if len(o.Payload) > MaxNullDataLen {
			return ErrTxBadPayloadLen
		}
		if o.Asset != NativeAsset {
			return ErrTxAssetNullData
		}
		if o.Amount != 0 {
			return ErrTxNullDataAmount
		}
	default:
		return ErrTxBadOutputType
	}
	return nil
}

// CheckTxStateless enforces SPEC §6.1 (everything that needs no UTXO set and
// no block height). It accepts both coinbase and non-coinbase transactions;
// coinbase-specific rules that need the height live in CheckCoinbase.
func CheckTxStateless(tx *Tx) error {
	if tx.Version != TxVersion && tx.Version != TxVersionAssets {
		return ErrTxVersion
	}
	if len(tx.Issuances) > 0 && !tx.HasAssets() {
		return ErrTxAssetVersion
	}
	if len(tx.Issuances) > MaxTxIssuances {
		return ErrTxIssuanceCount
	}
	if len(tx.Inputs) == 0 {
		return ErrTxNoInputs
	}
	if len(tx.Outputs) == 0 {
		return ErrTxNoOutputs
	}
	if len(tx.Inputs) > MaxTxInputs {
		return ErrTxTooManyInputs
	}
	if len(tx.Outputs) > MaxTxOutputs {
		return ErrTxTooManyOutputs
	}
	if len(tx.Witnesses) != len(tx.Inputs)+len(tx.Issuances) {
		return ErrTxWitnessCount
	}
	if tx.SerializedSize() > MaxTxSize {
		return ErrTxTooLarge
	}

	seen := make(map[OutPoint]struct{}, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if _, dup := seen[in.PrevOut]; dup {
			return ErrTxDuplicateInput
		}
		seen[in.PrevOut] = struct{}{}
	}

	for i := range tx.Outputs {
		if err := CheckOutputV(&tx.Outputs[i], tx.Version); err != nil {
			return fmt.Errorf("output %d: %w", i, err)
		}
	}
	// Checked sums: ARG under MAX_MONEY, each asset under MAX_ASSET_AMOUNT.
	if _, _, err := assetTotals(tx.Outputs); err != nil {
		return err
	}
	for i := range tx.Issuances {
		if err := CheckIssuance(&tx.Issuances[i]); err != nil {
			return fmt.Errorf("issuance %d: %w", i, err)
		}
	}

	if tx.IsCoinbase() {
		if len(tx.Witnesses[0]) > MaxCoinbaseTagLen {
			return ErrTxCoinbaseTagLen
		}
	} else {
		for _, in := range tx.Inputs {
			if in.PrevOut == NullOutPoint {
				return ErrTxNullPrevout
			}
		}
	}
	return nil
}

// CheckCoinbase enforces SPEC §3.3 shape rules that depend on the height.
// CheckTxStateless must also pass.
func CheckCoinbase(tx *Tx, height uint32) error {
	if !tx.IsCoinbase() {
		return ErrTxCoinbaseShape
	}
	if tx.LockHeight != height {
		return ErrTxCoinbaseHeight
	}
	// New coins are ARG only: a coinbase never issues or pays in assets.
	if len(tx.Issuances) > 0 {
		return ErrTxIssuanceCoinbase
	}
	for i := range tx.Outputs {
		if tx.Outputs[i].Asset != NativeAsset {
			return ErrTxIssuanceCoinbase
		}
	}
	return nil
}

// CheckTxInputs enforces SPEC §6.2 steps 1-3 against view at the given
// height: lock height, existence, maturity, checked input sum, non-negative
// fee. It returns the fee and the spent outputs (for signature checking).
// Not for coinbase transactions.
func CheckTxInputs(tx *Tx, view UTXOView, height uint32) (Amount, []TxOut, error) {
	if tx.LockHeight > height {
		return 0, nil, ErrTxLockHeight
	}
	spent := make([]TxOut, len(tx.Inputs))
	for i, txin := range tx.Inputs {
		coin, ok := view.Lookup(txin.PrevOut)
		if !ok {
			return 0, nil, fmt.Errorf("%w: %s", ErrTxMissingInput, txin.PrevOut)
		}
		if coin.Coinbase && (height < coin.Height || height-coin.Height < CoinbaseMaturity) {
			return 0, nil, ErrTxImmatureCoinbase
		}
		// A version-1 transaction cannot see asset ids, so it must not spend
		// asset coins: that would silently destroy them.
		if coin.Out.Asset != NativeAsset && !tx.HasAssets() {
			return 0, nil, ErrTxAssetVersion
		}
		spent[i] = coin.Out
	}
	in, inAssets, err := assetTotals(spent)
	if err != nil {
		return 0, nil, err
	}
	out, outAssets, err := assetTotals(tx.Outputs)
	if err != nil {
		return 0, nil, err
	}
	if out > in {
		return 0, nil, ErrTxFeeNegative
	}
	// Every asset is conserved on its own: outputs cannot exceed what was
	// spent plus what this transaction issues. Anything less is burned.
	for asset, o := range outAssets {
		avail := inAssets[asset]
		for i := range tx.Issuances {
			if tx.Issuances[i].Asset == asset {
				if avail, err = AddAssetAmounts(avail, tx.Issuances[i].Amount); err != nil {
					return 0, nil, err
				}
			}
		}
		if o > avail {
			return 0, nil, fmt.Errorf("%w: asset %s", ErrTxAssetUnbacked, asset)
		}
	}
	return in - out, spent, nil
}

// VerifyTxSignatures enforces SPEC §6.2 step 4: every input spending a P2PK
// output carries a valid 64-byte BIP-340 signature over sighash(tx, i).
func VerifyTxSignatures(chainTag Hash, tx *Tx, spent []TxOut) error {
	if len(tx.Witnesses) != len(tx.Inputs)+len(tx.Issuances) {
		return ErrTxWitnessCount
	}
	cache := NewSigHashCache(chainTag, tx, spent)
	for i := range tx.Issuances {
		is := &tx.Issuances[i]
		wit := tx.Witnesses[len(tx.Inputs)+i]
		if len(wit) != SchnorrSigLen {
			return fmt.Errorf("issuance %d: %w", i, ErrTxBadWitnessLen)
		}
		pub, err := schnorr.ParsePubKey(is.Issuer[:])
		if err != nil {
			return fmt.Errorf("issuance %d: %w", i, ErrTxBadPubKey)
		}
		sig, err := schnorr.ParseSignature(wit)
		if err != nil {
			return fmt.Errorf("issuance %d: %w", i, ErrTxIssuanceSignature)
		}
		msg := cache.IssuanceHash()
		if !sig.Verify(msg[:], pub) {
			return fmt.Errorf("issuance %d: %w", i, ErrTxIssuanceSignature)
		}
	}
	for i := range tx.Inputs {
		// Only P2PK coins can be in the UTXO set, but be explicit.
		if spent[i].Type != OutputTypeP2PK {
			return fmt.Errorf("input %d: %w", i, ErrTxBadOutputType)
		}
		wit := tx.Witnesses[i]
		if len(wit) != SchnorrSigLen {
			return fmt.Errorf("input %d: %w", i, ErrTxBadWitnessLen)
		}
		pub, err := schnorr.ParsePubKey(spent[i].Payload)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, ErrTxBadPubKey)
		}
		sig, err := schnorr.ParseSignature(wit)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, ErrTxBadSignature)
		}
		msg := cache.Hash(uint32(i))
		if !sig.Verify(msg[:], pub) {
			return fmt.Errorf("input %d: %w", i, ErrTxBadSignature)
		}
	}
	return nil
}

// CheckBlockCommitments reports whether a block's transactions are the ones
// its header commits to: both merkle roots match (SPEC §7). Proof of work
// covers the header only, so a body that fails this is not that block at
// all, whoever sent it. The error wraps ErrBlockMutated: the header must
// never be judged by such a body; only the sender is at fault.
func CheckBlockCommitments(blk *Block) error {
	if len(blk.Txs) == 0 {
		return fmt.Errorf("%w: %w", ErrBlockMutated, ErrBlockNoTxs)
	}
	txRoot, err := MerkleRoot(blk.TxIDs())
	if err != nil || txRoot != blk.Header.TxRoot {
		return fmt.Errorf("%w: %w", ErrBlockMutated, ErrBlockBadTxRoot)
	}
	wRoot, err := MerkleRoot(blk.WTxIDs())
	if err != nil || wRoot != blk.Header.WitnessRoot {
		return fmt.Errorf("%w: %w", ErrBlockMutated, ErrBlockBadWitnessRoot)
	}
	return nil
}

// ConnectBlock enforces SPEC §7 steps 2-6 for a block at height against an
// overlay of the parent's UTXO set, mutating the overlay on success. Header
// checks (§7.1) are done separately by CheckHeaderStateless/Context because
// they need the block index rather than the UTXO set.
//
// The body is checked against the header first (CheckBlockCommitments), so
// every other error is a verdict on the block the header names.
//
// On error the overlay may be partially mutated and must be discarded.
func ConnectBlock(p *Params, blk *Block, height uint32, view *Overlay) error {
	if err := CheckBlockCommitments(blk); err != nil {
		return err
	}
	if blk.SerializedSize() > MaxBlockSize {
		return ErrBlockTooLarge
	}

	// Coinbase placement.
	if !blk.Txs[0].IsCoinbase() {
		return ErrBlockFirstNotCoinbase
	}
	for i := 1; i < len(blk.Txs); i++ {
		if blk.Txs[i].IsCoinbase() {
			return ErrBlockExtraCoinbase
		}
	}

	// Duplicate txids (the roots matched above), duplicate spends across the block.
	txids := blk.TxIDs()
	seenTx := make(map[Hash]struct{}, len(txids))
	for _, id := range txids {
		if _, dup := seenTx[id]; dup {
			return ErrBlockDuplicateTx
		}
		seenTx[id] = struct{}{}
	}
	seenOut := map[OutPoint]struct{}{}
	for _, tx := range blk.Txs[1:] {
		for _, in := range tx.Inputs {
			if _, dup := seenOut[in.PrevOut]; dup {
				return ErrBlockDoubleSpend
			}
			seenOut[in.PrevOut] = struct{}{}
		}
	}

	// Coinbase stateless + shape.
	if err := CheckTxStateless(blk.Txs[0]); err != nil {
		return fmt.Errorf("coinbase: %w", err)
	}
	if err := CheckCoinbase(blk.Txs[0], height); err != nil {
		return err
	}
	if err := CheckTreasury(p, blk.Txs[0], height); err != nil {
		return err
	}

	// Each non-coinbase tx, sequentially against the evolving view.
	var fees Amount
	for i, tx := range blk.Txs[1:] {
		if err := CheckTxStateless(tx); err != nil {
			return fmt.Errorf("tx %d: %w", i+1, err)
		}
		fee, spent, err := CheckTxInputs(tx, view, height)
		if err != nil {
			return fmt.Errorf("tx %d: %w", i+1, err)
		}
		if err := VerifyTxSignatures(p.ChainTag, tx, spent); err != nil {
			return fmt.Errorf("tx %d: %w", i+1, err)
		}
		if fees, err = AddAmounts(fees, fee); err != nil {
			return err
		}
		applyTx(view, tx, txids[i+1], height, false)
	}

	// Coinbase value.
	allowed, err := AddAmounts(Subsidy(height), fees)
	if err != nil {
		return err
	}
	var cbOut Amount
	for i := range blk.Txs[0].Outputs {
		if cbOut, err = AddAmounts(cbOut, blk.Txs[0].Outputs[i].Amount); err != nil {
			return err
		}
	}
	if cbOut > allowed {
		return ErrBlockBadCoinbaseValue
	}

	// Genesis outputs never enter the UTXO set (SPEC §1).
	if height > 0 {
		applyTx(view, blk.Txs[0], txids[0], height, true)
	}
	return nil
}

// applyTx spends the inputs (non-coinbase) and adds the P2PK outputs.
// NULLDATA outputs never enter the set.
func applyTx(view *Overlay, tx *Tx, txid Hash, height uint32, coinbase bool) {
	if !coinbase {
		for _, in := range tx.Inputs {
			coin, _ := view.Lookup(in.PrevOut)
			view.Spend(in.PrevOut, coin)
		}
	}
	for i := range tx.Outputs {
		o := &tx.Outputs[i]
		if o.Type != OutputTypeP2PK {
			continue
		}
		view.Add(OutPoint{TxID: txid, Index: uint32(i)}, Coin{Out: *o, Height: height, Coinbase: coinbase})
	}
}
