package wallet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"argonmine/consensus"
)

// Lookahead is how many unused addresses beyond the last handed-out one are
// watched, so a restored wallet finds all its funds (BIP-44 gap limit).
const Lookahead = 20

// DustLimit matches the node's default relay policy.
const DustLimit consensus.Amount = 546

// Relay policy for asset outputs, mirroring the nodes' defaults
// (mempool.DefaultConfig().AssetOutputFee, mempool.FreeAssetOutputs; a
// test checks they agree): every asset output beyond the first two owes
// assetOutputFee on top of the fee per byte.
const (
	assetOutputFee   consensus.Amount = 546
	freeAssetOutputs                  = 2
)

// assetSurcharge is what tx owes for its asset outputs beyond the free ones.
func assetSurcharge(tx *consensus.Tx) consensus.Amount {
	n := 0
	for i := range tx.Outputs {
		if tx.Outputs[i].Asset != consensus.NativeAsset {
			n++
		}
	}
	if n <= freeAssetOutputs {
		return 0
	}
	return assetOutputFee * consensus.Amount(n-freeAssetOutputs)
}

var (
	ErrNoClient          = errors.New("wallet: no node connection configured")
	ErrInsufficientFunds = errors.New("wallet: insufficient confirmed funds")
	ErrAmount            = errors.New("wallet: invalid amount")
)

// Wallet is an HD wallet that tracks its coins through a node's RPC.
type Wallet struct {
	mu     sync.Mutex
	store  *Store
	kc     *KeyChain
	params *consensus.Params
	client *Client
	keys   map[string]*Key // address -> key (all derived, including lookahead)
}

// Create makes a new wallet file with a fresh mnemonic. Returns the mnemonic,
// which the user must back up; it is also stored encrypted in the file.
func Create(path string, params *consensus.Params, password, passphrase string) (*Wallet, string, error) {
	m, err := NewMnemonic()
	if err != nil {
		return nil, "", err
	}
	w, err := Restore(path, params, m, passphrase, password)
	if err != nil {
		return nil, "", err
	}
	return w, m, nil
}

// Restore creates a wallet file from an existing mnemonic.
func Restore(path string, params *consensus.Params, mnemonic, passphrase, password string) (*Wallet, error) {
	st, err := CreateStore(path, params.Name, strings.TrimSpace(mnemonic), passphrase, password)
	if err != nil {
		return nil, err
	}
	return fromStore(st, params)
}

// Open loads and unlocks an existing wallet file.
func Open(path string, params *consensus.Params, password string) (*Wallet, error) {
	st, err := OpenStore(path)
	if err != nil {
		return nil, err
	}
	if st.Network() != params.Name {
		return nil, ErrWrongNetwork
	}
	if err := st.Unlock(password); err != nil {
		return nil, err
	}
	return fromStore(st, params)
}

func fromStore(st *Store, params *consensus.Params) (*Wallet, error) {
	kc, err := NewKeyChain(st.Seed(), CoinTypeFor(params), 0)
	if err != nil {
		return nil, err
	}
	w := &Wallet{store: st, kc: kc, params: params, keys: map[string]*Key{}}
	if err := w.ensureKeys(); err != nil {
		return nil, err
	}
	return w, st.Save()
}

// SetClient connects the wallet to a node.
func (w *Wallet) SetClient(c *Client) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.client = c
}

// Mnemonic returns the backup phrase.
func (w *Wallet) Mnemonic() string { return w.store.Mnemonic() }

// Params returns the network.
func (w *Wallet) Params() *consensus.Params { return w.params }

// ensureKeys derives every address up to the lookahead window.
func (w *Wallet) ensureKeys() error {
	st := w.store.State()
	for _, change := range []bool{false, true} {
		next := st.NextReceive
		if change {
			next = st.NextChange
		}
		for i := uint32(0); i < next+Lookahead; i++ {
			k, err := w.kc.Derive(change, i)
			if err != nil {
				return err
			}
			addr := k.Address(w.params.HRP)
			if _, ok := w.keys[addr]; ok {
				continue
			}
			w.keys[addr] = k
			if _, ok := st.Addresses[addr]; !ok {
				st.Addresses[addr] = AddrMeta{Change: change, Index: i}
			}
		}
	}
	return nil
}

