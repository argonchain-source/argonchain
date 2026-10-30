package bridge

// Withdrawals (docs/BRIDGE.md, "Withdrawals"): how the bridge page follows
// one payout from the request to the token arriving on the EVM chain.
//
// The user names the payout (chain, token, EVM recipient) and the uARG
// amount. The bridge answers with the payout's swap address (swapaddr.go)
// and a memo of its own: eight characters, unique among its withdrawals.
// The user pays the uARG to the address from any Argonmine wallet, with the
// memo, and the page shows the payment's progress until the payout.
//
// The memo only matches a payment to its withdrawal; it moves no money.
// The swap address alone decides where the uARG is paid out, as it does for
// every guardian. So a payment without the memo, or with a mistyped one,
// is still paid to the right recipient, and is shown on the newest
// withdrawal to that address made before the payment was first seen. Each
// payment is paid out as it arrives, whatever amount it carries: less than
// requested pays out what arrived (the rest can follow with the same memo),
// more pays out all of it. Withdrawals are kept by the guardian that serves
// the page; nothing here is signed or shared with the other guardians, and
// nothing here is read by a release.
//
// Withdrawals cost nothing to make, so everything about them is bounded:
// one nothing was paid to (no uARG payment of at least withdrawMin) is
// forgotten after withdrawKeepIdle, and the oldest such one makes room when
// the bridge, a swap address or a client has too many open; one view is
// computed per address and cached; a withdrawal lists only its newest
// payments, with the totals of all of them; and the mempool is followed
// only up to a size.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/consensus"
	"argonmine/wallet"
)

const (
	withdrawCodeLen  = 8
	withdrawMin      = consensus.Amount(consensus.UnitsPerCoin) // the smallest withdrawal the page asks for, and the smallest payment that counts as one: 1 uARG
	withdrawCap      = 200_000                                  // open withdrawals (nothing paid to them yet) across the bridge; past it the oldest makes room
	withdrawPerAddr  = 20                                       // open withdrawals per swap address; past it one is handed out again or the oldest makes room
	withdrawPerIP    = 30                                       // withdrawals one client may have open (made within withdrawKeepIdle)
	withdrawKeepIdle = 48 * time.Hour                           // a withdrawal nothing was paid to: free to make again
	withdrawKeep     = 180 * 24 * time.Hour                     // any withdrawal
	withdrawSlack    = 120                                      // seconds a payment may predate its withdrawal (clocks, the scans)
	withdrawListMax  = 50                                       // payments listed on one withdrawal: the newest; the totals count every one
	withdrawViewTTL  = 5 * time.Second                          // how long an address's computed view is served (changes refresh it at once)
	withdrawViewMax  = 1000                                     // addresses whose view is cached
	withdrawIPsMax   = 100_000                                  // clients remembered for withdrawPerIP

	mempoolScanEvery  = 4 * time.Second
	mempoolScanPerRun = 300              // new transactions read per scan
	mempoolScanMax    = 50_000           // a mempool holding more transactions is not followed (payments show once mined)
	mempoolPerAddr    = 50               // mempool payments kept per swap address
	mempoolPaysMax    = 10_000           // mempool payments kept in all
	mempoolKeepMined  = 10 * time.Minute // a payment that left the mempool stays shown until its block is indexed, at most this long
)

// Key layout (state.go has the rest):
//
//	q/w/ ‖ code                                   Withdrawal
//	q/a/ ‖ address ‖ / ‖ be64(created_ms) ‖ code   the withdrawals to a swap address, oldest first
//	q/t/ ‖ be64(created_ms) ‖ code ‖ → address    the withdrawals not known to be paid, oldest first (the open ones)
//	q/p/ ‖ address ‖ / ‖ txid:vout                swapPaymentRef: a confirmed payment to a swap address
const (
	pfxWithdrawal   = "q/w/"
	pfxWithdrawTo   = "q/a/"
	pfxWithdrawOpen = "q/t/"
	pfxSwapPayment  = "q/p/"
)

func withdrawalKey(code string) []byte { return []byte(pfxWithdrawal + code) }
func withdrawToKey(addr string, createdMs int64, code string) []byte {
	return append(append([]byte(pfxWithdrawTo+addr+"/"), be64(uint64(createdMs))...), code...)
}
func withdrawOpenKey(createdMs int64, code string) []byte {
	return append(append([]byte(pfxWithdrawOpen), be64(uint64(createdMs))...), code...)
}
func swapPaymentKey(addr, id string) []byte { return []byte(pfxSwapPayment + addr + "/" + id) }

// Withdrawal is one payout the page asked for.
type Withdrawal struct {
	Code          string           `json:"code"`    // the memo, without its separator
	Address       string           `json:"address"` // the swap address to pay
	ChainID       uint64           `json:"chain_id"`
	Chain         string           `json:"chain"`
	Token         string           `json:"token"` // symbol
	TokenAddress  string           `json:"token_address"`
	Decimals      int              `json:"decimals"`
	To            string           `json:"to"`     // EIP-55
	Amount        consensus.Amount `json:"amount"` // uARG units the user means to send
	ReleaseFeeBps uint16           `json:"release_fee_bps"`
	Created       int64            `json:"created"`    // unix seconds
	CreatedMs     int64            `json:"created_ms"` // unix milliseconds: orders withdrawals made in the same second
}

// swapPaymentRef is a confirmed payment to a swap address, kept by address
// so a withdrawal finds its payments without reading every redemption. A
// uARG payment's redemption is filed under the same id; anything else sent
// to a swap address (ARG, another asset) can never be paid out or moved,
// and is kept only so the page can say so.
type swapPaymentRef struct {
	ID      string           `json:"id"` // txid:vout
	Amount  consensus.Amount `json:"amount"`
	Memo    string           `json:"memo,omitempty"`  // the withdrawal memo the transaction carried
	Lost    bool             `json:"lost,omitempty"`  // not uARG
	Asset   string           `json:"asset,omitempty"` // a lost payment's asset id; empty for ARG
	Height  uint32           `json:"height"`
	Created int64            `json:"created"` // when this guardian indexed it
	// Seen is when the payment was first seen: in the mempool by this
	// guardian if it was, otherwise its block's time (never later than
	// Created). A payment without a memo belongs to the newest withdrawal
	// made before it, so this, not Created, is what it is matched by.
	Seen int64 `json:"seen,omitempty"`
}

// seen is when the payment was first seen (Created on refs from before Seen).
func (r *swapPaymentRef) seen() int64 {
	if r.Seen != 0 {
		return r.Seen
	}
	return r.Created
}

// ---- memos ----

// FormatWithdrawCode shows a code as people read it: K7M2-X9RT.
func FormatWithdrawCode(code string) string {
	if len(code) != withdrawCodeLen {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// ParseWithdrawCode reads a memo as typed or pasted, in any case, with or
// without the separator, and returns the bare code, or "".
func ParseWithdrawCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "", "\r", "", "\n", "").Replace(s))
	if len(s) != withdrawCodeLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(handoffAlphabet, s[i]) < 0 {
			return ""
		}
	}
	return s
}

