// Package swap is the native swap service (docs/SWAP.md): a maker that
// trades ARG, wARG and uARG with wallets on the Argonmine chain, each trade
// one transaction that both sides sign, and the checks a wallet runs before
// it signs its part.
//
// A swap goes like this. The wallet picks coins of what it pays and asks
// for a quote. The maker builds the whole transaction, the wallet's coins
// and its own as inputs, what each side gets as outputs, and the network
// fee paid by the maker, and returns it unsigned. The wallet checks the
// transaction against its own records (CheckQuote): it gives exactly the
// amount it asked to swap, gets at least the quoted amount, and nothing
// else of its leaves. It signs its inputs; every signature covers every
// input and output (SPEC 6.2), so it is worth nothing for any other
// transaction. The maker checks those signatures, signs its own inputs and
// broadcasts. Either the whole swap confirms or none of it does.
//
// wARG is ARG held by the maker, one for one: the maker issues wARG for ARG
// it receives (the issuance is in the swap transaction) and burns the wARG
// it receives back. It never pays out ARG that backs wARG in circulation.
//
// The wire format and the wallet's check live in package swap/quote, which
// the browser wallet shares; they keep their names here.
package swap

import (
	"argonmine/consensus"
	"argonmine/swap/quote"
)

// The three coins of the Argonmine chain, as the protocol names them.
const (
	ARG  = quote.ARG
	UARG = quote.UARG
	WARG = quote.WARG
)

// WARGSymbol is the symbol of the maker's wARG issuance.
const WARGSymbol = quote.WARGSymbol

// The messages of the swap protocol (package swap/quote).
type (
	Info         = quote.Info
	Pair         = quote.Pair
	Outpoint     = quote.Outpoint
	QuoteRequest = quote.QuoteRequest
	SpentOut     = quote.SpentOut
	Quote        = quote.Quote
	Sig          = quote.Sig
	SwapRequest  = quote.SwapRequest
	SwapResult   = quote.SwapResult
)

// Normalize maps a coin name in any case ("uarg", "wARG") to its protocol
// name, or "" when it is not one of the three.
func Normalize(s string) string { return quote.Normalize(s) }

func spentJSON(o consensus.TxOut) SpentOut { return quote.SpentJSON(o) }