// NewAddress hands out the next unused receive address.
func (w *Wallet) NewAddress() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.store.State()
	k, err := w.kc.Derive(false, st.NextReceive)
	if err != nil {
		return "", err
	}
	st.NextReceive++
	if err := w.ensureKeys(); err != nil {
		return "", err
	}
	return k.Address(w.params.HRP), w.store.Save()
}

// MiningAddress returns receive address 0, marking it as handed out. All
// mining rewards go here so they are easy to find in the history.
func (w *Wallet) MiningAddress() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.store.State()
	if st.NextReceive == 0 {
		st.NextReceive = 1
		if err := w.ensureKeys(); err != nil {
			return "", err
		}
		if err := w.store.Save(); err != nil {
			return "", err
		}
	}
	k, err := w.kc.Derive(false, 0)
	if err != nil {
		return "", err
	}
	return k.Address(w.params.HRP), nil
}

// FirstReceiveAddress reads receive address 0 from a (possibly locked)
// store, so mining can start without the password.
func FirstReceiveAddress(st *Store) string {
	for addr, m := range st.State().Addresses {
		if !m.Change && m.Index == 0 {
			return addr
		}
	}
	return ""
}

// Addresses lists the receive addresses handed out so far, oldest first.
func (w *Wallet) Addresses() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.store.State()
	type item struct {
		addr string
		idx  uint32
	}
	var items []item
	for addr, m := range st.Addresses {
		if !m.Change && m.Index < st.NextReceive {
			items = append(items, item{addr, m.Index})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].idx < items[j].idx })
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.addr
	}
	return out
}

func utxoKey(txid string, index uint32) string {
	return txid + ":" + strconv.FormatUint(uint64(index), 10)
}

// Sync scans new blocks from the node and updates coins and history. It
// handles reorganisations by rescanning from genesis when the last synced
// block is no longer on the main chain.
func (w *Wallet) Sync() (uint32, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client == nil {
		return 0, ErrNoClient
	}
	st := w.store.State()
	info, err := w.client.ChainInfo()
	if err != nil {
		return 0, err
	}
	if info.Chain != w.params.Name {
		return 0, fmt.Errorf("wallet: node is on %s, wallet is %s", info.Chain, w.params.Name)
	}
	if st.SyncedHeight >= 0 {
		h, err := w.client.BlockHash(uint32(st.SyncedHeight))
		if err != nil || h != st.SyncedHash {
			// Reorg below our sync point: start over.
			st.SyncedHeight = -1
			st.SyncedHash = ""
			st.UTXOs = map[string]UTXORecord{}
			st.History = nil
		}
	}
	for h := st.SyncedHeight + 1; h <= int64(info.Blocks); h++ {
		hash, err := w.client.BlockHash(uint32(h))
		if err != nil {
			return 0, err
		}
		blk, err := w.client.Block(hash)
		if err != nil {
			return 0, err
		}
		if err := w.applyBlock(blk); err != nil {
			return 0, err
		}
		st.SyncedHeight = h
		st.SyncedHash = hash
		if h%500 == 0 {
			if err := w.store.Save(); err != nil {
				return 0, err
			}
		}
	}
	w.releaseStaleLocks()
	return uint32(st.SyncedHeight), w.store.Save()
}