// withdrawMemo is the first withdrawal memo among a transaction's NULLDATA
// payloads, or "".
func withdrawMemo(memos [][]byte) string {
	for _, m := range memos {
		if len(m) <= 32 {
			if c := ParseWithdrawCode(string(m)); c != "" {
				return c
			}
		}
	}
	return ""
}

func newWithdrawCode() (string, error) {
	var b [withdrawCodeLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	out := make([]byte, withdrawCodeLen)
	for i, x := range b {
		out[i] = handoffAlphabet[x&31]
	}
	return string(out), nil
}

// ---- the store ----

// GetWithdrawal returns the withdrawal with this code.
func (s *State) GetWithdrawal(code string) (*Withdrawal, bool) {
	var w Withdrawal
	if code == "" || !s.get(withdrawalKey(code), &w) {
		return nil, false
	}
	return &w, true
}

// wdOrder is a withdrawal as the index of its address lists it.
type wdOrder struct {
	Code string
	Ms   int64 // created, unix milliseconds
}

func (o wdOrder) created() int64 { return o.Ms / 1000 }

// withdrawalOrders lists the withdrawals to a swap address, oldest first,
// from the address's index alone (no record is read).
func (s *State) withdrawalOrders(addr string) []wdOrder {
	pfx := []byte(pfxWithdrawTo + addr + "/")
	var out []wdOrder
	iter := s.db.NewIterator(util.BytesPrefix(pfx), nil)
	defer iter.Release()
	for iter.Next() {
		k := iter.Key()[len(pfx):]
		if len(k) != 8+withdrawCodeLen {
			continue
		}
		out = append(out, wdOrder{Code: string(k[8:]), Ms: int64(binary.BigEndian.Uint64(k[:8]))})
	}
	return out
}

// swapRefs lists the confirmed payments to a swap address.
func (s *State) swapRefs(addr string) []swapPaymentRef {
	var out []swapPaymentRef
	s.eachSwapRef(addr, func(r *swapPaymentRef) { out = append(out, *r) })
	return out
}

func (s *State) eachSwapRef(addr string, each func(*swapPaymentRef)) {
	iter := s.db.NewIterator(util.BytesPrefix([]byte(pfxSwapPayment+addr+"/")), nil)
	defer iter.Release()
	for iter.Next() {
		var r swapPaymentRef
		if json.Unmarshal(iter.Value(), &r) == nil {
			each(&r)
		}
	}
}

func (s *State) countWithdrawals() int {
	n := 0
	s.eachKey([]byte(pfxWithdrawal), func([]byte) { n++ })
	return n
}

// openWithdrawals counts the withdrawals not known to be paid (q/t/).
func (s *State) openWithdrawals() int {
	n := 0
	s.eachKey([]byte(pfxWithdrawOpen), func([]byte) { n++ })
	return n
}

// keyOpenIndex marks a state whose open index (q/t/) is kept.
var keyOpenIndex = []byte("q/m")

// prepareOpenIndex fills the open index from the withdrawals on record,
// once, in a state from before it, and returns the open count.
func (s *State) prepareOpenIndex() (int, error) {
	if s.has(keyOpenIndex) {
		return s.openWithdrawals(), nil
	}
	ops := []kvOp{{key: keyOpenIndex, value: []byte("1")}}
	iter := s.db.NewIterator(util.BytesPrefix([]byte(pfxWithdrawal)), nil)
	for iter.Next() {
		var wd Withdrawal
		if json.Unmarshal(iter.Value(), &wd) == nil && wd.Code != "" {
			ops = append(ops, kvOp{key: withdrawOpenKey(wd.CreatedMs, wd.Code), value: []byte(wd.Address)})
		}
	}
	iter.Release()
	if err := s.write(ops, true); err != nil {
		return 0, err
	}
	return s.openWithdrawals(), nil
}

// oldestOpen is the oldest withdrawal not known to be paid.
func (s *State) oldestOpen() (wdOrder, string, bool) {
	iter := s.db.NewIterator(util.BytesPrefix([]byte(pfxWithdrawOpen)), nil)
	defer iter.Release()
	for iter.Next() {
		k := iter.Key()[len(pfxWithdrawOpen):]
		if len(k) != 8+withdrawCodeLen {
			continue
		}
		return wdOrder{Code: string(k[8:]), Ms: int64(binary.BigEndian.Uint64(k[:8]))}, string(iter.Value()), true
	}
	return wdOrder{}, "", false
}

// forgetOps deletes a withdrawal and its index entries.
func forgetOps(addr string, o wdOrder) []kvOp {
	return []kvOp{{key: withdrawalKey(o.Code)}, {key: withdrawToKey(addr, o.Ms, o.Code)}, {key: withdrawOpenKey(o.Ms, o.Code)}}
}

// recordSwapRef files a confirmed payment to a swap address by address,
// in the same write as the rest of its transaction (undone on a reorg).
func recordSwapRef(w *l1Writer, addr string, ref swapPaymentRef) {
	w.put(swapPaymentKey(addr, ref.ID), &ref)
}

// paymentSeen is when a payment was first seen: in the mempool by this
// guardian if it was, otherwise its block's time, never later than now.
func (s *Service) paymentSeen(id string, blockTime int64) int64 {
	if s.mempool != nil {
		if t, ok := s.mempool.firstSeen(id); ok {
			return t
		}
	}
	now := time.Now().Unix()
	if blockTime > 0 && blockTime < now {
		return blockTime
	}
	return now
}

// swapWriteAddr says whether a write can change what a withdrawal shows,
// and for which swap address ("": not known, possibly any). State.write
// calls it for every key it writes, so the cached views (wdViewCache)
// follow the store exactly.
func swapWriteAddr(op kvOp) (string, bool) {
	k := op.key
	switch {
	case bytes.HasPrefix(k, []byte(pfxWithdrawTo)), bytes.HasPrefix(k, []byte(pfxSwapPayment)):
		rest := k[len(pfxWithdrawTo):]
		if i := bytes.IndexByte(rest, '/'); i > 0 {
			return string(rest[:i]), true
		}
		return "", true
	case len(k) > 1 && k[0] == 'x': // a redemption: a payment's progress
		if op.value == nil {
			return "", true
		}
		var r struct {
			Address string `json:"address"`
		}
		if json.Unmarshal(op.value, &r) != nil {
			return "", true
		}
		return r.Address, r.Address != "" // a burn with a memo is on no withdrawal
	}
	return "", false
}

// ---- creating one ----

// minWithdrawUnits is the smallest withdrawal the page asks for in token t:
// at least withdrawMin (1 uARG), and at least what pays the token's minimum
// release (gross), below which a payout is held, never sent.
func minWithdrawUnits(t *Token) consensus.Amount {
	min := withdrawMin
	if t.MinRelease == nil || t.MinRelease.Sign() <= 0 {
		return min
	}
	var u *big.Int
	if t.Decimals >= 8 {
		step := pow10(t.Decimals - 8) // the smallest u with u × 10^(D−8) ≥ the minimum
		u = new(big.Int).Add(t.MinRelease, new(big.Int).Sub(step, big.NewInt(1)))
		u.Quo(u, step)
	} else {
		u = new(big.Int).Mul(t.MinRelease, pow10(8-t.Decimals))
	}
	if !u.IsInt64() || !consensus.AssetRange(consensus.Amount(u.Int64())) {
		return consensus.MaxAssetAmount
	}
	if a := consensus.Amount(u.Int64()); a > min {
		min = a
	}
	return min
}

// netPayout is what units of uARG pay in a token after the release fee.
func netPayout(units consensus.Amount, decimals int, bps uint16) *big.Int {
	gross := TokenFromUnits(units, decimals)
	return gross.Sub(gross, FeeOf(gross, bps))
}

// createWithdrawal checks a request as the page would and files it under a
// new code. It refuses what could not be paid out now: a chain or token the
// bridge does not serve, a paused or unreachable chain, an amount below
// the minimum, above the largest automatic payout or above what the vault
// can pay right now.
func (s *Service) createWithdrawal(chainID uint64, symbol, toStr string, amount consensus.Amount) (*Withdrawal, error) {
	return s.createWithdrawalFor("", chainID, symbol, toStr, amount)
}

// createWithdrawalFor is createWithdrawal for a client (its address, the
// key of withdrawPerIP; "" for none).
func (s *Service) createWithdrawalFor(client string, chainID uint64, symbol, toStr string, amount consensus.Amount) (*Withdrawal, error) {
	bad := func(format string, args ...any) error {
		return &apiErr{http.StatusBadRequest, fmt.Sprintf(format, args...)}
	}
	busy := func(format string, args ...any) error {
		return &apiErr{http.StatusServiceUnavailable, fmt.Sprintf(format, args...)}
	}
	c, ok := s.byID[chainID]
	if !ok {
		return nil, bad("The bridge does not pay out on chain %d.", chainID)
	}
	t, err := c.tokenBySymbol(symbol)
	if err != nil {
		return nil, bad("%s", err.Error())
	}
	to, err := ParseAddress(toStr)
	if err != nil || to == ([20]byte{}) {
		return nil, bad("Enter an EVM address: 0x and 40 hex digits.")
	}
	if to == c.Vault || to == c.Router || to == t.Address {
		return nil, bad("That is one of the bridge's own contracts. Enter your own address.")
	}
	sym := ui0(s.cfg.AssetSymbol)
	if min := minWithdrawUnits(t); amount < min {
		return nil, bad("The smallest withdrawal is %s %s.", wallet.FormatAmount(min), sym)
	}
	if n := len(s.cfg.ReleaseTiers); n > 0 && amount > s.cfg.ReleaseTiers[n-1].UpTo {
		return nil, bad("At most %s %s per withdrawal.", wallet.FormatAmount(s.cfg.ReleaseTiers[n-1].UpTo), sym)
	}
	_, bps := c.fees()
	if netPayout(amount, t.Decimals, bps).Sign() <= 0 {
		return nil, bad("This amount is too small to pay out.")
	}
	st := s.snapshot().status
	if len(st.Paused) > 0 {
		return nil, busy("The bridge is paused right now. Try again later.")
	}
	for _, cs := range st.Chains {
		if cs.ChainID != c.ID {
			continue
		}
		if cs.Paused != "" || cs.VaultPaused {
			return nil, busy("Payouts on %s are paused right now. Try again later.", c.Name)
		}
		if cs.Error != "" {
			return nil, busy("%s cannot be reached right now. Try again in a few minutes.", c.Name)
		}
		for _, ts := range cs.Tokens {
			if !strings.EqualFold(ts.Address, HexAddress(t.Address)) || ts.AvailableNow == "" {
				continue
			}
			avail, ok := new(big.Int).SetString(ts.AvailableNow, 10)
			if !ok || TokenFromUnits(amount, t.Decimals).Cmp(avail) <= 0 {
				continue
			}
			if avail.Sign() == 0 {
				return nil, bad("There is no %s liquidity on %s right now. Choose another token or network, or try again later.", t.Symbol, c.Name)
			}
			return nil, bad("Only %s %s can be paid on %s right now.", FormatBigTokenAmount(avail, t.Decimals), t.Symbol, c.Name)
		}
	}
	addr, err := SwapAddress(s.cfg.Params.HRP, SwapDest{ChainID: c.ID, Symbol: t.Symbol, To: to})
	if err != nil {
		return nil, bad("%s", err.Error())
	}
	s.wdMu.Lock()
	defer s.wdMu.Unlock()
	now := time.Now()
	if client != "" && s.clientOpen(client, now) >= withdrawPerIP {
		return nil, &apiErr{http.StatusTooManyRequests, fmt.Sprintf("You have started %d withdrawals in the last %d hours. Use one of them, or try again later.", withdrawPerIP, int(withdrawKeepIdle/time.Hour))}
	}

	// Too many open withdrawals to this address: one with the same amount
	// made recently is handed out again, or the oldest open one is
	// forgotten. A withdrawal something was paid to is never forgotten here.
	if orders := s.st.withdrawalOrders(addr); len(orders) >= withdrawPerAddr {
		av := s.computeAddr(addr, false)
		var open []wdOrder
		for _, o := range orders {
			if tl := av.by[o.Code]; tl == nil || !tl.paid {
				open = append(open, o)
			}
		}
		if len(open) >= withdrawPerAddr {
			for i := len(open) - 1; i >= 0 && i >= len(open)-withdrawPerAddr; i-- {
				if now.Sub(time.UnixMilli(open[i].Ms)) > withdrawKeepIdle/2 {
					break
				}
				w, ok := s.st.GetWithdrawal(open[i].Code)
				if ok && w.Amount == amount && w.ReleaseFeeBps == bps && w.Decimals == t.Decimals && strings.EqualFold(w.TokenAddress, HexAddress(t.Address)) {
					return w, nil
				}
			}
			var ops []kvOp
			n := 0
			for ; len(open)-n >= withdrawPerAddr; n++ {
				ops = append(ops, forgetOps(addr, open[n])...)
			}
			if err := s.st.write(ops, false); err != nil {
				return nil, err
			}
			s.wdCount -= n
			s.cfg.Logf("bridge: withdrawals: %d open to %s already; forgot the oldest %d nothing was paid to", len(open), addr, n)
		}
	}
	// Too many open withdrawals in all: the oldest open one makes room.
	for tries := 0; s.wdCount >= withdrawCap; tries++ {
		if tries == 1000 {
			return nil, busy("Too many withdrawals are open right now. Try again later.")
		}
		if err := s.makeRoom(); err != nil {
			return nil, err
		}
	}
	for try := 0; try < 20; try++ {
		code, err := newWithdrawCode()
		if err != nil {
			return nil, err
		}
		if s.st.has(withdrawalKey(code)) {
			continue
		}
		wd := &Withdrawal{Code: code, Address: addr, ChainID: c.ID, Chain: c.Name, Token: strings.ToUpper(t.Symbol), TokenAddress: HexAddress(t.Address),
			Decimals: t.Decimals, To: ChecksumAddress(to), Amount: amount, ReleaseFeeBps: bps, Created: now.Unix(), CreatedMs: now.UnixMilli()}
		b, err := json.Marshal(wd)
		if err != nil {
			return nil, err
		}
		if err := s.st.write([]kvOp{{key: withdrawalKey(code), value: b}, {key: withdrawToKey(addr, wd.CreatedMs, code), value: []byte(code)},
			{key: withdrawOpenKey(wd.CreatedMs, code), value: []byte(addr)}}, true); err != nil {
			return nil, err
		}
		s.wdCount++
		if client != "" {
			s.clientAdd(client, code, now)
		}
		s.cfg.Logf("bridge: withdrawal %s: %s %s -> %s on %s to %s", FormatWithdrawCode(code), wallet.FormatAmount(amount), sym, wd.Token, c.Name, wd.To)
		return wd, nil
	}
	return nil, errors.New("could not allocate a memo")
}

// makeRoom takes the oldest open withdrawal out of the open count: it is
// forgotten when nothing was paid to it, and only leaves the open index
// when something was (it is kept until withdrawKeep). Callers hold wdMu.
func (s *Service) makeRoom() error {
	o, addr, ok := s.st.oldestOpen()
	if !ok {
		s.wdCount = 0 // nothing is open: the count was off
		return nil
	}
	ops := forgetOps(addr, o)
	if tl := s.computeAddr(addr, false).by[o.Code]; tl != nil && tl.paid {
		ops = ops[2:] // paid: keep it, it no longer counts as open
	}
	if err := s.st.write(ops, false); err != nil {
		return err
	}
	s.wdCount--
	return nil
}

// clientOpen counts the withdrawals a client made within withdrawKeepIdle
// that are still on record (callers hold wdMu).
func (s *Service) clientOpen(client string, now time.Time) int {
	l := s.wdClients[client]
	keep := l[:0]
	for _, x := range l {
		if now.Unix()-x.at < int64(withdrawKeepIdle/time.Second) && s.st.has(withdrawalKey(x.code)) {
			keep = append(keep, x)
		}
	}
	if len(keep) == 0 {
		delete(s.wdClients, client)
	} else {
		s.wdClients[client] = keep
	}
	return len(keep)
}

func (s *Service) clientAdd(client, code string, now time.Time) {
	if _, ok := s.wdClients[client]; !ok && len(s.wdClients) >= withdrawIPsMax {
		for k, l := range s.wdClients {
			if now.Unix()-l[len(l)-1].at >= int64(withdrawKeepIdle/time.Second) {
				delete(s.wdClients, k)
			}
		}
		if len(s.wdClients) >= withdrawIPsMax {
			s.wdClients = map[string][]wdClientEntry{} // a flood from more clients than this: start again
		}
	}
	s.wdClients[client] = append(s.wdClients[client], wdClientEntry{code: code, at: now.Unix()})
}

// wdClientEntry is a withdrawal a client made.
type wdClientEntry struct {
	code string
	at   int64
}

// ui0 is the asset symbol as the page writes it (uARG).
func ui0(sym string) string {
	if strings.EqualFold(sym, "UARG") {
		return "uARG"
	}
	return sym
}

// pruneWithdrawals forgets withdrawals nothing was paid to after
// withdrawKeepIdle, and every withdrawal after withdrawKeep. Whether
// something was paid is worked out once per address.
func (s *Service) pruneWithdrawals(now time.Time) {
	paid := map[string]bool{} // withdrawal codes a qualifying payment belongs to
	done := map[string]bool{} // addresses worked out
	var ops []kvOp
	forgot := 0
	iter := s.st.db.NewIterator(util.BytesPrefix([]byte(pfxWithdrawal)), nil)
	for iter.Next() {
		var wd Withdrawal
		if json.Unmarshal(iter.Value(), &wd) != nil {
			continue
		}
		age := time.Duration(now.Unix()-wd.Created) * time.Second
		if age <= withdrawKeepIdle {
			continue
		}
		o := wdOrder{Code: wd.Code, Ms: wd.CreatedMs}
		if age <= withdrawKeep {
			if !done[wd.Address] {
				done[wd.Address] = true
				for code, tl := range s.computeAddr(wd.Address, false).by {
					if tl.paid {
						paid[code] = true
					}
				}
			}
			if paid[wd.Code] {
				if s.st.has(withdrawOpenKey(o.Ms, o.Code)) {
					ops = append(ops, kvOp{key: withdrawOpenKey(o.Ms, o.Code)}) // paid: no longer open
				}
				continue
			}
		}
		ops = append(ops, forgetOps(wd.Address, o)...)
		forgot++
	}
	iter.Release()
	if len(ops) == 0 {
		return
	}
	s.wdMu.Lock()
	defer s.wdMu.Unlock()
	if err := s.st.write(ops, false); err != nil {
		s.cfg.Logf("bridge: pruning withdrawals: %v", err)
		return
	}
	s.wdCount = s.st.openWithdrawals()
	if forgot > 0 {
		s.cfg.Logf("bridge: forgot %d withdrawals (nothing paid to them for %v, or older than %v)", forgot, withdrawKeepIdle, withdrawKeep)
	}
}

// ---- the mempool ----

// mempoolPay is a payment to a swap address waiting in the node's mempool.
type mempoolPay struct {
	ID      string // txid:vout
	TxID    string
	Address string
	Amount  consensus.Amount
	Asset   string // hex; "" for ARG
	Memo    string
	Seen    int64 // unix seconds this guardian first read it
	// Payout is what the whole transaction pays out of the bridge
	// (txPayout), the amount its payout depth follows.
	Payout consensus.Amount
}

// mempoolWatch keeps the payments to swap addresses that are not in an
// indexed block yet, so the page shows a payment the moment it is
// broadcast and until its block is indexed.
type mempoolWatch struct {
	mu     sync.Mutex
	txs    map[string][]mempoolPay // every transaction read, by txid: its payments to swap addresses (most have none)
	gone   map[string]goneTx       // transactions with payments that left the mempool, until their block is indexed (at most mempoolKeepMined)
	byAddr map[string][]mempoolPay // what is shown: the above by address, earliest first, at most mempoolPerAddr each
	ok     bool                    // the last scan worked
}

// goneTx is a transaction's payments after it left the mempool.
type goneTx struct {
	pays []mempoolPay
	at   time.Time
}

func newMempoolWatch() *mempoolWatch {
	return &mempoolWatch{txs: map[string][]mempoolPay{}, gone: map[string]goneTx{}, byAddr: map[string][]mempoolPay{}}
}

func (m *mempoolWatch) payments(addr string) ([]mempoolPay, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mempoolPay{}, m.byAddr[addr]...), m.ok
}

