package litewallet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"argonmine/consensus"
	"argonmine/swap/quote"
)

// reviewCQuote builds, offline, a maker's quote of 10 ARG for out units of
// uARG to account a (CheckQuote does not look at the maker's inputs, so no
// node is needed), with the assets and the check the app would use.
func reviewCQuote(t *testing.T, a *Account, out consensus.Amount) (*quote.Quote, QuoteCheck) {
	t.Helper()
	rnd := func() consensus.Hash { var h consensus.Hash; rand.Read(h[:]); return h }
	uarg := rnd()
	var issuer [32]byte
	copy(issuer[:], a.xonly) // any valid x-only key will do for the wARG id
	info := &quote.Info{UARG: uarg.String(), Issuer: hex.EncodeToString(issuer[:]), WARG: consensus.AssetID(issuer, quote.WARGSymbol).String()}
	assets, err := quote.Assets(info, uarg.String())
	if err != nil {
		t.Fatal(err)
	}
	makerPayload := append([]byte{}, a.xonly...) // shape only; the maker's own key is never checked
	makerPayload[31] ^= 1

	coin := Coin{TxID: rnd().String(), Index: 0, Amount: 10 * unit, Height: 5}
	op, _ := coin.outpoint()
	makerOp := consensus.OutPoint{TxID: rnd(), Index: 1}
	makerU := consensus.TxOut{Amount: 100 * unit, Type: consensus.OutputTypeP2PK, Payload: makerPayload, Asset: uarg}
	tx := &consensus.Tx{Version: consensus.TxVersionAssets,
		Inputs: []consensus.TxIn{{PrevOut: op}, {PrevOut: makerOp}},
		Outputs: []consensus.TxOut{
			{Amount: out, Type: consensus.OutputTypeP2PK, Payload: a.xonly, Asset: uarg},                 // what the wallet gets
			{Amount: 10 * unit, Type: consensus.OutputTypeP2PK, Payload: makerPayload},                   // the maker gets the 10 ARG
			{Amount: 100*unit - out, Type: consensus.OutputTypeP2PK, Payload: makerPayload, Asset: uarg}, // its uARG change
		},
		Witnesses: [][]byte{nil, nil}}
	q := &quote.Quote{ID: "x", Expires: time.Now().Unix() + 30, From: quote.ARG, To: quote.UARG, AmountIn: 10 * unit, AmountOut: out, Rate: "0.099",
		Tx: hex.EncodeToString(tx.Serialize()), UserInputs: []int{0},
		Spent: []quote.SpentOut{quote.SpentJSON(a.self(10 * unit)), quote.SpentJSON(makerU)}}
	return q, QuoteCheck{From: quote.ARG, To: quote.UARG, Assets: assets, AmountIn: 10 * unit, Offered: []Coin{coin}, Tip: 100}
}

// Security review C (2026-10-28): the app passes a floor from the posted
// rate (minOut), so a maker quoting far less is refused, and the wallet
// signs only the transaction the user reviewed (PreviewQuote's txid).
func TestQuoteFloorAndReviewedTx(t *testing.T) {
	a := newAccount(t)
	q, check := reviewCQuote(t, a, 1)
	check.MinOut = 99_000_000 // 10 ARG x 0.10 x 0.99
	if _, err := a.PreviewQuote(q, check); err == nil || !strings.Contains(err.Error(), "less than") {
		t.Fatalf("a quote below the posted rate was previewed: %v", err)
	}
	if _, err := a.SignQuote(q, check); err == nil {
		t.Fatal("a quote below the posted rate was signed")
	}

	fair, check2 := reviewCQuote(t, a, 99_000_000)
	check2.MinOut = 99_000_000
	pv, err := a.PreviewQuote(fair, check2)
	if err != nil || pv.AmountOut != 99_000_000 || pv.AmountIn != 10*unit {
		t.Fatalf("preview: %+v %v", pv, err)
	}
	if _, err := a.SignQuote(fair, check2); err == nil {
		t.Fatal("signed without the reviewed transaction id")
	}
	wrong := check2
	wrong.TxID = strings.Repeat("ab", 32)
	if _, err := a.SignQuote(fair, wrong); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("signed another transaction than the one reviewed: %v", err)
	}
	check2.TxID = pv.TxID
	s, err := a.SignQuote(fair, check2)
	if err != nil || s.TxID != pv.TxID {
		t.Fatalf("the reviewed swap was not signed: %+v %v", s, err)
	}
}

// Security review C (2026-10-28): what the review shows comes from the
// wallet's own reading of the quote, so a reply with keys in two cases
// cannot show one amount and sign another; quote.CheckKeys refuses it
// before the wallet decodes it at all.
func TestQuoteKeyCaseCannotSplitDisplayAndSignature(t *testing.T) {
	a := newAccount(t)
	q, check := reviewCQuote(t, a, 1) // the transaction pays 1 unit
	shown := *q
	shown.AmountOut = 99_000_000
	b, _ := json.Marshal(&shown)
	served := strings.TrimSuffix(string(b), "}") + `,"AMOUNT_OUT":1}`
	if err := quote.CheckKeys([]byte(served)); err == nil {
		t.Fatal("a quote with AMOUNT_OUT next to amount_out passed the key check")
	}
	// Were it decoded anyway, the review would show what the wallet reads.
	var got quote.Quote
	if err := json.Unmarshal([]byte(served), &got); err != nil {
		t.Fatal(err)
	}
	pv, err := a.PreviewQuote(&got, check)
	if err == nil && pv.AmountOut != 1 {
		t.Fatalf("the preview shows %d units for a transaction that pays 1", pv.AmountOut)
	}
}