// applyBlock updates coins and history for one block.
func (w *Wallet) applyBlock(blk *BlockJSON) error {
	st := w.store.State()
	for _, tx := range blk.Tx {
		// Remember every asset the chain has seen issued, so holdings can be
		// shown with their symbol even when they arrive later.
		for _, is := range tx.Issuances {
			if st.Assets == nil {
				st.Assets = map[string]AssetInfo{}
			}
			if _, known := st.Assets[is.Asset]; !known {
				st.Assets[is.Asset] = AssetInfo{Symbol: is.Symbol, Issuer: is.Issuer}
			}
		}
		var spent, recv, totalOut consensus.Amount
		assets := map[string]consensus.Amount{} // net units per asset id
		mine := false
		allInputsOurs := !tx.Coinbase
		for _, in := range tx.Inputs {
			if in.Coinbase {
				continue
			}
			k := utxoKey(in.TxID, in.Index)
			u, ok := st.UTXOs[k]
			if !ok {
				allInputsOurs = false
				continue
			}
			delete(st.UTXOs, k)
			if u.Asset != "" {
				assets[u.Asset] -= u.Amount
			} else {
				spent += u.Amount
			}
			mine = true
		}
		for _, o := range tx.Outputs {
			if o.Asset == "" {
				totalOut += o.Amount
			}
			if o.Address == "" {
				continue
			}
			if _, ours := w.keys[o.Address]; !ours {
				continue
			}
			st.UTXOs[utxoKey(tx.TxID, uint32(o.N))] = UTXORecord{
				TxID: tx.TxID, Index: uint32(o.N), Amount: o.Amount, Address: o.Address,
				Height: blk.Height, Coinbase: tx.Coinbase, Asset: o.Asset,
			}
			if o.Asset != "" {
				assets[o.Asset] += o.Amount
			} else {
				recv += o.Amount
			}
			mine = true
			meta := st.Addresses[o.Address]
			meta.Used = true
			st.Addresses[o.Address] = meta
			// Receiving on a lookahead address advances the counters (restore case).
			if !meta.Change && meta.Index >= st.NextReceive {
				st.NextReceive = meta.Index + 1
			}
			if meta.Change && meta.Index >= st.NextChange {
				st.NextChange = meta.Index + 1
			}
		}
		if !mine {
			continue
		}
		if err := w.ensureKeys(); err != nil {
			return err
		}
		rec := HistoryRecord{TxID: tx.TxID, Height: blk.Height, Time: blk.Time, Received: recv, Sent: spent, Net: recv - spent}
		for id, n := range assets {
			if n == 0 {
				continue
			}
			if rec.Assets == nil {
				rec.Assets = map[string]consensus.Amount{}
			}
			rec.Assets[id] = n
		}
		if allInputsOurs && spent >= totalOut {
			rec.Fee = spent - totalOut
		}
		replaced := false
		for i := range st.History {
			if st.History[i].TxID == tx.TxID {
				st.History[i] = rec
				replaced = true
				break
			}
		}
		if !replaced {
			st.History = append(st.History, rec)
		}
	}
	return nil
}

// releaseStaleLocks unlocks coins whose pending spend is no longer in the
// node's mempool (it was dropped or replaced) so they can be used again.
func (w *Wallet) releaseStaleLocks() {
	st := w.store.State()
	pending := map[string]bool{}
	for _, u := range st.UTXOs {
		if u.LockedBy != "" {
			pending[u.LockedBy] = false
		}
	}
	if len(pending) == 0 {
		return
	}
	ids, err := w.client.RawMempool()
	if err != nil {
		return
	}
	for _, id := range ids {
		if _, ok := pending[id]; ok {
			pending[id] = true
		}
	}
	for k, u := range st.UTXOs {
		if u.LockedBy != "" && !pending[u.LockedBy] {
			gone := u.LockedBy
			u.LockedBy = ""
			st.UTXOs[k] = u
			// Drop the unconfirmed history entry too.
			for i := range st.History {
				if st.History[i].TxID == gone && st.History[i].Height == 0 {
					st.History = append(st.History[:i], st.History[i+1:]...)
					break
				}
			}
		}
	}
}

// Balance returns confirmed spendable, immature (young coinbase) and
// locked (pending outgoing) amounts given the current tip height.
func (w *Wallet) Balance(tipHeight uint32) (spendable, immature, locked consensus.Amount) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, u := range w.store.State().UTXOs {
		if u.Asset != "" {
			continue // asset units are not ARG; see AssetBalances
		}
		switch {
		case u.LockedBy != "":
			locked += u.Amount
		case u.Coinbase && tipHeight+1-u.Height < consensus.CoinbaseMaturity:
			immature += u.Amount
		default:
			spendable += u.Amount
		}
	}
	return
}