// firstSeen is when this guardian first read a payment in the mempool.
func (m *mempoolWatch) firstSeen(id string) (int64, bool) {
	txid, _, _ := strings.Cut(id, ":")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ps := range [][]mempoolPay{m.txs[txid], m.gone[txid].pays} {
		for _, p := range ps {
			if p.ID == id {
				return p.Seen, true
			}
		}
	}
	return 0, false
}

// rebuildLocked lays out byAddr anew, within its bounds, and returns the
// addresses whose payments changed.
func (m *mempoolWatch) rebuildLocked() []string {
	var all []mempoolPay
	for _, ps := range m.txs {
		all = append(all, ps...)
	}
	for _, g := range m.gone {
		all = append(all, g.pays...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Seen != all[j].Seen {
			return all[i].Seen < all[j].Seen
		}
		return all[i].ID < all[j].ID
	})
	by := map[string][]mempoolPay{}
	n := 0
	for _, p := range all {
		if n == mempoolPaysMax {
			break
		}
		if len(by[p.Address]) < mempoolPerAddr {
			by[p.Address] = append(by[p.Address], p)
			n++
		}
	}
	var changed []string
	for a, ps := range by {
		if !samePays(ps, m.byAddr[a]) {
			changed = append(changed, a)
		}
	}
	for a := range m.byAddr {
		if _, ok := by[a]; !ok {
			changed = append(changed, a)
		}
	}
	m.byAddr = by
	return changed
}

