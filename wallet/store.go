package wallet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"argonmine/consensus"
)

// The wallet file is JSON. The seed and mnemonic are encrypted with
// XChaCha20-Poly1305 under an Argon2id-derived key; the public bookkeeping
// (addresses, UTXOs, history) is stored in clear so balances can be shown
// without the password.

const (
	fileVersion  = 1
	kdfTime      = 3
	kdfMemoryKiB = 64 * 1024
	kdfThreads   = 4
)

var (
	ErrWalletExists   = errors.New("wallet: file already exists")
	ErrWrongPassword  = errors.New("wallet: wrong password")
	ErrLocked         = errors.New("wallet: locked")
	ErrWrongNetwork   = errors.New("wallet: file belongs to a different network")
	ErrUnsupportedVer = errors.New("wallet: unsupported file version")
)

// UTXORecord is an unspent output owned by the wallet. Asset is the hex id
// of a non-ARG asset (SPEC 3.5); empty for ARG. Asset units never count
// toward the ARG balance and never pay fees.
type UTXORecord struct {
	TxID     string           `json:"txid"`
	Index    uint32           `json:"index"`
	Amount   consensus.Amount `json:"amount"`
	Address  string           `json:"address"`
	Height   uint32           `json:"height"`
	Coinbase bool             `json:"coinbase"`
	Asset    string           `json:"asset,omitempty"`
	LockedBy string           `json:"locked_by,omitempty"` // txid of our unconfirmed spend
}

// AssetInfo is what the wallet knows about an asset it has seen issued.
type AssetInfo struct {
	Symbol string `json:"symbol"`
	Issuer string `json:"issuer"` // x-only public key, hex
}

// HistoryRecord summarises one transaction touching the wallet. The ARG
// figures exclude asset units; Assets holds the net change per asset id
// (positive = received, negative = sent or burned).
type HistoryRecord struct {
	TxID     string                      `json:"txid"`
	Height   uint32                      `json:"height"` // 0 = unconfirmed
	Time     uint64                      `json:"time"`
	Received consensus.Amount            `json:"received"`
	Sent     consensus.Amount            `json:"sent"`
	Fee      consensus.Amount            `json:"fee"`
	Net      consensus.Amount            `json:"net"`
	Assets   map[string]consensus.Amount `json:"assets,omitempty"`
}

// AddrMeta describes a derived address.
type AddrMeta struct {
	Change bool   `json:"change"`
	Index  uint32 `json:"index"`
	Used   bool   `json:"used"`
}

// State is the public bookkeeping.
type State struct {
	NextReceive  uint32                `json:"next_receive"`
	NextChange   uint32                `json:"next_change"`
	SyncedHeight int64                 `json:"synced_height"` // -1 = never synced
	SyncedHash   string                `json:"synced_hash"`
	Addresses    map[string]AddrMeta   `json:"addresses"`
	UTXOs        map[string]UTXORecord `json:"utxos"` // "txid:index"
	History      []HistoryRecord       `json:"history"`
	Assets       map[string]AssetInfo  `json:"assets,omitempty"` // asset id -> symbol, issuer
}

type kdfParams struct {
	Salt      string `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

type walletFile struct {
	Version    int       `json:"version"`
	Network    string    `json:"network"`
	KDF        kdfParams `json:"kdf"`
	Nonce      string    `json:"nonce"`
	Ciphertext string    `json:"ciphertext"`
	State      State     `json:"state"`
}

type secretBlob struct {
	Mnemonic string `json:"mnemonic"`
	Seed     string `json:"seed"`
}

// Store is the on-disk wallet file.
type Store struct {
	path   string
	file   walletFile
	secret *secretBlob
}

func deriveKey(password string, k kdfParams) ([]byte, error) {
	salt, err := hex.DecodeString(k.Salt)
	if err != nil || len(salt) < 16 {
		return nil, errors.New("wallet: bad kdf salt")
	}
	return argon2.IDKey([]byte(password), salt, k.Time, k.MemoryKiB, k.Threads, chacha20poly1305.KeySize), nil
}

// CreateStore writes a new wallet file. passphrase is the optional BIP-39
// passphrase (part of the key derivation); password protects the file.
func CreateStore(path, network, mnemonic, passphrase, password string) (*Store, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, ErrWalletExists
	}
	seed, err := SeedFromMnemonic(mnemonic, passphrase)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	kdf := kdfParams{Salt: hex.EncodeToString(salt), Time: kdfTime, MemoryKiB: kdfMemoryKiB, Threads: kdfThreads}
	key, err := deriveKey(password, kdf)
	if err != nil {
		return nil, err
	}
	secret := &secretBlob{Mnemonic: mnemonic, Seed: hex.EncodeToString(seed)}
	plain, _ := json.Marshal(secret)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	rand.Read(nonce)
	ct := aead.Seal(nil, nonce, plain, []byte(network))
	s := &Store{path: path, secret: secret}
	s.file = walletFile{
		Version:    fileVersion,
		Network:    network,
		KDF:        kdf,
		Nonce:      hex.EncodeToString(nonce),
		Ciphertext: hex.EncodeToString(ct),
		State:      State{SyncedHeight: -1, Addresses: map[string]AddrMeta{}, UTXOs: map[string]UTXORecord{}},
	}
	if err := s.Save(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenStore loads a wallet file (locked).
func OpenStore(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path}
	if err := json.Unmarshal(data, &s.file); err != nil {
		return nil, err
	}
	if s.file.Version != fileVersion {
		return nil, ErrUnsupportedVer
	}
	if s.file.State.Addresses == nil {
		s.file.State.Addresses = map[string]AddrMeta{}
	}
	if s.file.State.UTXOs == nil {
		s.file.State.UTXOs = map[string]UTXORecord{}
	}
	return s, nil
}

// Unlock decrypts the seed with password.
func (s *Store) Unlock(password string) error {
	key, err := deriveKey(password, s.file.KDF)
	if err != nil {
		return err
	}
	nonce, err := hex.DecodeString(s.file.Nonce)
	if err != nil {
		return err
	}
	ct, err := hex.DecodeString(s.file.Ciphertext)
	if err != nil {
		return err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	plain, err := aead.Open(nil, nonce, ct, []byte(s.file.Network))
	if err != nil {
		return ErrWrongPassword
	}
	var secret secretBlob
	if err := json.Unmarshal(plain, &secret); err != nil {
		return err
	}
	s.secret = &secret
	return nil
}

// Locked reports whether the seed is unavailable.
func (s *Store) Locked() bool { return s.secret == nil }

// Seed returns the BIP-39 seed (nil when locked).
func (s *Store) Seed() []byte {
	if s.secret == nil {
		return nil
	}
	b, _ := hex.DecodeString(s.secret.Seed)
	return b
}

// Mnemonic returns the mnemonic ("" when locked).
func (s *Store) Mnemonic() string {
	if s.secret == nil {
		return ""
	}
	return s.secret.Mnemonic
}

// Network returns the network name the wallet was created for.
func (s *Store) Network() string { return s.file.Network }

// State returns the mutable public state.
func (s *Store) State() *State { return &s.file.State }

// Path returns the file path.
func (s *Store) Path() string { return s.path }

// Save writes the file atomically.
func (s *Store) Save() error {
	data, err := json.MarshalIndent(&s.file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
