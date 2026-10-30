package bridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

// EVM is what the bridge needs from one chain that holds a vault. RPCEVM
// talks to any JSON-RPC node; tests use an in-memory fake. The daemon holds
// one client per configured chain.
type EVM interface {
	// ChainID is the configured id of this chain (an RPCEVM checks that the
	// node agrees before it trusts anything the node says).
	ChainID() uint64
	Vault() [20]byte
	BlockNumber(ctx context.Context) (uint64, error)
	// Heads returns the latest block number and, with finalized, the number
	// of the block the node calls finalized (otherwise final = latest).
	Heads(ctx context.Context, finalized bool) (latest, final uint64, err error)
	// BlockHash returns the hash of the block at a height (0x-prefixed,
	// lowercase), or "" when the node has no block there.
	BlockHash(ctx context.Context, height uint64) (string, error)
	// Deposits returns the vault's Deposited events in a block range, each
	// with its block hash and log index.
	Deposits(ctx context.Context, fromBlock, toBlock uint64) ([]DepositEvent, error)
	// DepositReceipt returns a transaction's receipt with the vault's
	// Deposited events in it, or nil when the node does not have the
	// transaction mined.
	DepositReceipt(ctx context.Context, txHash string) (*DepositReceipt, error)
	// IsReleased reports whether the vault has already paid out a burn.
	IsReleased(ctx context.Context, burnID [32]byte) (bool, error)
	// Release submits release(token, to, amount, burnId, sigs) from the
	// client's gas key, or sends req.Replace again with higher fees, and
	// returns the transaction as sent (releasetx.go).
	Release(ctx context.Context, req ReleaseRequest) (*TxSend, error)
	Receipt(ctx context.Context, txHash string) (ReceiptStatus, uint64, error)
	// TxKnown reports whether the node knows a transaction (mined or in its
	// mempool).
	TxKnown(ctx context.Context, txHash string) (bool, error)
	// ConfirmedNonce is the number of transactions of addr mined so far.
	ConfirmedNonce(ctx context.Context, addr [20]byte) (uint64, error)
	// NativeBalance is addr's balance of the chain's native coin, in wei.
	NativeBalance(ctx context.Context, addr [20]byte) (*big.Int, error)
	// TokenDecimals reads token.decimals().
	TokenDecimals(ctx context.Context, token [20]byte) (int, error)
	// ReleaseLimits reads what stands between a release of token and the
	// vault paying it: paused(), maxRelease(token), periodAvailable(token).
	// A vault without one of those views reads as not paused or unlimited.
	ReleaseLimits(ctx context.Context, token [20]byte) (*ReleaseLimits, error)
	// Reserve is the balance of token the vault holds.
	Reserve(ctx context.Context, token [20]byte) (*big.Int, error)
	// Fees are the vault's deposit and release fees in basis points
	// (depositFeeBps(), releaseFeeBps()).
	Fees(ctx context.Context) (depositBps, releaseBps uint16, err error)
	// Obligations is the vault's obligations(token): the token units that back
	// issued uARG. What the vault holds beyond that is its fee surplus.
	Obligations(ctx context.Context, token [20]byte) (*big.Int, error)
}

// DepositReceipt is what a receipt says about a deposit transaction: its
// outcome, its block, and the vault's Deposited events in it.
type DepositReceipt struct {
	Status    ReceiptStatus
	Block     uint64
	BlockHash string
	Deposits  []DepositEvent // the vault's Deposited logs only, with their log indexes
}

// ReleaseLimits are the vault's release limits for one token as read now.
// MaxRelease and PeriodAvailable are nil when the vault sets none (or has
// no such view).
type ReleaseLimits struct {
	Paused          bool
	MaxRelease      *big.Int // gross per release
	PeriodAvailable *big.Int // gross still releasable in the current window
}

// SenderEVM is implemented by clients that send transactions: Sender is the
// address they send from (zero when read-only).
type SenderEVM interface {
	Sender() [20]byte
}

// RoleReader is implemented by clients that can read the vault's roles,
// used to check that the gas key is neither a guardian nor the owner.
type RoleReader interface {
	VaultRoles(ctx context.Context, addr [20]byte) (isGuardian bool, owner [20]byte, err error)
}