func samePays(a, b []mempoolPay) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Seen != b[i].Seen {
			return false
		}
	}
	return true
}

func (m *mempoolWatch) count() int {
	n := 0
	for _, ps := range m.txs {
		n += len(ps)
	}
	for _, g := range m.gone {
		n += len(g.pays)
	}
	return n
}

// swapPays lists what a transaction pays to swap addresses, at least
// withdrawMin each and at most mempoolPerAddr, with the withdrawal memo it
// carries.
func (s *Service) swapPays(tx *wallet.TxJSON, now int64) []mempoolPay {
	var memos [][]byte
	var pays []mempoolPay
	for _, o := range tx.Outputs {
		switch {
		case o.Type == consensus.OutputTypeNullData:
			if b, err := hex.DecodeString(o.Payload); err == nil {
				memos = append(memos, b)
			}
		case o.Type == consensus.OutputTypeP2PK && o.Amount >= withdrawMin && len(pays) < mempoolPerAddr && IsSwapAddress(s.cfg.Params.HRP, o.Address):
			pays = append(pays, mempoolPay{ID: tx.TxID + ":" + strconv.Itoa(o.N), TxID: tx.TxID, Address: o.Address, Amount: o.Amount, Asset: o.Asset, Seen: now})
		}
	}
	memo := withdrawMemo(memos)
	payout := s.txPayout(tx)
	for i := range pays {
		pays[i].Memo, pays[i].Payout = memo, payout
	}
	return pays
}

