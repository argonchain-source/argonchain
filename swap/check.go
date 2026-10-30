package swap

import (
	"argonmine/consensus"
	"argonmine/swap/quote"
)

// argDust is the most ARG a swap may take beyond the amount asked: a change
// smaller than the dust limit cannot be paid back, so the maker keeps it.
const argDust = quote.ArgDust

// Expect is what a wallet asked for, and what it knows about its own coins.
type Expect = quote.Expect

// CheckQuote decodes a quote's transaction and checks that signing the
// wallet's inputs in it is safe (see quote.CheckQuote). It returns the
// transaction and the outputs its inputs spend.
func CheckQuote(q *Quote, e Expect) (*consensus.Tx, []consensus.TxOut, error) {
	return quote.CheckQuote(q, e)
}
