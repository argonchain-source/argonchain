package wallet

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/tyler-smith/go-bip39"

	"argonmine/consensus"
)

// Key derivation follows BIP-39 (mnemonic -> seed) and BIP-32 with a
// BIP-86-style path, since our outputs are x-only keys:
//
//	m / 86' / coin_type' / account' / change / index
const (
	Purpose      uint32 = 86
	CoinTypeMain uint32 = 9333 // placeholder until a SLIP-44 number is registered
	CoinTypeTest uint32 = 1    // SLIP-44 convention for all test networks
)

var ErrBadMnemonic = errors.New("wallet: invalid mnemonic")

// CoinTypeFor returns the SLIP-44 coin type used for a network.
func CoinTypeFor(p *consensus.Params) uint32 {
	if p == consensus.MainNet {
		return CoinTypeMain
	}
	return CoinTypeTest
}

// NewMnemonic generates a 24-word BIP-39 mnemonic (256 bits of entropy).
func NewMnemonic() (string, error) {
	ent, err := bip39.NewEntropy(256)
	if err != nil {
		return "", err
	}
	return bip39.NewMnemonic(ent)
}

// ValidateMnemonic checks the word list and checksum.
func ValidateMnemonic(m string) bool { return bip39.IsMnemonicValid(m) }

// SeedFromMnemonic derives the 64-byte BIP-39 seed.
func SeedFromMnemonic(m, passphrase string) ([]byte, error) {
	if !bip39.IsMnemonicValid(m) {
		return nil, ErrBadMnemonic
	}
	return bip39.NewSeed(m, passphrase), nil
}

// KeyChain derives keys for one account of one network.
type KeyChain struct {
	account *hdkeychain.ExtendedKey // m/86'/coin'/account'
}

// NewKeyChain builds the account-level key from a seed.
func NewKeyChain(seed []byte, coinType, account uint32) (*KeyChain, error) {
	// The chaincfg params only select serialization version bytes, which we
	// never serialize; any value works.
	master, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}
	k := master
	for _, i := range []uint32{Purpose, coinType, account} {
		if k, err = k.Derive(hdkeychain.HardenedKeyStart + i); err != nil {
			return nil, err
		}
	}
	return &KeyChain{account: k}, nil
}

// Key is a derived signing key.
type Key struct {
	Priv   *btcec.PrivateKey
	XOnly  []byte // 32-byte x-only public key = P2PK payload
	Change bool
	Index  uint32
}

// Derive returns the key at change/index.
func (kc *KeyChain) Derive(change bool, index uint32) (*Key, error) {
	c := uint32(0)
	if change {
		c = 1
	}
	k, err := kc.account.Derive(c)
	if err != nil {
		return nil, err
	}
	if k, err = k.Derive(index); err != nil {
		return nil, err
	}
	priv, err := k.ECPrivKey()
	if err != nil {
		return nil, err
	}
	return &Key{Priv: priv, XOnly: schnorr.SerializePubKey(priv.PubKey()), Change: change, Index: index}, nil
}

// Output builds a P2PK output paying this key.
func (k *Key) Output(amount consensus.Amount) consensus.TxOut {
	return consensus.TxOut{Amount: amount, Type: consensus.OutputTypeP2PK, Payload: append([]byte{}, k.XOnly...)}
}

// Address renders the key's address for the given network.
func (k *Key) Address(hrp string) string {
	a, err := EncodeAddress(hrp, consensus.OutputTypeP2PK, k.XOnly)
	if err != nil {
		panic(err) // 32-byte key cannot fail
	}
	return a
}

// Sign fills witness i of tx with a BIP-340 signature over sighash(tx, i).
// spent lists the outputs consumed by every input, in order.
func (k *Key) Sign(chainTag consensus.Hash, tx *consensus.Tx, spent []consensus.TxOut, i int) error {
	if i < 0 || i >= len(tx.Inputs) {
		return fmt.Errorf("wallet: input %d out of range", i)
	}
	msg := consensus.SigHash(chainTag, tx, spent, uint32(i))
	sig, err := schnorr.Sign(k.Priv, msg[:])
	if err != nil {
		return err
	}
	tx.Witnesses[i] = sig.Serialize()
	return nil
}
