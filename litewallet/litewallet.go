// Package litewallet is the signing core of the browser wallet. It has one
// key, the first receive key of a 24-word phrase (m/86'/coin'/0'/0/0, the
// address the browser app has always shown, so a phrase restores the same
// coins in every wallet), takes its coins from the explorer's coin list
// instead of a node, and builds and signs payments and swaps with pure
// functions: no state, no I/O. The page fetches the coins and broadcasts;
// the WebAssembly build (cmd/argonmine-webminer) keeps the unlocked key in
// the worker's memory, so the page script never sees it.
//
// Every payment is checked like the node checks it (stateless rules and
// every signature) before it is handed out, and pays change back to the
// same key. Coins sent to a swap address can never move again, so only
// uARG may be paid to one, and only to a canonical one.
package litewallet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/swap/quote"
	"argonmine/swapaddr"
	"argonmine/wallet"
)

// Relay policy the payments meet, mirroring the nodes' defaults
// (mempool.DefaultConfig: AssetOutputFee, FreeAssetOutputs; checked by a
// test): every asset output beyond the first two owes assetOutputFee.
const (
	assetOutputFee   consensus.Amount = 546
	freeAssetOutputs                  = 2
)

// DefaultFeeRate is the fee rate the browser wallet pays, in units per
// byte: twice the relay minimum, a few thousandths of a cent of ARG.
const DefaultFeeRate consensus.Amount = 2

// maxInputs bounds a payment's inputs, so it stays far below the largest
// transaction a node relays.
const maxInputs = 400

// Errors the page turns into plain words.
var (
	ErrFunds    = errors.New("not enough coins")
	ErrFeeFunds = errors.New("not enough ARG for the network fee")
	ErrSwapAddr = errors.New("swap address")
	// ErrUnconfirmed: the coins are there, but the swap service takes only
	// confirmed ones, and some arrive with the next block.
	ErrUnconfirmed = errors.New("available after the next block")
)

// Coin is one of the wallet's unspent outputs, as the explorer lists it.
// Height 0 is a coin of an unconfirmed transaction (the wallet's own change
// or a swap it signed).
type Coin struct {
	TxID     string           `json:"txid"`
	Index    uint32           `json:"index"`
	Amount   consensus.Amount `json:"amount"`
	Asset    string           `json:"asset,omitempty"` // non-ARG asset id, hex; "" for ARG
	Height   uint32           `json:"height"`
	Coinbase bool             `json:"coinbase,omitempty"`
}

func (c Coin) outpoint() (consensus.OutPoint, error) {
	h, err := consensus.HashFromHex(c.TxID)
	if err != nil {
		return consensus.OutPoint{}, fmt.Errorf("a coin has a bad transaction id")
	}
	return consensus.OutPoint{TxID: h, Index: c.Index}, nil
}

func (c Coin) key() string { return fmt.Sprintf("%s:%d", strings.ToLower(c.TxID), c.Index) }

// Account is the browser wallet's key on one network. A watch account
// (Watch) knows only the address: it previews payments and asks for swap
// quotes, so the page can show the exact fee before the password is
// entered, but it signs nothing.
type Account struct {
	params *consensus.Params
	key    *wallet.Key // nil for a watch account
	xonly  []byte      // the P2PK payload of the address
	addr   string
}

// ErrLocked is returned when a watch account is asked to sign.
var ErrLocked = errors.New("the wallet is locked")

// NormalizePhrase lower-cases a phrase and joins its words with single spaces.
func NormalizePhrase(m string) string { return strings.Join(strings.Fields(strings.ToLower(m)), " ") }

// FromMnemonic derives the account of a 24-word phrase on network p.
func FromMnemonic(p *consensus.Params, mnemonic string) (*Account, error) {
	seed, err := wallet.SeedFromMnemonic(NormalizePhrase(mnemonic), "")
	if err != nil {
		return nil, errors.New("that is not a valid 24-word phrase")
	}
	kc, err := wallet.NewKeyChain(seed, wallet.CoinTypeFor(p), 0)
	if err != nil {
		return nil, err
	}
	k, err := kc.Derive(false, 0)
	if err != nil {
		return nil, err
	}
	return &Account{params: p, key: k, xonly: k.XOnly, addr: k.Address(p.HRP)}, nil
}