// VaultInfoReader is implemented by clients that can read how the vault
// numbers its deposits and whether its setup is over. A client without it
// reads as a vault whose deposits start at 1 and whose setup state is not
// known.
type VaultInfoReader interface {
	// FirstDepositID is the vault's firstDepositId(): the id of its first
	// deposit. A vault deployed before the view existed numbers from 1.
	FirstDepositID(ctx context.Context) (uint64, error)
	// SetupFinalized is the vault's finalized(): false while its owner may
	// still change tokens, fees and limits at once, without the timelock.
	// ok is false when the vault has no such view.
	SetupFinalized(ctx context.Context) (finalized, ok bool, err error)
}

// ReleaseLogReader is implemented by clients that can read the Released
// event of a mined release transaction. The daemon uses it, when available,
// to record the fee and the net amount the vault actually paid instead of
// the figures it computed from the fee rate.
type ReleaseLogReader interface {
	// ReleasedLog returns the vault's Released event in the transaction, or
	// nil if the transaction has no such event (not mined, or failed).
	ReleasedLog(ctx context.Context, txHash string) (*ReleaseEvent, error)
}

// ReceiptStatus is the outcome of a submitted transaction.
type ReceiptStatus int

const (
	ReceiptPending ReceiptStatus = iota
	ReceiptFailed
	ReceiptOK
)

// DepositEvent is one Deposited(id, token, from, amount, fee, to) log of a
// vault. Amount is the net the depositor is credited with (what becomes
// uARG); Fee is what the vault kept of the deposit.
type DepositEvent struct {
	ID        uint64
	Token     [20]byte // the ERC-20 deposited
	From      [20]byte
	Amount    *big.Int // token units credited, after the fee
	Fee       *big.Int // token units kept by the vault
	To        string   // Argonmine address as typed by the depositor
	Block     uint64
	BlockHash string // 0x-prefixed lowercase
	TxHash    string
	LogIndex  uint64
}

// sameDeposit reports whether two Deposited events say the same thing
// (id, token, depositor, amount, fee, destination), wherever they are.
func sameDeposit(a, b DepositEvent) bool {
	return a.ID == b.ID && a.Token == b.Token && a.From == b.From && a.Amount != nil && b.Amount != nil && a.Amount.Cmp(b.Amount) == 0 &&
		a.Fee != nil && b.Fee != nil && a.Fee.Cmp(b.Fee) == 0 && a.To == b.To
}

// ReleaseEvent is one Released(burnId, token, to, amount, fee) log of a
// vault: Amount is the gross value of the burn in token units, Fee what the
// vault kept; it paid Amount - Fee.
type ReleaseEvent struct {
	BurnID [32]byte
	Token  [20]byte
	To     [20]byte
	Amount *big.Int
	Fee    *big.Int
}

// ---- hashing and ABI helpers (exported for tests and tooling) ----

