package swap

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"

	"argonmine/consensus"
	"argonmine/wallet"
)

// The swap fees belong to the project treasury (docs/FEES.md). Every swap
// owes the treasury its fee: the spread (or the wrap fee) on what the wallet
// paid, kept in ARG when the wallet paid ARG or wARG and in uARG when it paid
// uARG. The fee is booked when the swap is broadcast and earned once the swap
// has 6 confirmations (dropped, like the wARG ledger's entries, if it never
// confirms). Booked fees are never counted free for quotes, so the maker
// never trades the treasury's share away. When a coin's earned fees reach
// Config.FeeSweepMin, they are paid to Config.Treasury in one transaction,
// and for ARG only out of what is left after the ARG behind the wARG.

type feeEntry struct {
	TxID      string           `json:"txid"`
	Coin      string           `json:"coin"` // ARG or UARG
	Amount    consensus.Amount `json:"amount"`
	Time      int64            `json:"time"`
	Confirmed bool             `json:"confirmed"`
	Sweep     string           `json:"sweep,omitempty"` // the transaction that paid it to the treasury
}

type feeBook struct {
	mu      sync.Mutex
	path    string
	Entries []feeEntry `json:"entries"`
}

// swapFee is what a swap of in, paid in from, owes the treasury at bps.
func swapFee(from string, in consensus.Amount, bps uint32) (coin string, fee consensus.Amount) {
	f := new(big.Int).Mul(big.NewInt(int64(in)), big.NewInt(int64(bps)))
	f.Quo(f, big.NewInt(10_000))
	coin = ARG
	if from == UARG {
		coin = UARG
	}
	return coin, consensus.Amount(f.Int64())
}

func (m *Maker) feeBps(from, to string) uint32 {
	if (from == ARG && to == WARG) || (from == WARG && to == ARG) {
		return m.cfg.WrapFeeBps
	}
	return m.cfg.SpreadBps
}

func openFeeBook(path string) (*feeBook, error) {
	b := &feeBook{path: path}
	if path == "" {
		return b, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, b); err != nil {
		return nil, fmt.Errorf("swap: fee book %s: %w", path, err)
	}
	return b, nil
}

func (b *feeBook) saveLocked() error {
	if b.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(b, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, b.path)
}

func (b *feeBook) add(e feeEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Entries = append(b.Entries, e)
	return b.saveLocked()
}

// owed is every booked fee of coin not yet paid to the treasury.
func (b *feeBook) owed(coin string) consensus.Amount {
	b.mu.Lock()
	defer b.mu.Unlock()
	var s consensus.Amount
	for _, e := range b.Entries {
		if e.Coin == coin && e.Sweep == "" {
			s += e.Amount
		}
	}
	return s
}

// earned is the confirmed, unpaid fees of coin and the swaps they came from.
func (b *feeBook) earned(coin string) (consensus.Amount, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var s consensus.Amount
	var ids []string
	for _, e := range b.Entries {
		if e.Coin == coin && e.Confirmed && e.Sweep == "" {
			s += e.Amount
			ids = append(ids, e.TxID)
		}
	}
	return s, ids
}

func (b *feeBook) unconfirmed() []feeEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []feeEntry
	for _, e := range b.Entries {
		if !e.Confirmed {
			out = append(out, e)
		}
	}
	return out
}

func (b *feeBook) set(txid string, f func(*feeEntry) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := b.Entries[:0]
	for i := range b.Entries {
		e := b.Entries[i]
		if e.TxID == txid && !f(&e) {
			continue
		}
		keep = append(keep, e)
	}
	b.Entries = keep
	b.saveLocked()
}

func (b *feeBook) swept(ids []string, sweep string) {
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.Entries {
		if in[b.Entries[i].TxID] && b.Entries[i].Sweep == "" {
			b.Entries[i].Sweep = sweep
		}
	}
	b.saveLocked()
}

// bookFee records what a broadcast swap owes the treasury.
func (m *Maker) bookFee(txid string, q Quote) {
	if m.fees == nil || m.cfg.Treasury == "" {
		return
	}
	coin, fee := swapFee(q.From, q.AmountIn, m.feeBps(q.From, q.To))
	if fee <= 0 {
		return
	}
	if err := m.fees.add(feeEntry{TxID: txid, Coin: coin, Amount: fee, Time: m.cfg.Now().Unix()}); err != nil {
		m.cfg.Logf("swap: booking the treasury fee of %s: %v", txid, err)
	}
}

// reconcileFees earns the fees of swaps with 6 confirmations and drops those
// of swaps that never made it into a block within half an hour.
func (m *Maker) reconcileFees() {
	if m.fees == nil {
		return
	}
	for _, e := range m.fees.unconfirmed() {
		raw, err := m.cfg.Node.Call("getrawtransaction", e.TxID)
		if err == nil {
			var t struct {
				Confirmations uint32 `json:"confirmations"`
			}
			if json.Unmarshal(raw, &t) == nil && t.Confirmations >= 6 {
				m.fees.set(e.TxID, func(x *feeEntry) bool { x.Confirmed = true; return true })
			}
			continue
		}
		if m.cfg.Now().Unix()-e.Time > 1800 {
			m.cfg.Logf("swap: %s never confirmed; its treasury fee is dropped", e.TxID)
			m.fees.set(e.TxID, func(*feeEntry) bool { return false })
		}
	}
}

// sweepFees pays the earned fees of each coin to the treasury once they
// reach FeeSweepMin. It runs only while no quote is open, so it never spends
// a coin a quote holds.
func (m *Maker) sweepFees() {
	if m.fees == nil || m.cfg.Treasury == "" {
		return
	}
	for _, coin := range []string{ARG, UARG} {
		amt, ids := m.fees.earned(coin)
		if amt <= 0 || amt < m.cfg.FeeSweepMin {
			continue
		}
		m.mu.Lock()
		m.expireLocked()
		if len(m.quotes) > 0 || len(m.inflight) > 0 {
			m.mu.Unlock()
			return
		}
		asset := ""
		if coin == UARG {
			asset = m.cfg.UARG.String()
		}
		coins, err := m.cfg.Wallet.SpendableCoins(asset)
		if err != nil {
			m.mu.Unlock()
			m.cfg.Logf("swap: treasury sweep: %v", err)
			return
		}
		var have consensus.Amount
		for _, c := range coins {
			have += c.Amount
		}
		need := amt
		if coin == ARG {
			need += m.wargSupply() + consensus.UnitsPerCoin/100 // and a little for the network fee
		}
		if have < need {
			m.mu.Unlock()
			m.cfg.Logf("swap: treasury sweep of %s %s waits: %s spendable", wallet.FormatAmount(amt), coin, wallet.FormatAmount(have))
			continue
		}
		rs := []wallet.Recipient{{Address: m.cfg.Treasury, Amount: amt}}
		var txid string
		if coin == ARG {
			txid, err = m.cfg.Wallet.SendMany(rs, m.cfg.FeeRate)
		} else {
			txid, err = m.cfg.Wallet.SendAssetMany(asset, rs, m.cfg.FeeRate)
		}
		m.mu.Unlock()
		if err != nil {
			m.cfg.Logf("swap: treasury sweep of %s %s: %v", wallet.FormatAmount(amt), coin, err)
			continue
		}
		m.fees.swept(ids, txid)
		m.cfg.Logf("swap: paid %s %s of swap fees to the treasury (%s)", wallet.FormatAmount(amt), coin, txid)
	}
}