// Watch is the account of an address, without its key.
func Watch(p *consensus.Params, addr string) (*Account, error) {
	d, err := wallet.DecodeAddress(p.HRP, strings.ToLower(strings.TrimSpace(addr)))
	if err != nil || d.Type != consensus.OutputTypeP2PK {
		return nil, errors.New("not a valid address on this network")
	}
	return &Account{params: p, xonly: append([]byte{}, d.Payload...), addr: strings.ToLower(strings.TrimSpace(addr))}, nil
}

// CanSign reports whether the account holds its key.
func (a *Account) CanSign() bool { return a.key != nil }

// Address is the account's address.
func (a *Account) Address() string { return a.addr }

// Owns reports whether an output pays the account.
func (a *Account) Owns(o consensus.TxOut) bool {
	return o.Type == consensus.OutputTypeP2PK && string(o.Payload) == string(a.xonly)
}

// output is the output a coin of the account is.
func (a *Account) output(c Coin) (consensus.TxOut, error) {
	o := a.self(c.Amount)
	if c.Asset != "" {
		h, err := consensus.HashFromHex(c.Asset)
		if err != nil || h == consensus.NativeAsset {
			return consensus.TxOut{}, errors.New("a coin has a bad asset id")
		}
		o.Asset = h
	}
	return o, nil
}

// self is an output paying the account.
func (a *Account) self(amount consensus.Amount) consensus.TxOut {
	return consensus.TxOut{Amount: amount, Type: consensus.OutputTypeP2PK, Payload: append([]byte{}, a.xonly...)}
}

