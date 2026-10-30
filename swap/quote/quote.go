// Package quote is the wire format of the native swap service (docs/SWAP.md)
// and the check a wallet runs on a quote before it signs its part. It
// depends only on consensus, so the browser wallet (WebAssembly) checks
// quotes with the same code as the command-line and desktop wallets; package
// swap re-exports everything here under its old names.
package quote

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"argonmine/consensus"
)

// The three coins of the Argonmine chain, as the protocol names them.
const (
	ARG  = "ARG"
	UARG = "UARG"
	WARG = "WARG"
)

// WARGSymbol is the symbol of the maker's wARG issuance.
const WARGSymbol = "WARG"

// Normalize maps a coin name in any case ("uarg", "wARG") to its protocol
// name, or "" when it is not one of the three.
func Normalize(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case ARG:
		return ARG
	case UARG:
		return UARG
	case WARG:
		return WARG
	}
	return ""
}

// Info is the answer of GET /api/info.
type Info struct {
	Network    string           `json:"network"`
	Maker      string           `json:"maker"`        // the maker's address: what it buys is paid here
	Issuer     string           `json:"issuer"`       // x-only key that issues wARG, hex
	UARG       string           `json:"uarg"`         // the bridge's uARG asset id
	WARG       string           `json:"warg"`         // AssetID(issuer, "WARG")
	Price      string           `json:"price"`        // uARG per ARG, before the spread
	SpreadBps  uint32           `json:"spread_bps"`   // taken on each side of ARG and wARG against uARG
	WrapFeeBps uint32           `json:"wrap_fee_bps"` // taken on ARG to wARG and back
	Pairs      []Pair           `json:"pairs"`
	QuoteTTL   int              `json:"quote_ttl"`   // seconds a quote holds
	WARGSupply consensus.Amount `json:"warg_supply"` // wARG in circulation
	Reserve    consensus.Amount `json:"arg_reserve"` // the maker's ARG, which backs it
	Height     uint32           `json:"height"`
	// Closed says why the maker trades nothing right now (no pairs, every
	// quote refused with this message); empty when it is open.
	Closed string `json:"closed,omitempty"`
}

// Pair is one direction the maker trades.
type Pair struct {
	From      string           `json:"from"`
	To        string           `json:"to"`
	Rate      string           `json:"rate"`      // units of To per unit of From, after fees
	Min       consensus.Amount `json:"min"`       // smallest amount of From
	Max       consensus.Amount `json:"max"`       // largest amount of From
	Available consensus.Amount `json:"available"` // of To the maker can pay now
}

// Outpoint names a coin.
type Outpoint struct {
	TxID  string `json:"txid"`
	Index uint32 `json:"index"`
}

// QuoteRequest is the body of POST /api/quote.
type QuoteRequest struct {
	From    string           `json:"from"`
	To      string           `json:"to"`
	Amount  consensus.Amount `json:"amount"`  // of From
	Inputs  []Outpoint       `json:"inputs"`  // the wallet's coins of From, at least Amount
	Receive string           `json:"receive"` // the wallet's address for what it gets
	Change  string           `json:"change"`  // the wallet's address for its change
}

// SpentOut is the output an input spends.
type SpentOut struct {
	Amount  consensus.Amount `json:"amount"`
	Type    uint8            `json:"type"`
	Payload string           `json:"payload"`         // hex
	Asset   string           `json:"asset,omitempty"` // hex, empty for ARG
}

// Quote is the answer of POST /api/quote: the unsigned transaction and
// what it trades.
type Quote struct {
	ID         string           `json:"id"`
	Expires    int64            `json:"expires"` // unix seconds
	From       string           `json:"from"`
	To         string           `json:"to"`
	AmountIn   consensus.Amount `json:"amount_in"`
	AmountOut  consensus.Amount `json:"amount_out"`
	Rate       string           `json:"rate"`
	Tx         string           `json:"tx"`          // hex, every witness empty
	Spent      []SpentOut       `json:"spent"`       // what every input spends, in order
	UserInputs []int            `json:"user_inputs"` // the inputs the wallet signs
}

// Sig is one of the wallet's input signatures.
type Sig struct {
	Index int    `json:"index"`
	Sig   string `json:"sig"` // 64 bytes, hex
}