// UTXOs returns a copy of the wallet's coins.
func (w *Wallet) UTXOs() []UTXORecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]UTXORecord, 0, len(w.store.State().UTXOs))
	for _, u := range w.store.State().UTXOs {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Amount > out[j].Amount })
	return out
}

// History returns transactions touching the wallet, oldest first.
func (w *Wallet) History() []HistoryRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]HistoryRecord{}, w.store.State().History...)
	sort.SliceStable(out, func(i, j int) bool {
		hi, hj := out[i].Height, out[j].Height
		if hi == 0 {
			hi = 1<<32 - 1
		}
		if hj == 0 {
			hj = 1<<32 - 1
		}
		return hi < hj
	})
	return out
}

// SyncedHeight returns the last scanned height (-1 if none).
func (w *Wallet) SyncedHeight() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.State().SyncedHeight
}

// Recipient is one output of a payment.
type Recipient struct {
	Address string
	Amount  consensus.Amount
}

// Send pays a single recipient; see SendMany.
func (w *Wallet) Send(toAddress string, amount, feeRate consensus.Amount) (string, error) {
	return w.SendMany([]Recipient{{Address: toAddress, Amount: amount}}, feeRate)
}

// SendMany builds, signs and broadcasts one transaction paying every
// recipient, at the given fee rate (units per byte). Returns the txid.
func (w *Wallet) SendMany(recipients []Recipient, feeRate consensus.Amount) (string, error) {
	return w.sendMany(recipients, nil, feeRate)
}

// SendManyMemo is SendMany with an extra NULLDATA output carrying memo (at
// most 80 bytes), as used for bridge deposits.
func (w *Wallet) SendManyMemo(recipients []Recipient, memo []byte, feeRate consensus.Amount) (string, error) {
	if len(memo) == 0 {
		return w.sendMany(recipients, nil, feeRate)
	}
	return w.sendMany(recipients, memo, feeRate)
}

func (w *Wallet) sendMany(recipients []Recipient, memo []byte, feeRate consensus.Amount) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client == nil {
		return "", ErrNoClient
	}
	if len(recipients) == 0 {
		return "", ErrAmount
	}
	var amount consensus.Amount
	dests := make([]consensus.TxOut, 0, len(recipients))
	for _, r := range recipients {
		if r.Amount < DustLimit || !consensus.MoneyRange(r.Amount) {
			return "", ErrAmount
		}
		d, err := DecodeAddress(w.params.HRP, r.Address)
		if err != nil {
			return "", err
		}
		dests = append(dests, consensus.TxOut{Amount: r.Amount, Type: d.Type, Payload: d.Payload})
		if amount, err = consensus.AddAmounts(amount, r.Amount); err != nil {
			return "", ErrAmount
		}
	}
	if memo != nil {
		if len(memo) > consensus.MaxNullDataLen {
			return "", fmt.Errorf("%w: memo longer than %d bytes", ErrAmount, consensus.MaxNullDataLen)
		}
		dests = append(dests, consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: append([]byte{}, memo...)})
	}
	if feeRate < 1 {
		feeRate = 1
	}
	info, err := w.client.ChainInfo()
	if err != nil {
		return "", err
	}
	st := w.store.State()

	// Spendable ARG coins, largest first.
	coins := w.spendableARG(st, info.Blocks)

	changeKey, err := w.kc.Derive(true, st.NextChange)
	if err != nil {
		return "", err
	}

	// Greedy selection with exact size-based fee.
	var selected []UTXORecord
	var inTotal consensus.Amount
	var tx *consensus.Tx
	var spent []consensus.TxOut
	var fee consensus.Amount
	for _, c := range coins {
		selected = append(selected, c)
		inTotal += c.Amount
		tx, spent, fee, err = w.buildTx(selected, dests, amount, changeKey, feeRate)
		if err != nil {
			return "", err
		}
		if tx != nil {
			break
		}
	}
	if tx == nil {
		return "", fmt.Errorf("%w: need %s plus fee, have %s", ErrInsufficientFunds, FormatAmount(amount), FormatAmount(inTotal))
	}

	// Sign.
	for i, c := range selected {
		k, ok := w.keys[c.Address]
		if !ok {
			return "", fmt.Errorf("wallet: no key for %s", c.Address)
		}
		if err := k.Sign(w.params.ChainTag, tx, spent, i); err != nil {
			return "", err
		}
	}
	// Belt and braces: the node will check this too.
	if err := consensus.CheckTxStateless(tx); err != nil {
		return "", err
	}
	if err := consensus.VerifyTxSignatures(w.params.ChainTag, tx, spent); err != nil {
		return "", err
	}

	txid, err := w.client.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
	if err != nil {
		return "", err
	}
	// Bookkeeping: lock inputs, bump change index if used, record pending.
	usedChange := false
	for _, o := range tx.Outputs {
		if string(o.Payload) == string(changeKey.XOnly) {
			usedChange = true
		}
	}
	if usedChange {
		st.NextChange++
		if err := w.ensureKeys(); err != nil {
			return "", err
		}
	}
	for _, c := range selected {
		k := utxoKey(c.TxID, c.Index)
		u := st.UTXOs[k]
		u.LockedBy = txid
		st.UTXOs[k] = u
	}
	st.History = append(st.History, HistoryRecord{TxID: txid, Height: 0, Sent: inTotal, Received: inTotal - amount - fee, Fee: fee, Net: -(amount + fee)})
	return txid, w.store.Save()
}

