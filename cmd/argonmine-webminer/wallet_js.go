//go:build js && wasm

package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"argonmine/consensus"
	"argonmine/litewallet"
	"argonmine/swap/quote"
	"argonmine/swapaddr"
)

// The browser wallet's signing half (package litewallet). The page calls
// argonmineMiner.wallet(op, argsJSON) through the worker and gets JSON back:
// the result, or {"error", "code"}. Every call is synchronous computation;
// the page does all network traffic. The unlocked account stays in this
// worker's memory: the page never holds the key or the words, except when
// the user asks to see the words again ("reveal", password every time).
// The account locks itself after idleLock without a signing call, whatever
// the page does.

const idleLock = 15 * time.Minute

var (
	wmu      sync.Mutex
	unlocked *litewallet.Account
	lastUse  time.Time
)

// walletArgs carries every operation's arguments. Amounts are decimal
// strings of units, so no JavaScript number rounds them.
type walletArgs struct {
	HRP      string             `json:"hrp"`
	Mnemonic string             `json:"mnemonic"`
	Password string             `json:"password"`
	Sealed   *litewallet.Sealed `json:"sealed"`
	Address  string             `json:"address"`
	Coins    []litewallet.Coin  `json:"coins"`
	Tip      uint32             `json:"tip"`
	Asset    string             `json:"asset"`
	To       string             `json:"to"`
	Amount   string             `json:"amount"`
	Max      bool               `json:"max"`
	UARG     string             `json:"uarg"`
	Memo     string             `json:"memo"`
	Info     *quote.Info        `json:"info"`
	From     string             `json:"from"`
	ToCoin   string             `json:"toCoin"`
	MinOut   string             `json:"minOut"`
	Quote    *quote.Quote       `json:"quote"`
	TxID     string             `json:"txid"` // signQuote: the transaction previewQuote showed
	Offered  []litewallet.Coin  `json:"offered"`
	ChainID  uint64             `json:"chainId"`
	Symbol   string             `json:"symbol"`
	EVM      string             `json:"evm"`
}

func units(s string) (consensus.Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, errors.New("bad amount")
	}
	return consensus.Amount(v), nil
}

type codedError struct {
	code string
	err  error
}

func (e codedError) Error() string { return e.err.Error() }

func coded(err error) (string, string) {
	var ce codedError
	if errors.As(err, &ce) {
		return ce.code, ce.err.Error()
	}
	switch {
	case errors.Is(err, litewallet.ErrPassword):
		return "password", "Wrong password."
	case errors.Is(err, litewallet.ErrLocked):
		return "locked", "The wallet is locked."
	case errors.Is(err, litewallet.ErrFeeFunds):
		return "fee", err.Error()
	case errors.Is(err, litewallet.ErrFunds):
		return "funds", err.Error()
	case errors.Is(err, litewallet.ErrSwapAddr):
		return "swapaddr", err.Error()
	case errors.Is(err, litewallet.ErrUnconfirmed):
		return "wait", err.Error()
	}
	return "", err.Error()
}

// signer returns the unlocked account of address on network p, or ErrLocked.
func signer(p *consensus.Params, address string) (*litewallet.Account, error) {
	wmu.Lock()
	defer wmu.Unlock()
	if unlocked != nil && time.Since(lastUse) > idleLock {
		unlocked = nil
	}
	if unlocked == nil || !strings.EqualFold(unlocked.Address(), strings.TrimSpace(address)) {
		return nil, litewallet.ErrLocked
	}
	lastUse = time.Now()
	return unlocked, nil
}

func setUnlocked(a *litewallet.Account) {
	wmu.Lock()
	unlocked, lastUse = a, time.Now()
	wmu.Unlock()
}

