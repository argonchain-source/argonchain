package swap

import (
	"context"
	"strings"
	"testing"

	"argonmine/consensus"
	"argonmine/litewallet"
)

// The browser wallet (package litewallet, built to WebAssembly) swaps with
// the maker like the other wallets: it picks its coins, checks the quote,
// signs its inputs, and the maker completes and broadcasts. Every pair the
// app offers, and a quote that pays it less is refused.
func TestBrowserWalletSwaps(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	acct, err := litewallet.FromMnemonic(consensus.RegTest, e.user.Mnemonic())
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := e.user.ReceiveAddress()
	if acct.Address() != addr {
		t.Fatalf("the browser wallet derives %s, the wallet %s", acct.Address(), addr)
	}
	assets := e.assets()
	coins := func() ([]litewallet.Coin, uint32) {
		e.sync()
		var out []litewallet.Coin
		for _, u := range e.user.UTXOs() {
			if u.Address == addr && u.LockedBy == "" {
				out = append(out, litewallet.Coin{TxID: u.TxID, Index: u.Index, Amount: u.Amount, Asset: u.Asset, Height: u.Height, Coinbase: u.Coinbase})
			}
		}
		return out, uint32(e.user.SyncedHeight())
	}
	for _, c := range []struct {
		from, to string
		amount   consensus.Amount
	}{
		{ARG, WARG, 3 * unit},
		{ARG, UARG, 10 * unit},
		{WARG, UARG, 2 * unit},
		{UARG, ARG, unit},
		{UARG, WARG, unit / 2},
		{WARG, ARG, unit},
	} {
		have, tip := coins()
		before := e.bal(e.user, c.to)
		req, offered, err := acct.QuoteRequest(have, tip, assets, c.from, c.to, c.amount)
		if err != nil {
			t.Fatalf("%s to %s: %v", c.from, c.to, err)
		}
		q, err := e.c.Quote(ctx, req)
		if err != nil {
			t.Fatalf("%s to %s: quote: %v", c.from, c.to, err)
		}
		check := litewallet.QuoteCheck{From: c.from, To: c.to, Assets: assets, AmountIn: c.amount, Offered: offered, Tip: tip}
		pv, err := acct.PreviewQuote(q, check)
		if err != nil {
			t.Fatalf("%s to %s: the browser wallet refused the maker's quote: %v", c.from, c.to, err)
		}
		check.TxID = pv.TxID
		s, err := acct.SignQuote(q, check)
		if err != nil {
			t.Fatalf("%s to %s: the browser wallet refused the maker's quote: %v", c.from, c.to, err)
		}
		res, err := e.c.Swap(ctx, &SwapRequest{QuoteID: q.ID, Sigs: s.Sigs})
		if err != nil {
			t.Fatalf("%s to %s: swap: %v", c.from, c.to, err)
		}
		if res.TxID != s.TxID {
			t.Fatalf("%s to %s: maker broadcast %s, wallet signed %s", c.from, c.to, res.TxID, s.TxID)
		}
		var in consensus.Amount
		for _, k := range s.Incoming {
			if strings.EqualFold(k.Asset, assetHex(assets[c.to])) {
				in += k.Amount
			}
		}
		if in < q.AmountOut {
			t.Fatalf("%s to %s: the wallet expects %d back, quoted %d", c.from, c.to, in, q.AmountOut)
		}
		e.mine(1, e.sink)
		e.sync()
		if got := e.bal(e.user, c.to) - before; got < q.AmountOut {
			t.Fatalf("%s to %s: received %d, quoted %d", c.from, c.to, got, q.AmountOut)
		}
	}

	// A quote whose transaction pays the wallet one unit less than it says.
	have, tip := coins()
	req, offered, err := acct.QuoteRequest(have, tip, assets, UARG, ARG, unit)
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.c.Quote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	short := retx(t, q, func(tx *consensus.Tx, _ *Quote) {
		for i := range tx.Outputs {
			if acct.Owns(tx.Outputs[i]) && tx.Outputs[i].Asset == consensus.NativeAsset {
				tx.Outputs[i].Amount--
				return
			}
		}
	})
	if _, err := acct.PreviewQuote(short, litewallet.QuoteCheck{From: UARG, To: ARG, Assets: assets, AmountIn: unit, Offered: offered, Tip: tip}); err == nil {
		t.Fatal("the browser wallet signed a quote that pays it less than quoted")
	}
}

func assetHex(h consensus.Hash) string {
	if h == consensus.NativeAsset {
		return ""
	}
	return h.String()
}

// With SplitUARG the maker keeps its uARG in several coins, so two swaps
// paying uARG go through in the same block instead of the second waiting
// for the first one's change to confirm.
func TestMakerSplitsUARG(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	assets := e.assets()
	swapTwice := func() error {
		for i := 0; i < 2; i++ {
			p, err := Prepare(ctx, e.user, e.c, assets, ARG, UARG, 10*unit, 0)
			if err != nil {
				return err
			}
			if _, err := p.Execute(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	// One uARG coin: the second payout in the block has nothing to pay with.
	if err := swapTwice(); err == nil {
		t.Fatal("two uARG payouts from one coin in one block")
	}
	e.mine(1, e.sink)
	e.sync()
	e.m.cfg.SplitUARG = 50 * unit
	e.m.splitUARG()
	e.mine(1, e.sink)
	e.sync()
	coins, err := e.maker.SpendableCoins(e.uarg.String())
	if err != nil || len(coins) < 3 {
		t.Fatalf("the maker holds %d uARG coins after the split (%v)", len(coins), err)
	}
	if err := swapTwice(); err != nil {
		t.Fatalf("two swaps paying uARG in one block: %v", err)
	}
	if ids, _ := e.node.RawMempool(); len(ids) != 2 {
		t.Fatalf("mempool %v, want both swaps", ids)
	}
}
