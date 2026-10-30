package main

// Native swaps (docs/SWAP.md): ARG, wARG and uARG traded with the swap
// service on the Argonmine chain, one transaction per swap that the wallet
// and the service both sign. The wallet signs only after swap.CheckQuote
// has found that the transaction takes exactly the amount being swapped and
// pays at least the amount quoted, and only with the password.

import (
	"context"
	"errors"
	"flag"
	"strings"
	"sync"
	"time"

	"argonmine/consensus"
	"argonmine/swap"
	"argonmine/wallet"
)

var swapURL = flag.String("swap-url", "", "swap service URL for ARG, wARG and uARG swaps (empty: native swaps are off)")

var nativeSwaps struct {
	mu       sync.Mutex
	prepared map[string]*swap.Prepared
	info     *swap.Info
	infoAt   time.Time
	warg     string // the genuine wARG asset id, from the last info that checked out; "" until then
}

// wargID is the genuine wARG asset id as the swap service last named it
// (and swap.Assets recomputed it from the issuer), or "" when unknown. Only
// that id is shown as wARG; any other asset calling itself WARG is not.
func wargID() string {
	nativeSwaps.mu.Lock()
	defer nativeSwaps.mu.Unlock()
	return nativeSwaps.warg
}

// swapClient is the swap service, or an error when none is configured.
func swapClient() (*swap.Client, error) {
	if strings.TrimSpace(*swapURL) == "" {
		return nil, errors.New("swaps between ARG, wARG and uARG are not available in this build")
	}
	return swap.NewClient(*swapURL), nil
}

// swapNativeInfo answers swap/native-info: the service's pairs with their
// rates, limits and what it can pay now, and the asset ids, checked against
// the bridge's uARG and recomputed for wARG.
func (a *app) swapNativeInfo() (any, error) {
	info, _, err := a.nativeInfo(false)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (a *app) nativeInfo(fresh bool) (*swap.Info, map[string]consensus.Hash, error) {
	c, err := swapClient()
	if err != nil {
		return nil, nil, err
	}
	nativeSwaps.mu.Lock()
	info, at := nativeSwaps.info, nativeSwaps.infoAt
	nativeSwaps.mu.Unlock()
	if fresh || info == nil || time.Since(at) > 15*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if info, err = c.Info(ctx); err != nil {
			return nil, nil, err
		}
		nativeSwaps.mu.Lock()
		nativeSwaps.info, nativeSwaps.infoAt = info, time.Now()
		nativeSwaps.mu.Unlock()
	}
	if info.Network != a.params.Name {
		return nil, nil, errors.New("the swap service is for another network")
	}
	genuine, _ := a.uargStatus()
	assets, err := swap.Assets(info, genuine)
	if err != nil {
		return nil, nil, err
	}
	nativeSwaps.mu.Lock()
	nativeSwaps.warg = strings.ToLower(assets[swap.WARG].String())
	nativeSwaps.mu.Unlock()
	return info, assets, nil
}

// swapNativePrepare answers swap/native-prepare: it gets a binding quote
// for amount of from, with the wallet's own coins, and checks it. Nothing
// is signed yet; the answer is what the review screen shows.
func (a *app) swapNativePrepare(from, to, amount string) (any, error) {
	amt, err := wallet.ParseAmount(amount)
	if err != nil || amt <= 0 {
		return nil, errors.New("enter a valid amount, for example 1.25")
	}
	_, assets, err := a.nativeInfo(true)
	if err != nil {
		return nil, err
	}
	c, _ := swapClient()
	return a.withWallet(func(w *wallet.Wallet) (any, error) {
		if _, err := w.Sync(); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		p, err := swap.Prepare(ctx, w, c, assets, from, to, amt, 0)
		if err != nil {
			return nil, err
		}
		nativeSwaps.mu.Lock()
		if nativeSwaps.prepared == nil {
			nativeSwaps.prepared = map[string]*swap.Prepared{}
		}
		for id, old := range nativeSwaps.prepared {
			if time.Now().Unix() > old.Quote.Expires {
				delete(nativeSwaps.prepared, id)
			}
		}
		nativeSwaps.prepared[p.Quote.ID] = p
		nativeSwaps.mu.Unlock()
		q := p.Quote
		return map[string]any{"quote_id": q.ID, "from": q.From, "to": q.To, "amount_in": wallet.FormatAmount(q.AmountIn),
			"amount_out": wallet.FormatAmount(q.AmountOut), "rate": q.Rate, "expires": q.Expires, "network_fee": "0"}, nil
	})
}

// swapNativeExecute answers swap/native-execute: with the password, it
// signs the wallet's inputs of a prepared quote and completes the swap.
func (a *app) swapNativeExecute(quoteID, password string) (any, error) {
	nativeSwaps.mu.Lock()
	p, ok := nativeSwaps.prepared[quoteID]
	delete(nativeSwaps.prepared, quoteID)
	nativeSwaps.mu.Unlock()
	if !ok {
		return nil, errors.New("this quote is no longer available; review the swap again")
	}
	if err := a.verifyPassword(password); err != nil {
		nativeSwaps.mu.Lock()
		nativeSwaps.prepared[quoteID] = p // a wrong password does not use up the quote
		nativeSwaps.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	txid, err := p.Execute(ctx)
	if err != nil {
		return nil, err
	}
	q := p.Quote
	return map[string]any{"txid": txid, "from": q.From, "to": q.To, "amount_in": wallet.FormatAmount(q.AmountIn), "amount_out": wallet.FormatAmount(q.AmountOut)}, nil
}

// swapNativeStatus answers swap/native-status: how far a swap's transaction
// is, from this app's own node and wallet: its block height (0 while it
// waits in the mempool) and the chain's height.
func (a *app) swapNativeStatus(txid string) (any, error) {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if len(txid) != 64 || strings.Trim(txid, "0123456789abcdef") != "" {
		return nil, errors.New("not a transaction id")
	}
	a.mu.Lock()
	tip := a.ch.Tip().Height
	a.mu.Unlock()
	out := map[string]any{"txid": txid, "height": uint32(0), "tip": tip, "confirmations": 0}
	_, err := a.withWallet(func(w *wallet.Wallet) (any, error) {
		for _, h := range w.History() {
			if h.TxID == txid && h.Height > 0 {
				out["height"] = h.Height
				if tip >= h.Height {
					out["confirmations"] = int(tip-h.Height) + 1
				}
			}
		}
		return nil, nil
	})
	return out, err
}