// Keccak256 is the EVM hash.
func Keccak256(parts ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Selector is the 4-byte function selector of a Solidity signature.
func Selector(sig string) [4]byte {
	h := Keccak256([]byte(sig))
	var s [4]byte
	copy(s[:], h[:4])
	return s
}

var (
	selRelease       = Selector("release(address,address,uint256,bytes32,bytes[])")
	selReleased      = Selector("released(bytes32)")
	selBalanceOf     = Selector("balanceOf(address)")
	selDepositFeeBps = Selector("depositFeeBps()")
	selReleaseFeeBps = Selector("releaseFeeBps()")
	selObligations   = Selector("obligations(address)")
	selFirstDeposit  = Selector("firstDepositId()")
	selFinalized     = Selector("finalized()")
	// SelApprove and SelDeposit are what the bridge page uses to build the
	// depositor's two transactions in the browser.
	SelApprove = Selector("approve(address,uint256)")
	SelDeposit = Selector("deposit(address,uint256,string)")
	// SelMint is the public faucet of a test stablecoin (contracts/MockUSDC.sol);
	// the page offers it for tokens configured with "mint": true.
	SelMint       = Selector("mint(address,uint256)")
	topicDeposit  = Keccak256([]byte("Deposited(uint256,address,address,uint256,uint256,string)"))
	topicReleased = Keccak256([]byte("Released(bytes32,address,address,uint256,uint256)"))
	maxUint256    = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func word(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func wordUint(v *big.Int) []byte {
	if v.Sign() < 0 || v.Cmp(maxUint256) > 0 {
		panic("evm: uint256 out of range")
	}
	return word(v.Bytes())
}

func padRight(b []byte) []byte {
	n := (len(b) + 31) / 32 * 32
	out := make([]byte, n)
	copy(out, b)
	return out
}

// EncodeReleaseCall ABI-encodes release(token, to, amount, burnId, sigs).
func EncodeReleaseCall(token, to [20]byte, amount *big.Int, burnID [32]byte, sigs [][]byte) []byte {
	var out []byte
	out = append(out, selRelease[:]...)
	out = append(out, word(token[:])...)
	out = append(out, word(to[:])...)
	out = append(out, wordUint(amount)...)
	out = append(out, burnID[:]...)
	out = append(out, wordUint(big.NewInt(5*32))...) // offset of the bytes[] (after 5 head words)
	out = append(out, wordUint(big.NewInt(int64(len(sigs))))...)
	offset := int64(len(sigs) * 32)
	var elems []byte
	for _, s := range sigs {
		out = append(out, wordUint(big.NewInt(offset))...)
		elem := append(wordUint(big.NewInt(int64(len(s)))), padRight(s)...)
		elems = append(elems, elem...)
		offset += int64(len(elem))
	}
	return append(out, elems...)
}

// EncodeReleasedCall ABI-encodes released(burnId).
func EncodeReleasedCall(burnID [32]byte) []byte {
	return append(append([]byte{}, selReleased[:]...), burnID[:]...)
}

// ReleaseDigest is the hash a guardian signs to approve a release: the
// EIP-191 prefix over keccak256(abi.encode(chainId, vault, token, to, amount, burnId)).
// It must match Vault.releaseDigest on that chain.
func ReleaseDigest(chainID uint64, vault, token, to [20]byte, amount *big.Int, burnID [32]byte) [32]byte {
	inner := Keccak256(wordUint(new(big.Int).SetUint64(chainID)), word(vault[:]), word(token[:]), word(to[:]), wordUint(amount), burnID[:])
	return Keccak256([]byte("\x19Ethereum Signed Message:\n32"), inner[:])
}

// SignDigest produces a 65-byte r||s||v (v = 27/28) Ethereum signature, in
// the one encoding the vault accepts (low s; see VaultSignatureEncoding).
func SignDigest(key *secp256k1.PrivateKey, digest [32]byte) []byte {
	compact := ecdsa.SignCompact(key, digest[:], false) // [v+27, r(32), s(32)]
	out := make([]byte, 65)
	copy(out[0:32], compact[1:33])
	copy(out[32:64], compact[33:65])
	out[64] = compact[0]
	return lowS(out)
}

// secpN is the order of secp256k1, and secpHalfN = N/2 (rounded down) the
// largest s that Vault._recover accepts.
var (
	secpN, _     = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)
	secpHalfN, _ = new(big.Int).SetString("7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0", 16)
)

// lowS returns an r||s||v signature with s at most N/2: a high s is replaced
// by N − s and v flipped between 27 and 28, which is the same signature for
// ecrecover. The signer above already produces low s; this keeps it so.
func lowS(sig []byte) []byte {
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		return sig
	}
	s := new(big.Int).SetBytes(sig[32:64])
	if s.Cmp(secpHalfN) <= 0 {
		return sig
	}
	out := append([]byte{}, sig...)
	new(big.Int).Sub(secpN, s).FillBytes(out[32:64])
	out[64] = 55 - sig[64] // 27 <-> 28
	return out
}

// VaultSignatureEncoding is why Vault._recover would refuse sig for its
// encoding alone (nil when it would not): exactly 65 bytes r||s||v, v 27 or
// 28 (0 and 1 are taken as 27 and 28), and s at most N/2. Anything else
// (the "compressed key" recovery codes 31 to 34, a high s) recovers to the
// signer off chain but makes release revert.
func VaultSignatureEncoding(sig []byte) error {
	if len(sig) != 65 {
		return errors.New("evm: signature must be 65 bytes")
	}
	if v := sig[64]; v != 0 && v != 1 && v != 27 && v != 28 {
		return fmt.Errorf("evm: signature recovery byte %d: the vault takes 27 or 28 (or 0 or 1)", v)
	}
	if new(big.Int).SetBytes(sig[32:64]).Cmp(secpHalfN) > 0 {
		return errors.New("evm: signature s is above half the curve order: the vault takes only low s")
	}
	return nil
}

// RecoverSigner returns the Ethereum address that produced sig over digest,
// for a signature the vault would accept: its encoding as Vault._recover
// requires (VaultSignatureEncoding), recovered as ecrecover does.
func RecoverSigner(digest [32]byte, sig []byte) ([20]byte, error) {
	var addr [20]byte
	if err := VaultSignatureEncoding(sig); err != nil {
		return addr, err
	}
	compact := make([]byte, 65)
	compact[0] = sig[64]
	if compact[0] < 27 {
		compact[0] += 27
	}
	copy(compact[1:], sig[:64])
	pub, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return addr, err
	}
	return AddressOf(pub), nil
}