// spendableARG lists the wallet's unlocked, mature ARG coins, largest first.
func (w *Wallet) spendableARG(st *State, tip uint32) []UTXORecord {
	var coins []UTXORecord
	for _, u := range st.UTXOs {
		if u.Asset != "" || u.LockedBy != "" {
			continue
		}
		if u.Coinbase && tip+1-u.Height < consensus.CoinbaseMaturity {
			continue
		}
		coins = append(coins, u)
	}
	sort.Slice(coins, func(i, j int) bool { return coins[i].Amount > coins[j].Amount })
	return coins
}

// AssetBalance is the wallet's holding of one non-ARG asset (SPEC 3.5).
type AssetBalance struct {
	Asset     string           `json:"asset"`  // id, hex
	Symbol    string           `json:"symbol"` // from the issuance, if the wallet has seen it
	Issuer    string           `json:"issuer"`
	Confirmed consensus.Amount `json:"confirmed"` // units that can be spent now
	Locked    consensus.Amount `json:"locked"`    // units in an unconfirmed outgoing transaction
}

// AssetBalances returns every non-ARG asset the wallet holds, by symbol.
func (w *Wallet) AssetBalances() []AssetBalance {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.store.State()
	by := map[string]*AssetBalance{}
	for _, u := range st.UTXOs {
		if u.Asset == "" {
			continue
		}
		b := by[u.Asset]
		if b == nil {
			info := st.Assets[u.Asset]
			b = &AssetBalance{Asset: u.Asset, Symbol: info.Symbol, Issuer: info.Issuer}
			by[u.Asset] = b
		}
		if u.LockedBy != "" {
			b.Locked += u.Amount
		} else {
			b.Confirmed += u.Amount
		}
	}
	out := make([]AssetBalance, 0, len(by))
	for _, b := range by {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Asset < out[j].Asset
	})
	return out
}

// Assets returns every asset the wallet has seen issued, by id.
func (w *Wallet) Assets() map[string]AssetInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]AssetInfo, len(w.store.State().Assets))
	for id, info := range w.store.State().Assets {
		out[id] = info
	}
	return out
}

// AssetInfoFor returns what the wallet knows about an asset id.
func (w *Wallet) AssetInfoFor(id string) (AssetInfo, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, ok := w.store.State().Assets[id]
	return info, ok
}

