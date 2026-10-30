package bridge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// Release transactions (docs/BRIDGE.md, "Release transactions").
//
// The leader sends every release from a dedicated gas key, never from a
// guardian's signing key: anyone may call release() with a threshold of
// guardian signatures, so the sender needs nothing but gas. Each release is
// tracked by its nonce, its fees and every hash broadcast for it. One that
// is not mined after a timeout is sent again with the same nonce and the
// same call at higher fees (EIP-1559: tip and fee cap +12.5 percent at
// least; legacy: gas price +10 percent), never above the chain's cap. One
// whose nonce went to another transaction is sent again with a new nonce.
// The vault pays a burn id once (released[burnId]), so no resubmission can
// pay twice; the daemon reads released() to know it was paid.

var (
	// ErrLimitExceeded: the vault would refuse the release now because of its
	// per-release or 24-hour limit. The release waits; it never fails for it.
	ErrLimitExceeded = errors.New("the vault's release limit is reached (LimitExceeded)")
	// ErrVaultPaused: the vault is paused; releases wait for the owner.
	ErrVaultPaused = errors.New("the vault is paused (IsPaused)")
	// ErrAlreadyReleased: the vault has paid this burn already.
	ErrAlreadyReleased = errors.New("the vault has already paid this burn (AlreadyReleased)")
	// ErrGasCap: raising the fees as a replacement needs would pass the
	// chain's max_gas_price_gwei; the transaction stays as it is.
	ErrGasCap = errors.New("raising the fees would pass the chain's gas price cap")
	// ErrUnderpriced: the node refused a replacement as underpriced. The
	// returned TxSend carries the fees tried, so the next bump goes higher.
	ErrUnderpriced = errors.New("the node refused the replacement as underpriced")
	// ErrNonceUsed: the nonce of the transaction being replaced was taken by
	// a mined transaction.
	ErrNonceUsed = errors.New("the nonce is already used")
	// ErrNoDecimals: the token answers no decimals() (the call reverts or
	// returns nothing); such a token is refused.
	ErrNoDecimals = errors.New("the token answers no decimals()")
)

var (
	errLimitExceededSel   = Selector("LimitExceeded()")
	errAlreadyReleasedSel = Selector("AlreadyReleased()")
	selPaused             = Selector("paused()")
	selMaxRelease         = Selector("maxRelease(address)")
	selPeriodAvailable    = Selector("periodAvailable(address)")
	selDecimals           = Selector("decimals()")
	selIsGuardian         = Selector("isGuardian(address)")
	selOwner              = Selector("owner()")
)

// ReleaseRequest is one release transaction to send.
type ReleaseRequest struct {
	Data        []byte   // the release(...) call, EncodeReleaseCall; ignored with Replace
	Replace     *TxSend  // non-nil: send Replace's call again with its nonce and higher fees
	MaxGasPrice *big.Int // cap on the gas price (legacy) or the fee cap (EIP-1559), wei; nil: none
}

// TxSend is a release transaction as sent: what it takes to follow it and to
// send it again.
type TxSend struct {
	From     string   `json:"from"`
	Nonce    uint64   `json:"nonce"`
	Type     uint8    `json:"type"`                // 0: legacy (EIP-155), 2: EIP-1559
	GasPrice string   `json:"gas_price,omitempty"` // legacy, wei
	FeeCap   string   `json:"fee_cap,omitempty"`   // EIP-1559 maxFeePerGas, wei
	TipCap   string   `json:"tip_cap,omitempty"`   // EIP-1559 maxPriorityFeePerGas, wei
	Gas      uint64   `json:"gas"`
	Data     string   `json:"data"`   // the call, hex
	Hashes   []string `json:"hashes"` // every transaction broadcast for this release, oldest first
	Sent     int64    `json:"sent"`   // first broadcast (unix), set by the service
	Last     int64    `json:"last"`   // latest broadcast or attempt (unix), set by the service
	Bumps    int      `json:"bumps"`  // replacements at higher fees
}

// Hash is the latest transaction broadcast.
func (t *TxSend) Hash() string {
	if t == nil || len(t.Hashes) == 0 {
		return ""
	}
	return t.Hashes[len(t.Hashes)-1]
}