// errMempoolTooBig: the mempool holds more than mempoolScanMax
// transactions, so it is not read; payments show once mined.
type errMempoolTooBig struct{ n int }

func (e errMempoolTooBig) Error() string {
	return fmt.Sprintf("the mempool holds %d transactions, more than the %d this guardian follows", e.n, mempoolScanMax)
}

// scanMempool reads the transactions that entered the node's mempool since
// the last scan and moves the ones that left it (mined or dropped) aside
// until their block is indexed.
func (s *Service) scanMempool() error {
	m := s.mempool
	var info struct {
		Size int `json:"size"`
	}
	if raw, err := s.cfg.Node.Call("getmempoolinfo"); err == nil && json.Unmarshal(raw, &info) == nil && info.Size > mempoolScanMax {
		return s.mempoolTooBig(info.Size)
	}
	ids, err := s.cfg.Node.RawMempool()
	if err != nil {
		m.mu.Lock()
		m.ok = false
		m.mu.Unlock()
		return fmt.Errorf("node: %w", err)
	}
	if len(ids) > mempoolScanMax {
		return s.mempoolTooBig(len(ids))
	}
	now := time.Now()
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	m.mu.Lock()
	for id, ps := range m.txs {
		if !live[id] {
			delete(m.txs, id)
			if len(ps) > 0 {
				m.gone[id] = goneTx{pays: ps, at: now}
			}
		}
	}
	var fresh []string
	for _, id := range ids {
		if _, ok := m.txs[id]; !ok {
			fresh = append(fresh, id)
			if len(fresh) == mempoolScanPerRun {
				break
			}
		}
	}
	gone := map[string][]mempoolPay{}
	for id, g := range m.gone {
		if now.Sub(g.at) > mempoolKeepMined {
			delete(m.gone, id)
		} else {
			gone[id] = g.pays
		}
	}
	m.mu.Unlock()

	read := make(map[string][]mempoolPay, len(fresh))
	for _, id := range fresh {
		tx, err := s.cfg.Node.MempoolTx(id)
		if err != nil {
			continue // left the mempool since the list; the next scan knows
		}
		read[id] = s.swapPays(tx, now.Unix())
	}
	var indexed []string
	for id, ps := range gone {
		for _, p := range ps {
			if s.st != nil && s.st.has(swapPaymentKey(p.Address, p.ID)) {
				indexed = append(indexed, id)
				break
			}
		}
	}

	m.mu.Lock()
	for _, id := range indexed {
		delete(m.gone, id)
	}
	kept := m.count()
	for id, ps := range read {
		if g, ok := m.gone[id]; ok { // back from a block that left the chain: keep when it was first seen
			for i := range ps {
				for _, q := range g.pays {
					if q.ID == ps[i].ID && q.Seen < ps[i].Seen {
						ps[i].Seen = q.Seen
					}
				}
			}
			kept -= len(g.pays)
			delete(m.gone, id)
		}
		if kept+len(ps) > mempoolPaysMax {
			ps = nil // shown once mined
		}
		kept += len(ps)
		m.txs[id] = ps
	}
	changed := m.rebuildLocked()
	m.ok = true
	m.mu.Unlock()
	s.viewsChanged(changed)
	return nil
}

// mempoolTooBig stops following the mempool: what was in it is shown until
// its block is indexed (mempoolKeepMined at most), as if it had left.
func (s *Service) mempoolTooBig(n int) error {
	m := s.mempool
	now := time.Now()
	m.mu.Lock()
	for id, ps := range m.txs {
		if _, ok := m.gone[id]; !ok && len(ps) > 0 {
			m.gone[id] = goneTx{pays: ps, at: now}
		}
	}
	m.txs = map[string][]mempoolPay{}
	for id, g := range m.gone {
		if now.Sub(g.at) > mempoolKeepMined {
			delete(m.gone, id)
		}
	}
	changed := m.rebuildLocked()
	m.ok = false
	m.mu.Unlock()
	s.viewsChanged(changed)
	return errMempoolTooBig{n}
}