// AddressOf derives the Ethereum address of a public key.
func AddressOf(pub *secp256k1.PublicKey) [20]byte {
	unc := pub.SerializeUncompressed() // 0x04 || X || Y
	h := Keccak256(unc[1:])
	var addr [20]byte
	copy(addr[:], h[12:])
	return addr
}

// ParseAddress parses a 0x-prefixed 20-byte hex address.
func ParseAddress(s string) ([20]byte, error) {
	var a [20]byte
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return a, errors.New("evm: address must be 20 hex bytes")
	}
	copy(a[:], b)
	return a, nil
}

// HexAddress renders an address as 0x-prefixed lowercase hex.
func HexAddress(a [20]byte) string { return "0x" + hex.EncodeToString(a[:]) }

// ParseTokenAmount parses a decimal stablecoin amount ("12.5") into token
// units with the given number of decimals.
func ParseTokenAmount(s string, decimals int) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	if len(frac) > decimals {
		return 0, fmt.Errorf("at most %d decimals", decimals)
	}
	for len(frac) < decimals {
		frac += "0"
	}
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return 0, nil
	}
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok || !v.IsUint64() {
		return 0, errors.New("not a valid amount")
	}
	return v.Uint64(), nil
}

// FormatTokenAmount renders token units as a decimal string.
func FormatTokenAmount(units uint64, decimals int) string {
	s := fmt.Sprintf("%0*d", decimals+1, units)
	if decimals == 0 {
		return s
	}
	return strings.TrimRight(strings.TrimRight(s[:len(s)-decimals]+"."+s[len(s)-decimals:], "0"), ".")
}

// DecodeDepositLog decodes a Deposited(id, token, from, amount, fee, to)
// log's topics (signature, id, token, from) and data (amount, fee, to).
func DecodeDepositLog(topics []string, data string) (DepositEvent, error) {
	var ev DepositEvent
	if len(topics) != 4 {
		return ev, errors.New("evm: Deposited log needs 4 topics")
	}
	t0, _ := hexBytes(topics[0])
	if !bytes.Equal(t0, topicDeposit[:]) {
		return ev, errors.New("evm: not a Deposited log")
	}
	id, err := hexBytes(topics[1])
	if err != nil || len(id) != 32 {
		return ev, errors.New("evm: bad id topic")
	}
	idv := new(big.Int).SetBytes(id)
	if !idv.IsUint64() {
		return ev, errors.New("evm: deposit id out of range")
	}
	ev.ID = idv.Uint64()
	token, err := hexBytes(topics[2])
	if err != nil || len(token) != 32 {
		return ev, errors.New("evm: bad token topic")
	}
	copy(ev.Token[:], token[12:])
	from, err := hexBytes(topics[3])
	if err != nil || len(from) != 32 {
		return ev, errors.New("evm: bad from topic")
	}
	copy(ev.From[:], from[12:])
	d, err := hexBytes(data)
	if err != nil || len(d) < 128 {
		return ev, errors.New("evm: bad Deposited data")
	}
	ev.Amount = new(big.Int).SetBytes(d[0:32])
	ev.Fee = new(big.Int).SetBytes(d[32:64])
	off := new(big.Int).SetBytes(d[64:96])
	if !off.IsInt64() || off.Int64()+32 > int64(len(d)) {
		return ev, errors.New("evm: bad string offset")
	}
	o := off.Int64()
	n := new(big.Int).SetBytes(d[o : o+32])
	if !n.IsInt64() || o+32+n.Int64() > int64(len(d)) || n.Int64() > 128 {
		return ev, errors.New("evm: bad string length")
	}
	ev.To = string(d[o+32 : o+32+n.Int64()])
	return ev, nil
}

