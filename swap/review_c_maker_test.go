package swap

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
)

// Review C: Swap drops a quote (and with it the maker's coin locks and the
// ARG it reserves) before it broadcasts the transaction, and the maker's
// wallet only marks those coins spent (RecordSpend) after the node answered.
// A quote asked for in between sees the in-flight swap's ARG as free and its
// coins as unlocked, so it can pass the reserve check that protects the ARG
// behind wARG. Two swaps then pay out more ARG than was free, and the maker
// ends up holding less ARG than the wARG in circulation.
//
// The test makes the window deterministic with an RPC proxy that holds the
// maker's first sendrawtransaction; in production the window is the node
// round trip, which an attacker who sends both requests can aim at.
func TestMakerReserveHeldWhileSwapInFlight(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// An RPC proxy in front of the node that can hold one sendrawtransaction.
	nodeURL := reflect.ValueOf(e.node).Elem().FieldByName("url").String()
	var armed atomic.Bool
	held, release := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"sendrawtransaction"`)) && armed.CompareAndSwap(true, false) {
			close(held)
			<-release
		}
		req, _ := http.NewRequest(http.MethodPost, nodeURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	m, err := New(Config{Params: consensus.RegTest, Node: wallet.NewClient(proxy.URL, "t"), Wallet: e.maker, UARG: e.uarg, Price: big.NewRat(1, 10), SpreadBps: 100,
		MaxIn: map[string]consensus.Amount{UARG: 1000 * unit}, LedgerPath: filepath.Join(t.TempDir(), "ledger.json"), Now: e.clk.now, Logf: t.Logf, RatePerSec: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	c := NewClient(srv.URL)
	info, err := m.Info()
	if err != nil {
		t.Fatal(err)
	}
	assets, err := Assets(info, e.uarg.String())
	if err != nil {
		t.Fatal(err)
	}

	// The user wraps 185 ARG: the maker now holds 185 ARG that back 185 wARG.
	p, err := Prepare(ctx, e.user, c, assets, ARG, WARG, 185*unit, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.sink)
	e.sync()
	// The user also splits its uARG into two coins (20 and 180).
	userAddr, _ := e.user.ReceiveAddress()
	if _, err := e.user.SendAsset(e.uarg.String(), userAddr, 20*unit, 1); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.sink)
	e.sync()
	supply := m.wargSupply()
	before := e.bal(e.maker, ARG)
	t.Logf("maker: %s ARG, wARG out: %s, free: %s", wallet.FormatAmount(before), wallet.FormatAmount(supply), wallet.FormatAmount(before-supply))

	ucoins, err := e.user.SpendableCoins(e.uarg.String())
	if err != nil || len(ucoins) != 2 {
		t.Fatalf("user uARG coins: %v %v", ucoins, err)
	}
	var small, large wallet.Coin
	for _, k := range ucoins {
		if k.Amount == 20*unit {
			small = k
		} else {
			large = k
		}
	}
	recv, _ := e.user.ReceiveAddress()
	change, _ := e.user.ChangeAddress()
	ask := func(k wallet.Coin, amount consensus.Amount, ip string) (*Quote, error) {
		return m.Quote(&QuoteRequest{From: UARG, To: ARG, Amount: amount, Inputs: []Outpoint{{TxID: k.TxID, Index: k.Index}}, Receive: recv, Change: change}, ip)
	}
	sign := func(q *Quote) *SwapRequest {
		tx, spent, err := q.Decode()
		if err != nil {
			t.Fatal(err)
		}
		if err := e.user.SignInputs(tx, spent, q.UserInputs); err != nil {
			t.Fatal(err)
		}
		r := &SwapRequest{QuoteID: q.ID}
		for _, i := range q.UserInputs {
			r.Sigs = append(r.Sigs, Sig{Index: i, Sig: hex.EncodeToString(tx.Witnesses[i])})
		}
		return r
	}
	makerIns := func(q *Quote) map[consensus.OutPoint]bool {
		tx, _, _ := q.Decode()
		mine := map[int]bool{}
		for _, i := range q.UserInputs {
			mine[i] = true
		}
		out := map[consensus.OutPoint]bool{}
		for i, in := range tx.Inputs {
			if !mine[i] {
				out[in.PrevOut] = true
			}
		}
		return out
	}

	// q0 holds the maker's largest coin; q1 buys 160.38 ARG with other coins.
	q0, err := ask(small, 510_000_000, "a") // 5.1 uARG -> 50.49 ARG
	if err != nil {
		t.Fatal(err)
	}
	q1, err := ask(large, 1_620_000_000, "a") // 16.2 uARG -> 160.38 ARG
	if err != nil {
		t.Fatal(err)
	}
	// q0 is let go (one failed attempt drops a quote).
	if _, err := m.Swap(&SwapRequest{QuoteID: q0.ID}); err == nil {
		t.Fatal("an unsigned swap went through")
	}
	// What is free once q1 has paid: a second 160.38 ARG must be refused.
	t.Logf("free after q1 pays: about %s ARG; q2 needs %s", wallet.FormatAmount(before-supply-q1.AmountOut), wallet.FormatAmount(q1.AmountOut))

	r1 := sign(q1)
	armed.Store(true)
	done := make(chan error, 1)
	go func() { _, err := m.Swap(r1); done <- err }()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the maker never broadcast q1")
	}
	// While q1 is in flight: the same purchase again.
	q2, err := ask(small, 1_620_000_000, "b")
	close(release)
	if err1 := <-done; err1 != nil {
		t.Fatalf("q1: %v", err1)
	}
	if err != nil {
		t.Logf("q2 refused while q1 is in flight (%v)", err)
		return
	}
	for op := range makerIns(q2) {
		if makerIns(q1)[op] {
			t.Logf("q2 reuses q1's coin %s: it would fail at broadcast", op)
		}
	}
	if _, err := m.Swap(sign(q2)); err != nil {
		t.Fatalf("q2: %v", err)
	}
	e.mine(1, e.sink)
	e.sync()
	after := e.bal(e.maker, ARG)
	t.Logf("after both swaps: maker holds %s ARG behind %s wARG", wallet.FormatAmount(after), wallet.FormatAmount(m.wargSupply()))
	if after < m.wargSupply() {
		t.Fatalf("wARG is under-backed by %s ARG", wallet.FormatAmount(m.wargSupply()-after))
	}
}