// SwapRequest is the body of POST /api/swap.
type SwapRequest struct {
	QuoteID string `json:"quote_id"`
	Sigs    []Sig  `json:"sigs"`
}

// SwapResult is the answer of POST /api/swap.
type SwapResult struct {
	TxID string `json:"txid"`
}

// SpentJSON describes the output an input spends, as a quote lists it.
func SpentJSON(o consensus.TxOut) SpentOut {
	s := SpentOut{Amount: o.Amount, Type: o.Type, Payload: hex.EncodeToString(o.Payload)}
	if o.Asset != consensus.NativeAsset {
		s.Asset = o.Asset.String()
	}
	return s
}

// Out is the output s describes.
func (s SpentOut) Out() (consensus.TxOut, error) {
	p, err := hex.DecodeString(s.Payload)
	if err != nil {
		return consensus.TxOut{}, errors.New("swap: bad spent payload")
	}
	o := consensus.TxOut{Amount: s.Amount, Type: s.Type, Payload: p}
	if s.Asset != "" {
		h, err := consensus.HashFromHex(s.Asset)
		if err != nil {
			return consensus.TxOut{}, errors.New("swap: bad spent asset")
		}
		o.Asset = h
	}
	return o, nil
}

// Decode returns the quote's transaction and the outputs its inputs spend.
func (q *Quote) Decode() (*consensus.Tx, []consensus.TxOut, error) {
	raw, err := hex.DecodeString(q.Tx)
	if err != nil {
		return nil, nil, errors.New("swap: the quote's transaction is not hex")
	}
	tx, err := consensus.DecodeTx(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("swap: the quote's transaction does not decode: %w", err)
	}
	if len(q.Spent) != len(tx.Inputs) {
		return nil, nil, errors.New("swap: the quote lists a different number of spent outputs than inputs")
	}
	spent := make([]consensus.TxOut, len(q.Spent))
	for i, s := range q.Spent {
		if spent[i], err = s.Out(); err != nil {
			return nil, nil, err
		}
	}
	return tx, spent, nil
}

// Assets maps the three coins to their asset ids, from what the wallet
// trusts: the bridge's uARG id, and the maker's issuer key for wARG (whose
// id is recomputed, never taken from the maker as given).
func Assets(info *Info, genuineUARG string) (map[string]consensus.Hash, error) {
	if genuineUARG == "" || !strings.EqualFold(info.UARG, genuineUARG) {
		return nil, errors.New("the swap service trades a different uARG than the bridge issues")
	}
	u, err := consensus.HashFromHex(genuineUARG)
	if err != nil {
		return nil, err
	}
	ib, err := hex.DecodeString(info.Issuer)
	if err != nil || len(ib) != 32 {
		return nil, errors.New("the swap service names a bad wARG issuer")
	}
	var issuer [32]byte
	copy(issuer[:], ib)
	w := consensus.AssetID(issuer, WARGSymbol)
	if !strings.EqualFold(info.WARG, w.String()) {
		return nil, errors.New("the swap service's wARG id does not match its issuer")
	}
	return map[string]consensus.Hash{ARG: consensus.NativeAsset, UARG: u, WARG: w}, nil
}

// Limits on a quoted transaction a wallet will consider signing.
const (
	maxQuoteInputs  = 200
	maxQuoteOutputs = 20
	maxQuoteBytes   = 100_000
)

// ArgDust is the most ARG a swap may take beyond the amount asked: a change
// smaller than the dust limit cannot be paid back, so the maker keeps it.
const ArgDust consensus.Amount = 546

// Expect is what a wallet asked for, and what it knows about its own coins.
type Expect struct {
	From     consensus.Hash                         // asset paid; consensus.NativeAsset for ARG
	To       consensus.Hash                         // asset bought
	AmountIn consensus.Amount                       // exactly this much of From leaves the wallet
	MinOut   consensus.Amount                       // at least this much of To arrives
	Coins    map[consensus.OutPoint]consensus.TxOut // the coins the wallet offered, as it recorded them
	Owns     func(consensus.TxOut) bool             // whether an output pays the wallet
	Tip      uint32                                 // current chain height
}