func walletOp(op string, in *walletArgs) (any, error) {
	p := paramsForHRP(in.HRP)
	if p == nil {
		return nil, errors.New("unknown address prefix")
	}
	switch op {
	case "seal":
		// Seals a phrase under a new password and unlocks it.
		s, acct, err := litewallet.Seal(p, in.Mnemonic, in.Password)
		if err != nil {
			return nil, err
		}
		setUnlocked(acct)
		return map[string]any{"sealed": s, "address": acct.Address()}, nil
	case "unlock":
		_, acct, err := litewallet.Open(p, in.Sealed, in.Password)
		if err != nil {
			return nil, err
		}
		setUnlocked(acct)
		return map[string]any{"address": acct.Address()}, nil
	case "lock":
		setUnlocked(nil)
		return map[string]any{}, nil
	case "status":
		// Asking does not count as use: it must not hold off the auto-lock.
		wmu.Lock()
		on := unlocked != nil && time.Since(lastUse) <= idleLock && strings.EqualFold(unlocked.Address(), strings.TrimSpace(in.Address))
		wmu.Unlock()
		return map[string]any{"unlocked": on}, nil
	case "reveal":
		m, _, err := litewallet.Open(p, in.Sealed, in.Password)
		if err != nil {
			return nil, err
		}
		return map[string]any{"mnemonic": m}, nil
	case "preview", "pay":
		amount, err := units(in.Amount)
		if err != nil {
			return nil, err
		}
		var acct *litewallet.Account
		if op == "pay" {
			acct, err = signer(p, in.Address)
		} else {
			acct, err = litewallet.Watch(p, in.Address)
		}
		if err != nil {
			return nil, err
		}
		return acct.Pay(in.Coins, in.Tip, litewallet.PayRequest{Asset: in.Asset, To: in.To, Amount: amount, Max: in.Max, UARG: in.UARG, Memo: in.Memo})
	case "assets":
		// The ids of uARG (the bridge's) and wARG (recomputed from the
		// maker's issuer key), never taken from a server as given.
		if in.Info == nil {
			return nil, errors.New("the swap service has not answered")
		}
		assets, err := quote.Assets(in.Info, in.UARG)
		if err != nil {
			return nil, codedError{"maker", err}
		}
		return map[string]string{"UARG": assets[quote.UARG].String(), "WARG": assets[quote.WARG].String()}, nil
	case "swapAddress":
		// The swap address of a payout, computed here: the bridge's own
		// answer is only compared with it.
		h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(in.EVM)), "0x")
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 20 {
			return nil, errors.New("not a valid 0x address")
		}
		var to [20]byte
		copy(to[:], b)
		a, err := swapaddr.Address(p.HRP, swapaddr.Dest{ChainID: in.ChainID, Symbol: in.Symbol, To: to})
		if err != nil {
			return nil, err
		}
		return map[string]string{"address": a}, nil
	case "quoteRequest", "previewQuote", "signQuote":
		if in.Info == nil {
			return nil, errors.New("the swap service has not answered")
		}
		assets, err := quote.Assets(in.Info, in.UARG)
		if err != nil {
			return nil, codedError{"maker", err}
		}
		amount, err := units(in.Amount)
		if err != nil {
			return nil, err
		}
		if op == "quoteRequest" {
			acct, err := litewallet.Watch(p, in.Address)
			if err != nil {
				return nil, err
			}
			req, offered, err := acct.QuoteRequest(in.Coins, in.Tip, assets, in.From, in.ToCoin, amount)
			if err != nil {
				return nil, err
			}
			return map[string]any{"request": req, "offered": offered}, nil
		}
		if in.Quote == nil {
			return nil, errors.New("no quote to sign")
		}
		minOut, err := units(in.MinOut)
		if err != nil {
			return nil, err
		}
		if minOut <= 0 {
			// The page always sends the floor from the posted rate: without
			// it the maker's own quoted amount would be the only check.
			return nil, codedError{"maker", errors.New("no minimum from the posted price; nothing was signed")}
		}
		check := litewallet.QuoteCheck{From: in.From, To: in.ToCoin, Assets: assets, AmountIn: amount, MinOut: minOut, Offered: in.Offered, Tip: in.Tip, TxID: in.TxID}
		if op == "previewQuote" {
			acct, err := litewallet.Watch(p, in.Address)
			if err != nil {
				return nil, err
			}
			return acct.PreviewQuote(in.Quote, check)
		}
		if in.TxID == "" {
			return nil, errors.New("review the swap before signing it")
		}
		acct, err := signer(p, in.Address)
		if err != nil {
			return nil, err
		}
		return acct.SignQuote(in.Quote, check)
	}
	return nil, errors.New("unknown wallet request")
}

// walletCall is argonmineMiner.wallet(op, argsJSON) -> resultJSON.
func walletCall(this js.Value, args []js.Value) any {
	reply := func(v any) any {
		b, err := json.Marshal(v)
		if err != nil {
			return `{"error":"internal error"}`
		}
		return string(b)
	}
	if len(args) < 2 || args[0].Type() != js.TypeString || args[1].Type() != js.TypeString {
		return reply(map[string]string{"error": "bad wallet request"})
	}
	raw := []byte(args[1].String())
	// What the swap service sent (quote, info) is decoded only if every key
	// is exactly as the protocol writes it: Go's decoder matches keys in any
	// case, and the page reads them exactly, so a reply with "amount_out"
	// and "AMOUNT_OUT" would show one amount and sign another.
	if err := strictServerParts(raw, "quote", "info"); err != nil {
		return reply(map[string]string{"error": "the swap service sent a malformed answer: " + err.Error(), "code": "maker"})
	}
	var in walletArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return reply(map[string]string{"error": "bad wallet request"})
	}
	out, err := walletOp(args[0].String(), &in)
	in.Password, in.Mnemonic = "", ""
	if err != nil {
		code, msg := coded(err)
		return reply(map[string]string{"error": msg, "code": code})
	}
	return reply(out)
}

// strictServerParts checks the named members of the request object: in
// them every object key must be lower-case snake case and appear once.
func strictServerParts(raw []byte, names ...string) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	for _, n := range names {
		if v, ok := top[n]; ok {
			if err := quote.CheckKeys(v); err != nil {
				return err
			}
		}
	}
	return nil
}