// spendable returns the coins of asset ("" for ARG) that can be spent at
// tip: well-formed, each outpoint once, and mining rewards only once
// mature. Largest first, then by outpoint, so selection is deterministic.
func spendable(coins []Coin, asset string, tip uint32) []Coin {
	seen := map[string]bool{}
	var out []Coin
	for _, c := range coins {
		c.TxID, c.Asset = strings.ToLower(c.TxID), strings.ToLower(c.Asset)
		if c.Asset != asset || c.Amount <= 0 || seen[c.key()] {
			continue
		}
		if _, err := c.outpoint(); err != nil {
			continue
		}
		if c.Coinbase && (c.Height == 0 || int64(tip)+1-int64(c.Height) < int64(consensus.CoinbaseMaturity)) {
			continue
		}
		seen[c.key()] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		if out[i].TxID != out[j].TxID {
			return out[i].TxID < out[j].TxID
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// Balance is what can be spent now of asset ("" for ARG) at tip.
func Balance(coins []Coin, asset string, tip uint32) consensus.Amount {
	var n consensus.Amount
	for _, c := range spendable(coins, strings.ToLower(asset), tip) {
		n += c.Amount
	}
	return n
}

// PayRequest is a payment of one coin to one address.
type PayRequest struct {
	Asset   string           // asset id, hex; "" for ARG
	To      string           // an address of the account's network
	Amount  consensus.Amount // paid to To; ignored with Max
	Max     bool             // pay everything of Asset that can be spent (for ARG, less the fee)
	FeeRate consensus.Amount // units per byte; DefaultFeeRate when below 1
	UARG    string           // the bridge's uARG id: the one asset a swap address may be paid
	// Memo, if any, rides in a NULLDATA output (at most 80 bytes): the
	// bridge's withdrawal memo, which matches a payment to a swap address
	// to its withdrawal.
	Memo string
}

// Payment is a signed transaction, checked and ready to broadcast.
type Payment struct {
	TxID   string           `json:"txid"`
	Hex    string           `json:"hex"`
	Asset  string           `json:"asset,omitempty"`
	Amount consensus.Amount `json:"amount"` // paid to the recipient
	Fee    consensus.Amount `json:"fee"`    // in ARG
	Spent  []Coin           `json:"spent"`  // the account's coins it spends
	Change []Coin           `json:"change"` // its outputs that pay the account back (height 0)
}

// Pay builds and signs a payment from the account's coins at tip.
func (a *Account) Pay(coins []Coin, tip uint32, r PayRequest) (*Payment, error) {
	asset := strings.ToLower(strings.TrimSpace(r.Asset))
	var id consensus.Hash
	if asset != "" {
		h, err := consensus.HashFromHex(asset)
		if err != nil || h == consensus.NativeAsset {
			return nil, errors.New("not an asset id")
		}
		id = h
	}
	dst, err := wallet.DecodeAddress(a.params.HRP, strings.ToLower(strings.TrimSpace(r.To)))
	if err != nil {
		return nil, errors.New("not a valid address on this network")
	}
	if dst.Type == consensus.OutputTypeP2PK && swapaddr.LooksLike(dst.Payload) {
		_, canonical := swapaddr.ParsePayload(dst.Payload)
		if asset == "" || !strings.EqualFold(asset, strings.TrimSpace(r.UARG)) || !canonical {
			return nil, fmt.Errorf("%w: only uARG can be sent to a bridge withdrawal address", ErrSwapAddr)
		}
	}
	rate := r.FeeRate
	if rate < 1 {
		rate = DefaultFeeRate
	}
	dest := consensus.TxOut{Amount: r.Amount, Type: dst.Type, Payload: dst.Payload, Asset: id}
	var extra []consensus.TxOut
	if m := strings.TrimSpace(r.Memo); m != "" {
		if len(m) > consensus.MaxNullDataLen {
			return nil, fmt.Errorf("the memo can be at most %d characters", consensus.MaxNullDataLen)
		}
		extra = append(extra, consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: []byte(m)})
	}
	var p *Payment
	if asset == "" {
		p, err = a.payARG(spendable(coins, "", tip), dest, extra, r.Max, rate)
	} else {
		p, err = a.payAsset(spendable(coins, asset, tip), spendable(coins, "", tip), dest, extra, r.Max, rate)
	}
	if err != nil {
		return nil, err
	}
	p.Asset = asset
	return p, nil
}

// draft is a transaction being sized: inputs with placeholder witnesses of
// a signature's length, so its size is the signed size.
type draft struct {
	tx    *consensus.Tx
	spent []consensus.TxOut
	coins []Coin
}

func (a *Account) draft(version uint32, coins []Coin) (*draft, error) {
	d := &draft{tx: &consensus.Tx{Version: version}}
	for _, c := range coins {
		op, err := c.outpoint()
		if err != nil {
			return nil, err
		}
		o, err := a.output(c)
		if err != nil {
			return nil, err
		}
		d.tx.Inputs = append(d.tx.Inputs, consensus.TxIn{PrevOut: op})
		d.tx.Witnesses = append(d.tx.Witnesses, make([]byte, consensus.SchnorrSigLen))
		d.spent = append(d.spent, o)
		d.coins = append(d.coins, c)
	}
	return d, nil
}

// fee is what the transaction pays at rate, with the asset surcharge.
func fee(tx *consensus.Tx, rate consensus.Amount) consensus.Amount {
	f := consensus.Amount(tx.SerializedSize()) * rate
	n := 0
	for i := range tx.Outputs {
		if tx.Outputs[i].Asset != consensus.NativeAsset {
			n++
		}
	}
	if n > freeAssetOutputs {
		f += assetOutputFee * consensus.Amount(n-freeAssetOutputs)
	}
	return f
}

// payARG pays dest (ARG) from coins, largest first, with the extra outputs
// (a memo) and change back to the account when it is above the dust limit.
func (a *Account) payARG(coins []Coin, dest consensus.TxOut, extra []consensus.TxOut, max bool, rate consensus.Amount) (*Payment, error) {
	var have consensus.Amount
	for _, c := range coins {
		have += c.Amount
	}
	if max {
		if len(coins) > maxInputs {
			coins = coins[:maxInputs]
		}
		d, err := a.draft(consensus.TxVersion, coins)
		if err != nil {
			return nil, err
		}
		var in consensus.Amount
		for _, c := range coins {
			in += c.Amount
		}
		d.tx.Outputs = append([]consensus.TxOut{dest}, extra...)
		f := fee(d.tx, rate)
		if in-f < wallet.DustLimit {
			return nil, fmt.Errorf("%w: %s ARG can be spent, less than the network fee plus the smallest payment", ErrFunds, wallet.FormatAmount(have))
		}
		d.tx.Outputs[0].Amount = in - f
		return a.finish(d, in-f, f)
	}
	if dest.Amount < wallet.DustLimit || !consensus.MoneyRange(dest.Amount) {
		return nil, fmt.Errorf("the smallest ARG payment is %s ARG", wallet.FormatAmount(wallet.DustLimit))
	}
	var sel []Coin
	var in consensus.Amount
	for _, c := range coins {
		if len(sel) >= maxInputs {
			return nil, errors.New("this payment needs too many small coins; send a smaller amount")
		}
		sel = append(sel, c)
		in += c.Amount
		if in < dest.Amount {
			continue
		}
		d, err := a.draft(consensus.TxVersion, sel)
		if err != nil {
			return nil, err
		}
		change := a.self(0)
		d.tx.Outputs = append(append([]consensus.TxOut{dest}, extra...), change)
		f := fee(d.tx, rate)
		if in-dest.Amount-f >= wallet.DustLimit {
			d.tx.Outputs[len(d.tx.Outputs)-1].Amount = in - dest.Amount - f
			return a.finish(d, dest.Amount, f)
		}
		d.tx.Outputs = append([]consensus.TxOut{dest}, extra...)
		if f = fee(d.tx, rate); in >= dest.Amount+f {
			return a.finish(d, dest.Amount, in-dest.Amount) // a remainder below dust goes to the fee
		}
	}
	return nil, fmt.Errorf("%w: %s ARG can be spent, and the payment needs %s plus the network fee", ErrFunds, wallet.FormatAmount(have), wallet.FormatAmount(dest.Amount))
}

// payAsset pays dest (an asset) from assetCoins, largest first, with the
// extra outputs (a memo) and the asset's change back to the account; the
// fee is paid in ARG from argCoins.
func (a *Account) payAsset(assetCoins, argCoins []Coin, dest consensus.TxOut, extra []consensus.TxOut, max bool, rate consensus.Amount) (*Payment, error) {
	var have consensus.Amount
	for _, c := range assetCoins {
		have += c.Amount
	}
	var sel []Coin
	var in consensus.Amount
	if max {
		if len(assetCoins) > maxInputs/2 {
			assetCoins = assetCoins[:maxInputs/2]
		}
		sel = assetCoins
		for _, c := range sel {
			in += c.Amount
		}
		dest.Amount = in
	} else {
		for _, c := range assetCoins {
			if in >= dest.Amount {
				break
			}
			sel = append(sel, c)
			in += c.Amount
		}
	}
	if !consensus.AssetRange(dest.Amount) || dest.Amount <= 0 {
		if max {
			return nil, fmt.Errorf("%w: nothing of this coin can be spent now", ErrFunds)
		}
		return nil, errors.New("enter an amount above zero")
	}
	if in < dest.Amount {
		return nil, fmt.Errorf("%w: %s can be spent, and the payment needs %s", ErrFunds, wallet.FormatAmount(have), wallet.FormatAmount(dest.Amount))
	}
	if len(sel) > maxInputs/2 {
		return nil, errors.New("this payment needs too many small coins; send a smaller amount")
	}
	fixed := append([]consensus.TxOut{dest}, extra...)
	if in > dest.Amount {
		ch := a.self(in - dest.Amount)
		ch.Asset = dest.Asset
		fixed = append(fixed, ch)
	}
	var argSel []Coin
	var argIn, argHave consensus.Amount
	for _, c := range argCoins {
		argHave += c.Amount
	}
	for _, c := range argCoins {
		if len(argSel) >= maxInputs/2 {
			break
		}
		argSel = append(argSel, c)
		argIn += c.Amount
		d, err := a.draft(consensus.TxVersionAssets, append(append([]Coin{}, sel...), argSel...))
		if err != nil {
			return nil, err
		}
		d.tx.Outputs = append(append([]consensus.TxOut{}, fixed...), a.self(0))
		f := fee(d.tx, rate)
		if argIn-f >= wallet.DustLimit {
			d.tx.Outputs[len(d.tx.Outputs)-1].Amount = argIn - f
			return a.finish(d, dest.Amount, f)
		}
		d.tx.Outputs = append([]consensus.TxOut{}, fixed...)
		if f = fee(d.tx, rate); argIn >= f {
			return a.finish(d, dest.Amount, argIn) // the whole ARG input pays the fee
		}
	}
	return nil, fmt.Errorf("%w: the fee is paid in ARG, and %s ARG can be spent", ErrFeeFunds, wallet.FormatAmount(argHave))
}

// finish signs every input, checks the transaction as a node would and
// describes it. A watch account describes it unsigned: the same coins,
// amounts, fee and transaction id (which does not cover the signatures),
// and no Hex.
func (a *Account) finish(d *draft, amount, f consensus.Amount) (*Payment, error) {
	if a.key == nil {
		txid := d.tx.TxID().String()
		return &Payment{TxID: txid, Amount: amount, Fee: f, Spent: d.coins, Change: a.owned(txid, d.tx)}, nil
	}
	for i := range d.tx.Inputs {
		if err := a.key.Sign(a.params.ChainTag, d.tx, d.spent, i); err != nil {
			return nil, err
		}
	}
	if err := consensus.CheckTxStateless(d.tx); err != nil {
		return nil, fmt.Errorf("the payment came out invalid: %w", err)
	}
	if err := consensus.VerifyTxSignatures(a.params.ChainTag, d.tx, d.spent); err != nil {
		return nil, fmt.Errorf("the payment came out invalid: %w", err)
	}
	txid := d.tx.TxID().String()
	p := &Payment{TxID: txid, Hex: hex.EncodeToString(d.tx.Serialize()), Amount: amount, Fee: f, Spent: d.coins, Change: a.owned(txid, d.tx)}
	return p, nil
}

// owned lists the outputs of tx that pay the account, as unconfirmed coins.
func (a *Account) owned(txid string, tx *consensus.Tx) []Coin {
	out := []Coin{}
	for i, o := range tx.Outputs {
		if !a.Owns(o) {
			continue
		}
		c := Coin{TxID: txid, Index: uint32(i), Amount: o.Amount}
		if o.Asset != consensus.NativeAsset {
			c.Asset = o.Asset.String()
		}
		out = append(out, c)
	}
	return out
}

// ---- swaps (docs/SWAP.md) ----

// maxQuoteInputs is how many coins a swap request offers at most; the
// maker refuses more.
const maxQuoteInputs = 50

// QuoteRequest picks the account's coins of from and asks for amount of it
// to be swapped to to; what comes back and the change pay the account.
// The swap service takes only confirmed coins, so the account's
// unconfirmed change and swap proceeds are not offered. It returns the
// request and the coins it offers.
func (a *Account) QuoteRequest(coins []Coin, tip uint32, assets map[string]consensus.Hash, from, to string, amount consensus.Amount) (*quote.QuoteRequest, []Coin, error) {
	from, to = quote.Normalize(from), quote.Normalize(to)
	if from == "" || to == "" || from == to {
		return nil, nil, errors.New("choose two different coins")
	}
	if amount <= 0 {
		return nil, nil, errors.New("enter an amount above zero")
	}
	fa, ok := assets[from]
	if _, ok2 := assets[to]; !ok || !ok2 {
		return nil, nil, errors.New("the swap service's coins are not known")
	}
	asset := ""
	if from != quote.ARG {
		if fa == consensus.NativeAsset {
			return nil, nil, errors.New("the swap service's coins are not known")
		}
		asset = fa.String()
	}
	req := &quote.QuoteRequest{From: from, To: to, Amount: amount, Receive: a.addr, Change: a.addr}
	var offered []Coin
	var have, waiting consensus.Amount
	for _, c := range spendable(coins, asset, tip) {
		if c.Height == 0 {
			waiting += c.Amount
			continue
		}
		if have >= amount || len(offered) >= maxQuoteInputs {
			continue
		}
		offered = append(offered, c)
		have += c.Amount
		req.Inputs = append(req.Inputs, quote.Outpoint{TxID: c.TxID, Index: c.Index})
	}
	if have < amount {
		if have+waiting >= amount {
			return nil, nil, fmt.Errorf("%w: %s can be swapped now, the rest arrives with the next block", ErrUnconfirmed, wallet.FormatAmount(have))
		}
		return nil, nil, fmt.Errorf("%w: %s can be swapped now", ErrFunds, wallet.FormatAmount(have))
	}
	return req, offered, nil
}

// QuoteCheck is what the account asked the maker for.
type QuoteCheck struct {
	From, To string
	Assets   map[string]consensus.Hash // quote.Assets of the maker's info, checked against the bridge's uARG
	AmountIn consensus.Amount
	MinOut   consensus.Amount // the least accepted; the quote's own amount when not above zero
	Offered  []Coin           // the coins the request offered
	Tip      uint32
	Now      time.Time // for the expiry; time.Now when zero
	// TxID is the transaction the user reviewed (PreviewQuote); SignQuote
	// signs nothing else, so what is signed is what was shown.
	TxID string
}

// QuotePreview is a quote as the wallet reads and checks it: the numbers
// the review shows, so they never come from the page's own reading of the
// swap service's answer.
type QuotePreview struct {
	TxID      string           `json:"txid"`
	AmountIn  consensus.Amount `json:"amount_in"`
	AmountOut consensus.Amount `json:"amount_out"`
	Expires   int64            `json:"expires"`
}

// Signed is the account's part of a swap: its signatures for the maker,
// and what the swap does to the account once it confirms.
type Signed struct {
	TxID      string           `json:"txid"`
	Sigs      []quote.Sig      `json:"sigs"`
	Spent     []Coin           `json:"spent"`    // the account's coins it spends
	Incoming  []Coin           `json:"incoming"` // its outputs that pay the account (height 0)
	AmountIn  consensus.Amount `json:"amount_in"`
	AmountOut consensus.Amount `json:"amount_out"`
}

// SignQuote checks a maker's quote (quote.CheckQuote: the account gives
// exactly what it asked to swap, gets at least what was quoted and nothing
// else of its leaves) and signs the account's inputs.
func (a *Account) SignQuote(q *quote.Quote, c QuoteCheck) (*Signed, error) {
	if a.key == nil {
		return nil, ErrLocked
	}
	if strings.TrimSpace(c.TxID) == "" {
		return nil, errors.New("review the swap before signing it")
	}
	tx, spent, byOut, err := a.checkQuote(q, c)
	if err != nil {
		return nil, err
	}
	if tx.TxID().String() != strings.ToLower(strings.TrimSpace(c.TxID)) {
		return nil, errors.New("the swap service changed the swap after it was reviewed; review it again")
	}
	s := &Signed{AmountIn: q.AmountIn, AmountOut: q.AmountOut}
	for _, i := range q.UserInputs {
		if err := a.key.Sign(a.params.ChainTag, tx, spent, i); err != nil {
			return nil, err
		}
		s.Sigs = append(s.Sigs, quote.Sig{Index: i, Sig: hex.EncodeToString(tx.Witnesses[i])})
		s.Spent = append(s.Spent, byOut[tx.Inputs[i].PrevOut])
	}
	s.TxID = tx.TxID().String()
	s.Incoming = a.owned(s.TxID, tx)
	return s, nil
}

// PreviewQuote runs every check SignQuote runs and reports the swap as the
// wallet reads it. A watch account can preview; only a signer can sign.
func (a *Account) PreviewQuote(q *quote.Quote, c QuoteCheck) (*QuotePreview, error) {
	tx, _, _, err := a.checkQuote(q, c)
	if err != nil {
		return nil, err
	}
	return &QuotePreview{TxID: tx.TxID().String(), AmountIn: q.AmountIn, AmountOut: q.AmountOut, Expires: q.Expires}, nil
}

// checkQuote is SignQuote's checks: the pair, the expiry, the coins
// offered, and quote.CheckQuote.
func (a *Account) checkQuote(q *quote.Quote, c QuoteCheck) (*consensus.Tx, []consensus.TxOut, map[consensus.OutPoint]Coin, error) {
	from, to := quote.Normalize(c.From), quote.Normalize(c.To)
	if from == "" || to == "" || quote.Normalize(q.From) != from || quote.Normalize(q.To) != to {
		return nil, nil, nil, errors.New("the swap service quoted another pair")
	}
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	if now.Unix() >= q.Expires-2 {
		return nil, nil, nil, errors.New("the quote expired; review the swap again")
	}
	fa, ok1 := c.Assets[from]
	ta, ok2 := c.Assets[to]
	if !ok1 || !ok2 {
		return nil, nil, nil, errors.New("the swap service's coins are not known")
	}
	offered := map[consensus.OutPoint]consensus.TxOut{}
	byOut := map[consensus.OutPoint]Coin{}
	for _, k := range c.Offered {
		op, err := k.outpoint()
		if err != nil {
			return nil, nil, nil, err
		}
		o, err := a.output(k)
		if err != nil {
			return nil, nil, nil, err
		}
		offered[op], byOut[op] = o, k
	}
	minOut := c.MinOut
	if minOut <= 0 {
		minOut = q.AmountOut
	}
	if q.AmountOut < minOut {
		return nil, nil, nil, fmt.Errorf("the swap service offers %s, less than the %s its posted price gives; nothing was signed", wallet.FormatAmount(q.AmountOut), wallet.FormatAmount(minOut))
	}
	tx, spent, err := quote.CheckQuote(q, quote.Expect{From: fa, To: ta, AmountIn: c.AmountIn, MinOut: minOut, Coins: offered, Owns: a.Owns, Tip: c.Tip})
	if err != nil {
		return nil, nil, nil, err
	}
	return tx, spent, byOut, nil
}