func (t *TxSend) clone() *TxSend {
	c := *t
	c.Hashes = append([]string{}, t.Hashes...)
	return &c
}

// ---- fees ----

// mulDivUp is ceil(v × num / den).
func mulDivUp(v *big.Int, num, den int64) *big.Int {
	n := new(big.Int).Mul(v, big.NewInt(num))
	n.Add(n, big.NewInt(den-1))
	return n.Quo(n, big.NewInt(den))
}

func maxBig(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return new(big.Int).Set(a)
	}
	return new(big.Int).Set(b)
}

// BumpLegacy is the gas price of a legacy replacement: at least 10 percent
// above prev (rounded up), at least the network's price plus 10 percent,
// and never above limit (nil: none). ErrGasCap when the 10 percent step does
// not fit under the limit.
func BumpLegacy(prev, network, limit *big.Int) (*big.Int, error) {
	min := mulDivUp(prev, 110, 100)
	gp := maxBig(min, mulDivUp(network, 110, 100))
	if limit != nil && gp.Cmp(limit) > 0 {
		if min.Cmp(limit) > 0 {
			return nil, ErrGasCap
		}
		gp = new(big.Int).Set(limit)
	}
	return gp, nil
}

// Bump1559 are the fees of an EIP-1559 replacement: tip and fee cap each at
// least 12.5 percent above the previous ones (rounded up), the tip at
// least the network's, the fee cap at least twice the base fee plus the
// tip, and the fee cap never above limit (nil: none). ErrGasCap when the
// 12.5 percent steps do not fit under the limit.
func Bump1559(prevTip, prevFeeCap, netTip, baseFee, limit *big.Int) (tip, feeCap *big.Int, err error) {
	minTip, minCap := mulDivUp(prevTip, 1125, 1000), mulDivUp(prevFeeCap, 1125, 1000)
	tip = maxBig(minTip, netTip)
	want := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)
	feeCap = maxBig(minCap, want)
	if limit != nil && feeCap.Cmp(limit) > 0 {
		if minCap.Cmp(limit) > 0 {
			return nil, nil, ErrGasCap
		}
		feeCap = new(big.Int).Set(limit)
	}
	if tip.Cmp(feeCap) > 0 {
		if minTip.Cmp(feeCap) > 0 {
			return nil, nil, ErrGasCap
		}
		tip = new(big.Int).Set(feeCap)
	}
	return tip, feeCap, nil
}

// ---- EIP-1559 transactions ----

// SignDynamicFeeTx builds and signs a type-2 (EIP-1559) transaction with an
// empty access list. Returns the raw bytes for eth_sendRawTransaction.
func SignDynamicFeeTx(key *secp256k1.PrivateKey, chainID, nonce uint64, tip, feeCap *big.Int, gas uint64, to [20]byte, value *big.Int, data []byte) []byte {
	fields := [][]byte{rlpUint(new(big.Int).SetUint64(chainID)), rlpUint(new(big.Int).SetUint64(nonce)), rlpUint(tip), rlpUint(feeCap),
		rlpUint(new(big.Int).SetUint64(gas)), rlpBytes(to[:]), rlpUint(value), rlpBytes(data), rlpList()}
	h := Keccak256([]byte{0x02}, rlpList(fields...))
	compact := ecdsa.SignCompact(key, h[:], false)
	parity := big.NewInt(int64(compact[0] - 27))
	r := new(big.Int).SetBytes(compact[1:33])
	s := new(big.Int).SetBytes(compact[33:65])
	signed := append(fields, rlpUint(parity), rlpUint(r), rlpUint(s))
	return append([]byte{0x02}, rlpList(signed...)...)
}

// TxHashOf is the hash of a raw signed transaction (legacy or typed).
func TxHashOf(raw []byte) string {
	h := Keccak256(raw)
	return "0x" + hex.EncodeToString(h[:])
}

