package bridge

// Reviewer A (2026-10-28): the findings of the review of the withdrawal
// flow (withdraw.go, l1index.go swap refs, the public API), each kept as a
// regression test of its fix. The comment on each test states the finding
// as it was; the test asserts the behaviour that replaced it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
)

// reviewAWd creates a withdrawal on the leader and fails the test if it
// cannot.
func reviewAWd(t *testing.T, e *pocEnv, to [20]byte, amount consensus.Amount) *Withdrawal {
	t.Helper()
	wd, err := e.leader.createWithdrawal(e.chains[0].ID, "USDC", HexAddress(to), amount)
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func reviewASwapOut(t *testing.T, e *pocEnv, to [20]byte, units consensus.Amount, asset consensus.Hash) consensus.TxOut {
	t.Helper()
	p, err := SwapPayload(SwapDest{ChainID: e.chains[0].ID, Symbol: "USDC", To: to})
	if err != nil {
		t.Fatal(err)
	}
	return consensus.TxOut{Amount: units, Type: consensus.OutputTypeP2PK, Payload: p[:], Asset: asset}
}

func reviewAMemo(code string) consensus.TxOut {
	return consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: []byte(code)}
}

// TestReviewA_ReorgLosesSwapRefOfReleasedPayment (L1): a swap payment
// whose redemption changed after it was indexed (released, paid) survived a
// reorganisation that re-mined the same transaction, but its q/p/ ref did
// not: the undo deleted the unchanged ref and recordSwap returned early
// because the redemption was still there, so the page read "waiting" for a
// payout already delivered. recordSwap now writes the ref whenever it is
// missing.
func TestReviewA_ReorgLosesSwapRefOfReleasedPayment(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	e.issueDeposit(e.chains[0].Tokens[0].Address, usd(50, 6))
	unit := consensus.Amount(consensus.UnitsPerCoin)
	var payee [20]byte
	payee[19] = 0xD1
	wd := reviewAWd(t, e, payee, 5*unit)
	pay := e.paySwap([]consensus.TxOut{reviewASwapOut(t, e, payee, 5*unit, e.leader.Asset()), reviewAMemo(wd.Code)}, 10)
	ptx, err := e.client.MempoolTx(pay)
	if err != nil || ptx.Hex == "" {
		t.Fatalf("raw payment: %v", err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(3, e.uAddr)
	for i := 0; i < 3; i++ {
		if err := e.round(); err != nil {
			t.Fatal(err)
		}
	}
	if v := e.leader.withdrawalView(wd); v.Status != StageCompleted {
		t.Fatalf("not completed before the reorg: %+v", v)
	}
	// The payment's block and the three above it are replaced; the same
	// payment is mined again in the new branch.
	e.reorgL1(4)
	if _, err := e.client.SendRawTransaction(ptx.Hex); err != nil {
		t.Logf("rebroadcast: %v (may already be back in the pool)", err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	raw, err := e.client.Call("getrawtransaction", pay)
	if err != nil {
		t.Fatal(err)
	}
	var mined struct {
		BlockHash string `json:"blockhash"`
		Height    uint32 `json:"height"`
	}
	json.Unmarshal(raw, &mined)
	if mined.BlockHash == "" {
		t.Fatal("the payment was not mined again; the scenario did not happen")
	}
	r, ok := e.leader.st.GetRedemption(pay + ":0")
	if !ok || r.Status != RedemptionConfirmed {
		t.Fatalf("redemption: %+v", r)
	}
	refs := e.leader.st.swapRefs(wd.Address)
	v := e.leader.withdrawalView(wd)
	if len(refs) != 1 || refs[0].Height != mined.Height || refs[0].Memo != wd.Code {
		t.Fatalf("the payment's ref after it was mined again at %d: %+v", mined.Height, refs)
	}
	if v.Status != StageCompleted || len(v.Payments) != 1 || v.Received != 5*unit || v.Remaining != 0 {
		t.Fatalf("the delivered withdrawal after the reorg: %+v", v)
	}
}

// TestReviewA_DustKeepsIdleWithdrawalsAndCapIsGlobal (M1): withdrawals cost
// nothing to make, the 200 000 cap was shared by every user and refused
// everyone once full, one lost dust output to the address kept every
// withdrawal to it for 180 days, and a text/plain POST (no CORS preflight)
// made one. Now: a withdrawal counts as paid only with a uARG payment of
// at least 1 uARG, one never paid is forgotten after 48 hours, the oldest
// open one makes room when the bridge or an address is full, one client
// keeps at most withdrawPerIP open, and the POST takes JSON only.
func TestReviewA_DustKeepsIdleWithdrawalsAndCapIsGlobal(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	e.issueDeposit(e.chains[0].Tokens[0].Address, usd(50, 6))
	unit := consensus.Amount(consensus.UnitsPerCoin)
	asset := e.leader.Asset()
	chainID := e.chains[0].ID
	var attacker [20]byte
	attacker[19] = 0xEE
	var ws []*Withdrawal
	for i := 0; i < 5; i++ {
		ws = append(ws, reviewAWd(t, e, attacker, 10*unit))
	}
	// Dust to the newest one, in ARG (lost) and in uARG below 1 uARG:
	// neither makes a withdrawal paid.
	e.paySwap([]consensus.TxOut{reviewASwapOut(t, e, attacker, 546, consensus.NativeAsset), reviewASwapOut(t, e, attacker, unit/2, asset), reviewAMemo(ws[4].Code)}, 10)
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	// One withdrawal paid 1 uARG with its memo.
	paid := reviewAWd(t, e, attacker, 1*unit)
	e.paySwap([]consensus.TxOut{reviewASwapOut(t, e, attacker, unit, asset), reviewAMemo(paid.Code)}, 11)
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if v := e.leader.withdrawalView(ws[4]); v.DustCount != 1 || v.LostCount != 1 || len(v.Payments) != 0 || v.Status != StageWaiting {
		t.Fatalf("the newest unpaid withdrawal holds the dust: %+v", v)
	}
	kept := func() int {
		n := 0
		for _, w := range ws {
			if _, ok := e.leader.st.GetWithdrawal(w.Code); ok {
				n++
			}
		}
		return n
	}
	e.leader.pruneWithdrawals(time.Now().Add(withdrawKeepIdle - time.Hour))
	if n := kept(); n != len(ws) {
		t.Fatalf("pruned before %v: %d of %d kept", withdrawKeepIdle, n, len(ws))
	}
	e.leader.pruneWithdrawals(time.Now().Add(withdrawKeepIdle + time.Hour))
	if n := kept(); n != 0 {
		t.Fatalf("after %v, %d of %d withdrawals nobody paid 1 uARG to are still kept", withdrawKeepIdle, n, len(ws))
	}
	if _, ok := e.leader.st.GetWithdrawal(paid.Code); !ok {
		t.Fatal("a paid withdrawal was forgotten")
	}
	if e.leader.st.has(withdrawOpenKey(paid.CreatedMs, paid.Code)) {
		t.Fatal("a paid withdrawal still counts as open")
	}

	// The bridge full: the oldest open withdrawal makes room, the paid one
	// is never touched, and another user is served.
	older, newer := reviewAWd(t, e, attacker, 2*unit), reviewAWd(t, e, attacker, 3*unit)
	e.leader.wdMu.Lock()
	e.leader.wdCount = withdrawCap
	e.leader.wdMu.Unlock()
	var victim [20]byte
	victim[0], victim[19] = 0x5a, 0x77
	post := func(body, ip, ctype string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, e.urls[0]+"/api/withdrawals", strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", ip)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	body := fmt.Sprintf(`{"chain_id":%d,"token":"USDC","to":"%s","amount":"10"}`, chainID, HexAddress(victim))
	if code, b := post(body, "198.51.100.200", "application/json"); code != http.StatusOK {
		t.Fatalf("a user at the cap: %d %s", code, b)
	}
	if _, ok := e.leader.st.GetWithdrawal(older.Code); ok {
		t.Fatal("the oldest open withdrawal did not make room")
	}
	for _, w := range []*Withdrawal{newer, paid} {
		if _, ok := e.leader.st.GetWithdrawal(w.Code); !ok {
			t.Fatalf("withdrawal %s was forgotten to make room", w.Code)
		}
	}
	e.leader.wdMu.Lock()
	e.leader.wdCount = e.leader.st.openWithdrawals()
	e.leader.wdMu.Unlock()

	// A page elsewhere can POST text/plain without a CORS preflight: refused.
	if code, b := post(body, "198.51.100.201", "text/plain"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain POST: %d %s", code, b)
	}

	// One client keeps at most withdrawPerIP open; another is still served.
	for i := 0; i < withdrawPerIP; i++ {
		var to [20]byte
		to[0], to[19] = 0x61, byte(i+1)
		if _, err := e.leader.createWithdrawalFor("203.0.113.9", chainID, "USDC", HexAddress(to), unit); err != nil {
			t.Fatalf("withdrawal %d of one client: %v", i, err)
		}
	}
	var to [20]byte
	to[0], to[19] = 0x62, 0x01
	_, err := e.leader.createWithdrawalFor("203.0.113.9", chainID, "USDC", HexAddress(to), unit)
	var ae *apiErr
	if !errors.As(err, &ae) || ae.code != http.StatusTooManyRequests {
		t.Fatalf("withdrawal %d of one client: %v", withdrawPerIP+1, err)
	}
	if _, err := e.leader.createWithdrawalFor("203.0.113.10", chainID, "USDC", HexAddress(to), unit); err != nil {
		t.Fatalf("another client: %v", err)
	}

	// One address keeps at most withdrawPerAddr open. Past it, a recent one
	// with the same amount is handed out again, or the oldest open one is
	// forgotten; one with a payment (here still in the mempool) never is.
	var payee [20]byte
	payee[19] = 0x99
	var made []*Withdrawal
	for i := 1; i <= withdrawPerAddr+1; i++ {
		made = append(made, reviewAWd(t, e, payee, consensus.Amount(i)*unit))
		if i == 1 {
			e.paySwap([]consensus.TxOut{reviewASwapOut(t, e, payee, unit, asset), reviewAMemo(made[0].Code)}, 12)
			if err := e.leader.scanMempool(); err != nil {
				t.Fatal(err)
			}
		}
	}
	addr := made[0].Address
	if n := len(e.leader.st.withdrawalOrders(addr)); n != withdrawPerAddr+1 {
		t.Fatalf("%d withdrawals to the address, %d of them open", n, withdrawPerAddr)
	}
	if again := reviewAWd(t, e, payee, 7*unit); again.Code != made[6].Code {
		t.Fatalf("the same amount past the cap made %s, not %s again", again.Code, made[6].Code)
	}
	fresh := reviewAWd(t, e, payee, 42*unit)
	orders := e.leader.st.withdrawalOrders(addr)
	if len(orders) != withdrawPerAddr+1 || orders[len(orders)-1].Code != fresh.Code {
		t.Fatalf("after one more: %+v", orders)
	}
	if _, ok := e.leader.st.GetWithdrawal(made[1].Code); ok {
		t.Fatal("the oldest open withdrawal to the address was kept past the cap")
	}
	if _, ok := e.leader.st.GetWithdrawal(made[0].Code); !ok {
		t.Fatal("the withdrawal with a payment was forgotten")
	}
}

// TestReviewA_PerAddressAmplification (H1): GET /api/withdrawal/{memo} read
// every withdrawal to the address and every payment ever made to it,
// matched each payment against every withdrawal (O(payments x
// withdrawals)), returned all of them, lost dust included, cached nothing,
// and pruning read every payment once per withdrawal. Both counts are the
// attacker's to grow. Now one pass per address, with a sorted search for a
// memo-less payment's owner, reads the withdrawals' index only, lists the
// newest withdrawListMax payments of at least 1 uARG with the totals of all,
// counts dust and lost payments, and is cached until something about the
// address changes; pruning works out each address once.
func TestReviewA_PerAddressAmplification(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	e.issueDeposit(e.chains[0].Tokens[0].Address, usd(50, 6))
	unit := consensus.Amount(consensus.UnitsPerCoin)
	var attacker [20]byte
	attacker[19] = 0xEF
	wd := reviewAWd(t, e, attacker, 10*unit)

	// On chain: one transaction with 250 dust ARG outputs to the address:
	// counted on the withdrawal, not listed.
	var outs []consensus.TxOut
	for i := 0; i < 250; i++ {
		outs = append(outs, reviewASwapOut(t, e, attacker, 546, consensus.NativeAsset))
	}
	e.paySwap(outs, 10)
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.leader.st.swapRefs(wd.Address)); n != 250 {
		t.Fatalf("%d refs from one transaction", n)
	}
	if v := e.leader.withdrawalView(wd); len(v.Payments) != 0 || v.LostCount != 250 || v.LostARG != 250*546 || v.Status != StageWaiting || v.Received != 0 {
		t.Fatalf("250 lost dust outputs: %+v", v)
	}

	// Scaled as an attacker would (straight into the store, past every
	// limit the API keeps): 50 000 withdrawals to the address, 3 000 more
	// lost payments, and 120 real ones (1 uARG and more, paid out) carrying
	// the withdrawal's memo.
	const nWd, nLost, nReal = 50_000, 3_000, 120
	var ops []kvOp
	base := time.Now().Add(-time.Hour)
	for i := 0; i < nWd; i++ {
		x := *wd
		x.Code = reviewACode(i)
		x.CreatedMs = base.Add(time.Duration(i) * time.Millisecond).UnixMilli()
		x.Created = x.CreatedMs / 1000
		b, _ := json.Marshal(&x)
		ops = append(ops, kvOp{key: withdrawalKey(x.Code), value: b}, kvOp{key: withdrawToKey(x.Address, x.CreatedMs, x.Code), value: []byte(x.Code)},
			kvOp{key: withdrawOpenKey(x.CreatedMs, x.Code), value: []byte(x.Address)})
	}
	now := time.Now().Unix()
	for i := 0; i < nLost; i++ {
		ref := swapPaymentRef{ID: fmt.Sprintf("%064x:%d", i, 0), Amount: 546, Lost: true, Height: 1, Created: now, Seen: now}
		b, _ := json.Marshal(&ref)
		ops = append(ops, kvOp{key: swapPaymentKey(wd.Address, ref.ID), value: b})
	}
	var received consensus.Amount
	one := uint32(1)
	for i := 0; i < nReal; i++ {
		txid := fmt.Sprintf("%064x", 1_000_000+i)
		amount := unit + consensus.Amount(i)
		received += amount
		ref := swapPaymentRef{ID: txid + ":1", Amount: amount, Memo: wd.Code, Height: 1, Created: now, Seen: now - nReal + int64(i)}
		r := Redemption{TxID: txid, Vout: &one, Address: wd.Address, Memo: wd.Code, Height: 1, Amount: amount, Chain: wd.ChainID, Status: RedemptionConfirmed,
			TokenAmount: "1000", TokenPaid: "1000", EVMTx: "0x" + strings.Repeat("ab", 32), Created: now, Updated: now}
		rb, _ := json.Marshal(&ref)
		xb, _ := json.Marshal(&r)
		ops = append(ops, kvOp{key: swapPaymentKey(wd.Address, ref.ID), value: rb}, kvOp{key: redemptionKey(r.ID()), value: xb})
	}
	if err := e.leader.st.write(ops, false); err != nil {
		t.Fatal(err)
	}

	get := func() (WithdrawalView, int64, time.Duration, float64) {
		t.Helper()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		alloc0 := ms.TotalAlloc
		start := time.Now()
		resp, err := http.Get(e.urls[0] + "/api/withdrawal/" + wd.Code)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		took := time.Since(start)
		runtime.ReadMemStats(&ms)
		if resp.StatusCode != 200 {
			t.Fatalf("GET: %d %s", resp.StatusCode, b)
		}
		var v WithdrawalView
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v, int64(len(b)), took, float64(ms.TotalAlloc-alloc0) / 1e6
	}
	computed := e.leader.wdViews.computed.Load()
	v, n, took, mb := get()
	t.Logf("one GET with %d withdrawals and %d payments to the address: %v, %d bytes of JSON, %.0f MB allocated (other tests run alongside)", nWd+1, 250+nLost+nReal, took, n, mb)
	if len(v.Payments) != withdrawListMax || v.PaymentsTotal != nReal || v.LostCount != 250+nLost || v.Received != received || v.Status != StageCompleted ||
		v.TokenPaid != fmt.Sprint(nReal*1000) || v.TokenDue != v.TokenPaid {
		t.Fatalf("the view: %d payments listed, total %d, lost %d, received %d (want %d), status %s, paid %s, due %s",
			len(v.Payments), v.PaymentsTotal, v.LostCount, v.Received, received, v.Status, v.TokenPaid, v.TokenDue)
	}
	if first, last := v.Payments[0].ID, v.Payments[len(v.Payments)-1].ID; first != fmt.Sprintf("%064x:1", 1_000_000+nReal-withdrawListMax) || last != fmt.Sprintf("%064x:1", 1_000_000+nReal-1) {
		t.Fatalf("not the newest payments, oldest first: %s .. %s", first, last)
	}
	if n > 64<<10 || took > 5*time.Second {
		t.Fatalf("one GET: %d bytes in %v", n, took)
	}
	if got := e.leader.wdViews.computed.Load() - computed; got != 1 {
		t.Fatalf("%d computations for one GET", got)
	}
	// Asked again while nothing changes: from the cache.
	for i := 0; i < 5; i++ {
		get()
	}
	if got := e.leader.wdViews.computed.Load() - computed; got != 1 {
		t.Fatalf("%d computations for six GETs of an unchanged address", got)
	}
	// A change to the address (a new withdrawal to it) is seen at once.
	wd2 := reviewAWd(t, e, attacker, 3*unit)
	if v := e.leader.withdrawalView(wd2); v.Code != wd2.Code || v.Status != StageWaiting || v.LostCount != 0 {
		t.Fatalf("a new withdrawal to the address: %+v", v)
	}
	if got := e.leader.wdViews.computed.Load() - computed; got != 2 {
		t.Fatalf("a new withdrawal did not refresh the address's view: %d computations", got)
	}

	// Pruning works out the address once: every withdrawal nothing was
	// paid to goes after withdrawKeepIdle, the paid one stays.
	start := time.Now()
	e.leader.pruneWithdrawals(time.Now().Add(withdrawKeepIdle + time.Hour))
	took = time.Since(start)
	orders := e.leader.st.withdrawalOrders(wd.Address)
	t.Logf("pruning %d withdrawals to one address with %d payments: %v", nWd+2, 250+nLost+nReal, took)
	if len(orders) != 1 || orders[0].Code != wd.Code {
		t.Fatalf("after pruning, %d withdrawals to the address", len(orders))
	}
	if took > 60*time.Second {
		t.Fatalf("pruning took %v", took)
	}
}

// TestReviewA_PaymentVanishesBetweenMempoolAndIndex (L2): once mined, a
// payment left the mempool view at the next scan (every 4 s) but entered
// the confirmed view only at the next Argonmine scan, and in between the
// page asked for it again. Now the scan keeps a payment that left the
// mempool until its block is indexed (mempoolKeepMined at most).
func TestReviewA_PaymentVanishesBetweenMempoolAndIndex(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	e.issueDeposit(e.chains[0].Tokens[0].Address, usd(50, 6))
	unit := consensus.Amount(consensus.UnitsPerCoin)
	var payee [20]byte
	payee[19] = 0xD2
	wd := reviewAWd(t, e, payee, 5*unit)
	pay := e.paySwap([]consensus.TxOut{reviewASwapOut(t, e, payee, 5*unit, e.leader.Asset()), reviewAMemo(wd.Code)}, 10)
	if err := e.leader.scanMempool(); err != nil {
		t.Fatal(err)
	}
	if v := e.leader.withdrawalView(wd); v.Status != StageDetected {
		t.Fatalf("in the mempool: %+v", v)
	}
	e.mine(1, e.uAddr) // mined; the leader has not indexed the block yet
	if err := e.leader.scanMempool(); err != nil {
		t.Fatal(err)
	}
	v := e.leader.withdrawalView(wd)
	if v.Status != StageDetected || len(v.Payments) != 1 || v.Payments[0].State != "mempool" || v.Remaining != 0 {
		t.Fatalf("mined, not yet indexed: %+v", v)
	}
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if err := e.leader.scanMempool(); err != nil {
		t.Fatal(err)
	}
	v = e.leader.withdrawalView(wd)
	if len(v.Payments) != 1 || v.Payments[0].ID != pay+":0" || v.Payments[0].State != "confirming" || v.Received != 5*unit {
		t.Fatalf("indexed: shown once, from its block: %+v", v)
	}
	m := e.leader.mempool
	m.mu.Lock()
	gone := len(m.gone)
	m.mu.Unlock()
	if gone != 0 {
		t.Fatalf("%d indexed transactions still kept aside", gone)
	}
}

// TestReviewA_ThirdPartyCapturesMemolessPayment (L3): a payment without
// the memo was matched to the newest withdrawal made before the leader
// indexed it, so a stranger who made a withdrawal to the same payout while
// the victim's payment waited for its block took it off the victim's
// page. Now a payment is matched by when it was first seen (in the
// mempool, else its block's time).
func TestReviewA_ThirdPartyCapturesMemolessPayment(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	e.issueDeposit(e.chains[0].Tokens[0].Address, usd(50, 6))
	unit := consensus.Amount(consensus.UnitsPerCoin)
	var victim [20]byte
	victim[19] = 0xD3
	w1 := reviewAWd(t, e, victim, 5*unit)
	time.Sleep(1100 * time.Millisecond)
	e.paySwap([]consensus.TxOut{reviewASwapOut(t, e, victim, 5*unit, e.leader.Asset())}, 10) // no memo
	if err := e.leader.scanMempool(); err != nil {
		t.Fatal(err)
	}
	if v := e.leader.withdrawalView(w1); len(v.Payments) != 1 {
		t.Fatalf("victim's view in the mempool: %+v", v)
	}
	seen := e.leader.withdrawalView(w1).Payments[0].Seen
	time.Sleep(1100 * time.Millisecond)
	w2 := reviewAWd(t, e, victim, 1*unit) // a stranger, same payout
	if v := e.leader.withdrawalView(w1); len(v.Payments) != 1 {
		t.Fatalf("victim's view after the stranger's withdrawal, still in the mempool: %+v", v)
	}
	time.Sleep(1100 * time.Millisecond)
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.leader.scanMempool()
	v1, v2 := e.leader.withdrawalView(w1), e.leader.withdrawalView(w2)
	if len(v1.Payments) != 1 || v1.Payments[0].State != "confirming" || len(v2.Payments) != 0 || v2.Status != StageWaiting {
		t.Fatalf("victim %s: %+v; stranger %s: %+v", w1.Code, v1, w2.Code, v2)
	}
	refs := e.leader.st.swapRefs(w1.Address)
	if len(refs) != 1 || refs[0].Seen != seen || refs[0].Seen >= w2.Created {
		t.Fatalf("the payment's ref keeps when it was first seen (%d, before %d): %+v", seen, w2.Created, refs)
	}
}

// TestReviewA_MempoolScanCost (M2): every scan fetched the whole mempool's
// txid list and kept every payment to a swap address without a bound; RPC
// answers above 64 MB failed to parse and the error carried the whole
// body. Now the scan asks the mempool's size first and stops above
// mempoolScanMax (and when the list itself is longer), skips outputs below
// 1 uARG, keeps at most mempoolPerAddr payments per address and
// mempoolPaysMax in all, and the client's error quotes 256 bytes at most.
func TestReviewA_MempoolScanCost(t *testing.T) {
	t.Parallel()
	hrp := consensus.RegTest.HRP
	unit := consensus.Amount(consensus.UnitsPerCoin)
	var to [20]byte
	to[19] = 0xC4
	addr, err := SwapAddress(hrp, SwapDest{ChainID: 31337, Symbol: "USDC", To: to})
	if err != nil {
		t.Fatal(err)
	}
	to[19] = 0xC5
	addr2, _ := SwapAddress(hrp, SwapDest{ChainID: 31337, Symbol: "USDC", To: to})

	type node struct {
		mu      sync.Mutex
		size    int      // getmempoolinfo's size; -1: no such method
		ids     []string // getrawmempool
		garbage int      // getrawmempool answers this many bytes that are no JSON
		calls   map[string]int
	}
	serve := func(n *node) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			n.mu.Lock()
			n.calls[req.Method]++
			size, ids, garbage := n.size, n.ids, n.garbage
			n.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch req.Method {
			case "getmempoolinfo":
				if size < 0 {
					io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
					return
				}
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"size":%d,"bytes":0}}`, size)
			case "getrawmempool":
				if garbage > 0 {
					w.Write([]byte(strings.Repeat("x", garbage)))
					return
				}
				b, _ := json.Marshal(ids)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, b)
			case "getrawtransaction":
				var id string
				if len(req.Params) > 0 {
					json.Unmarshal(req.Params[0], &id)
				}
				// Every transaction pays addr 1 uARG and 1000 units (dust);
				// those from "b" on pay addr2 instead.
				a := addr
				if strings.HasPrefix(id, "b") {
					a = addr2
				}
				tx := wallet.TxJSON{TxID: id, Outputs: []wallet.TxOutJSON{
					{N: 0, Amount: unit, Type: consensus.OutputTypeP2PK, Address: a, Asset: "aa"},
					{N: 1, Amount: 1000, Type: consensus.OutputTypeP2PK, Address: a, Asset: "aa"},
				}}
				b, _ := json.Marshal(&tx)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, b)
			}
		}))
	}
	ids := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%063x", prefix, i+1)
		}
		return out
	}
	newSvc := func(n *node) (*Service, func()) {
		srv := serve(n)
		return &Service{cfg: Config{Params: consensus.RegTest, Node: wallet.NewClient(srv.URL, "")}, mempool: newMempoolWatch(), wdViews: newWdViewCache()}, srv.Close
	}

	// A mempool larger than the scan follows: not even listed.
	n := &node{size: 1_100_000, calls: map[string]int{}}
	s, done := newSvc(n)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	a0 := ms.TotalAlloc
	start := time.Now()
	err = s.scanMempool()
	took := time.Since(start)
	runtime.ReadMemStats(&ms)
	var tooBig errMempoolTooBig
	if !errors.As(err, &tooBig) || n.calls["getrawmempool"] != 0 || s.mempool.ok {
		t.Fatalf("a mempool of 1 100 000: %v, %d lists read", err, n.calls["getrawmempool"])
	}
	t.Logf("a mempool of 1 100 000 transactions: one scan %v, %.1f MB allocated: %v", took, float64(ms.TotalAlloc-a0)/1e6, err)
	done()

	// A node that does not report its size: the list's length decides.
	n = &node{size: -1, ids: ids("a", mempoolScanMax+1), calls: map[string]int{}}
	s, done = newSvc(n)
	if err := s.scanMempool(); !errors.As(err, &tooBig) || n.calls["getrawtransaction"] != 0 {
		t.Fatalf("a list of %d: %v, %d transactions read", mempoolScanMax+1, err, n.calls["getrawtransaction"])
	}
	done()

	// An answer that does not parse: the error quotes its start only.
	n = &node{size: 10, garbage: 1 << 20, calls: map[string]int{}}
	s, done = newSvc(n)
	if err := s.scanMempool(); err == nil || len(err.Error()) > 512 {
		t.Fatalf("an unparsable answer of 1 MB: error of %d bytes", len(fmt.Sprint(err)))
	}
	done()

	// 120 transactions to one address and 10 to another: at most
	// mempoolPerAddr kept for the first, none of the dust.
	n = &node{size: 130, ids: append(ids("a", 120), ids("b", 10)...), calls: map[string]int{}}
	s, done = newSvc(n)
	defer done()
	if err := s.scanMempool(); err != nil {
		t.Fatal(err)
	}
	p1, ok := s.mempool.payments(addr)
	p2, _ := s.mempool.payments(addr2)
	if !ok || len(p1) != mempoolPerAddr || len(p2) != 10 {
		t.Fatalf("kept %d and %d payments", len(p1), len(p2))
	}
	for _, p := range append(p1, p2...) {
		if p.Amount < withdrawMin {
			t.Fatalf("dust kept: %+v", p)
		}
	}
	if seen, ok := s.mempool.firstSeen(p2[0].ID); !ok || seen != p2[0].Seen {
		t.Fatal("first seen")
	}
	// Mined (they left the mempool): kept until indexed, at most
	// mempoolKeepMined.
	n.mu.Lock()
	n.ids, n.size = nil, 0
	n.mu.Unlock()
	if err := s.scanMempool(); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.mempool.payments(addr2); len(p) != 10 {
		t.Fatalf("payments that left the mempool: %d kept", len(p))
	}
	s.mempool.mu.Lock()
	for id, g := range s.mempool.gone {
		g.at = g.at.Add(-mempoolKeepMined - time.Second)
		s.mempool.gone[id] = g
	}
	s.mempool.mu.Unlock()
	if err := s.scanMempool(); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.mempool.payments(addr2); len(p) != 0 {
		t.Fatalf("payments gone for %v: %d kept", mempoolKeepMined, len(p))
	}
}

// reviewACode is the i-th memo in the memo alphabet.
func reviewACode(i int) string {
	b := make([]byte, withdrawCodeLen)
	for j := range b {
		b[j] = handoffAlphabet[(i>>(5*j))&31]
	}
	return string(b)
}
