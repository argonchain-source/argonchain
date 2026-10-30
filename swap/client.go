package swap

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/swap/quote"
	"argonmine/wallet"
)

// Client talks to a maker.
type Client struct {
	URL string
	HC  *http.Client
}

// NewClient returns a client for the maker at url.
func NewClient(url string) *Client {
	return &Client{URL: strings.TrimRight(url, "/"), HC: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return errors.New("the swap service cannot be reached")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("the swap service answered %d", resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}

// Info reads the maker's prices, limits and asset ids.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	return &i, c.do(ctx, http.MethodGet, "/api/info", nil, &i)
}

// Quote asks for a quote.
func (c *Client) Quote(ctx context.Context, req *QuoteRequest) (*Quote, error) {
	var q Quote
	return &q, c.do(ctx, http.MethodPost, "/api/quote", req, &q)
}

// Swap completes a quote.
func (c *Client) Swap(ctx context.Context, req *SwapRequest) (*SwapResult, error) {
	var r SwapResult
	return &r, c.do(ctx, http.MethodPost, "/api/swap", req, &r)
}

// Assets maps the three coins to their asset ids, from what the wallet
// trusts: the bridge's uARG id, and the maker's issuer key for wARG (whose
// id is recomputed, never taken from the maker as given).
func Assets(info *Info, genuineUARG string) (map[string]consensus.Hash, error) {
	return quote.Assets(info, genuineUARG)
}

// Prepared is a quote the wallet checked and can sign.
type Prepared struct {
	Quote *Quote
	tx    *consensus.Tx
	spent []consensus.TxOut
	w     *wallet.Wallet
	c     *Client
}

// Prepare picks the wallet's coins of from, asks the maker for a quote for
// amount of it, and checks the quote (CheckQuote). minOut, if positive, is
// the least the caller accepts; the quote's own amount is checked in any
// case.
func Prepare(ctx context.Context, w *wallet.Wallet, c *Client, assets map[string]consensus.Hash, from, to string, amount, minOut consensus.Amount) (*Prepared, error) {
	from, to = Normalize(from), Normalize(to)
	if from == "" || to == "" || from == to {
		return nil, errors.New("choose two different coins")
	}
	fa, ta := assets[from], assets[to]
	if (from != ARG && fa == consensus.NativeAsset) || (to != ARG && ta == consensus.NativeAsset) {
		return nil, errors.New("the swap service's coins are not known")
	}
	asset := ""
	if from != ARG {
		asset = fa.String()
	}
	coins, err := w.SpendableCoins(asset)
	if err != nil {
		return nil, err
	}
	offered := map[consensus.OutPoint]consensus.TxOut{}
	var req QuoteRequest
	var have consensus.Amount
	for _, k := range coins {
		if have >= amount || len(req.Inputs) >= 50 {
			break
		}
		h, err := consensus.HashFromHex(k.TxID)
		if err != nil {
			return nil, err
		}
		offered[consensus.OutPoint{TxID: h, Index: k.Index}] = k.Out
		req.Inputs = append(req.Inputs, Outpoint{TxID: k.TxID, Index: k.Index})
		have += k.Amount
	}
	if have < amount {
		return nil, fmt.Errorf("not enough %s: %s can be spent now", displayName(from), wallet.FormatAmount(have))
	}
	if req.Receive, err = w.ReceiveAddress(); err != nil {
		return nil, err
	}
	if req.Change, err = w.ChangeAddress(); err != nil {
		return nil, err
	}
	req.From, req.To, req.Amount = from, to, amount
	q, err := c.Quote(ctx, &req)
	if err != nil {
		return nil, err
	}
	if Normalize(q.From) != from || Normalize(q.To) != to {
		return nil, errors.New("the swap service quoted another pair")
	}
	if minOut <= 0 {
		minOut = q.AmountOut
	}
	tip, err := tipOf(w)
	if err != nil {
		return nil, err
	}
	tx, spent, err := CheckQuote(q, Expect{From: fa, To: ta, AmountIn: amount, MinOut: minOut, Coins: offered, Owns: w.Owns, Tip: tip})
	if err != nil {
		return nil, err
	}
	return &Prepared{Quote: q, tx: tx, spent: spent, w: w, c: c}, nil
}

func tipOf(w *wallet.Wallet) (uint32, error) {
	h := w.SyncedHeight()
	if h < 0 {
		return 0, errors.New("the wallet has not synced yet")
	}
	return uint32(h), nil
}

func displayName(s string) string {
	switch s {
	case UARG:
		return "uARG"
	case WARG:
		return "wARG"
	}
	return s
}

// Execute signs the wallet's inputs, hands the signatures to the maker,
// checks that the maker broadcast exactly the transaction that was checked,
// and records it in the wallet. It returns the transaction id.
func (p *Prepared) Execute(ctx context.Context) (string, error) {
	if time.Now().Unix() >= p.Quote.Expires-2 {
		return "", errors.New("the quote expired; review the swap again")
	}
	if err := p.w.SignInputs(p.tx, p.spent, p.Quote.UserInputs); err != nil {
		return "", err
	}
	want := p.tx.TxID().String()
	req := &SwapRequest{QuoteID: p.Quote.ID}
	for _, i := range p.Quote.UserInputs {
		req.Sigs = append(req.Sigs, Sig{Index: i, Sig: hex.EncodeToString(p.tx.Witnesses[i])})
	}
	res, err := p.c.Swap(ctx, req)
	if err != nil {
		return "", err
	}
	if res.TxID != want {
		return "", fmt.Errorf("the swap service reported transaction %s, not the one signed (%s)", res.TxID, want)
	}
	if err := p.w.RecordSpend(want, p.tx, p.spent); err != nil {
		return want, err
	}
	return want, nil
}
