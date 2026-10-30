package consensus

import "errors"

// Every consensus rejection is one of these sentinel errors (possibly
// wrapped with context). Tests assert on them with errors.Is.
var (
	// Encoding
	ErrBadHashLength      = errors.New("hash: wrong length")
	ErrVarintTooLong      = errors.New("varint: more than 9 bytes")
	ErrVarintNonCanonical = errors.New("varint: non-minimal encoding")
	ErrVarintTooLarge     = errors.New("varint: value exceeds 2^63-1")
	ErrUnexpectedEOF      = errors.New("decode: unexpected end of data")
	ErrTrailingBytes      = errors.New("decode: trailing bytes after structure")
	ErrLengthExceedsMax   = errors.New("decode: length field exceeds maximum")

	// Money
	ErrAmountOutOfRange = errors.New("amount: outside [0, MAX_MONEY]")
	ErrAmountOverflow   = errors.New("amount: sum overflows MAX_MONEY")

	// Compact target
	ErrCompactSignBit      = errors.New("compact: sign bit set")
	ErrCompactZero         = errors.New("compact: target is zero")
	ErrCompactOverflow     = errors.New("compact: target exceeds 256 bits")
	ErrCompactNonCanon     = errors.New("compact: non-canonical encoding")
	ErrTargetAbovePowLimit = errors.New("target: above proof-of-work limit")

	// Header
	ErrBadHeaderVersion = errors.New("header: version must be 1")
	ErrBadHeaderLength  = errors.New("header: must be exactly 120 bytes")
	ErrHighHash         = errors.New("header: proof of work hash above target")
	ErrBadBits          = errors.New("header: bits do not match expected difficulty")
	ErrTimeTooOld       = errors.New("header: timestamp not greater than median time past")
	ErrTimeTooNew       = errors.New("header: timestamp too far in the future")
	ErrTimeOverflow     = errors.New("header: timestamp exceeds 2^63-1")
	ErrBadPrevHash      = errors.New("header: prev_hash does not match parent")

	// Transaction (stateless)
	ErrTxVersion        = errors.New("tx: version must be 1 or 2")
	ErrTxNoInputs       = errors.New("tx: no inputs")
	ErrTxNoOutputs      = errors.New("tx: no outputs")
	ErrTxTooManyInputs  = errors.New("tx: too many inputs")
	ErrTxTooManyOutputs = errors.New("tx: too many outputs")
	ErrTxTooLarge       = errors.New("tx: exceeds max serialized size")
	ErrTxDuplicateInput = errors.New("tx: duplicate input outpoint")
	ErrTxWitnessCount   = errors.New("tx: witness count must equal input count")
	ErrTxBadOutputType  = errors.New("tx: unknown output type")
	ErrTxBadPayloadLen  = errors.New("tx: output payload has wrong length")
	ErrTxNullDataAmount = errors.New("tx: NULLDATA output amount must be 0")
	ErrTxNullPrevout    = errors.New("tx: non-coinbase input has null prevout")
	ErrTxCoinbaseShape  = errors.New("tx: coinbase must have exactly one null input")
	ErrTxCoinbaseTagLen = errors.New("tx: coinbase miner tag longer than 100 bytes")
	ErrTxCoinbaseHeight = errors.New("tx: coinbase lock_height must equal block height")

	// Transaction (stateful)
	ErrTxLockHeight       = errors.New("tx: lock_height not yet reached")
	ErrTxMissingInput     = errors.New("tx: input not found in UTXO set")
	ErrTxImmatureCoinbase = errors.New("tx: spends immature coinbase output")
	ErrTxFeeNegative      = errors.New("tx: outputs exceed inputs")
	ErrTxBadWitnessLen    = errors.New("tx: P2PK witness must be 64 bytes")
	ErrTxBadSignature     = errors.New("tx: invalid Schnorr signature")
	ErrTxBadPubKey        = errors.New("tx: invalid x-only public key")

	// Block
	ErrBlockTooLarge         = errors.New("block: exceeds max serialized size")
	ErrBlockNoTxs            = errors.New("block: no transactions")
	ErrBlockFirstNotCoinbase = errors.New("block: first tx is not coinbase")
	ErrBlockExtraCoinbase    = errors.New("block: more than one coinbase")
	ErrBlockBadTxRoot        = errors.New("block: tx_root mismatch")
	ErrBlockBadWitnessRoot   = errors.New("block: witness_root mismatch")
	ErrBlockDuplicateTx      = errors.New("block: duplicate txid")
	// ErrBlockMutated: the body is not the one the header commits to (its
	// merkle roots differ, or it has no transactions). It says nothing about
	// the header, which may be a valid block someone altered in transit.
	ErrBlockMutated          = errors.New("block: body does not match its header")
	ErrBlockDoubleSpend      = errors.New("block: outpoint spent twice in block")
	ErrBlockBadCoinbaseValue = errors.New("block: coinbase pays more than subsidy + fees")
	ErrBlockTreasury         = errors.New("block: coinbase pays the treasury less than its share of the subsidy")
	ErrBlockUnknownParent    = errors.New("block: parent not known")

	// Merkle
	ErrMerkleEmpty = errors.New("merkle: empty leaf set")
)