func (s *Service) viewsChanged(addrs []string) {
	if s.wdViews == nil {
		return
	}
	for _, a := range addrs {
		s.wdViews.changed(a)
	}
}

// mempoolLoop scans the mempool every few seconds and prunes old
// withdrawals every hour, until ctx ends. A failure is logged once, when
// it starts.
func (s *Service) mempoolLoop(ctx context.Context) {
	failing := false
	var pruned time.Time
	for {
		err := s.scanMempool()
		if err != nil && !failing {
			s.cfg.Logf("bridge: mempool scan: %v (payments to withdrawal addresses show once mined)", err)
		}
		if err == nil && failing {
			s.cfg.Logf("bridge: mempool scan works again")
		}
		failing = err != nil
		if time.Since(pruned) >= time.Hour {
			s.pruneWithdrawals(time.Now())
			pruned = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(mempoolScanEvery):
		}
	}
}

// ---- progress ----

// Withdrawal and payment stages, in order. A payment is detected from the
// moment it is seen (in the mempool or a block) until it has the
// confirmations its amount needs; then the guardians process it (sign and
// send the release, follow it on the EVM chain) until it is completed.
const (
	StageWaiting    = "waiting"
	StageDetected   = "detected"
	StageProcessing = "processing"
	StageCompleted  = "completed"
	StageFailed     = "failed"
)

func stageRank(s string) int {
	switch s {
	case StageDetected:
		return 1
	case StageProcessing:
		return 2
	case StageCompleted:
		return 3
	}
	return 0
}

// WithdrawalPayment is one payment shown on a withdrawal.
type WithdrawalPayment struct {
	ID            string           `json:"id"` // txid:vout
	TxID          string           `json:"txid"`
	Amount        consensus.Amount `json:"amount"`          // units of what was paid
	Asset         string           `json:"asset,omitempty"` // set when it was not uARG: "ARG" or an asset id (no longer listed: see lost_count)
	Memo          bool             `json:"memo"`            // it carried this withdrawal's memo
	Stage         string           `json:"stage"`           // detected, processing, completed or failed
	State         string           `json:"state"`           // mempool, confirming, sending, paying, paid or failed
	Height        uint32           `json:"height,omitempty"`
	Confirmations uint32           `json:"confirmations"` // blocks so far
	Needed        uint32           `json:"needed"`        // blocks before it is paid out
	TokenPaid     string           `json:"token_paid"`    // token units it pays, after the fee
	EVMTx         string           `json:"evm_tx,omitempty"`
	// Wait says why a processing payment waits: "liquidity" (the vault is
	// short of the token), "paused", "limit" (the vault's release limits),
	// "review" (above the largest automatic payout) or "retry".
	Wait string `json:"wait,omitempty"`
	// Reason says why a payment failed: "small" (too small to pay out, or
	// below the token's minimum withdrawal) or "manual".
	Reason string `json:"reason,omitempty"`
	Seen   int64  `json:"seen"` // when it was first seen
}

// WithdrawalView is a withdrawal with its payments and where they are.
// Payments lists the uARG payments of at least 1 uARG that belong to it,
// the newest withdrawListMax of them, oldest first; every total counts all
// of its payments. Smaller uARG payments (dust) are only counted (in
// received while they are being paid out), and anything other than uARG
// sent to the address is lost and only counted.
type WithdrawalView struct {
	Withdrawal
	Memo          string              `json:"memo"`           // the code as people read it, K7M2-X9RT
	Status        string              `json:"status"`         // waiting, detected, processing, completed or failed
	ExpectedPaid  string              `json:"expected_paid"`  // token units the full amount pays, after the fee
	Received      consensus.Amount    `json:"received"`       // uARG units paid so far, the mempool included (dust only while it is paid out)
	Remaining     consensus.Amount    `json:"remaining"`      // short of the amount
	Extra         consensus.Amount    `json:"extra"`          // above the amount
	TokenPaid     string              `json:"token_paid"`     // token units delivered so far
	TokenDue      string              `json:"token_due"`      // token units its payments pay once delivered (failed ones aside)
	Payments      []WithdrawalPayment `json:"payments"`       // the newest withdrawListMax
	PaymentsTotal int                 `json:"payments_total"` // the payments that could be listed, all of them
	DustCount     int                 `json:"dust_count"`     // uARG payments below 1 uARG (counted, not listed)
	DustAmount    consensus.Amount    `json:"dust_amount"`
	LostCount     int                 `json:"lost_count"` // payments of ARG or another asset: never paid out
	LostARG       consensus.Amount    `json:"lost_arg"`   // ARG units among them
	L1Height      uint64              `json:"l1_height"`
	Mempool       bool                `json:"mempool"` // payments show before their block
}

// waitOf sorts a pending redemption's hold reason (holdRedemption).
func waitOf(why string) string {
	w := strings.ToLower(why)
	switch {
	case why == "":
		return ""
	case strings.Contains(w, "liquidity"):
		return "liquidity"
	case strings.Contains(w, "paused"):
		return "paused"
	case strings.Contains(w, "limit") || strings.Contains(w, "window"):
		return "limit"
	case strings.Contains(w, "manual release") || strings.Contains(w, "largest automatic"):
		return "review"
	}
	return "retry"
}

// ownerOf is the withdrawal a payment belongs to: the one whose memo it
// carries, when that is a withdrawal to this address; otherwise the newest
// made at or before the payment was first seen, or, when it was seen a
// moment before any (clocks, the scans), the first. orders are oldest
// first; "" when there is none.
func ownerOf(orders []wdOrder, codes map[string]bool, memo string, seen int64) (string, bool) {
	if memo != "" && codes[memo] {
		return memo, true
	}
	if i := sort.Search(len(orders), func(i int) bool { return orders[i].created() > seen }); i > 0 {
		return orders[i-1].Code, false
	}
	if len(orders) > 0 && orders[0].created()-withdrawSlack <= seen {
		return orders[0].Code, false
	}
	return "", false
}

// wdTally is what the payments that belong to one withdrawal come to.
type wdTally struct {
	listed    []WithdrawalPayment // oldest first, the newest withdrawListMax
	count     int                 // listed before the cut
	received  consensus.Amount
	tokenPaid *big.Int
	tokenDue  *big.Int
	rank      int // the least advanced stage of the listed payments that did not fail; -1: none
	dustCount int
	dust      consensus.Amount
	lostCount int
	lostARG   consensus.Amount
	paid      bool // a uARG payment of at least withdrawMin belongs to it, confirmed or in the mempool
}

// addrView is what every payment to one swap address, confirmed or in the
// mempool, comes to, by the withdrawal it belongs to.
type addrView struct {
	l1       uint64 // the Argonmine height it was computed at
	at       time.Time
	ver, all uint64 // wdViewCache versions it was computed at
	mempool  bool
	by       map[string]*wdTally
}