// DecodeReleasedLog decodes a Released(burnId, token, to, amount, fee) log's
// topics (signature, burnId, token, to) and data (amount, fee).
func DecodeReleasedLog(topics []string, data string) (ReleaseEvent, error) {
	var ev ReleaseEvent
	if len(topics) != 4 {
		return ev, errors.New("evm: Released log needs 4 topics")
	}
	t0, _ := hexBytes(topics[0])
	if !bytes.Equal(t0, topicReleased[:]) {
		return ev, errors.New("evm: not a Released log")
	}
	id, err := hexBytes(topics[1])
	if err != nil || len(id) != 32 {
		return ev, errors.New("evm: bad burn id topic")
	}
	copy(ev.BurnID[:], id)
	token, err := hexBytes(topics[2])
	if err != nil || len(token) != 32 {
		return ev, errors.New("evm: bad token topic")
	}
	copy(ev.Token[:], token[12:])
	to, err := hexBytes(topics[3])
	if err != nil || len(to) != 32 {
		return ev, errors.New("evm: bad to topic")
	}
	copy(ev.To[:], to[12:])
	d, err := hexBytes(data)
	if err != nil || len(d) < 64 {
		return ev, errors.New("evm: bad Released data")
	}
	ev.Amount = new(big.Int).SetBytes(d[0:32])
	ev.Fee = new(big.Int).SetBytes(d[32:64])
	return ev, nil
}

// FeeOf is the fee a vault takes on amount at bps basis points, rounded
// down as the contract does: amount × bps / 10000.
func FeeOf(amount *big.Int, bps uint16) *big.Int {
	if amount == nil || amount.Sign() <= 0 || bps == 0 {
		return new(big.Int)
	}
	f := new(big.Int).Mul(amount, big.NewInt(int64(bps)))
	return f.Quo(f, big.NewInt(10000))
}

// ---- RLP and legacy (EIP-155) transaction signing ----

func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	return append(rlpLen(len(b), 0x80), b...)
}

func rlpUint(v *big.Int) []byte {
	if v.Sign() == 0 {
		return []byte{0x80}
	}
	return rlpBytes(v.Bytes())
}

func rlpList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	return append(rlpLen(len(payload), 0xc0), payload...)
}

func rlpLen(n int, offset byte) []byte {
	if n <= 55 {
		return []byte{offset + byte(n)}
	}
	be := new(big.Int).SetInt64(int64(n)).Bytes()
	return append([]byte{offset + 55 + byte(len(be))}, be...)
}

// SignLegacyTx builds and signs a type-0 transaction with EIP-155 replay
// protection. Returns the raw bytes for eth_sendRawTransaction.
func SignLegacyTx(key *secp256k1.PrivateKey, chainID uint64, nonce uint64, gasPrice *big.Int, gas uint64, to [20]byte, value *big.Int, data []byte) []byte {
	cid := new(big.Int).SetUint64(chainID)
	unsigned := rlpList(rlpUint(new(big.Int).SetUint64(nonce)), rlpUint(gasPrice), rlpUint(new(big.Int).SetUint64(gas)),
		rlpBytes(to[:]), rlpUint(value), rlpBytes(data), rlpUint(cid), rlpUint(big.NewInt(0)), rlpUint(big.NewInt(0)))
	h := Keccak256(unsigned)
	compact := ecdsa.SignCompact(key, h[:], false)
	recid := uint64(compact[0] - 27)
	v := new(big.Int).SetUint64(chainID*2 + 35 + recid)
	r := new(big.Int).SetBytes(compact[1:33])
	s := new(big.Int).SetBytes(compact[33:65])
	return rlpList(rlpUint(new(big.Int).SetUint64(nonce)), rlpUint(gasPrice), rlpUint(new(big.Int).SetUint64(gas)),
		rlpBytes(to[:]), rlpUint(value), rlpBytes(data), rlpUint(v), rlpUint(r), rlpUint(s))
}

// ---- JSON-RPC client ----

// RPCEVM is an EVM client over JSON-RPC. key is the gas key the client
// sends transactions from (releases, sweeps); nil for read-only use. It is
// never a guardian's signing key: Service.New refuses a client whose sender
// is the guardian's own EVM address.
type RPCEVM struct {
	url     string
	hc      *http.Client
	vault   [20]byte
	key     *secp256k1.PrivateKey
	chainID uint64
	gasCap  uint64

	mu       sync.Mutex
	verified bool // the node's eth_chainId matched chainID
}

// NewRPCEVM returns a client for the chain with the given id at url, sending
// transactions from key (nil: read-only). It does
// not touch the network: the first call checks that the node really is that
// chain (and fails with a clear error if not), so a chain whose RPC is down
// at startup does not stop the daemon.
func NewRPCEVM(url string, chainID uint64, vault [20]byte, key *secp256k1.PrivateKey) *RPCEVM {
	return &RPCEVM{url: url, hc: &http.Client{Timeout: 30 * time.Second}, vault: vault, key: key, chainID: chainID, gasCap: 400_000}
}

func (e *RPCEVM) Vault() [20]byte { return e.vault }
func (e *RPCEVM) ChainID() uint64 { return e.chainID }

