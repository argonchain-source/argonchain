package consensus

import (
	"errors"
	"fmt"
)

// Assets (SPEC §3.5). A version-2 transaction can carry outputs denominated
// in an asset other than ARG and can create units of an asset with an
// issuance signed by the asset's issuer key. ARG itself is never issued this
// way: it only ever comes from the coinbase. Each asset is conserved on its
// own; fees are paid in ARG only.

const (
	// TxVersionAssets is the transaction version that carries asset ids and
	// issuances. Version-1 transactions are ARG only and keep their exact
	// pre-asset encoding, so every existing block, including genesis, is
	// unchanged.
	TxVersionAssets uint32 = 2

	// MaxTxIssuances is the number of issuances one transaction may carry.
	MaxTxIssuances = 1

	// MaxAssetSymbolLen bounds the ticker that is part of the asset id.
	MaxAssetSymbolLen = 8

	// MaxAssetAmount is the per-output and per-sum bound for non-ARG assets.
	// It leaves headroom below int64 so checked sums cannot wrap: 46 billion
	// units with eight decimals.
	MaxAssetAmount Amount = 1 << 62

	// IssuanceSigIndex is the sighash input index used by the issuance
	// signature. Input indexes are below MaxTxInputs, so it cannot collide.
	IssuanceSigIndex uint32 = 0xFFFFFFFF
)

// NativeAsset is the asset id of ARG: the zero hash.
var NativeAsset Hash

// Issuance creates Amount units of Asset. Asset must equal
// AssetID(Issuer, Symbol), which binds the id to the key that may issue it,
// and the transaction must carry a signature by Issuer over the issuance
// sighash as its last witness.
type Issuance struct {
	Asset  Hash
	Issuer [32]byte // x-only BIP-340 public key
	Symbol string   // 1..8 characters of A-Z and 0-9
	Amount Amount
}

var (
	ErrTxAssetVersion      = errors.New("tx: assets need transaction version 2")
	ErrTxAssetAmount       = errors.New("tx: asset amount outside [1, MAX_ASSET_AMOUNT]")
	ErrTxAssetNullData     = errors.New("tx: NULLDATA output must be ARG")
	ErrTxAssetUnbacked     = errors.New("tx: asset outputs exceed inputs plus issuance")
	ErrTxAssetOverflow     = errors.New("tx: asset sum exceeds MAX_ASSET_AMOUNT")
	ErrTxIssuanceCount     = errors.New("tx: at most one issuance per transaction")
	ErrTxIssuanceID        = errors.New("tx: issuance asset id does not match issuer and symbol")
	ErrTxIssuanceSymbol    = errors.New("tx: asset symbol must be 1-8 characters of A-Z and 0-9")
	ErrTxIssuanceCoinbase  = errors.New("tx: coinbase cannot issue assets or pay in assets")
	ErrTxIssuanceSignature = errors.New("tx: invalid issuance signature")
)

// AssetID derives the id of the asset that issuer may create under symbol.
func AssetID(issuer [32]byte, symbol string) Hash {
	var w Writer
	w.Raw(issuer[:])
	w.VarBytes([]byte(symbol))
	return TaggedHash(TagAsset, w.Bytes())
}

// ValidSymbol reports whether s is a legal asset symbol.
func ValidSymbol(s string) bool {
	if len(s) == 0 || len(s) > MaxAssetSymbolLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// AssetRange reports whether a is a legal non-ARG asset amount for an output.
func AssetRange(a Amount) bool { return a >= 1 && a <= MaxAssetAmount }

// AddAssetAmounts adds two non-ARG asset amounts with the asset bound.
func AddAssetAmounts(a, b Amount) (Amount, error) {
	if a < 0 || b < 0 || a > MaxAssetAmount || b > MaxAssetAmount {
		return 0, ErrTxAssetOverflow
	}
	s := a + b
	if s > MaxAssetAmount {
		return 0, ErrTxAssetOverflow
	}
	return s, nil
}

// CheckIssuance enforces the structural rules of one issuance.
func CheckIssuance(is *Issuance) error {
	if !ValidSymbol(is.Symbol) {
		return ErrTxIssuanceSymbol
	}
	if !AssetRange(is.Amount) {
		return ErrTxAssetAmount
	}
	if is.Asset != AssetID(is.Issuer, is.Symbol) || is.Asset == NativeAsset {
		return ErrTxIssuanceID
	}
	return nil
}

func (is *Issuance) write(w *Writer) {
	w.Hash(is.Asset)
	w.Raw(is.Issuer[:])
	w.VarBytes([]byte(is.Symbol))
	w.I64(int64(is.Amount))
}

func readIssuance(r *Reader) (Issuance, error) {
	var is Issuance
	var err error
	if is.Asset, err = r.Hash(); err != nil {
		return is, err
	}
	k, err := r.Raw(32)
	if err != nil {
		return is, err
	}
	copy(is.Issuer[:], k)
	sym, err := r.VarBytes(MaxAssetSymbolLen)
	if err != nil {
		return is, err
	}
	is.Symbol = string(sym)
	a, err := r.I64()
	if err != nil {
		return is, err
	}
	is.Amount = Amount(a)
	return is, nil
}

// assetTotals sums output (or spent) amounts per asset with checked
// arithmetic: ARG under MAX_MONEY, every other asset under MAX_ASSET_AMOUNT.
func assetTotals(outs []TxOut) (native Amount, others map[Hash]Amount, err error) {
	for i := range outs {
		o := &outs[i]
		if o.Asset == NativeAsset {
			if native, err = AddAmounts(native, o.Amount); err != nil {
				return 0, nil, err
			}
			continue
		}
		if others == nil {
			others = map[Hash]Amount{}
		}
		if others[o.Asset], err = AddAssetAmounts(others[o.Asset], o.Amount); err != nil {
			return 0, nil, fmt.Errorf("asset %s: %w", o.Asset, err)
		}
	}
	return native, others, nil
}