// DecodeReleaseCall parses a release(token, to, amount, burnId, sigs) call.
func DecodeReleaseCall(data []byte) (token, to [20]byte, amount *big.Int, burnID [32]byte, sigs [][]byte, err error) {
	bad := errors.New("evm: not a release call")
	if len(data) < 4+5*32 || [4]byte(data[:4]) != selRelease {
		return token, to, nil, burnID, nil, bad
	}
	d := data[4:]
	copy(token[:], d[12:32])
	copy(to[:], d[44:64])
	amount = new(big.Int).SetBytes(d[64:96])
	copy(burnID[:], d[96:128])
	off := new(big.Int).SetBytes(d[128:160])
	if !off.IsInt64() || off.Int64()+32 > int64(len(d)) {
		return token, to, nil, burnID, nil, bad
	}
	o := int(off.Int64())
	n := new(big.Int).SetBytes(d[o : o+32])
	if !n.IsInt64() || n.Int64() > 64 {
		return token, to, nil, burnID, nil, bad
	}
	head := o + 32
	for i := 0; i < int(n.Int64()); i++ {
		if head+32*(i+1) > len(d) {
			return token, to, nil, burnID, nil, bad
		}
		eo := new(big.Int).SetBytes(d[head+32*i : head+32*(i+1)])
		if !eo.IsInt64() || head+int(eo.Int64())+32 > len(d) {
			return token, to, nil, burnID, nil, bad
		}
		at := head + int(eo.Int64())
		l := new(big.Int).SetBytes(d[at : at+32])
		if !l.IsInt64() || at+32+int(l.Int64()) > len(d) {
			return token, to, nil, burnID, nil, bad
		}
		sigs = append(sigs, append([]byte{}, d[at+32:at+32+int(l.Int64())]...))
	}
	return token, to, amount, burnID, sigs, nil
}

// releaseRefusal explains why the node says a release would revert.
func releaseRefusal(err error) error {
	sel, ok := revertSelector(err)
	switch {
	case !ok:
		return fmt.Errorf("release would fail: %w", err)
	case sel == errAlreadyReleasedSel:
		return ErrAlreadyReleased
	case sel == errLimitExceededSel:
		return ErrLimitExceeded
	case sel == errIsPaused:
		return ErrVaultPaused
	}
	return fmt.Errorf("release would fail: %w", err)
}

// ---- RPCEVM: release transactions ----

// networkFees reads what the chain charges now: a gas price for a legacy
// chain (BNB Chain, or any chain without a base fee), else the base fee of
// the latest block and the node's suggested tip.
func (e *RPCEVM) networkFees(ctx context.Context) (legacy bool, gasPrice, baseFee, tip *big.Int, err error) {
	gpRaw, err := e.call(ctx, "eth_gasPrice")
	if err != nil {
		return false, nil, nil, nil, err
	}
	gasPrice, err = hexBig(gpRaw)
	if err != nil {
		return false, nil, nil, nil, err
	}
	if UsesLegacyTx(e.chainID) {
		return true, gasPrice, nil, nil, nil
	}
	raw, err := e.call(ctx, "eth_getBlockByNumber", "latest", false)
	if err != nil {
		return false, nil, nil, nil, err
	}
	var blk struct {
		BaseFee *string `json:"baseFeePerGas"`
	}
	if err := json.Unmarshal(raw, &blk); err != nil || blk.BaseFee == nil {
		return true, gasPrice, nil, nil, nil
	}
	b, _ := hexBytes(*blk.BaseFee)
	baseFee = new(big.Int).SetBytes(b)
	tip = big.NewInt(1_000_000_000) // 1 gwei when the node suggests nothing
	if traw, err := e.call(ctx, "eth_maxPriorityFeePerGas"); err == nil {
		if t, err := hexBig(traw); err == nil {
			tip = t
		}
	} else if gasPrice.Cmp(baseFee) > 0 {
		tip = new(big.Int).Sub(gasPrice, baseFee)
	}
	if tip.Sign() == 0 {
		tip = big.NewInt(1)
	}
	return false, gasPrice, baseFee, tip, nil
}

