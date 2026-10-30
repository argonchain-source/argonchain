package consensus

import (
	"math/big"

	"golang.org/x/crypto/argon2"
)

// Proof-of-work parameters (SPEC §1, §4). Changing any of these is a hard fork.
const (
	ArgonTime      uint32 = 1
	ArgonMemoryKiB uint32 = 8 * 1024 // 8 MiB working set per attempt
	ArgonLanes     uint8  = 1
	ArgonKeyLen    uint32 = 32
)

// PowSalt is the fixed Argon2id salt. Argon2 requires a salt of at least 8
// bytes; it is a constant here because the header already contains all
// per-block entropy.
var PowSalt = []byte("Argonmine/pow/v1")

// PowHash returns the Argon2id digest of the serialized header. This is the
// value compared against the target. It is NOT the block identifier; see
// Header.Hash for that.
func PowHash(headerBytes []byte) Hash {
	var out Hash
	copy(out[:], argon2.IDKey(headerBytes, PowSalt, ArgonTime, ArgonMemoryKiB, ArgonLanes, ArgonKeyLen))
	return out
}

// HashToBig interprets h as a little-endian 256-bit unsigned integer
// (SPEC §2.1).
func HashToBig(h Hash) *big.Int {
	var be [HashSize]byte
	for i := 0; i < HashSize; i++ {
		be[i] = h[HashSize-1-i]
	}
	return new(big.Int).SetBytes(be[:])
}

// CheckProofOfWork verifies that the header's compact target is canonical and
// no easier than powLimit, and that its Argon2id hash does not exceed the
// target (SPEC §4). It costs one Argon2id evaluation.
func CheckProofOfWork(h *Header, powLimit *big.Int) error {
	target, err := CompactToTarget(h.Bits)
	if err != nil {
		return err
	}
	if target.Cmp(powLimit) > 0 {
		return ErrTargetAbovePowLimit
	}
	pow := HashToBig(PowHash(h.Serialize()))
	if pow.Cmp(target) > 0 {
		return ErrHighHash
	}
	return nil
}