// computeAddr works out which withdrawal each payment to addr belongs to,
// in one pass over the address's withdrawals (their index only) and its
// payments. With progress it reads each uARG payment's redemption and
// fills the stages and totals; without, it only says which withdrawals
// were paid (pruning, making room).
func (s *Service) computeAddr(addr string, progress bool) *addrView {
	l1, _, _ := s.st.Cursor("l1")
	v := &addrView{l1: l1, at: time.Now(), by: map[string]*wdTally{}}
	orders := s.st.withdrawalOrders(addr)
	pool, live := s.mempool.payments(addr)
	v.mempool = live
	if len(orders) == 0 {
		return v
	}
	codes := make(map[string]bool, len(orders))
	for _, o := range orders {
		codes[o.Code] = true
	}
	tally := func(code string) *wdTally {
		t := v.by[code]
		if t == nil {
			t = &wdTally{tokenPaid: new(big.Int), tokenDue: new(big.Int), rank: -1}
			v.by[code] = t
		}
		return t
	}
	add := func(t *wdTally, p WithdrawalPayment) {
		failed := p.Stage == StageFailed
		if x, ok := new(big.Int).SetString(p.TokenPaid, 10); ok {
			if p.State == "paid" {
				t.tokenPaid.Add(t.tokenPaid, x)
			}
			if !failed {
				t.tokenDue.Add(t.tokenDue, x)
			}
		}
		if p.Amount < withdrawMin {
			// Dust: counted, not listed, and part of what arrived only
			// while it is being paid out (nothing on the page explains a
			// failed one).
			t.dustCount++
			t.dust += p.Amount
			if !failed {
				t.received += p.Amount
			}
			return
		}
		t.received += p.Amount
		t.paid = true
		t.count++
		t.listed = append(t.listed, p)
		if p.Stage != StageFailed && (t.rank < 0 || stageRank(p.Stage) < t.rank) {
			t.rank = stageRank(p.Stage)
		}
	}

	confirmed := map[string]bool{}
	s.st.eachSwapRef(addr, func(ref *swapPaymentRef) {
		confirmed[ref.ID] = true
		owner, memoOK := ownerOf(orders, codes, ref.Memo, ref.seen())
		if owner == "" {
			return
		}
		t := tally(owner)
		if ref.Lost {
			t.lostCount++
			if ref.Asset == "" {
				t.lostARG += ref.Amount
			}
			return
		}
		if !progress {
			t.paid = t.paid || ref.Amount >= withdrawMin
			return
		}
		r, ok := s.st.GetRedemption(ref.ID)
		if !ok {
			return
		}
		p := WithdrawalPayment{ID: ref.ID, TxID: strings.SplitN(ref.ID, ":", 2)[0], Amount: ref.Amount, Memo: memoOK, Height: ref.Height, Seen: ref.seen(), TokenPaid: "0"}
		if l1 >= uint64(ref.Height) {
			p.Confirmations = uint32(l1 - uint64(ref.Height) + 1)
		}
		s.paymentProgress(&p, r)
		add(t, p)
	})

	assetHex := s.asset.String()
	recs := map[string]*Withdrawal{}
	for _, mp := range pool {
		if confirmed[mp.ID] {
			continue // indexed since: shown from its block
		}
		owner, memoOK := ownerOf(orders, codes, mp.Memo, mp.Seen)
		if owner == "" {
			continue
		}
		t := tally(owner)
		if mp.Asset != assetHex {
			t.lostCount++
			if mp.Asset == "" {
				t.lostARG += mp.Amount
			}
			continue
		}
		if !progress {
			t.paid = t.paid || mp.Amount >= withdrawMin
			continue
		}
		p := WithdrawalPayment{ID: mp.ID, TxID: mp.TxID, Amount: mp.Amount, Memo: memoOK, Seen: mp.Seen, Stage: StageDetected, State: "mempool", TokenPaid: "0"}
		p.Needed = s.cfg.L1Confirmations
		depth := mp.Amount
		if mp.Payout > depth {
			depth = mp.Payout
		}
		if need, ok := s.burnDepth(mp.ID, depth); ok {
			p.Needed = need
		}
		wd, ok := recs[owner]
		if !ok {
			wd, _ = s.st.GetWithdrawal(owner)
			recs[owner] = wd
		}
		if wd != nil {
			p.TokenPaid = netPayout(mp.Amount, wd.Decimals, wd.ReleaseFeeBps).String()
		}
		add(t, p)
	}

	for _, t := range v.by {
		sort.Slice(t.listed, func(i, j int) bool {
			if t.listed[i].Seen != t.listed[j].Seen {
				return t.listed[i].Seen < t.listed[j].Seen
			}
			return t.listed[i].ID < t.listed[j].ID
		})
		if n := len(t.listed); n > withdrawListMax {
			t.listed = append([]WithdrawalPayment(nil), t.listed[n-withdrawListMax:]...)
		}
	}
	return v
}

// addrViewOf is computeAddr with progress, from the cache while nothing
// about the address changed (wdViewCache), at most withdrawViewTTL old.
func (s *Service) addrViewOf(addr string) *addrView {
	l1, _, _ := s.st.Cursor("l1")
	return s.wdViews.get(addr, func(v *addrView) bool {
		return v.l1 == l1 && time.Since(v.at) < withdrawViewTTL
	}, func() *addrView { return s.computeAddr(addr, true) })
}

// withdrawalView reports where each payment that belongs to the
// withdrawal is.
func (s *Service) withdrawalView(wd *Withdrawal) WithdrawalView {
	av := s.addrViewOf(wd.Address)
	v := WithdrawalView{Withdrawal: *wd, Memo: FormatWithdrawCode(wd.Code), Payments: []WithdrawalPayment{}, TokenPaid: "0", TokenDue: "0",
		L1Height: av.l1, Mempool: av.mempool, Status: StageWaiting}
	v.ExpectedPaid = netPayout(wd.Amount, wd.Decimals, wd.ReleaseFeeBps).String()
	if t := av.by[wd.Code]; t != nil {
		v.Payments = append(v.Payments, t.listed...)
		v.PaymentsTotal, v.Received, v.TokenPaid, v.TokenDue = t.count, t.received, t.tokenPaid.String(), t.tokenDue.String()
		v.DustCount, v.DustAmount, v.LostCount, v.LostARG = t.dustCount, t.dust, t.lostCount, t.lostARG
		switch {
		case t.count == 0:
			v.Status = StageWaiting
		case t.rank < 0:
			v.Status = StageFailed
		default:
			v.Status = []string{StageWaiting, StageDetected, StageProcessing, StageCompleted}[t.rank]
		}
	}
	if v.Received < wd.Amount {
		v.Remaining = wd.Amount - v.Received
	} else {
		v.Extra = v.Received - wd.Amount
	}
	return v
}