func hexBig(raw json.RawMessage) (*big.Int, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	b, err := hexBytes(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// estimate runs a call from the gas key against the latest block (never
// the pending one, which holds our own transaction being replaced): an
// error means the call would revert there (wouldFail). It then returns the
// node's gas estimate, or the client's cap when the node gives none.
func (e *RPCEVM) estimate(ctx context.Context, from, to [20]byte, data []byte) (uint64, error) {
	msg := map[string]any{"from": HexAddress(from), "to": HexAddress(to), "data": "0x" + hex.EncodeToString(data)}
	if _, err := e.call(ctx, "eth_call", msg, "latest"); err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			return 0, &wouldFail{err}
		}
		return 0, err
	}
	raw, err := e.call(ctx, "eth_estimateGas", msg, "latest")
	if err != nil {
		return e.gasCap, nil
	}
	est, err := hexQuantity(raw)
	if err != nil || est == 0 || est > e.gasCap {
		return e.gasCap, nil
	}
	return est, nil
}

// Release sends a release from the gas key. A new one takes the pending
// nonce, the node's gas estimate plus 20 percent, and fees from the
// network capped at req.MaxGasPrice; a replacement keeps the nonce, the gas
// and the call of req.Replace and raises the fees (BumpLegacy, Bump1559).
// Both are checked with eth_estimateGas first, so a burn paid meanwhile
// (by an earlier transaction landing late) comes back as
// ErrAlreadyReleased and nothing is sent.
func (e *RPCEVM) Release(ctx context.Context, req ReleaseRequest) (*TxSend, error) {
	if e.key == nil {
		return nil, errors.New("evm: no gas key to send releases from")
	}
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	from := e.Sender()
	data := req.Data
	if req.Replace != nil {
		b, err := hexBytes(req.Replace.Data)
		if err != nil || len(b) == 0 {
			return nil, errors.New("evm: the release to replace has no call data")
		}
		data = b
	}
	est, err := e.estimate(ctx, from, e.vault, data)
	if errors.Is(err, errWouldFail) {
		return nil, releaseRefusal(errors.Unwrap(err))
	}
	if err != nil {
		return nil, err
	}
	legacy, gasPrice, baseFee, tip, err := e.networkFees(ctx)
	if err != nil {
		return nil, err
	}
	var tx *TxSend
	if req.Replace != nil {
		tx = req.Replace.clone()
		if tx.Type == 0 {
			prev, _ := new(big.Int).SetString(tx.GasPrice, 10)
			if prev == nil {
				prev = new(big.Int)
			}
			gp, err := BumpLegacy(prev, gasPrice, req.MaxGasPrice)
			if err != nil {
				return nil, err
			}
			tx.GasPrice = gp.String()
		} else {
			pt, _ := new(big.Int).SetString(tx.TipCap, 10)
			pc, _ := new(big.Int).SetString(tx.FeeCap, 10)
			if pt == nil || pc == nil {
				return nil, errors.New("evm: the release to replace has no fees on record")
			}
			if baseFee == nil { // the chain lost its base fee: keep the type, price from the gas price
				baseFee, tip = gasPrice, big.NewInt(1)
			}
			t, c, err := Bump1559(pt, pc, tip, baseFee, req.MaxGasPrice)
			if err != nil {
				return nil, err
			}
			tx.TipCap, tx.FeeCap = t.String(), c.String()
		}
	} else {
		nonceRaw, err := e.call(ctx, "eth_getTransactionCount", HexAddress(from), "pending")
		if err != nil {
			return nil, err
		}
		nonce, err := hexQuantity(nonceRaw)
		if err != nil {
			return nil, err
		}
		tx = &TxSend{From: HexAddress(from), Nonce: nonce, Gas: est + est/5, Data: hex.EncodeToString(data)}
		if legacy {
			gp := mulDivUp(gasPrice, 110, 100)
			if req.MaxGasPrice != nil && gp.Cmp(req.MaxGasPrice) > 0 {
				gp = new(big.Int).Set(req.MaxGasPrice)
			}
			tx.GasPrice = gp.String()
		} else {
			fc := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)
			if req.MaxGasPrice != nil && fc.Cmp(req.MaxGasPrice) > 0 {
				fc = new(big.Int).Set(req.MaxGasPrice)
			}
			t := new(big.Int).Set(tip)
			if t.Cmp(fc) > 0 {
				t.Set(fc)
			}
			tx.Type, tx.TipCap, tx.FeeCap = 2, t.String(), fc.String()
		}
	}
	raw := e.signTx(tx, data)
	hash := TxHashOf(raw)
	if _, err := e.call(ctx, "eth_sendRawTransaction", "0x"+hex.EncodeToString(raw)); err != nil {
		m := strings.ToLower(err.Error())
		switch {
		case strings.Contains(m, "already known") || strings.Contains(m, "known transaction"):
			// The node has exactly this transaction: it was sent.
		case strings.Contains(m, "nonce too low") || strings.Contains(m, "nonce is too low"):
			return nil, ErrNonceUsed
		case req.Replace != nil && (strings.Contains(m, "underpriced") || strings.Contains(m, "replacement")):
			return tx, fmt.Errorf("%w: %v", ErrUnderpriced, err)
		default:
			return nil, err
		}
	}
	tx.Hashes = append(tx.Hashes, hash)
	if req.Replace != nil {
		tx.Bumps++
	}
	return tx, nil
}