// CheckQuote decodes a quote's transaction and checks that signing the
// wallet's inputs in it is safe: the inputs it signs are exactly coins it
// offered, described as it recorded them; it gives exactly AmountIn of
// From (for ARG, at most a dust change more), receives at least MinOut of
// To, and loses nothing of any other coin; the transaction is valid now,
// not later. It returns the transaction and the outputs its inputs spend.
func CheckQuote(q *Quote, e Expect) (*consensus.Tx, []consensus.TxOut, error) {
	if e.From == e.To {
		return nil, nil, errors.New("swap: paying and buying the same coin")
	}
	if e.AmountIn <= 0 || e.MinOut <= 0 {
		return nil, nil, errors.New("swap: amounts must be positive")
	}
	if len(q.Tx) > 2*maxQuoteBytes {
		return nil, nil, errors.New("swap: the quoted transaction is too large")
	}
	tx, spent, err := q.Decode()
	if err != nil {
		return nil, nil, err
	}
	if err := consensus.CheckTxStateless(tx); err != nil {
		return nil, nil, fmt.Errorf("swap: the quoted transaction is invalid: %w", err)
	}
	if tx.Version != consensus.TxVersionAssets {
		return nil, nil, errors.New("swap: the quoted transaction has the wrong version")
	}
	if len(tx.Inputs) > maxQuoteInputs || len(tx.Outputs) > maxQuoteOutputs {
		return nil, nil, errors.New("swap: the quoted transaction has too many inputs or outputs")
	}
	// Valid in the next block: a maker holding signatures for a transaction
	// that only becomes valid later would hold an option on the wallet.
	if tx.LockHeight > e.Tip+1 {
		return nil, nil, errors.New("swap: the quoted transaction is not valid until a later block")
	}
	if q.AmountIn != e.AmountIn || q.AmountOut < e.MinOut {
		return nil, nil, errors.New("swap: the quote does not trade the amounts asked")
	}

	// The inputs the wallet signs are exactly its offered coins, each as it
	// recorded it; any other input spending one of them is refused.
	signs := map[int]bool{}
	for _, i := range q.UserInputs {
		if i < 0 || i >= len(tx.Inputs) || signs[i] {
			return nil, nil, errors.New("swap: the quote names a bad input to sign")
		}
		signs[i] = true
	}
	in := map[consensus.Hash]consensus.Amount{}
	for i, txin := range tx.Inputs {
		mine, offered := e.Coins[txin.PrevOut]
		if offered != signs[i] {
			return nil, nil, fmt.Errorf("swap: input %d is not one of the coins offered for signing", i)
		}
		if !offered {
			continue
		}
		s := spent[i]
		if s.Amount != mine.Amount || s.Type != mine.Type || !bytes.Equal(s.Payload, mine.Payload) || s.Asset != mine.Asset {
			return nil, nil, fmt.Errorf("swap: input %d is described differently from the wallet's coin", i)
		}
		if mine.Asset != e.From {
			return nil, nil, fmt.Errorf("swap: input %d is not a coin of what the wallet pays", i)
		}
		in[mine.Asset] += mine.Amount
	}
	if len(signs) == 0 {
		return nil, nil, errors.New("swap: the quote asks the wallet to sign nothing")
	}

	// What comes back to the wallet, per coin.
	out := map[consensus.Hash]consensus.Amount{}
	for _, o := range tx.Outputs {
		if e.Owns(o) {
			out[o.Asset] += o.Amount
		}
	}
	gives := in[e.From] - out[e.From]
	maxGive := e.AmountIn
	if e.From == consensus.NativeAsset {
		maxGive += ArgDust - 1
	}
	if gives < e.AmountIn || gives > maxGive {
		return nil, nil, fmt.Errorf("swap: the transaction takes %d units of what the wallet pays, not %d", gives, e.AmountIn)
	}
	if got := out[e.To] - in[e.To]; got < q.AmountOut {
		return nil, nil, fmt.Errorf("swap: the transaction pays the wallet %d units, not the %d quoted", got, q.AmountOut)
	}
	for a, n := range in {
		if a != e.From && a != e.To && out[a] < n {
			return nil, nil, errors.New("swap: the transaction takes a coin the wallet did not offer to pay")
		}
	}
	return tx, spent, nil
}