// ensureChain checks once that the node serves the configured chain id, so a
// mistyped RPC URL cannot make the daemon scan the wrong chain or sign for it.
func (e *RPCEVM) ensureChain(ctx context.Context) error {
	e.mu.Lock()
	ok := e.verified
	e.mu.Unlock()
	if ok {
		return nil
	}
	raw, err := e.call(ctx, "eth_chainId")
	if err != nil {
		return err
	}
	id, err := hexQuantity(raw)
	if err != nil {
		return err
	}
	if id != e.chainID {
		return fmt.Errorf("evm: %s serves chain %d, configured as chain %d", e.url, id, e.chainID)
	}
	e.mu.Lock()
	e.verified = true
	e.mu.Unlock()
	return nil
}

// Sender is the address the client sends transactions from (zero if read-only).
func (e *RPCEVM) Sender() [20]byte {
	if e.key == nil {
		return [20]byte{}
	}
	return AddressOf(e.key.PubKey())
}

func (e *RPCEVM) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("evm: bad response from %s: %s", method, bytes.TrimSpace(data))
	}
	if r.Error != nil {
		e := &RPCError{Method: method, Code: r.Error.Code, Message: r.Error.Message}
		var d string
		if json.Unmarshal(r.Error.Data, &d) == nil {
			e.Data = d
		} else if len(r.Error.Data) > 0 && string(r.Error.Data) != "null" {
			e.Data = string(r.Error.Data)
		}
		return nil, e
	}
	return r.Result, nil
}

// RPCError is an error answer of a JSON-RPC node. Data carries the revert
// data of a failed eth_call or eth_estimateGas when the node returns it.
type RPCError struct {
	Method  string
	Code    int
	Message string
	Data    string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("evm: %s: %s (%d)", e.Method, e.Message, e.Code)
}

func hexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

func hexQuantity(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("evm: bad quantity %q", s)
	}
	return v.Uint64(), nil
}

func quantity(v uint64) string { return "0x" + new(big.Int).SetUint64(v).Text(16) }