func (e *RPCEVM) signTx(tx *TxSend, data []byte) []byte {
	if tx.Type == 2 {
		tip, _ := new(big.Int).SetString(tx.TipCap, 10)
		fc, _ := new(big.Int).SetString(tx.FeeCap, 10)
		return SignDynamicFeeTx(e.key, e.chainID, tx.Nonce, tip, fc, tx.Gas, e.vault, big.NewInt(0), data)
	}
	gp, _ := new(big.Int).SetString(tx.GasPrice, 10)
	return SignLegacyTx(e.key, e.chainID, tx.Nonce, gp, tx.Gas, e.vault, big.NewInt(0), data)
}

// ---- RPCEVM: reads for finality, receipts, limits and gas ----

func (e *RPCEVM) Heads(ctx context.Context, finalized bool) (uint64, uint64, error) {
	latest, err := e.BlockNumber(ctx)
	if err != nil {
		return 0, 0, err
	}
	if !finalized {
		return latest, latest, nil
	}
	raw, err := e.call(ctx, "eth_getBlockByNumber", "finalized", false)
	if err != nil {
		return 0, 0, fmt.Errorf("evm: the node serves no finalized block (set \"finality\":\"depth\" for this chain): %w", err)
	}
	var blk struct {
		Number string `json:"number"`
	}
	if string(raw) == "null" || json.Unmarshal(raw, &blk) != nil || blk.Number == "" {
		return 0, 0, errors.New("evm: the node serves no finalized block (set \"finality\":\"depth\" for this chain)")
	}
	b, _ := hexBytes(blk.Number)
	return latest, new(big.Int).SetBytes(b).Uint64(), nil
}

func (e *RPCEVM) BlockHash(ctx context.Context, height uint64) (string, error) {
	if err := e.ensureChain(ctx); err != nil {
		return "", err
	}
	raw, err := e.call(ctx, "eth_getBlockByNumber", quantity(height), false)
	if err != nil {
		return "", err
	}
	if string(raw) == "null" {
		return "", nil
	}
	var blk struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &blk); err != nil {
		return "", err
	}
	return strings.ToLower(blk.Hash), nil
}

func (e *RPCEVM) DepositReceipt(ctx context.Context, txHash string) (*DepositReceipt, error) {
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	raw, err := e.call(ctx, "eth_getTransactionReceipt", txHash)
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var rc struct {
		Status    string   `json:"status"`
		Block     string   `json:"blockNumber"`
		BlockHash string   `json:"blockHash"`
		Logs      []rpcLog `json:"logs"`
	}
	if err := json.Unmarshal(raw, &rc); err != nil {
		return nil, err
	}
	if rc.Block == "" || rc.BlockHash == "" {
		return nil, nil // pending
	}
	b, _ := hexBytes(rc.Block)
	out := &DepositReceipt{Status: ReceiptFailed, Block: new(big.Int).SetBytes(b).Uint64(), BlockHash: strings.ToLower(rc.BlockHash)}
	if rc.Status == "0x1" {
		out.Status = ReceiptOK
	}
	for _, l := range rc.Logs {
		if addr, err := ParseAddress(l.Address); err != nil || addr != e.vault || l.Removed {
			continue
		}
		ev, err := DecodeDepositLog(l.Topics, l.Data)
		if err != nil {
			continue
		}
		l.fill(&ev)
		out.Deposits = append(out.Deposits, ev)
	}
	return out, nil
}

