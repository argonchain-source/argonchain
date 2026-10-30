package bridge

// Withdrawals (withdraw.go): a payout's swap address with a memo, and the
// progress of the payments to it, from the mempool to the payout.

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"argonmine/consensus"
)

func TestWithdrawCodes(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"K7M2-X9RT": "K7M2X9RT", "k7m2x9rt": "K7M2X9RT", " K7M2 X9RT\n": "K7M2X9RT",
		"K7M2-X9R": "", "K7M2-X9RT0": "", "K7M2-X9R0": "", "K7M2-X9RI": "", "": "",
	} {
		if got := ParseWithdrawCode(in); got != want {
			t.Errorf("ParseWithdrawCode(%q) = %q, want %q", in, got, want)
		}
	}
	if FormatWithdrawCode("K7M2X9RT") != "K7M2-X9RT" {
		t.Fatal("format")
	}
	if withdrawMemo([][]byte{[]byte("hello"), []byte("k7m2-x9rt")}) != "K7M2X9RT" || withdrawMemo([][]byte{{0x42, 0x52, 0x47, 0x31}}) != "" {
		t.Fatal("memo among payloads")
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := newWithdrawCode()
		if err != nil || ParseWithdrawCode(c) != c || seen[c] {
			t.Fatalf("code %q: %v", c, err)
		}
		seen[c] = true
	}
}

