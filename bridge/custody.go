// Package bridge implements the guardian daemon of the federated bridge
// (docs/BRIDGE.md): deposit scanning on this chain, guardian-signed minting
// on an EVM chain, and MuSig2 n-of-n releases from the custody key.
package bridge

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"

	"argonmine/consensus"
	"argonmine/wallet"
)

// Custody is the n-of-n MuSig2 (BIP-327) aggregate of the guardians' keys.
// The aggregate's x-only form is the custody address on this chain, so a
// release is an ordinary P2PK spend: nothing in consensus knows about the
// bridge.
type Custody struct {
	hrp     string
	signers []*btcec.PublicKey // sorted as MuSig2 sorts them
	agg     *btcec.PublicKey
	xonly   []byte
	address string
	local   *btcec.PrivateKey // nil for an observer
}

// NewCustody builds the custody from the guardians' 33-byte compressed public
// keys. local, if set, must be one of them.
func NewCustody(hrp string, guardianKeys [][]byte, local *btcec.PrivateKey) (*Custody, error) {
	if len(guardianKeys) == 0 {
		return nil, errors.New("custody: no guardian keys")
	}
	keys := make([]*btcec.PublicKey, 0, len(guardianKeys))
	seen := map[string]bool{}
	for i, b := range guardianKeys {
		if len(b) != 33 {
			return nil, fmt.Errorf("custody: guardian key %d must be 33 bytes compressed", i)
		}
		if seen[string(b)] {
			return nil, fmt.Errorf("custody: duplicate guardian key %d", i)
		}
		seen[string(b)] = true
		k, err := btcec.ParsePubKey(b)
		if err != nil {
			return nil, fmt.Errorf("custody: guardian key %d: %w", i, err)
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i].SerializeCompressed(), keys[j].SerializeCompressed()) < 0
	})
	if local != nil {
		lp := local.PubKey().SerializeCompressed()
		if !seen[string(lp)] {
			return nil, errors.New("custody: local key is not one of the guardians")
		}
	}
	agg, _, _, err := musig2.AggregateKeys(keys, false)
	if err != nil {
		return nil, fmt.Errorf("custody: aggregate: %w", err)
	}
	xonly := schnorr.SerializePubKey(agg.FinalKey)
	addr, err := wallet.EncodeAddress(hrp, consensus.OutputTypeP2PK, xonly)
	if err != nil {
		return nil, err
	}
	return &Custody{hrp: hrp, signers: keys, agg: agg.FinalKey, xonly: xonly, address: addr, local: local}, nil
}

// Address is the custody deposit address.
func (c *Custody) Address() string { return c.address }

// XOnly is the 32-byte aggregate key (the P2PK payload).
func (c *Custody) XOnly() []byte { return append([]byte{}, c.xonly...) }

// Output builds a P2PK output paying custody (used for change).
func (c *Custody) Output(amount consensus.Amount) consensus.TxOut {
	return consensus.TxOut{Amount: amount, Type: consensus.OutputTypeP2PK, Payload: c.XOnly()}
}

// Signers returns the sorted guardian keys as hex.
func (c *Custody) Signers() []string {
	out := make([]string, len(c.signers))
	for i, k := range c.signers {
		out[i] = hex.EncodeToString(k.SerializeCompressed())
	}
	return out
}

// N is the number of guardians.
func (c *Custody) N() int { return len(c.signers) }

// CanSign reports whether this instance holds a guardian key.
func (c *Custody) CanSign() bool { return c.local != nil }

// LocalKey is this guardian's own 33-byte compressed public key, or nil for
// an observer.
func (c *Custody) LocalKey() []byte {
	if c.local == nil {
		return nil
	}
	return c.local.PubKey().SerializeCompressed()
}

// IsSigner reports whether key (33 bytes, compressed) is one of the guardians.
func (c *Custody) IsSigner(key []byte) bool {
	for _, k := range c.signers {
		if bytes.Equal(k.SerializeCompressed(), key) {
			return true
		}
	}
	return false
}

// SignAuth signs a 32-byte message with this guardian's own key (BIP-340).
// The leader authenticates its requests to the other guardians with it; the
// message is a tagged hash of its own (auth.go), so it can never be mistaken
// for a transaction signature hash.
func (c *Custody) SignAuth(msg [32]byte) ([]byte, error) {
	if c.local == nil {
		return nil, errors.New("custody: this instance has no signing key")
	}
	sig, err := schnorr.Sign(c.local, msg[:])
	if err != nil {
		return nil, err
	}
	return sig.Serialize(), nil
}

// PaysCustody reports whether o is a P2PK output to the custody key.
func (c *Custody) PaysCustody(o *consensus.TxOut) bool {
	return o.Type == consensus.OutputTypeP2PK && bytes.Equal(o.Payload, c.xonly)
}

// Session is one MuSig2 signing session for one message (one input of a
// release transaction). Each guardian creates its own Session per input;
// nonces are exchanged, then partial signatures.
type Session struct {
	sess *musig2.Session
}

// NewSession starts a session for this guardian. Every session must be used
// for exactly one message: nonce reuse would leak the key.
func (c *Custody) NewSession() (*Session, error) {
	if c.local == nil {
		return nil, errors.New("custody: this instance has no signing key")
	}
	ctx, err := musig2.NewContext(c.local, false, musig2.WithKnownSigners(c.signers))
	if err != nil {
		return nil, err
	}
	s, err := ctx.NewSession()
	if err != nil {
		return nil, err
	}
	return &Session{sess: s}, nil
}

// PublicNonce is this guardian's 66-byte public nonce to share.
func (s *Session) PublicNonce() []byte {
	n := s.sess.PublicNonce()
	return n[:]
}

// RegisterNonce adds another guardian's public nonce. Returns true once all
// other nonces are known.
func (s *Session) RegisterNonce(nonce []byte) (bool, error) {
	if len(nonce) != musig2.PubNonceSize {
		return false, errors.New("custody: bad nonce size")
	}
	var n [musig2.PubNonceSize]byte
	copy(n[:], nonce)
	return s.sess.RegisterPubNonce(n)
}

// Sign produces this guardian's 32-byte partial signature over msg. All
// other nonces must be registered first.
func (s *Session) Sign(msg [32]byte) ([]byte, error) {
	ps, err := s.sess.Sign(msg)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := ps.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Combine adds another guardian's partial signature. Returns true once the
// final signature is available.
func (s *Session) Combine(partial []byte) (bool, error) {
	var ps musig2.PartialSignature
	if err := ps.Decode(bytes.NewReader(partial)); err != nil {
		return false, err
	}
	return s.sess.CombineSig(&ps)
}

// Final returns the 64-byte BIP-340 signature once all partials are combined.
func (s *Session) Final() ([]byte, error) {
	sig := s.sess.FinalSig()
	if sig == nil {
		return nil, errors.New("custody: signature not complete")
	}
	return sig.Serialize(), nil
}

// ParseGuardianKeys decodes hex-encoded compressed public keys.
func ParseGuardianKeys(hexKeys []string) ([][]byte, error) {
	out := make([][]byte, 0, len(hexKeys))
	for _, h := range hexKeys {
		b, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("guardian key %q: %w", h, err)
		}
		out = append(out, b)
	}
	return out, nil
}
