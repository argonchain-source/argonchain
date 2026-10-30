package wallet

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
)

// Two-party transactions (docs/SWAP.md): a transaction built by someone
// else that spends some of this wallet's coins beside theirs, as a swap
// does. The wallet signs only its own inputs, and only after checking each
// one against its own records; the signature covers every input, amount
// and output of the transaction (SPEC 6.2), so it is worth nothing for any
// other transaction.

// ErrNotOurCoin is returned when asked to sign an input the wallet cannot
// vouch for: not one of its unlocked coins, or described differently from
// its own record.
var ErrNotOurCoin = errors.New("wallet: input is not an unlocked coin of this wallet as recorded")

// Coin is a spendable coin with the output it spends, as a co-signed
// transaction lists it.
type Coin struct {
	UTXORecord
	Out consensus.TxOut
}

// SpendableCoins returns the coins of an asset (its id in hex, or "" for
// ARG) that can be spent now: unlocked and, for coinbase ARG, mature.
// Largest first, ties by outpoint, so selection is deterministic.
func (w *Wallet) SpendableCoins(asset string) ([]Coin, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client == nil {
		return nil, ErrNoClient
	}
	info, err := w.client.ChainInfo()
	if err != nil {
		return nil, err
	}
	st := w.store.State()
	var out []Coin
	for _, u := range st.UTXOs {
		if u.Asset != asset || u.LockedBy != "" {
			continue
		}
		if u.Coinbase && info.Blocks+1-u.Height < consensus.CoinbaseMaturity {
			continue
		}
		o, err := w.outputOf(u)
		if err != nil {
			return nil, err
		}
		out = append(out, Coin{UTXORecord: u, Out: o})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		if out[i].TxID != out[j].TxID {
			return out[i].TxID < out[j].TxID
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

// outputOf rebuilds the output a coin is, from the wallet's own key.
func (w *Wallet) outputOf(u UTXORecord) (consensus.TxOut, error) {
	k, ok := w.keys[u.Address]
	if !ok {
		return consensus.TxOut{}, fmt.Errorf("wallet: no key for %s", u.Address)
	}
	o := k.Output(u.Amount)
	if u.Asset != "" {
		id, err := consensus.HashFromHex(u.Asset)
		if err != nil {
			return consensus.TxOut{}, err
		}
		o.Asset = id
	}
	return o, nil
}

// ReceiveAddress is receive address 0, where swaps pay this wallet.
func (w *Wallet) ReceiveAddress() (string, error) { return w.MiningAddress() }

// ChangeAddress hands out the next change address.
func (w *Wallet) ChangeAddress() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.store.State()
	k, err := w.kc.Derive(true, st.NextChange)
	if err != nil {
		return "", err
	}
	st.NextChange++
	if err := w.ensureKeys(); err != nil {
		return "", err
	}
	return k.Address(w.params.HRP), w.store.Save()
}

// Owns reports whether an output pays one of this wallet's keys.
func (w *Wallet) Owns(o consensus.TxOut) bool {
	if o.Type != consensus.OutputTypeP2PK {
		return false
	}
	addr, err := EncodeAddress(w.params.HRP, o.Type, o.Payload)
	if err != nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.keys[addr]
	return ok
}

// SignInputs signs the inputs of tx at idx. Each must spend an unlocked coin
// of this wallet, and spent must describe it exactly as the wallet recorded
// it (amount, key and asset); nothing is signed otherwise.
func (w *Wallet) SignInputs(tx *consensus.Tx, spent []consensus.TxOut, idx []int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(spent) != len(tx.Inputs) || len(tx.Witnesses) < len(tx.Inputs) {
		return errors.New("wallet: the spent outputs do not match the inputs")
	}
	st := w.store.State()
	type job struct {
		i int
		k *Key
	}
	var jobs []job
	seen := map[int]bool{}
	for _, i := range idx {
		if i < 0 || i >= len(tx.Inputs) || seen[i] {
			return fmt.Errorf("wallet: bad input index %d", i)
		}
		seen[i] = true
		op := tx.Inputs[i].PrevOut
		u, ok := st.UTXOs[utxoKey(op.TxID.String(), op.Index)]
		if !ok || u.LockedBy != "" {
			return fmt.Errorf("%w (input %d)", ErrNotOurCoin, i)
		}
		mine, err := w.outputOf(u)
		if err != nil {
			return err
		}
		got := spent[i]
		if got.Amount != mine.Amount || got.Type != mine.Type || !bytes.Equal(got.Payload, mine.Payload) || got.Asset != mine.Asset {
			return fmt.Errorf("%w (input %d)", ErrNotOurCoin, i)
		}
		jobs = append(jobs, job{i, w.keys[u.Address]})
	}
	for _, j := range jobs {
		if err := j.k.Sign(w.params.ChainTag, tx, spent, j.i); err != nil {
			return err
		}
	}
	return nil
}

// SignIssuance signs the issuance of tx with the wallet key that is its
// issuer.
func (w *Wallet) SignIssuance(tx *consensus.Tx, spent []consensus.TxOut) error {
	if len(tx.Issuances) != 1 {
		return errors.New("wallet: the transaction has no issuance")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	is := tx.Issuances[0]
	addr, err := EncodeAddress(w.params.HRP, consensus.OutputTypeP2PK, is.Issuer[:])
	if err != nil {
		return err
	}
	k, ok := w.keys[addr]
	if !ok {
		return errors.New("wallet: the issuer is not a key of this wallet")
	}
	if len(tx.Witnesses) != len(tx.Inputs)+1 {
		return errors.New("wallet: the issuance has no witness slot")
	}
	msg := consensus.NewSigHashCache(w.params.ChainTag, tx, spent).IssuanceHash()
	sig, err := schnorr.Sign(k.Priv, msg[:])
	if err != nil {
		return err
	}
	tx.Witnesses[len(tx.Inputs)] = sig.Serialize()
	return nil
}

// RecordSpend books a co-signed transaction the wallet's coins went into,
// once it was broadcast as txid: its coins are locked until it confirms,
// and the history shows what it moved for this wallet (the block that
// confirms it replaces the entry).
func (w *Wallet) RecordSpend(txid string, tx *consensus.Tx, spent []consensus.TxOut) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.store.State()
	rec := HistoryRecord{TxID: txid}
	assets := map[string]consensus.Amount{}
	for i, in := range tx.Inputs {
		k := utxoKey(in.PrevOut.TxID.String(), in.PrevOut.Index)
		u, ok := st.UTXOs[k]
		if !ok {
			continue
		}
		u.LockedBy = txid
		st.UTXOs[k] = u
		if spent[i].Asset != consensus.NativeAsset {
			assets[spent[i].Asset.String()] -= spent[i].Amount
		} else {
			rec.Sent += spent[i].Amount
		}
	}
	for _, o := range tx.Outputs {
		if o.Type != consensus.OutputTypeP2PK {
			continue
		}
		addr, err := EncodeAddress(w.params.HRP, o.Type, o.Payload)
		if err != nil {
			continue
		}
		if _, ours := w.keys[addr]; !ours {
			continue
		}
		if o.Asset != consensus.NativeAsset {
			assets[o.Asset.String()] += o.Amount
		} else {
			rec.Received += o.Amount
		}
	}
	rec.Net = rec.Received - rec.Sent
	for id, n := range assets {
		if n != 0 {
			if rec.Assets == nil {
				rec.Assets = map[string]consensus.Amount{}
			}
			rec.Assets[id] = n
		}
	}
	st.History = append(st.History, rec)
	return w.store.Save()
}