func TestWithdrawalProgress(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	c := e.chains[0]
	e.issueDeposit(c.Tokens[0].Address, usd(100, 6)) // 99 uARG to the user
	unit := consensus.Amount(consensus.UnitsPerCoin)
	asset := e.leader.Asset()
	var payee [20]byte
	payee[19] = 0xB7

	post := func(body string) (int, WithdrawalView, string) {
		t.Helper()
		resp, err := http.Post(e.urls[0]+"/api/withdrawals", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var v WithdrawalView
		json.Unmarshal(b, &v)
		return resp.StatusCode, v, string(b)
	}
	get := func(code string) WithdrawalView {
		t.Helper()
		resp, err := http.Get(e.urls[0] + "/api/withdrawal/" + code)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v WithdrawalView
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&v) != nil {
			t.Fatalf("GET withdrawal %s: %d", code, resp.StatusCode)
		}
		return v
	}
	req := func(amount string, to [20]byte) string {
		return fmt.Sprintf(`{"chain_id":%d,"token":"usdc","to":"%s","amount":"%s"}`, c.ID, HexAddress(to), amount)
	}

	// Requests the page would refuse are refused here too.
	for body, why := range map[string]string{
		req("0.5", payee): "smallest withdrawal",
		req("0", payee):   "amount",
		fmt.Sprintf(`{"chain_id":999,"token":"usdc","to":"%s","amount":"5"}`, HexAddress(payee)):     "chain 999",
		fmt.Sprintf(`{"chain_id":%d,"token":"dai","to":"%s","amount":"5"}`, c.ID, HexAddress(payee)): "not paid out",
		req("5", c.Vault):                    "own contracts",
		req("5", [20]byte{}):                 "EVM address",
		`{"chain_id":1,"token":"usdc","to":`: "JSON",
	} {
		if code, _, b := post(body); code != 400 || !strings.Contains(b, why) {
			t.Fatalf("%s: %d %s, want %q", body, code, b, why)
		}
	}
	// Only as JSON: text/plain (a form or a page elsewhere, with no CORS
	// preflight) and form encodings are refused.
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		r, _ := http.NewRequest(http.MethodPost, e.urls[0]+"/api/withdrawals", strings.NewReader(req("10", payee)))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("POST as %q: %d", ct, resp.StatusCode)
		}
	}
	// Security headers, on the page and on the API.
	for _, path := range []string{"/", "/owner", "/api/status", "/api/withdrawal/ZZZZ-ZZZZ", "/qr.svg?d=0x00000000000000000000000000000000000000b7"} {
		resp, err := http.Get(e.urls[0] + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		h := resp.Header
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("%s: headers %v", path, h)
		}
		if csp := h.Get("Content-Security-Policy"); (path == "/" || path == "/owner") && (!strings.Contains(csp, "connect-src 'self' https:") || !strings.Contains(csp, "script-src 'self' 'unsafe-inline'")) {
			t.Fatalf("page CSP: %s", csp)
		}
	}

	code, w1, body := post(req("10", payee))
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	want, _ := SwapAddress(consensus.RegTest.HRP, SwapDest{ChainID: c.ID, Symbol: "USDC", To: payee})
	if w1.Address != want || ParseWithdrawCode(w1.Memo) != w1.Code || w1.Memo != FormatWithdrawCode(w1.Code) || w1.Amount != 10*unit ||
		w1.Status != StageWaiting || w1.ExpectedPaid != "9900000" || w1.To != ChecksumAddress(payee) || w1.Token != "USDC" || len(w1.Payments) != 0 {
		t.Fatalf("new withdrawal: %s", body)
	}
	if v := get(strings.ToLower(w1.Memo)); v.Code != w1.Code {
		t.Fatal("lookup by memo as typed")
	}
	if resp, _ := http.Get(e.urls[0] + "/api/withdrawal/ZZZZ-ZZZZ"); resp.StatusCode != 404 {
		t.Fatalf("unknown memo: %d", resp.StatusCode)
	}

	swapOut := func(units consensus.Amount, a consensus.Hash) consensus.TxOut {
		p, _ := SwapPayload(SwapDest{ChainID: c.ID, Symbol: "USDC", To: payee})
		return consensus.TxOut{Amount: units, Type: consensus.OutputTypeP2PK, Payload: p[:], Asset: a}
	}
	memo := func(m string) consensus.TxOut {
		return consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: []byte(m)}
	}

	// 4 uARG with the memo: seen in the mempool at once, then confirming.
	pay1 := e.paySwap([]consensus.TxOut{swapOut(4*unit, asset), memo(w1.Memo)}, 10)
	if err := e.leader.scanMempool(); err != nil {
		t.Fatal(err)
	}
	v := get(w1.Code)
	if v.Status != StageDetected || len(v.Payments) != 1 || v.Payments[0].State != "mempool" || !v.Payments[0].Memo || v.Payments[0].TxID != pay1 ||
		v.Received != 4*unit || v.Remaining != 6*unit || v.Payments[0].Needed != 3 || v.Payments[0].TokenPaid != "3960000" || !v.Mempool {
		t.Fatalf("in the mempool: %+v", v)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.leader.scanMempool()
	v = get(w1.Code)
	if len(v.Payments) != 1 || v.Payments[0].State != "confirming" || v.Payments[0].Confirmations != 1 || v.Payments[0].ID != pay1+":0" || v.Status != StageDetected {
		t.Fatalf("in a block: %+v", v)
	}
	// Every guardian keeps the memo with the payment.
	for _, s := range []*Service{e.leader, e.f1, e.f2} {
		if r, ok := s.st.GetRedemption(pay1 + ":0"); !ok || r.Memo != w1.Code {
			t.Fatalf("redemption: %+v", r)
		}
	}

	// A second withdrawal to the same payee: the next payment without a
	// memo belongs to it, a payment with the first memo still to the first.
	_, w2, _ := post(req("2", payee))
	if w2.Address != w1.Address || w2.Code == w1.Code {
		t.Fatal("second withdrawal")
	}
	e.paySwap([]consensus.TxOut{swapOut(7*unit, asset), memo(strings.ToLower(w1.Memo))}, 11)
	e.mine(1, e.uAddr)
	e.round()
	pay3 := e.paySwap([]consensus.TxOut{swapOut(2*unit, asset)}, 12)
	e.mine(1, e.uAddr)
	e.round()
	e.leader.scanMempool()
	v = get(w1.Code)
	if len(v.Payments) != 2 || v.Received != 11*unit || v.Extra != unit || v.Remaining != 0 {
		t.Fatalf("first withdrawal, overpaid: %+v", v)
	}
	v2 := get(w2.Code)
	if len(v2.Payments) != 1 || v2.Payments[0].TxID != pay3 || v2.Payments[0].Memo || v2.Received != 2*unit {
		t.Fatalf("second withdrawal, paid without its memo: %+v", v2)
	}

	// ARG sent to the address is lost: counted, never listed or received,
	// from the mempool to its block (and not twice in between). A uARG
	// payment below 1 uARG is counted, not listed.
	e.paySwap([]consensus.TxOut{swapOut(2*unit, consensus.NativeAsset), swapOut(unit/2, asset), memo(w2.Memo)}, 13)
	e.leader.scanMempool()
	if v2 = get(w2.Code); len(v2.Payments) != 1 || v2.LostCount != 1 || v2.LostARG != 2*unit || v2.Received != 2*unit || v2.DustCount != 0 {
		t.Fatalf("ARG in the mempool: %+v", v2) // the dust is not followed in the mempool
	}
	e.mine(1, e.uAddr)
	e.round()
	e.leader.scanMempool()
	if v2 = get(w2.Code); len(v2.Payments) != 1 || v2.LostCount != 1 || v2.LostARG != 2*unit || v2.DustCount != 1 || v2.DustAmount != unit/2 || v2.PaymentsTotal != 1 {
		t.Fatalf("ARG and dust in a block: %+v", v2)
	}

	// Deep enough: released, then paid.
	e.mine(3, e.uAddr)
	for i := 0; i < 3; i++ {
		if err := e.round(); err != nil {
			t.Fatal(err)
		}
	}
	v = get(w1.Code)
	for _, p := range v.Payments {
		if p.Stage != StageCompleted || p.EVMTx == "" {
			t.Fatalf("payment not completed: %+v", p)
		}
	}
	if v.Status != StageCompleted || v.TokenPaid != "10890000" {
		t.Fatalf("completed: %+v", v)
	}
	if v2 = get(w2.Code); v2.Status != StageCompleted || v2.LostCount != 1 {
		t.Fatalf("second completed (its lost ARG aside): %+v", v2)
	}
	if paid, _ := new(big.Int).SetString(v2.TokenPaid, 10); paid == nil || paid.Cmp(big.NewInt(1980000)) < 0 {
		t.Fatalf("second withdrawal's payout: %+v", v2) // 1.98 USDC, and the dust's if it was paid out
	}

	// The memo and the index survive a restart; pruning keeps what was paid.
	e.leader.pruneWithdrawals(timeNowPlus(31))
	if _, ok := e.leader.st.GetWithdrawal(w1.Code); !ok {
		t.Fatal("a paid withdrawal was pruned after 30 days")
	}
	_, w3, _ := post(req("3", [20]byte{19: 0xC1}))
	e.leader.pruneWithdrawals(timeNowPlus(31))
	if _, ok := e.leader.st.GetWithdrawal(w3.Code); ok {
		t.Fatal("an unpaid withdrawal was kept after 30 days")
	}
	e.leader.pruneWithdrawals(timeNowPlus(181))
	if _, ok := e.leader.st.GetWithdrawal(w1.Code); ok {
		t.Fatal("kept after 180 days")
	}
	if n := e.leader.st.countWithdrawals(); n != 0 || e.leader.wdCount != 0 {
		t.Fatalf("count after pruning: %d, %d", n, e.leader.wdCount)
	}
}

