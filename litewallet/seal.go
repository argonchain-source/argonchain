package litewallet

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"argonmine/consensus"
)

// The browser keeps the 24 words only sealed under the user's password:
// Argon2id derives the key, XChaCha20-Poly1305 encrypts, and the network
// and address are authenticated with the words, so a sealed phrase cannot
// be passed off as another wallet's. The parameters travel with the
// sealed phrase and are bounded on opening, so a tampered copy cannot make
// the page allocate gigabytes.

// MinPasswordLen is the shortest password Seal accepts.
const MinPasswordLen = 8

const (
	sealVersion   = 1
	sealTime      = 2
	sealMemoryKiB = 32 * 1024
	sealThreads   = 1
)

// Sealed is a phrase encrypted under a password, as the browser stores it.
type Sealed struct {
	Version   int    `json:"v"`
	Network   string `json:"network"`
	Address   string `json:"address"`
	Salt      string `json:"salt"`
	Time      uint32 `json:"t"`
	MemoryKiB uint32 `json:"m"`
	Threads   uint8  `json:"p"`
	Nonce     string `json:"nonce"`
	Data      string `json:"data"`
}

// ErrPassword is returned for a wrong password (or a damaged sealed phrase).
var ErrPassword = errors.New("wrong password")

func (s *Sealed) aad() []byte {
	return []byte("Argonmine/browser-wallet/v1|" + s.Network + "|" + s.Address)
}

func sealKey(password string, salt []byte, t, m uint32, p uint8) []byte {
	return argon2.IDKey([]byte(password), salt, t, m, p, chacha20poly1305.KeySize)
}

// Seal encrypts a 24-word phrase under password for network p. It also
// returns the account, so the caller need not derive it again.
func Seal(p *consensus.Params, mnemonic, password string) (*Sealed, *Account, error) {
	if len([]rune(password)) < MinPasswordLen {
		return nil, nil, errors.New("use a password of at least 8 characters")
	}
	m := NormalizePhrase(mnemonic)
	acct, err := FromMnemonic(p, m)
	if err != nil {
		return nil, nil, err
	}
	salt := make([]byte, 16)
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, errors.New("no secure randomness available")
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, errors.New("no secure randomness available")
	}
	s := &Sealed{Version: sealVersion, Network: p.Name, Address: acct.Address(), Salt: hex.EncodeToString(salt),
		Time: sealTime, MemoryKiB: sealMemoryKiB, Threads: sealThreads, Nonce: hex.EncodeToString(nonce)}
	aead, err := chacha20poly1305.NewX(sealKey(password, salt, s.Time, s.MemoryKiB, s.Threads))
	if err != nil {
		return nil, nil, err
	}
	s.Data = hex.EncodeToString(aead.Seal(nil, nonce, []byte(m), s.aad()))
	return s, acct, nil
}

// Open decrypts a sealed phrase and checks that it is the account it says
// it is, on network p.
func Open(p *consensus.Params, s *Sealed, password string) (string, *Account, error) {
	if s == nil || s.Version != sealVersion {
		return "", nil, errors.New("this wallet was saved by a newer version of the app")
	}
	if s.Network != p.Name {
		return "", nil, errors.New("this wallet belongs to another network")
	}
	if s.Time < 1 || s.Time > 10 || s.MemoryKiB < 8*1024 || s.MemoryKiB > 256*1024 || s.Threads < 1 || s.Threads > 8 {
		return "", nil, errors.New("the saved wallet is damaged")
	}
	salt, err1 := hex.DecodeString(s.Salt)
	nonce, err2 := hex.DecodeString(s.Nonce)
	data, err3 := hex.DecodeString(s.Data)
	if err1 != nil || err2 != nil || err3 != nil || len(salt) < 16 || len(nonce) != chacha20poly1305.NonceSizeX || len(data) > 4096 {
		return "", nil, errors.New("the saved wallet is damaged")
	}
	aead, err := chacha20poly1305.NewX(sealKey(password, salt, s.Time, s.MemoryKiB, s.Threads))
	if err != nil {
		return "", nil, err
	}
	plain, err := aead.Open(nil, nonce, data, s.aad())
	if err != nil {
		return "", nil, ErrPassword
	}
	m := string(plain)
	acct, err := FromMnemonic(p, m)
	if err != nil || acct.Address() != s.Address {
		return "", nil, errors.New("the saved wallet is damaged")
	}
	return m, acct, nil
}