// SendAsset pays amount units of the asset to toAddress in a version-2
// transaction. The fee is paid in ARG from the wallet's own coins.
func (w *Wallet) SendAsset(assetID, toAddress string, amount, feeRate consensus.Amount) (string, error) {
	return w.SendAssetMemo(assetID, toAddress, amount, nil, feeRate)
}

// SendAssetMemo is SendAsset with a NULLDATA output carrying memo (at most
// 80 bytes; none when empty), such as the bridge's withdrawal memo.
func (w *Wallet) SendAssetMemo(assetID, toAddress string, amount consensus.Amount, memo []byte, feeRate consensus.Amount) (string, error) {
	id, err := consensus.HashFromHex(assetID)
	if err != nil || id == consensus.NativeAsset {
		return "", fmt.Errorf("%w: not an asset id", ErrAmount)
	}
	d, err := DecodeAddress(w.params.HRP, toAddress)
	if err != nil {
		return "", err
	}
	dest := consensus.TxOut{Amount: amount, Type: d.Type, Payload: d.Payload, Asset: id}
	return w.sendAsset(id, amount, []consensus.TxOut{dest}, memo, feeRate)
}

// SendAssetMany pays every recipient its amount of the asset in one
// version-2 transaction; the fee, with the surcharge for asset outputs
// beyond the first two, is paid in ARG.
func (w *Wallet) SendAssetMany(assetID string, recipients []Recipient, feeRate consensus.Amount) (string, error) {
	id, err := consensus.HashFromHex(assetID)
	if err != nil || id == consensus.NativeAsset {
		return "", fmt.Errorf("%w: not an asset id", ErrAmount)
	}
	if len(recipients) == 0 {
		return "", ErrAmount
	}
	var total consensus.Amount
	dests := make([]consensus.TxOut, 0, len(recipients))
	for _, r := range recipients {
		if !consensus.AssetRange(r.Amount) {
			return "", ErrAmount
		}
		d, err := DecodeAddress(w.params.HRP, r.Address)
		if err != nil {
			return "", err
		}
		dests = append(dests, consensus.TxOut{Amount: r.Amount, Type: d.Type, Payload: d.Payload, Asset: id})
		if total, err = consensus.AddAmounts(total, r.Amount); err != nil || !consensus.AssetRange(total) {
			return "", ErrAmount
		}
	}
	return w.sendAsset(id, total, dests, nil, feeRate)
}

// BurnAsset destroys amount units of the asset: the transaction spends them
// and pays only the change forward (SPEC 3.5, conservation). memo, if any,
// is carried in a NULLDATA output; the bridge reads its EVM address from it
// and pays the stablecoin out for the burn, keyed by the returned txid.
func (w *Wallet) BurnAsset(assetID string, amount consensus.Amount, memo []byte, feeRate consensus.Amount) (string, error) {
	id, err := consensus.HashFromHex(assetID)
	if err != nil || id == consensus.NativeAsset {
		return "", fmt.Errorf("%w: not an asset id", ErrAmount)
	}
	return w.sendAsset(id, amount, nil, memo, feeRate)
}