// TestWithdrawalReorg: a payment whose block leaves the chain leaves the
// withdrawal with it, and shows again once mined anew.
func TestWithdrawalReorg(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	c := e.chains[0]
	e.issueDeposit(c.Tokens[0].Address, usd(50, 6))
	var payee [20]byte
	payee[19] = 0xB8
	wd, err := e.leader.createWithdrawal(c.ID, "USDC", HexAddress(payee), 5*consensus.Amount(consensus.UnitsPerCoin))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := SwapPayload(SwapDest{ChainID: c.ID, Symbol: "USDC", To: payee})
	pay := e.paySwap([]consensus.TxOut{{Amount: 5 * consensus.UnitsPerCoin, Type: consensus.OutputTypeP2PK, Payload: p[:], Asset: e.leader.Asset()},
		{Type: consensus.OutputTypeNullData, Payload: []byte(wd.Code)}}, 10)
	e.mine(1, e.uAddr)
	e.round()
	if v := e.leader.withdrawalView(wd); len(v.Payments) != 1 || v.Payments[0].ID != pay+":0" || !v.Payments[0].Memo {
		t.Fatalf("before the reorg: %+v", v)
	}
	e.reorgL1(1) // the block with the payment is replaced
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if refs := e.leader.st.swapRefs(wd.Address); len(refs) != 0 {
		t.Fatalf("index kept after the reorg: %+v", refs)
	}
	if v := e.leader.withdrawalView(wd); v.Status != StageWaiting || len(v.Payments) != 0 {
		t.Fatalf("after the reorg: %+v", v)
	}
}

func timeNowPlus(days int) time.Time { return time.Now().Add(time.Duration(days) * 24 * time.Hour) }