func (e *RPCEVM) BlockNumber(ctx context.Context) (uint64, error) {
	if err := e.ensureChain(ctx); err != nil {
		return 0, err
	}
	raw, err := e.call(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	return hexQuantity(raw)
}

func (e *RPCEVM) Deposits(ctx context.Context, fromBlock, toBlock uint64) ([]DepositEvent, error) {
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	raw, err := e.call(ctx, "eth_getLogs", map[string]any{
		"fromBlock": quantity(fromBlock), "toBlock": quantity(toBlock),
		"address": HexAddress(e.vault), "topics": []string{"0x" + hex.EncodeToString(topicDeposit[:])},
	})
	if err != nil {
		return nil, err
	}
	var logs []rpcLog
	if err := json.Unmarshal(raw, &logs); err != nil {
		return nil, err
	}
	var out []DepositEvent
	for _, l := range logs {
		if l.Removed {
			continue
		}
		if addr, err := ParseAddress(l.Address); err == nil && addr != e.vault {
			continue
		}
		ev, err := DecodeDepositLog(l.Topics, l.Data)
		if err != nil {
			continue
		}
		l.fill(&ev)
		out = append(out, ev)
	}
	return out, nil
}

// rpcLog is one log as eth_getLogs and receipts return it.
type rpcLog struct {
	Address   string   `json:"address"`
	Topics    []string `json:"topics"`
	Data      string   `json:"data"`
	Block     string   `json:"blockNumber"`
	BlockHash string   `json:"blockHash"`
	TxHash    string   `json:"transactionHash"`
	LogIdx    string   `json:"logIndex"`
	Removed   bool     `json:"removed"`
}

func (l *rpcLog) fill(ev *DepositEvent) {
	b, _ := hexBytes(l.Block)
	ev.Block = new(big.Int).SetBytes(b).Uint64()
	li, _ := hexBytes(l.LogIdx)
	ev.LogIndex = new(big.Int).SetBytes(li).Uint64()
	ev.TxHash = strings.ToLower(l.TxHash)
	ev.BlockHash = strings.ToLower(l.BlockHash)
}

func (e *RPCEVM) ethCall(ctx context.Context, to [20]byte, data []byte) ([]byte, error) {
	raw, err := e.call(ctx, "eth_call", map[string]any{"to": HexAddress(to), "data": "0x" + hex.EncodeToString(data)}, "latest")
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return hexBytes(s)
}

func (e *RPCEVM) IsReleased(ctx context.Context, burnID [32]byte) (bool, error) {
	if err := e.ensureChain(ctx); err != nil {
		return false, err
	}
	b, err := e.ethCall(ctx, e.vault, EncodeReleasedCall(burnID))
	if err != nil {
		return false, err
	}
	if len(b) == 0 {
		return false, errors.New("evm: bad released() result")
	}
	return b[len(b)-1] == 1, nil
}

func (e *RPCEVM) Reserve(ctx context.Context, token [20]byte) (*big.Int, error) {
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	b, err := e.ethCall(ctx, token, append(append([]byte{}, selBalanceOf[:]...), word(e.vault[:])...))
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// callUint reads one uint256 view of the vault.
func (e *RPCEVM) callUint(ctx context.Context, data []byte, what string) (*big.Int, error) {
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	b, err := e.ethCall(ctx, e.vault, data)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("evm: the vault at %s has no %s", HexAddress(e.vault), what)
	}
	return new(big.Int).SetBytes(b), nil
}

func (e *RPCEVM) Fees(ctx context.Context) (uint16, uint16, error) {
	dep, err := e.callUint(ctx, selDepositFeeBps[:], "depositFeeBps()")
	if err != nil {
		return 0, 0, err
	}
	rel, err := e.callUint(ctx, selReleaseFeeBps[:], "releaseFeeBps()")
	if err != nil {
		return 0, 0, err
	}
	if !dep.IsUint64() || dep.Uint64() > 10000 || !rel.IsUint64() || rel.Uint64() > 10000 {
		return 0, 0, fmt.Errorf("evm: vault fees out of range: %s/%s bps", dep, rel)
	}
	return uint16(dep.Uint64()), uint16(rel.Uint64()), nil
}

func (e *RPCEVM) Obligations(ctx context.Context, token [20]byte) (*big.Int, error) {
	return e.callUint(ctx, append(append([]byte{}, selObligations[:]...), word(token[:])...), "obligations(address)")
}

// noSuchView reports whether a failed eth_call means the contract has no
// such function (it reverted, as a contract without a matching selector
// does), as opposed to the node or the network failing.
func noSuchView(err error) bool {
	var re *RPCError
	return errors.As(err, &re) && strings.Contains(strings.ToLower(re.Message+" "+re.Data), "revert")
}

// ErrNoVaultCode: there is no contract at the configured vault address (a
// mistyped address, or the vault not deployed yet). An eth_call to such an
// address answers "0x", which must never be read as a vault's answer.
var ErrNoVaultCode = errors.New("evm: there is no contract at the vault address")

// HasCode reports whether a contract is deployed at addr (eth_getCode at
// the latest block is not "0x").
func (e *RPCEVM) HasCode(ctx context.Context, addr [20]byte) (bool, error) {
	raw, err := e.call(ctx, "eth_getCode", HexAddress(addr), "latest")
	if err != nil {
		return false, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, err
	}
	b, err := hexBytes(s)
	if err != nil {
		return false, err
	}
	return len(b) > 0, nil
}

// FirstDepositID reads the vault's firstDepositId(); a vault without the
// view (deployed before it, so the call reverts) numbers its deposits from
// 1. The address must hold a contract (ErrNoVaultCode otherwise), and an
// empty answer from one is an error: neither is a vault's answer.
func (e *RPCEVM) FirstDepositID(ctx context.Context) (uint64, error) {
	if err := e.ensureChain(ctx); err != nil {
		return 0, err
	}
	code, err := e.HasCode(ctx, e.vault)
	if err != nil {
		return 0, err
	}
	if !code {
		return 0, fmt.Errorf("%w %s on chain %d", ErrNoVaultCode, HexAddress(e.vault), e.chainID)
	}
	b, err := e.ethCall(ctx, e.vault, selFirstDeposit[:])
	if err != nil {
		if noSuchView(err) {
			return 1, nil
		}
		return 0, err
	}
	if len(b) == 0 {
		return 0, fmt.Errorf("evm: the contract at %s answers firstDepositId() with nothing: it is not an Argonmine vault", HexAddress(e.vault))
	}
	if len(b) != 32 {
		return 0, fmt.Errorf("evm: bad firstDepositId() answer from the vault at %s", HexAddress(e.vault))
	}
	v := new(big.Int).SetBytes(b)
	if v.Sign() == 0 || !v.IsUint64() {
		return 0, fmt.Errorf("evm: the vault at %s answers firstDepositId() %s", HexAddress(e.vault), v)
	}
	return v.Uint64(), nil
}

// SetupFinalized reads the vault's finalized().
func (e *RPCEVM) SetupFinalized(ctx context.Context) (bool, bool, error) {
	if err := e.ensureChain(ctx); err != nil {
		return false, false, err
	}
	b, err := e.ethCall(ctx, e.vault, selFinalized[:])
	if err != nil {
		if noSuchView(err) {
			return false, false, nil
		}
		return false, false, err
	}
	if len(b) != 32 {
		return false, false, nil
	}
	return b[31] == 1, true, nil
}

// ReleasedLog reads the vault's Released event from a release transaction's
// receipt (nil if there is none yet).
func (e *RPCEVM) ReleasedLog(ctx context.Context, txHash string) (*ReleaseEvent, error) {
	raw, err := e.call(ctx, "eth_getTransactionReceipt", txHash)
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var rc struct {
		Logs []struct {
			Address string   `json:"address"`
			Topics  []string `json:"topics"`
			Data    string   `json:"data"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(raw, &rc); err != nil {
		return nil, err
	}
	for _, l := range rc.Logs {
		if addr, err := ParseAddress(l.Address); err != nil || addr != e.vault {
			continue
		}
		if ev, err := DecodeReleasedLog(l.Topics, l.Data); err == nil {
			return &ev, nil
		}
	}
	return nil, nil
}

// errWouldFail wraps the node's answer when eth_estimateGas says a
// transaction would revert: it is never sent.
var errWouldFail = errors.New("would fail")

type wouldFail struct{ err error }

func (w *wouldFail) Error() string        { return "would fail: " + w.err.Error() }
func (w *wouldFail) Unwrap() error        { return w.err }
func (w *wouldFail) Is(target error) bool { return target == errWouldFail }

// send signs and submits a legacy (EIP-155) transaction from the client's
// key (the gas key, never a guardian key) calling `to` with data: the pending nonce, the node's gas price plus
// 10 percent, and the node's gas estimate plus 20 percent. A failed
// estimate means the call would revert; it is returned wrapped so that
// errors.Is(err, errWouldFail) and errors.As(err, *RPCError) both hold, and
// nothing is sent.
func (e *RPCEVM) send(ctx context.Context, to [20]byte, data []byte) (string, error) {
	if e.key == nil {
		return "", errors.New("evm: no signing key")
	}
	from := HexAddress(e.Sender())
	nonceRaw, err := e.call(ctx, "eth_getTransactionCount", from, "pending")
	if err != nil {
		return "", err
	}
	nonce, err := hexQuantity(nonceRaw)
	if err != nil {
		return "", err
	}
	gpRaw, err := e.call(ctx, "eth_gasPrice")
	if err != nil {
		return "", err
	}
	gp, err := hexQuantity(gpRaw)
	if err != nil {
		return "", err
	}
	gasPrice := new(big.Int).SetUint64(gp)
	gasPrice.Add(gasPrice, new(big.Int).Div(gasPrice, big.NewInt(10))) // +10% so it confirms
	gas := e.gasCap
	if raw, err := e.call(ctx, "eth_estimateGas", map[string]any{"from": from, "to": HexAddress(to), "data": "0x" + hex.EncodeToString(data)}); err == nil {
		if est, err := hexQuantity(raw); err == nil && est > 0 {
			gas = est + est/5
		}
	} else {
		return "", &wouldFail{err}
	}
	rawTx := SignLegacyTx(e.key, e.chainID, nonce, gasPrice, gas, to, big.NewInt(0), data)
	res, err := e.call(ctx, "eth_sendRawTransaction", "0x"+hex.EncodeToString(rawTx))
	if err != nil {
		return "", err
	}
	var h string
	json.Unmarshal(res, &h)
	return h, nil
}

func (e *RPCEVM) Receipt(ctx context.Context, txHash string) (ReceiptStatus, uint64, error) {
	raw, err := e.call(ctx, "eth_getTransactionReceipt", txHash)
	if err != nil {
		return ReceiptPending, 0, err
	}
	if string(raw) == "null" {
		return ReceiptPending, 0, nil
	}
	var rc struct {
		Status string `json:"status"`
		Block  string `json:"blockNumber"`
	}
	if err := json.Unmarshal(raw, &rc); err != nil {
		return ReceiptPending, 0, err
	}
	b, _ := hexBytes(rc.Block)
	block := new(big.Int).SetBytes(b).Uint64()
	if rc.Status == "0x1" {
		return ReceiptOK, block, nil
	}
	return ReceiptFailed, block, nil
}