func (w *Wallet) sendAsset(asset consensus.Hash, amount consensus.Amount, dests []consensus.TxOut, memo []byte, feeRate consensus.Amount) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client == nil {
		return "", ErrNoClient
	}
	if !consensus.AssetRange(amount) {
		return "", ErrAmount
	}
	if len(memo) > consensus.MaxNullDataLen {
		return "", fmt.Errorf("%w: memo longer than %d bytes", ErrAmount, consensus.MaxNullDataLen)
	}
	if feeRate < 1 {
		feeRate = 1
	}
	info, err := w.client.ChainInfo()
	if err != nil {
		return "", err
	}
	st := w.store.State()
	id := asset.String()
	symbol := st.Assets[id].Symbol
	if symbol == "" {
		symbol = "units"
	}

	// Asset coins, largest first, until the amount is covered.
	var assetCoins []UTXORecord
	for _, u := range st.UTXOs {
		if u.Asset == id && u.LockedBy == "" {
			assetCoins = append(assetCoins, u)
		}
	}
	sort.Slice(assetCoins, func(i, j int) bool { return assetCoins[i].Amount > assetCoins[j].Amount })
	var assetIn consensus.Amount
	n := 0
	for n < len(assetCoins) && assetIn < amount {
		assetIn += assetCoins[n].Amount
		n++
	}
	if assetIn < amount {
		return "", fmt.Errorf("%w: need %s %s, have %s", ErrInsufficientFunds, FormatAmount(amount), symbol, FormatAmount(assetIn))
	}
	assetCoins = assetCoins[:n]

	changeKey, err := w.kc.Derive(true, st.NextChange)
	if err != nil {
		return "", err
	}
	// Outputs that do not depend on the ARG inputs: memo, destination, asset change.
	var fixed []consensus.TxOut
	if len(memo) > 0 {
		fixed = append(fixed, consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: append([]byte{}, memo...)})
	}
	fixed = append(fixed, dests...)
	if assetIn > amount {
		ch := changeKey.Output(assetIn - amount)
		ch.Asset = asset
		fixed = append(fixed, ch)
	}

	assemble := func(argSel []UTXORecord, withChange bool) (*consensus.Tx, []consensus.TxOut, consensus.Amount, error) {
		tx := &consensus.Tx{Version: consensus.TxVersionAssets}
		var spent []consensus.TxOut
		var argIn consensus.Amount
		add := func(c UTXORecord) error {
			h, err := consensus.HashFromHex(c.TxID)
			if err != nil {
				return err
			}
			k, ok := w.keys[c.Address]
			if !ok {
				return fmt.Errorf("wallet: no key for %s", c.Address)
			}
			tx.Inputs = append(tx.Inputs, consensus.TxIn{PrevOut: consensus.OutPoint{TxID: h, Index: c.Index}})
			tx.Witnesses = append(tx.Witnesses, make([]byte, consensus.SchnorrSigLen))
			o := k.Output(c.Amount)
			if c.Asset != "" {
				o.Asset = asset
			} else {
				argIn += c.Amount
			}
			spent = append(spent, o)
			return nil
		}
		for _, c := range assetCoins {
			if err := add(c); err != nil {
				return nil, nil, 0, err
			}
		}
		for _, c := range argSel {
			if err := add(c); err != nil {
				return nil, nil, 0, err
			}
		}
		tx.Outputs = append([]consensus.TxOut{}, fixed...)
		if withChange {
			tx.Outputs = append(tx.Outputs, changeKey.Output(0))
		}
		fee := consensus.Amount(tx.SerializedSize())*feeRate + assetSurcharge(tx)
		if withChange {
			change := argIn - fee
			if change < DustLimit {
				return nil, nil, 0, nil
			}
			tx.Outputs[len(tx.Outputs)-1].Amount = change
			return tx, spent, fee, nil
		}
		if argIn < fee {
			return nil, nil, 0, nil
		}
		return tx, spent, argIn, nil
	}

	// Greedy ARG selection for the fee, exact size-based.
	var tx *consensus.Tx
	var spent []consensus.TxOut
	var fee, argIn consensus.Amount
	var argSel []UTXORecord
	for _, c := range w.spendableARG(st, info.Blocks) {
		argSel = append(argSel, c)
		argIn += c.Amount
		for _, withChange := range []bool{true, false} {
			if tx, spent, fee, err = assemble(argSel, withChange); err != nil {
				return "", err
			}
			if tx != nil {
				break
			}
		}
		if tx != nil {
			break
		}
	}
	if tx == nil {
		return "", fmt.Errorf("%w: the fee is paid in ARG and the wallet has %s spendable", ErrInsufficientFunds, FormatAmount(argIn))
	}

	// Sign every input with the key of the coin it spends.
	all := append(append([]UTXORecord{}, assetCoins...), argSel...)
	for i, c := range all {
		if err := w.keys[c.Address].Sign(w.params.ChainTag, tx, spent, i); err != nil {
			return "", err
		}
	}
	if err := consensus.CheckTxStateless(tx); err != nil {
		return "", err
	}
	if err := consensus.VerifyTxSignatures(w.params.ChainTag, tx, spent); err != nil {
		return "", err
	}
	txid, err := w.client.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
	if err != nil {
		return "", err
	}

	// Bookkeeping: lock inputs, bump the change index if used, record pending.
	usedChange := false
	for _, o := range tx.Outputs {
		if string(o.Payload) == string(changeKey.XOnly) {
			usedChange = true
		}
	}
	if usedChange {
		st.NextChange++
		if err := w.ensureKeys(); err != nil {
			return "", err
		}
	}
	for _, c := range all {
		k := utxoKey(c.TxID, c.Index)
		u := st.UTXOs[k]
		u.LockedBy = txid
		st.UTXOs[k] = u
	}
	st.History = append(st.History, HistoryRecord{
		TxID: txid, Sent: argIn, Received: argIn - fee, Fee: fee, Net: -fee,
		Assets: map[string]consensus.Amount{id: -amount},
	})
	return txid, w.store.Save()
}