func (e *RPCEVM) TxKnown(ctx context.Context, txHash string) (bool, error) {
	raw, err := e.call(ctx, "eth_getTransactionByHash", txHash)
	if err != nil {
		return false, err
	}
	return string(raw) != "null" && len(raw) > 0, nil
}

func (e *RPCEVM) ConfirmedNonce(ctx context.Context, addr [20]byte) (uint64, error) {
	raw, err := e.call(ctx, "eth_getTransactionCount", HexAddress(addr), "latest")
	if err != nil {
		return 0, err
	}
	return hexQuantity(raw)
}

func (e *RPCEVM) NativeBalance(ctx context.Context, addr [20]byte) (*big.Int, error) {
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	raw, err := e.call(ctx, "eth_getBalance", HexAddress(addr), "latest")
	if err != nil {
		return nil, err
	}
	return hexBig(raw)
}

func (e *RPCEVM) TokenDecimals(ctx context.Context, token [20]byte) (int, error) {
	if err := e.ensureChain(ctx); err != nil {
		return 0, err
	}
	b, err := e.ethCall(ctx, token, selDecimals[:])
	if err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			return 0, fmt.Errorf("%w: %s (%v)", ErrNoDecimals, HexAddress(token), err)
		}
		return 0, err
	}
	if len(b) != 32 {
		return 0, fmt.Errorf("%w: %s", ErrNoDecimals, HexAddress(token))
	}
	v := new(big.Int).SetBytes(b)
	if !v.IsInt64() || v.Int64() > 255 {
		return 0, fmt.Errorf("evm: %s answers decimals() out of range", HexAddress(token))
	}
	return int(v.Int64()), nil
}

// viewUint reads an optional uint256 view of the vault: ok is false when
// the vault has no such view (the call reverts or returns nothing).
func (e *RPCEVM) viewUint(ctx context.Context, data []byte) (*big.Int, bool, error) {
	b, err := e.ethCall(ctx, e.vault, data)
	if err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(b) != 32 {
		return nil, false, nil
	}
	return new(big.Int).SetBytes(b), true, nil
}

func (e *RPCEVM) ReleaseLimits(ctx context.Context, token [20]byte) (*ReleaseLimits, error) {
	if err := e.ensureChain(ctx); err != nil {
		return nil, err
	}
	out := &ReleaseLimits{}
	p, ok, err := e.viewUint(ctx, selPaused[:])
	if err != nil {
		return nil, err
	}
	out.Paused = ok && p.Sign() != 0
	arg := word(token[:])
	m, ok, err := e.viewUint(ctx, append(append([]byte{}, selMaxRelease[:]...), arg...))
	if err != nil {
		return nil, err
	}
	if ok && m.Sign() > 0 {
		out.MaxRelease = m
	}
	a, ok, err := e.viewUint(ctx, append(append([]byte{}, selPeriodAvailable[:]...), arg...))
	if err != nil {
		return nil, err
	}
	if ok && a.Cmp(maxUint256) != 0 {
		out.PeriodAvailable = a
	}
	return out, nil
}

func (e *RPCEVM) VaultRoles(ctx context.Context, addr [20]byte) (bool, [20]byte, error) {
	var owner [20]byte
	if err := e.ensureChain(ctx); err != nil {
		return false, owner, err
	}
	g, ok, err := e.viewUint(ctx, append(append([]byte{}, selIsGuardian[:]...), word(addr[:])...))
	if err != nil {
		return false, owner, err
	}
	isG := ok && g.Sign() != 0
	o, ok, err := e.viewUint(ctx, selOwner[:])
	if err != nil {
		return false, owner, err
	}
	if ok {
		b := o.FillBytes(make([]byte, 32))
		copy(owner[:], b[12:])
	}
	return isG, owner, nil
}
