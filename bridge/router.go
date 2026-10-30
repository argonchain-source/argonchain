package bridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Deposit addresses (contracts/DepositRouter.sol, docs/BRIDGE.md "Deposit
// addresses"): every Argonmine address has one deposit address per chain,
// the CREATE2 address of a Forwarder the chain's DepositRouter deploys with
// salt keccak256(bytes(to)). A plain ERC-20 transfer there, from any wallet
// or exchange, is moved into the vault by router.sweep(token, to), which
// anyone may call; the vault then emits the usual Deposited event.

// RouterEVM is what the leader needs from a chain that has a DepositRouter.
// RPCEVM implements it; a chain whose client does not is refused a router.
type RouterEVM interface {
	// ForwarderCodeHash reads the router's forwarderCodeHash(): the init code
	// hash in every deposit address of that router.
	ForwarderCodeHash(ctx context.Context, router [20]byte) ([32]byte, error)
	// RouterVault reads the router's vault(): where its sweeps deposit.
	RouterVault(ctx context.Context, router [20]byte) ([20]byte, error)
	// DepositAddressView calls the router's depositAddress(to) view, used once
	// per chain to cross-check DepositAddress.
	DepositAddressView(ctx context.Context, router [20]byte, to string) ([20]byte, error)
	// Transfers returns token's Transfer events to any of recipients in a
	// block range.
	Transfers(ctx context.Context, token [20]byte, recipients [][20]byte, fromBlock, toBlock uint64) ([]TransferEvent, error)
	// BalanceOf reads token.balanceOf(holder) at a block (0 = latest).
	BalanceOf(ctx context.Context, token, holder [20]byte, block uint64) (*big.Int, error)
	// Sweep submits router.sweep(token, to) and returns the transaction hash.
	// A sweep the router or the vault would refuse is not sent: the error is
	// ErrSweepBelowMinimum or ErrNothingToSweep when that is the reason.
	Sweep(ctx context.Context, router, token [20]byte, to string) (string, error)
	// SweptLog returns the router's Swept event in a mined sweep transaction,
	// or nil if it has none.
	SweptLog(ctx context.Context, router [20]byte, txHash string) (*SweptEvent, error)
}

// TransferEvent is one ERC-20 Transfer(from, to, value) log.
type TransferEvent struct {
	Token    [20]byte
	From     [20]byte
	To       [20]byte
	Amount   *big.Int
	Block    uint64
	TxHash   string
	LogIndex uint64
}

// SweptEvent is the router's Swept(token, forwarder, amount, depositId, to):
// Amount is the gross handed to the vault, DepositID the vault's deposit id.
type SweptEvent struct {
	Token     [20]byte
	Forwarder [20]byte
	Amount    *big.Int
	DepositID uint64
	To        string
}

var (
	// ErrSweepBelowMinimum: the vault refuses the sweep because the balance
	// is below its minimum deposit (AmountTooSmall). Not a failure: the
	// funds stay at the deposit address until more arrives.
	ErrSweepBelowMinimum = errors.New("below the vault's minimum deposit")
	// ErrNothingToSweep: the deposit address holds none of the token
	// (NothingToSweep), for instance because someone else swept it.
	ErrNothingToSweep = errors.New("nothing to sweep")
)

var (
	selSweep             = Selector("sweep(address,string)")
	selDepositAddress    = Selector("depositAddress(string)")
	selForwarderCodeHash = Selector("forwarderCodeHash()")
	selRouterVault       = Selector("vault()")
	topicTransfer        = Keccak256([]byte("Transfer(address,address,uint256)"))
	topicSwept           = Keccak256([]byte("Swept(address,address,uint256,uint256,string)"))
	// Revert selectors a sweep can come back with (the vault's errors bubble
	// through the router unchanged).
	errAmountTooSmall  = Selector("AmountTooSmall()")
	errNothingToSweep  = Selector("NothingToSweep()")
	errTokenNotAllowed = Selector("TokenNotAllowed()")
	errIsPaused        = Selector("IsPaused()")
	errBadDestination  = Selector("BadDestination()")
	errTransferFailed  = Selector("TransferFailed()")
)

// DepositAddress is the deposit address of the Argonmine address to under a
// router: keccak256(0xff ‖ router ‖ keccak256(bytes(to)) ‖ codeHash)[12:],
// the CREATE2 address of its Forwarder. to must be the canonical lowercase
// form: another spelling is another address.
func DepositAddress(router [20]byte, codeHash [32]byte, to string) [20]byte {
	salt := Keccak256([]byte(to))
	h := Keccak256([]byte{0xff}, router[:], salt[:], codeHash[:])
	var a [20]byte
	copy(a[:], h[12:])
	return a
}

// ChecksumAddress renders an address in EIP-55 mixed case, the form wallets
// and exchanges check when it is pasted.
func ChecksumAddress(a [20]byte) string {
	lower := hex.EncodeToString(a[:])
	h := Keccak256([]byte(lower))
	out := []byte(lower)
	for i, ch := range out {
		if ch < 'a' || ch > 'f' {
			continue
		}
		nib := h[i/2]
		if i%2 == 0 {
			nib >>= 4
		}
		if nib&0x0f >= 8 {
			out[i] = ch - 'a' + 'A'
		}
	}
	return "0x" + string(out)
}

// abiString is the tail encoding of a dynamic string argument: its length
// and its bytes padded to 32.
func abiString(s string) []byte {
	return append(wordUint(big.NewInt(int64(len(s)))), padRight([]byte(s))...)
}

// EncodeSweepCall ABI-encodes sweep(token, to).
func EncodeSweepCall(token [20]byte, to string) []byte {
	out := append([]byte{}, selSweep[:]...)
	out = append(out, word(token[:])...)
	out = append(out, wordUint(big.NewInt(64))...)
	return append(out, abiString(to)...)
}

// EncodeDepositAddressCall ABI-encodes depositAddress(to).
func EncodeDepositAddressCall(to string) []byte {
	out := append([]byte{}, selDepositAddress[:]...)
	out = append(out, wordUint(big.NewInt(32))...)
	return append(out, abiString(to)...)
}

// DecodeTransferLog decodes an ERC-20 Transfer(from, to, value) log: topics
// (signature, from, to) and data (value).
func DecodeTransferLog(topics []string, data string) (TransferEvent, error) {
	var ev TransferEvent
	if len(topics) != 3 {
		return ev, errors.New("evm: Transfer log needs 3 topics")
	}
	t0, _ := hexBytes(topics[0])
	if !bytes.Equal(t0, topicTransfer[:]) {
		return ev, errors.New("evm: not a Transfer log")
	}
	from, err := hexBytes(topics[1])
	if err != nil || len(from) != 32 {
		return ev, errors.New("evm: bad from topic")
	}
	copy(ev.From[:], from[12:])
	to, err := hexBytes(topics[2])
	if err != nil || len(to) != 32 {
		return ev, errors.New("evm: bad to topic")
	}
	copy(ev.To[:], to[12:])
	d, err := hexBytes(data)
	if err != nil || len(d) != 32 {
		return ev, errors.New("evm: bad Transfer data")
	}
	ev.Amount = new(big.Int).SetBytes(d)
	return ev, nil
}

// DecodeSweptLog decodes the router's Swept(token, forwarder, amount,
// depositId, to): topics (signature, token, forwarder) and data (amount,
// depositId, to).
func DecodeSweptLog(topics []string, data string) (SweptEvent, error) {
	var ev SweptEvent
	if len(topics) != 3 {
		return ev, errors.New("evm: Swept log needs 3 topics")
	}
	t0, _ := hexBytes(topics[0])
	if !bytes.Equal(t0, topicSwept[:]) {
		return ev, errors.New("evm: not a Swept log")
	}
	token, err := hexBytes(topics[1])
	if err != nil || len(token) != 32 {
		return ev, errors.New("evm: bad token topic")
	}
	copy(ev.Token[:], token[12:])
	fwd, err := hexBytes(topics[2])
	if err != nil || len(fwd) != 32 {
		return ev, errors.New("evm: bad forwarder topic")
	}
	copy(ev.Forwarder[:], fwd[12:])
	d, err := hexBytes(data)
	if err != nil || len(d) < 128 {
		return ev, errors.New("evm: bad Swept data")
	}
	ev.Amount = new(big.Int).SetBytes(d[0:32])
	id := new(big.Int).SetBytes(d[32:64])
	if !id.IsUint64() {
		return ev, errors.New("evm: deposit id out of range")
	}
	ev.DepositID = id.Uint64()
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

// revertSelector finds the 4-byte custom error of a reverted call in a
// node's answer: in its revert data, or failing that spelled in its message
// (some providers return only "execution reverted: custom error 0x…").
func revertSelector(err error) ([4]byte, bool) {
	var sel [4]byte
	var re *RPCError
	if !errors.As(err, &re) {
		return sel, false
	}
	if b, e := hexBytes(strings.TrimSpace(re.Data)); e == nil && len(b) >= 4 {
		copy(sel[:], b[:4])
		return sel, true
	}
	known := [][4]byte{errAmountTooSmall, errNothingToSweep, errTokenNotAllowed, errIsPaused, errBadDestination, errTransferFailed, errLimitExceededSel, errAlreadyReleasedSel}
	m := strings.ToLower(re.Message + " " + re.Data)
	for _, k := range known {
		if strings.Contains(m, "0x"+hex.EncodeToString(k[:])) {
			return k, true
		}
	}
	return sel, false
}

// sweepRefusal explains why the node says a sweep would revert.
func sweepRefusal(err error) error {
	sel, ok := revertSelector(err)
	switch {
	case !ok:
		return fmt.Errorf("sweep would fail: %w", err)
	case sel == errAmountTooSmall:
		return ErrSweepBelowMinimum
	case sel == errNothingToSweep:
		return ErrNothingToSweep
	case sel == errTokenNotAllowed:
		return errors.New("sweep would fail: the vault does not accept this token (TokenNotAllowed)")
	case sel == errIsPaused:
		return errors.New("sweep would fail: the vault is paused (IsPaused)")
	case sel == errBadDestination:
		return errors.New("sweep would fail: the router refuses the destination (BadDestination)")
	case sel == errTransferFailed:
		return errors.New("sweep would fail: the token transfer failed (TransferFailed)")
	}
	return fmt.Errorf("sweep would fail: %w", err)
}

// ---- RPCEVM: the router calls over JSON-RPC ----

func (e *RPCEVM) ethCallAt(ctx context.Context, to [20]byte, data []byte, block uint64) ([]byte, error) {
	tag := "latest"
	if block > 0 {
		tag = quantity(block)
	}
	raw, err := e.call(ctx, "eth_call", map[string]any{"to": HexAddress(to), "data": "0x" + hex.EncodeToString(data)}, tag)
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return hexBytes(s)
}

func (e *RPCEVM) ForwarderCodeHash(ctx context.Context, router [20]byte) ([32]byte, error) {
	var h [32]byte
	if err := e.ensureChain(ctx); err != nil {
		return h, err
	}
	b, err := e.ethCallAt(ctx, router, selForwarderCodeHash[:], 0)
	if err != nil {
		return h, err
	}
	if len(b) != 32 {
		return h, fmt.Errorf("evm: %s answers no forwarderCodeHash(): is it a DepositRouter?", HexAddress(router))
	}
	copy(h[:], b)
	return h, nil
}

func (e *RPCEVM) RouterVault(ctx context.Context, router [20]byte) ([20]byte, error) {
	var a [20]byte
	if err := e.ensureChain(ctx); err != nil {
		return a, err
	}
	b, err := e.ethCallAt(ctx, router, selRouterVault[:], 0)
	if err != nil {
		return a, err
	}
	if len(b) != 32 {
		return a, fmt.Errorf("evm: %s answers no vault(): is it a DepositRouter?", HexAddress(router))
	}
	copy(a[:], b[12:])
	return a, nil
}

func (e *RPCEVM) DepositAddressView(ctx context.Context, router [20]byte, to string) ([20]byte, error) {
	var a [20]byte
	if err := e.ensureChain(ctx); err != nil {
		return a, err
	}
	b, err := e.ethCallAt(ctx, router, EncodeDepositAddressCall(to), 0)
	if err != nil {
		return a, err
	}
	if len(b) != 32 {
		return a, fmt.Errorf("evm: bad depositAddress() result from %s", HexAddress(router))
	}
	copy(a[:], b[12:])
	return a, nil
}

func (e *RPCEVM) BalanceOf(ctx context.Context, token, holder [20]byte, block uint64) (*big.Int, error) {
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	b, err := e.ethCallAt(ctx, token, append(append([]byte{}, selBalanceOf[:]...), word(holder[:])...), block)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("evm: bad balanceOf() result from %s", HexAddress(token))
	}
	return new(big.Int).SetBytes(b), nil
}

func (e *RPCEVM) Transfers(ctx context.Context, token [20]byte, recipients [][20]byte, fromBlock, toBlock uint64) ([]TransferEvent, error) {
	if len(recipients) == 0 {
		return nil, nil
	}
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	tos := make([]string, len(recipients))
	for i, r := range recipients {
		tos[i] = "0x" + hex.EncodeToString(word(r[:]))
	}
	raw, err := e.call(ctx, "eth_getLogs", map[string]any{
		"fromBlock": quantity(fromBlock), "toBlock": quantity(toBlock), "address": HexAddress(token),
		"topics": []any{"0x" + hex.EncodeToString(topicTransfer[:]), nil, tos},
	})
	if err != nil {
		return nil, err
	}
	var logs []struct {
		Address string   `json:"address"`
		Topics  []string `json:"topics"`
		Data    string   `json:"data"`
		Block   string   `json:"blockNumber"`
		TxHash  string   `json:"transactionHash"`
		LogIdx  string   `json:"logIndex"`
		Removed bool     `json:"removed"`
	}
	if err := json.Unmarshal(raw, &logs); err != nil {
		return nil, err
	}
	var out []TransferEvent
	for _, l := range logs {
		if l.Removed {
			continue
		}
		if a, err := ParseAddress(l.Address); err != nil || a != token {
			continue
		}
		ev, err := DecodeTransferLog(l.Topics, l.Data)
		if err != nil {
			continue
		}
		ev.Token = token
		b, _ := hexBytes(l.Block)
		ev.Block = new(big.Int).SetBytes(b).Uint64()
		li, _ := hexBytes(l.LogIdx)
		ev.LogIndex = new(big.Int).SetBytes(li).Uint64()
		ev.TxHash = strings.ToLower(l.TxHash)
		out = append(out, ev)
	}
	return out, nil
}

func (e *RPCEVM) Sweep(ctx context.Context, router, token [20]byte, to string) (string, error) {
	if err := e.ensureChain(ctx); err != nil {
		return "", err
	}
	h, err := e.send(ctx, router, EncodeSweepCall(token, to))
	if errors.Is(err, errWouldFail) {
		return "", sweepRefusal(errors.Unwrap(err))
	}
	return h, err
}

func (e *RPCEVM) SweptLog(ctx context.Context, router [20]byte, txHash string) (*SweptEvent, error) {
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
		if addr, err := ParseAddress(l.Address); err != nil || addr != router {
			continue
		}
		if ev, err := DecodeSweptLog(l.Topics, l.Data); err == nil {
			return &ev, nil
		}
	}
	return nil, nil
}
