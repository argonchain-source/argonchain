// Package consensus implements the consensus rules of Argonmine
// exactly as written in docs/SPEC.md. Every exported function here is
// consensus-critical: a change in behaviour is a chain split.
package consensus

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashSize is the size in bytes of every hash in the protocol.
const HashSize = 32

// Hash is a 32-byte digest. Displayed as plain hex in natural byte order
// (no Bitcoin-style byte reversal).
type Hash [HashSize]byte

// Domain-separation tags (SPEC §2.1). Never reuse a tag for a second purpose.
const (
	TagBlock      = "Argonmine/block"
	TagTxID       = "Argonmine/txid"
	TagWTxID      = "Argonmine/wtxid"
	TagMerkleLeaf = "Argonmine/merkle/leaf"
	TagMerkleNode = "Argonmine/merkle/node"
	TagSighash    = "Argonmine/sighash"
	TagPrevouts   = "Argonmine/prevouts"
	TagAmounts    = "Argonmine/amounts"
	TagScripts    = "Argonmine/scripts"
	TagOutputs    = "Argonmine/outputs"
	TagAssets     = "Argonmine/assets"   // spent asset ids in the version-2 sighash
	TagIssuance   = "Argonmine/issuance" // issuances in the version-2 sighash
	TagAsset      = "Argonmine/asset"    // asset id = H(issuer key || symbol)
)

// TaggedHash computes SHA256(SHA256(tag) || SHA256(tag) || data...) as
// defined in BIP-340 and SPEC §2.1.
func TaggedHash(tag string, data ...[]byte) Hash {
	tagHash := sha256.Sum256([]byte(tag))
	h := sha256.New()
	h.Write(tagHash[:])
	h.Write(tagHash[:])
	for _, d := range data {
		h.Write(d)
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// String returns the lowercase hex encoding of the hash.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// IsZero reports whether every byte of the hash is zero.
func (h Hash) IsZero() bool { return h == Hash{} }

// HashFromHex parses a 64-character hex string.
func HashFromHex(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	if len(b) != HashSize {
		return h, ErrBadHashLength
	}
	copy(h[:], b)
	return h, nil
}