// paymentProgress fills a confirmed payment's stage from its redemption.
func (s *Service) paymentProgress(p *WithdrawalPayment, r *Redemption) {
	p.Needed = r.Confirmations
	if p.Needed == 0 {
		p.Needed = s.cfg.L1Confirmations
	}
	if r.TokenPaid != "" {
		p.TokenPaid = r.TokenPaid
	}
	p.EVMTx = r.EVMTx
	switch r.Status {
	case RedemptionPending:
		if p.Confirmations < p.Needed {
			p.Stage, p.State = StageDetected, "confirming"
		} else {
			p.Stage, p.State, p.Wait = StageProcessing, "sending", waitOf(r.Error)
		}
	case RedemptionReleased:
		p.Stage, p.State = StageProcessing, "paying"
	case RedemptionConfirmed:
		p.Stage, p.State = StageCompleted, "paid"
	default:
		p.Stage, p.State, p.Reason = StageFailed, StageFailed, "manual"
		if strings.Contains(r.Error, "too small") || strings.Contains(r.Error, "below the minimum withdrawal") {
			p.Reason = "small"
		}
	}
}

// ---- the cache of address views ----

// wdViewCache keeps each address's computed view. Every write to the store
// that concerns an address (its withdrawals, its payments, their
// redemptions: State.write through swapWriteAddr) and every change to its
// mempool payments makes it out of date at once, so a view is never older
// than what changed; while nothing changes, it is computed once per
// withdrawViewTTL or new Argonmine block however many ask.
type wdViewCache struct {
	mu       sync.Mutex
	all      uint64 // bumped by a change that names no address
	m        map[string]*wdViewEntry
	swept    time.Time
	computed atomic.Int64 // views computed (tests)
}

type wdViewEntry struct {
	mu   sync.Mutex // one computation at a time per address
	view *addrView  // under mu
	ver  uint64     // under wdViewCache.mu: bumped by every change to the address
	used time.Time  // under wdViewCache.mu
}

func newWdViewCache() *wdViewCache { return &wdViewCache{m: map[string]*wdViewEntry{}} }

// changed makes what is cached for addr ("": every address) out of date.
func (c *wdViewCache) changed(addr string) {
	c.mu.Lock()
	if addr == "" {
		c.all++
	} else if e := c.m[addr]; e != nil {
		e.ver++
	}
	c.mu.Unlock()
}

func (c *wdViewCache) get(addr string, fresh func(*addrView) bool, compute func() *addrView) *addrView {
	now := time.Now()
	c.mu.Lock()
	if now.Sub(c.swept) > withdrawViewTTL || len(c.m) >= withdrawViewMax {
		for k, x := range c.m {
			if now.Sub(x.used) > withdrawViewTTL {
				delete(c.m, k) // too old to be served anyway
			}
		}
		c.swept = now
		if len(c.m) >= withdrawViewMax { // that many addresses asked for within withdrawViewTTL: forget a quarter
			for k := range c.m {
				if len(c.m) < withdrawViewMax*3/4 {
					break
				}
				delete(c.m, k)
			}
		}
	}
	e := c.m[addr]
	if e == nil {
		e = &wdViewEntry{}
		c.m[addr] = e
	}
	e.used = now
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	c.mu.Lock()
	ver, all := e.ver, c.all
	c.mu.Unlock()
	if v := e.view; v != nil && v.ver == ver && v.all == all && fresh(v) {
		return v
	}
	v := compute()
	c.computed.Add(1)
	v.ver, v.all = ver, all // a change while it was computed leaves it out of date
	e.view = v
	return v
}

// ---- the API ----

// handleWithdrawals answers POST /api/withdrawals with a JSON body
// {"chain_id": 11155111, "token": "USDC", "to": "0x…", "amount": "10"}:
// the new withdrawal, its swap address and memo. Only the bridge page
// itself (this origin) creates withdrawals: the body must be sent as
// application/json, which a page elsewhere can only send after a CORS
// preflight, and the preflight is refused; no answer here carries
// Access-Control-Allow-Origin, so another origin cannot read one either.
// (The GET endpoints stay open to every origin.)
func (s *Service) handleWithdrawals(w http.ResponseWriter, r *http.Request) {
	w = sameOriginOnly{w}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodOptions:
		apiError(w, http.StatusForbidden, "Withdrawals are made on the bridge page.")
		return
	case http.MethodPost:
	default:
		apiError(w, http.StatusMethodNotAllowed, "POST a withdrawal")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send the withdrawal as JSON, with Content-Type: application/json")
		return
	}
	ip := s.clientIP(r)
	if !s.wdPost.allow(ip, time.Now()) {
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusTooManyRequests, "Too many withdrawals at once. Wait a few seconds.")
		return
	}
	var req struct {
		ChainID json.RawMessage `json:"chain_id"`
		Token   string          `json:"token"`
		To      string          `json:"to"`
		Amount  string          `json:"amount"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "the body must be JSON with chain_id, token, to and amount")
		return
	}
	chainID, err := strconv.ParseUint(strings.Trim(string(req.ChainID), "\" "), 10, 64)
	if err != nil || chainID == 0 {
		apiError(w, http.StatusBadRequest, "chain_id must be a chain id")
		return
	}
	units, err := wallet.ParseAmount(strings.TrimSpace(req.Amount))
	if err != nil || units <= 0 {
		apiError(w, http.StatusBadRequest, "Enter an amount with at most 8 decimals.")
		return
	}
	wd, err := s.createWithdrawalFor(ip, chainID, strings.TrimSpace(req.Token), strings.TrimSpace(req.To), units)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, s.withdrawalView(wd))
}

// sameOriginOnly is a response that never carries Access-Control-Allow-Origin
// (writeJSON and apiError set it for the public GET endpoints), so no page
// of another origin may read it.
type sameOriginOnly struct{ http.ResponseWriter }

func (w sameOriginOnly) WriteHeader(code int) {
	w.Header().Del("Access-Control-Allow-Origin")
	w.ResponseWriter.WriteHeader(code)
}

func (w sameOriginOnly) Write(b []byte) (int, error) {
	w.Header().Del("Access-Control-Allow-Origin")
	return w.ResponseWriter.Write(b)
}

// handleWithdrawal answers GET /api/withdrawal/{memo}: the withdrawal and
// the progress of the payments that belong to it.
func (s *Service) handleWithdrawal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		apiError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.wdGet.allow(s.clientIP(r), time.Now()) {
		w.Header().Set("Retry-After", "5")
		apiError(w, http.StatusTooManyRequests, "too many requests; wait a few seconds")
		return
	}
	wd, ok := s.st.GetWithdrawal(ParseWithdrawCode(strings.TrimPrefix(r.URL.Path, "/api/withdrawal/")))
	if !ok {
		apiError(w, http.StatusNotFound, "No withdrawal has this memo.")
		return
	}
	writeJSON(w, s.withdrawalView(wd))
}