// buildTx assembles an unsigned tx (dummy witnesses for sizing). Returns nil
// tx if the inputs do not cover amount + fee.
func (w *Wallet) buildTx(coins []UTXORecord, dests []consensus.TxOut, amount consensus.Amount, changeKey *Key, feeRate consensus.Amount) (*consensus.Tx, []consensus.TxOut, consensus.Amount, error) {
	var inTotal consensus.Amount
	tx := &consensus.Tx{Version: consensus.TxVersion}
	spent := make([]consensus.TxOut, 0, len(coins))
	for _, c := range coins {
		h, err := consensus.HashFromHex(c.TxID)
		if err != nil {
			return nil, nil, 0, err
		}
		tx.Inputs = append(tx.Inputs, consensus.TxIn{PrevOut: consensus.OutPoint{TxID: h, Index: c.Index}})
		tx.Witnesses = append(tx.Witnesses, make([]byte, consensus.SchnorrSigLen))
		k, ok := w.keys[c.Address]
		if !ok {
			return nil, nil, 0, fmt.Errorf("wallet: no key for %s", c.Address)
		}
		spent = append(spent, k.Output(c.Amount))
		inTotal += c.Amount
	}
	// Try with change first.
	tx.Outputs = append(append([]consensus.TxOut{}, dests...), changeKey.Output(0))
	fee := consensus.Amount(tx.SerializedSize()) * feeRate
	change := inTotal - amount - fee
	if change >= DustLimit {
		tx.Outputs[len(tx.Outputs)-1].Amount = change
		return tx, spent, fee, nil
	}
	// Without change: any remainder goes to fee.
	tx.Outputs = append([]consensus.TxOut{}, dests...)
	fee = consensus.Amount(tx.SerializedSize()) * feeRate
	if inTotal >= amount+fee {
		return tx, spent, inTotal - amount, nil
	}
	return nil, nil, 0, nil
}

// ParseAmount converts a decimal ARG string ("1.5") to units exactly.
func ParseAmount(s string) (consensus.Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrAmount
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 8 {
		return 0, fmt.Errorf("%w: more than 8 decimal places", ErrAmount)
	}
	for len(frac) < 8 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrAmount
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrAmount
	}
	if w > int64(consensus.MaxMoney/consensus.UnitsPerCoin) {
		return 0, ErrAmount
	}
	a := consensus.Amount(w)*consensus.UnitsPerCoin + consensus.Amount(f)
	if neg {
		a = -a
	}
	if !consensus.MoneyRange(a) {
		return 0, ErrAmount
	}
	return a, nil
}

// FormatAmount renders units as a decimal ARG string.
func FormatAmount(a consensus.Amount) string {
	neg := a < 0
	if neg {
		a = -a
	}
	s := fmt.Sprintf("%d.%08d", a/consensus.UnitsPerCoin, a%consensus.UnitsPerCoin)
	if neg {
		return "-" + s
	}
	return s
}
