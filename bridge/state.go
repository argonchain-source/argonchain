package bridge

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"

	"argonmine/consensus"
)

// Inbound statuses: a stablecoin deposit on the EVM chain paid out as uARG.
const (
	InboundPending     = "pending"     // seen on the EVM chain, waiting for confirmations
	InboundIssued      = "issued"      // uARG issuance broadcast on this chain
	InboundConfirmed   = "confirmed"   // issuance mined
	InboundUnclaimable = "unclaimable" // destination is not a valid address; needs manual handling
	// InboundReorged: the deposit's event left its chain in a reorganisation
	// before it was issued, and its transaction is not in the chain any
	// more; it is never issued (docs/BRIDGE.md, "Finality and providers").
	InboundReorged = "reorged"
)

// Redemption statuses: a uARG burn on this chain paid out from the vault.
const (
	RedemptionPending      = "pending"      // burn confirmed, release not yet sent
	RedemptionReleased     = "released"     // vault release transaction sent
	RedemptionConfirmed    = "confirmed"    // release receipt succeeded
	RedemptionUnreleasable = "unreleasable" // no valid memo; needs manual handling
)

// Inbound is one vault deposit and the uARG it becomes. Deposits are keyed
// by (chain id, deposit id): a vault numbers its deposits from its
// firstDepositId, and one that replaces another on the same chain starts
// above every id on record for the chain (docs/BRIDGE.md, "Replacing a
// vault"), so the pair names one deposit across vaults.
type Inbound struct {
	Chain       uint64           `json:"chain_id"`             // EVM chain id
	ChainName   string           `json:"chain"`                // for display
	ID          uint64           `json:"id"`                   // vault deposit id on that chain
	EVMTx       string           `json:"evm_tx"`               // deposit transaction hash
	From        string           `json:"from"`                 // 0x depositor
	Token       string           `json:"token"`                // 0x address of the token deposited
	TokenSymbol string           `json:"token_symbol"`         // USDC, USDT
	Decimals    int              `json:"decimals"`             // the token's decimals
	TokenAmount string           `json:"token_amount"`         // token units credited after the fee (decimal string; 18-decimal tokens exceed uint64)
	TokenFee    string           `json:"token_fee"`            // token units the vault kept as its deposit fee (decimal string)
	Amount      consensus.Amount `json:"amount"`               // uARG units to issue (8 decimals), from TokenAmount
	To          string           `json:"to"`                   // Argonmine address
	Block       uint64           `json:"block"`                // EVM block of the deposit
	BlockHash   string           `json:"block_hash,omitempty"` // that block's hash (empty on records from before it was kept)
	LogIndex    uint64           `json:"log_index"`            // the Deposited log's index in that block
	Status      string           `json:"status"`
	IssueTx     string           `json:"issue_tx,omitempty"` // issuance transaction on this chain
	Error       string           `json:"error,omitempty"`    // why a pending deposit waits (a limit, a missing event), or why it is unclaimable
	Attempts    int              `json:"attempts"`
	Created     int64            `json:"created"`
	Updated     int64            `json:"updated"`
}

// Redemption is one uARG burn with an EVM destination: a chain, a token and
// a recipient, from the memo.
type Redemption struct {
	TxID string `json:"txid"` // burn transaction on this chain, or the transaction that paid a swap address
	// Vout is the output that paid a swap address (docs/BRIDGE.md, "Swap
	// addresses"); absent for a burn with a memo. See ID.
	Vout        *uint32          `json:"vout,omitempty"`
	Address     string           `json:"address,omitempty"` // the swap address paid, when Vout is set
	Memo        string           `json:"memo,omitempty"`    // the withdrawal memo the payment carried (withdraw.go), when Vout is set
	Height      uint32           `json:"height"`            // block of the burn
	Amount      consensus.Amount `json:"amount"`            // uARG units burned
	Chain       uint64           `json:"chain_id"`
	ChainName   string           `json:"chain"`
	Token       string           `json:"token"`        // 0x address of the token paid out
	TokenSymbol string           `json:"token_symbol"` // USDC, USDT
	Decimals    int              `json:"decimals"`
	TokenAmount string           `json:"token_amount"` // gross token units of the burn, what release() is called with (decimal string)
	TokenFee    string           `json:"token_fee"`    // token units the vault keeps as its release fee (decimal string)
	TokenPaid   string           `json:"token_paid"`   // token units paid to the recipient: TokenAmount - TokenFee (decimal string)
	To          string           `json:"to"`           // 0x recipient
	Status      string           `json:"status"`
	EVMTx       string           `json:"evm_tx,omitempty"` // vault release transaction (the latest sent, or the one mined)
	// Confirmations is the Argonmine depth the burn needs before it is paid,
	// from the payout tiers (docs/BRIDGE.md, "Payout depth") for TxPayout.
	Confirmations uint32  `json:"confirmations,omitempty"`
	Send          *TxSend `json:"send,omitempty"` // the release transaction as sent: nonce, fees, every hash
	Error         string  `json:"error,omitempty"`
	Attempts      int     `json:"attempts"`
	Created       int64   `json:"created"`
	Updated       int64   `json:"updated"`
	// TxPayout is what the whole transaction pays out of the bridge (every
	// uARG output to a swap address and the burn, txPayout): the payout
	// depth and the hold above the last tier are chosen for it, since a
	// reorganisation undoes all of them together. 0 on a record from before
	// it was kept: the redemption's own amount.
	TxPayout consensus.Amount `json:"tx_payout,omitempty"`
}

// depthAmount is the amount the payout depth is chosen for: the bridge
// payout of the whole transaction, at least the redemption's own amount.
func (r *Redemption) depthAmount() consensus.Amount {
	if r.TxPayout > r.Amount {
		return r.TxPayout
	}
	return r.Amount
}

// ID names a redemption everywhere: in the store, in the guardians'
// signing records, in sign-release requests and (as burnID) in the vault.
// A burn with a memo is its transaction id; a payment to a swap address is
// "txid:vout", since one transaction can pay several.
func (r *Redemption) ID() string {
	if r.Vout == nil {
		return r.TxID
	}
	return r.TxID + ":" + strconv.FormatUint(uint64(*r.Vout), 10)
}

// ParseRedemptionID splits a redemption id into its transaction id and, for
// a swap payment, its output.
func ParseRedemptionID(id string) (txid string, vout *uint32, err error) {
	id = strings.ToLower(strings.TrimSpace(id))
	txid, out, hasOut := strings.Cut(id, ":")
	if b, e := hex.DecodeString(txid); e != nil || len(b) != 32 {
		return "", nil, errors.New("a redemption id is a transaction id (64 hex digits), with :output for a swap address payment")
	}
	if !hasOut {
		return txid, nil, nil
	}
	n, e := strconv.ParseUint(out, 10, 32)
	if e != nil || strconv.FormatUint(n, 10) != out {
		return "", nil, errors.New("a redemption id's output is a whole number")
	}
	v := uint32(n)
	return txid, &v, nil
}

// InboundRef names a deposit across chains.
type InboundRef struct {
	Chain uint64 `json:"chain_id"`
	ID    uint64 `json:"id"`
}

func (r InboundRef) String() string { return fmt.Sprintf("%d#%d", r.Chain, r.ID) }

// UTXO is a custody ARG coin the daemon knows about; custody ARG only pays
// the fees of issuance transactions.
type UTXO struct {
	TxID     string           `json:"txid"`
	Index    uint32           `json:"index"`
	Amount   consensus.Amount `json:"amount"`
	Height   uint32           `json:"height"` // 0 = unconfirmed (our own change)
	Coinbase bool             `json:"coinbase"`
}

// Outpoint is the coin's "txid:index".
func (u UTXO) Outpoint() string { return outpointString(u.TxID, u.Index) }

func outpointString(txid string, index uint32) string {
	return strings.ToLower(txid) + ":" + strconv.FormatUint(uint64(index), 10)
}

func parseOutpoint(s string) (string, uint32, bool) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(s[i+1:], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return s[:i], uint32(n), true
}

// Issue records one issuance transaction and the deposits it paid.
type Issue struct {
	TxID      string       `json:"txid"`
	Inbound   []InboundRef `json:"inbound"`
	Created   int64        `json:"created"`
	Confirmed bool         `json:"confirmed"`
	Hex       string       `json:"hex,omitempty"`         // the signed transaction, to broadcast again
	Rebroad   int64        `json:"rebroadcast,omitempty"` // last time it was broadcast again (unix)
}

// stateFormat is the layout version of the state directory.
//
//	2  deposits keyed by (chain, id), cursors by chain
//	3  vault fees on deposits and redemptions (the fee-taking vaults)
//	4  issuances settled by the deposit references they carry on chain,
//	   per-block undo records for Argonmine, the signing memory, the set of
//	   burns signed for release, vault epochs
//
// Nothing that records money already moved or promised (deposits,
// redemptions, issuances, the signing memory, signed burns) is ever wiped
// on a format change: 3 is migrated in place (only the custody coin set and
// the Argonmine cursor are dropped and rebuilt by a rescan), anything older
// is moved under "a/<format>/" and kept (docs/BRIDGE.md, "State"). Bump it
// whenever keys or records change shape, with a migration.
const stateFormat = 4

// Key prefixes. The single-byte ones are the original record types; every
// newer one is two bytes ending in '/', and none of them starts with i, x,
// u, s or p, so the old prefix listings never see them.
//
//	i ‖ chain ‖ id            Inbound
//	x ‖ txid                  Redemption
//	u ‖ txid ‖ index          UTXO (custody ARG coin)
//	s ‖ txid                  Issue (leader: issuance broadcast)
//	c/<name>                  scan cursor
//	w/ ‖ chain ‖ to           DepositWatch (depositaddr.go)
//	b/ ‖ height               L1Block: an Argonmine block indexed, its hash and the transactions that changed the state
//	t/ ‖ txid                 l1TxUndo: what indexing one transaction changed, to undo it on a reorg
//	k/ ‖ txid ‖ index         CustodySpend: the transaction that spent a custody coin
//	r/ ‖ chain ‖ id           the issuance that paid a deposit not (yet) on record here
//	g/tx/ ‖ txid              SignedIssue: an issuance this guardian partially signed
//	g/dep/ ‖ chain ‖ id ‖ txid  index of the above by deposit
//	g/time/ ‖ unix ‖ txid     index of the above by time (the daily limit)
//	o/ ‖ burn txid            SignedBurn: a release this guardian signed
//	e/ ‖ chain                Epoch: the vault a chain was last seen with
//	h/<scope>                 Pause: issuance or releases stopped until an operator resumes them (ops.go)
//	v/ ‖ chain ‖ height       an EVM block hash the scan passed (a range boundary), for reorganisations
//	z/ ‖ chain ‖ id ‖ tx      a deposit record replaced after its event left the chain
//	y/<burn txid>             an operator's approval to pay a burn above the last payout tier (ops.go)
//	a/<format>/ ‖ key         a record of an older format, kept
//	q/…                       withdrawals and the payments to swap addresses by address (withdraw.go)
const (
	pfxBlock      = "b/"
	pfxTxUndo     = "t/"
	pfxSpend      = "k/"
	pfxSettled    = "r/"
	pfxSignedTx   = "g/tx/"
	pfxSignedDep  = "g/dep/"
	pfxSignedTime = "g/time/"
	pfxSignedBurn = "o/"
	pfxEpoch      = "e/"
	pfxPause      = "h/"
	pfxCheckpoint = "v/"
	pfxReplaced   = "z/"
	pfxArchive    = "a/"
)

// State is the daemon's LevelDB-backed bookkeeping.
type State struct {
	db           *leveldb.DB
	Rebuilt      bool // the directory was empty or of a format older than 3: this run rescans from scratch
	MigratedFrom int  // the format the directory had when it was opened, if it was migrated (0 otherwise)
	wasEmpty     bool // the directory was new

	// In-memory indexes of the deposits, for the public API's filtered
	// queries: by destination address and by EVM transaction.
	mu   sync.RWMutex
	idx  map[InboundRef]inboundIdx
	byTo map[string]map[InboundRef]struct{}
	byTx map[string]InboundRef
	// The redemptions' amounts by Argonmine height, for the payout depth of
	// a window of blocks (PayoutVolume): each redemption's height and
	// amount, and per height the sum.
	rdIdx    map[string]rdEntry
	rdHeight map[uint32]consensus.Amount

	// swapChanged hears of every written key that can change what a
	// withdrawal shows (swapWriteAddr): the swap address, or "" when it is
	// not known. Set once before the daemon starts; it only marks cached
	// views out of date (withdraw.go) and must not write.
	swapChanged func(addr string)
}

type inboundIdx struct {
	to, tx string
	block  uint64
	status string
}

type rdEntry struct {
	height uint32
	amount consensus.Amount
}

func openState(path string) (*State, error) {
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, err
	}
	s := &State{db: db, idx: map[InboundRef]inboundIdx{}, byTo: map[string]map[InboundRef]struct{}{}, byTx: map[string]InboundRef{},
		rdIdx: map[string]rdEntry{}, rdHeight: map[uint32]consensus.Amount{}}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	s.rebuildIndex()
	return s, nil
}

// migrate brings the directory to stateFormat without losing a record.
func (s *State) migrate() error {
	var have int
	hasFormat := s.get([]byte("format"), &have)
	switch {
	case hasFormat && have == stateFormat:
		return nil
	case hasFormat && have > stateFormat:
		return fmt.Errorf("bridge: the state directory has format %d, newer than this daemon's %d; run the newer daemon", have, stateFormat)
	case s.empty():
		s.Rebuilt, s.wasEmpty = true, true
		return s.put([]byte("format"), stateFormat)
	case hasFormat && have == 3:
		s.MigratedFrom = 3
		return s.migrate3()
	default:
		if !hasFormat {
			have = 0
		}
		s.MigratedFrom, s.Rebuilt = have, true
		return s.archive(have)
	}
}

func (s *State) empty() bool {
	iter := s.db.NewIterator(nil, nil)
	defer iter.Release()
	return !iter.Next()
}

// migrate3 turns a format-3 directory into format 4 in place. Deposits,
// redemptions, issuances, watches, the EVM cursors, the signing memory, the
// signed burns and the epochs stay as they are. The custody coin set and
// the address-and-amount records of format 3 ('p') go, and so does the
// Argonmine cursor with everything derived from the Argonmine index: the
// next round rescans Argonmine from genesis, rebuilding the coin set and
// the per-block undo records, and settles nothing twice (a deposit is only
// ever settled from pending or issued, and a burn already on record is not
// recorded again).
func (s *State) migrate3() error {
	var ops []kvOp
	for _, p := range []string{"u", "p", pfxBlock, pfxTxUndo, pfxSpend, pfxSettled} {
		s.eachKey([]byte(p), func(k []byte) { ops = append(ops, kvOp{key: k}) })
	}
	ops = append(ops, kvOp{key: []byte("c/l1")})
	b, _ := json.Marshal(stateFormat)
	ops = append(ops, kvOp{key: []byte("format"), value: b})
	return s.write(ops, true)
}

// archive moves every record of an older, incompatible format under
// "a/<format>/" (nothing is deleted) and starts a fresh format-4 state;
// the signed burns, the signing memory and the epochs stay active.
func (s *State) archive(from int) error {
	prefix := []byte(pfxArchive + strconv.Itoa(from) + "/")
	var ops []kvOp
	iter := s.db.NewIterator(nil, nil)
	for iter.Next() {
		k := append([]byte{}, iter.Key()...)
		if bytes.HasPrefix(k, []byte(pfxArchive)) || bytes.HasPrefix(k, []byte(pfxSignedBurn)) || bytes.HasPrefix(k, []byte("g/")) || bytes.HasPrefix(k, []byte(pfxEpoch)) || bytes.HasPrefix(k, []byte(pfxPause)) {
			// An earlier archive stays where it is, and so do the signed
			// burns, the signing memory, the vault epochs and the pauses:
			// they keep their meaning in any format.
			continue
		}
		ops = append(ops, kvOp{key: append(append([]byte{}, prefix...), k...), value: append([]byte{}, iter.Value()...)}, kvOp{key: k})
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return err
	}
	b, _ := json.Marshal(stateFormat)
	ops = append(ops, kvOp{key: []byte("format"), value: b})
	return s.write(ops, true)
}

// ArchivedRedemption finds a burn in the records kept from an older format.
func (s *State) ArchivedRedemption(txid string) (*Redemption, bool) {
	for v := 0; v < stateFormat; v++ {
		var r Redemption
		if s.get(append([]byte(pfxArchive+strconv.Itoa(v)+"/x"), txid...), &r) {
			return &r, true
		}
	}
	return nil, false
}

func (s *State) Close() error { return s.db.Close() }

// ---- low-level access ----

// kvOp is one write: value nil deletes the key.
type kvOp struct {
	key   []byte
	value []byte
}

// write applies ops atomically (sync: flushed to disk before it returns)
// and keeps the in-memory indexes in step.
func (s *State) write(ops []kvOp, sync bool) error {
	b := new(leveldb.Batch)
	for _, op := range ops {
		if op.value == nil {
			b.Delete(op.key)
		} else {
			b.Put(op.key, op.value)
		}
	}
	if err := s.db.Write(b, &opt.WriteOptions{Sync: sync}); err != nil {
		return err
	}
	s.mu.Lock()
	for _, op := range ops {
		if isInboundKey(op.key) {
			s.indexInbound(op.key, op.value)
		} else if isRedemptionKey(op.key) {
			s.indexRedemption(op.key, op.value)
		}
	}
	s.mu.Unlock()
	if f := s.swapChanged; f != nil {
		for _, op := range ops {
			if addr, ok := swapWriteAddr(op); ok {
				f(addr)
			}
		}
	}
	return nil
}

func (s *State) put(key []byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write([]kvOp{{key: key, value: b}}, false)
}

func (s *State) putSync(key []byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write([]kvOp{{key: key, value: b}}, true)
}

func (s *State) del(key []byte) error { return s.write([]kvOp{{key: key}}, false) }

func (s *State) get(key []byte, v any) bool {
	b, err := s.db.Get(key, nil)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// getRaw returns the stored bytes of key, or nil when it is absent.
func (s *State) getRaw(key []byte) ([]byte, error) {
	b, err := s.db.Get(key, nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, nil
	}
	return b, err
}

func (s *State) has(key []byte) bool {
	ok, _ := s.db.Has(key, nil)
	return ok
}

func (s *State) list(prefix byte, each func([]byte)) {
	iter := s.db.NewIterator(util.BytesPrefix([]byte{prefix}), nil)
	defer iter.Release()
	for iter.Next() {
		each(append([]byte{}, iter.Value()...))
	}
}

func (s *State) eachKey(prefix []byte, each func(k []byte)) {
	iter := s.db.NewIterator(util.BytesPrefix(prefix), nil)
	defer iter.Release()
	for iter.Next() {
		each(append([]byte{}, iter.Key()...))
	}
}

func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// ---- inbound deposits ----

func inboundKey(chain, id uint64) []byte {
	return append(append([]byte{'i'}, be64(chain)...), be64(id)...)
}

func isInboundKey(k []byte) bool { return len(k) == 17 && k[0] == 'i' }

func (s *State) PutInbound(d *Inbound) error { return s.put(inboundKey(d.Chain, d.ID), d) }

func (s *State) GetInbound(chain, id uint64) (*Inbound, bool) {
	var d Inbound
	if !s.get(inboundKey(chain, id), &d) {
		return nil, false
	}
	return &d, true
}

// Inbounds returns every deposit, newest first (by the time it was seen,
// then by chain and id, so deposits from several chains interleave sensibly).
func (s *State) Inbounds() []Inbound {
	var out []Inbound
	s.list('i', func(b []byte) {
		var d Inbound
		if json.Unmarshal(b, &d) == nil {
			out = append(out, d)
		}
	})
	sortInbounds(out)
	return out
}

func sortInbounds(out []Inbound) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		if out[i].Chain != out[j].Chain {
			return out[i].Chain > out[j].Chain
		}
		return out[i].ID > out[j].ID
	})
}

// indexInbound updates the in-memory indexes for one written deposit key.
// The caller holds s.mu.
func (s *State) indexInbound(key, value []byte) {
	ref := InboundRef{Chain: binary.BigEndian.Uint64(key[1:9]), ID: binary.BigEndian.Uint64(key[9:17])}
	if old, ok := s.idx[ref]; ok {
		if set := s.byTo[old.to]; set != nil {
			delete(set, ref)
			if len(set) == 0 {
				delete(s.byTo, old.to)
			}
		}
		if s.byTx[old.tx] == ref {
			delete(s.byTx, old.tx)
		}
		delete(s.idx, ref)
	}
	if value == nil {
		return
	}
	var d Inbound
	if json.Unmarshal(value, &d) != nil {
		return
	}
	e := inboundIdx{to: strings.ToLower(d.To), tx: strings.ToLower(d.EVMTx), block: d.Block, status: d.Status}
	s.idx[ref] = e
	if s.byTo[e.to] == nil {
		s.byTo[e.to] = map[InboundRef]struct{}{}
	}
	s.byTo[e.to][ref] = struct{}{}
	if e.tx != "" {
		s.byTx[e.tx] = ref
	}
}

func (s *State) rebuildIndex() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idx, s.byTo, s.byTx = map[InboundRef]inboundIdx{}, map[string]map[InboundRef]struct{}{}, map[string]InboundRef{}
	iter := s.db.NewIterator(util.BytesPrefix([]byte{'i'}), nil)
	defer iter.Release()
	for iter.Next() {
		if isInboundKey(iter.Key()) {
			s.indexInbound(append([]byte{}, iter.Key()...), append([]byte{}, iter.Value()...))
		}
	}
	s.rdIdx, s.rdHeight = map[string]rdEntry{}, map[uint32]consensus.Amount{}
	rit := s.db.NewIterator(util.BytesPrefix([]byte{'x'}), nil)
	defer rit.Release()
	for rit.Next() {
		if isRedemptionKey(rit.Key()) {
			s.indexRedemption(append([]byte{}, rit.Key()...), append([]byte{}, rit.Value()...))
		}
	}
}

// InboundsTo returns the deposits paid to any of the addresses (lowercase),
// newest first, at most limit of them.
func (s *State) InboundsTo(addrs []string, limit int) []Inbound {
	s.mu.RLock()
	var refs []InboundRef
	for _, a := range addrs {
		for ref := range s.byTo[strings.ToLower(a)] {
			refs = append(refs, ref)
		}
	}
	s.mu.RUnlock()
	out := make([]Inbound, 0, len(refs))
	for _, ref := range refs {
		if d, ok := s.GetInbound(ref.Chain, ref.ID); ok {
			out = append(out, *d)
		}
	}
	sortInbounds(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// InboundByEVMTx finds a deposit by its EVM transaction hash on any chain.
func (s *State) InboundByEVMTx(hash string) (*Inbound, bool) {
	s.mu.RLock()
	ref, ok := s.byTx[strings.ToLower(hash)]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return s.GetInbound(ref.Chain, ref.ID)
}

// InboundsInBlocks returns the deposits of a chain whose block is in
// [from, to], with any status but reorged, from the in-memory index.
func (s *State) InboundsInBlocks(chain, from, to uint64) []Inbound {
	s.mu.RLock()
	var refs []InboundRef
	for ref, e := range s.idx {
		if ref.Chain == chain && e.block >= from && e.block <= to && e.status != InboundReorged {
			refs = append(refs, ref)
		}
	}
	s.mu.RUnlock()
	out := make([]Inbound, 0, len(refs))
	for _, ref := range refs {
		if d, ok := s.GetInbound(ref.Chain, ref.ID); ok {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ArchiveReplaced keeps a deposit record that is being replaced by another
// deposit with the same id (its event left the chain and the id went to
// another deposit in the reorganisation).
func (s *State) ArchiveReplaced(d *Inbound) error {
	k := append(append(append([]byte(pfxReplaced), be64(d.Chain)...), be64(d.ID)...), strings.ToLower(d.EVMTx)...)
	return s.putSync(k, d)
}

// ---- EVM checkpoints ----

func checkpointKey(chain, height uint64) []byte {
	return append(append([]byte(pfxCheckpoint), be64(chain)...), be64(height)...)
}

// EVMCheckpoint is one recorded block hash of an EVM chain.
type EVMCheckpoint struct {
	Height uint64
	Hash   string
}

// maxCheckpoints is how many block hashes are kept per chain: every scan
// round that reaches new final blocks records one, so this covers hours of
// history, far deeper than any reorganisation of final blocks.
const maxCheckpoints = 2000

// PutCheckpoint records the hash of a block the scan of a chain passed and
// keeps the newest maxCheckpoints of them.
func (s *State) PutCheckpoint(chain, height uint64, hash string) error {
	ops := []kvOp{{key: checkpointKey(chain, height), value: []byte(hash)}}
	prefix := append([]byte(pfxCheckpoint), be64(chain)...)
	var keys [][]byte
	s.eachKey(prefix, func(k []byte) { keys = append(keys, k) })
	for len(keys) >= maxCheckpoints {
		ops = append(ops, kvOp{key: keys[0]})
		keys = keys[1:]
	}
	return s.write(ops, false)
}

// Checkpoints returns a chain's recorded block hashes at or below height,
// highest first.
func (s *State) Checkpoints(chain, below uint64) []EVMCheckpoint {
	prefix := append([]byte(pfxCheckpoint), be64(chain)...)
	var out []EVMCheckpoint
	iter := s.db.NewIterator(util.BytesPrefix(prefix), nil)
	defer iter.Release()
	for iter.Next() {
		h := binary.BigEndian.Uint64(iter.Key()[len(prefix):])
		if h <= below {
			out = append(out, EVMCheckpoint{Height: h, Hash: string(iter.Value())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Height > out[j].Height })
	return out
}

// DropCheckpoints forgets every block hash recorded for a chain.
func (s *State) DropCheckpoints(chain uint64) error {
	var ops []kvOp
	s.eachKey(append([]byte(pfxCheckpoint), be64(chain)...), func(k []byte) { ops = append(ops, kvOp{key: k}) })
	if len(ops) == 0 {
		return nil
	}
	return s.write(ops, true)
}

// DropCheckpointsAbove forgets a chain's block hashes above height.
func (s *State) DropCheckpointsAbove(chain, height uint64) error {
	prefix := append([]byte(pfxCheckpoint), be64(chain)...)
	var ops []kvOp
	s.eachKey(prefix, func(k []byte) {
		if binary.BigEndian.Uint64(k[len(prefix):]) > height {
			ops = append(ops, kvOp{key: k})
		}
	})
	if len(ops) == 0 {
		return nil
	}
	return s.write(ops, true)
}

// ---- pauses ----

// Pause stops issuance on one chain ("evm/<chain id>") or issuance and
// releases everywhere ("l1") until an operator resumes it (ops.go).
type Pause struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
	Since  int64  `json:"since"`
}

func pauseKey(scope string) []byte { return []byte(pfxPause + scope) }

func (s *State) PutPause(p *Pause) error { return s.putSync(pauseKey(p.Scope), p) }

func (s *State) DelPause(scope string) error { return s.write([]kvOp{{key: pauseKey(scope)}}, true) }

// Pauses returns every pause in force, by scope.
func (s *State) Pauses() []Pause {
	var out []Pause
	iter := s.db.NewIterator(util.BytesPrefix([]byte(pfxPause)), nil)
	defer iter.Release()
	for iter.Next() {
		var p Pause
		if json.Unmarshal(iter.Value(), &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// ---- redemptions ----

func redemptionKey(txid string) []byte { return append([]byte{'x'}, txid...) }

// isRedemptionKey: 'x' and a redemption id (a transaction id, then
// ":output" for a payment to a swap address).
func isRedemptionKey(k []byte) bool { return len(k) >= 65 && k[0] == 'x' }

// indexRedemption keeps the amounts by height in step with one written
// redemption key. The caller holds s.mu.
func (s *State) indexRedemption(key, value []byte) {
	id := string(key[1:])
	if old, ok := s.rdIdx[id]; ok {
		if left := s.rdHeight[old.height] - old.amount; left > 0 {
			s.rdHeight[old.height] = left
		} else {
			delete(s.rdHeight, old.height)
		}
		delete(s.rdIdx, id)
	}
	if value == nil {
		return
	}
	var r Redemption
	if json.Unmarshal(value, &r) != nil || r.Amount <= 0 {
		return
	}
	s.rdIdx[id] = rdEntry{height: r.Height, amount: r.Amount}
	s.rdHeight[r.Height] += r.Amount
}

// PayoutVolume is the sum of the amounts of every redemption on record
// whose Argonmine height is in [from, to]: what the bridge pays out of
// those blocks, whatever each payout's status (docs/BRIDGE.md, "Payout
// depth"). It reads the in-memory index, at most one lookup per height.
func (s *State) PayoutVolume(from, to uint32) consensus.Amount {
	if from > to {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sum consensus.Amount
	if uint64(to-from) >= uint64(len(s.rdHeight)) {
		for h, a := range s.rdHeight {
			if h >= from && h <= to {
				sum += a
			}
		}
		return sum
	}
	for h := uint64(from); h <= uint64(to); h++ {
		sum += s.rdHeight[uint32(h)]
	}
	return sum
}

func (s *State) PutRedemption(r *Redemption) error { return s.put(redemptionKey(r.ID()), r) }

func (s *State) GetRedemption(id string) (*Redemption, bool) {
	var r Redemption
	if !s.get(redemptionKey(id), &r) {
		return nil, false
	}
	return &r, true
}

// Redemptions returns every redemption, newest first.
func (s *State) Redemptions() []Redemption {
	var out []Redemption
	s.list('x', func(b []byte) {
		var r Redemption
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Height != out[j].Height {
			return out[i].Height > out[j].Height
		}
		return out[i].ID() > out[j].ID()
	})
	return out
}

// ---- custody coins ----

func utxoKey(txid string, index uint32) []byte {
	k := append([]byte{'u'}, txid...)
	return append(k, be64(uint64(index))...)
}

func (s *State) PutUTXO(u *UTXO) error { return s.put(utxoKey(u.TxID, u.Index), u) }
func (s *State) DelUTXO(txid string, index uint32) error {
	return s.del(utxoKey(txid, index))
}
func (s *State) HasUTXO(txid string, index uint32) bool { return s.has(utxoKey(txid, index)) }

// UTXOs returns the custody coins, largest first (ties by outpoint, so coin
// selection is deterministic).
func (s *State) UTXOs() []UTXO {
	var out []UTXO
	s.list('u', func(b []byte) {
		var u UTXO
		if json.Unmarshal(b, &u) == nil {
			out = append(out, u)
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		if out[i].TxID != out[j].TxID {
			return out[i].TxID < out[j].TxID
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// ---- issuances ----

func issueKey(txid string) []byte { return append([]byte{'s'}, txid...) }

func (s *State) PutIssue(r *Issue) error { return s.put(issueKey(r.TxID), r) }

func (s *State) Issues() []Issue {
	var out []Issue
	s.list('s', func(b []byte) {
		var r Issue
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

// ---- scan cursors ----

// Cursors: "l1" for this chain, "evm/<chain id>" per EVM chain.

type cursor struct {
	Height uint64 `json:"height"`
	Hash   string `json:"hash"`
}

// EVMCursor is the cursor name of an EVM chain.
func EVMCursor(chainID uint64) string { return "evm/" + strconv.FormatUint(chainID, 10) }

func cursorKey(name string) []byte { return []byte("c/" + name) }

func (s *State) Cursor(name string) (uint64, string, bool) {
	var c cursor
	if !s.get(cursorKey(name), &c) {
		return 0, "", false
	}
	return c.Height, c.Hash, true
}

func (s *State) SetCursor(name string, height uint64, hash string) error {
	return s.put(cursorKey(name), cursor{Height: height, Hash: hash})
}

// ---- what Argonmine says happened (written by the L1 index, l1index.go) ----

// CustodySpend is the transaction that spent a custody coin, and its height.
type CustodySpend struct {
	TxID   string `json:"txid"`
	Height uint32 `json:"height"`
}

func spendKey(txid string, index uint32) []byte {
	return append(append([]byte(pfxSpend), txid...), be64(uint64(index))...)
}

func (s *State) CustodySpend(outpoint string) (*CustodySpend, bool) {
	txid, idx, ok := parseOutpoint(outpoint)
	if !ok {
		return nil, false
	}
	var sp CustodySpend
	if !s.get(spendKey(txid, idx), &sp) {
		return nil, false
	}
	return &sp, true
}

func settledKey(ref InboundRef) []byte {
	return append(append([]byte(pfxSettled), be64(ref.Chain)...), be64(ref.ID)...)
}

// SettledRef is the issuance that paid a deposit this guardian had not seen
// when the issuance was mined (a rebuilt state, an EVM scan behind), and the
// Argonmine height it was mined at (0 on a record from before heights were
// kept: unknown).
type SettledRef struct {
	TxID   string `json:"txid"`
	Height uint32 `json:"height"`
}

// Settled returns the issuance recorded as paying a deposit not on record
// when it was mined. Records from before heights were kept hold the
// transaction id alone.
func (s *State) Settled(ref InboundRef) (*SettledRef, bool) {
	b, err := s.getRaw(settledKey(ref))
	if err != nil || b == nil {
		return nil, false
	}
	var r SettledRef
	if json.Unmarshal(b, &r) == nil && r.TxID != "" {
		return &r, true
	}
	var txid string
	if json.Unmarshal(b, &txid) == nil && txid != "" {
		return &SettledRef{TxID: txid}, true
	}
	return nil, false
}

// MaxDepositID is the highest deposit id this guardian has on record for a
// chain, from any vault: its deposits (with those kept from older formats
// and those replaced in a reorganisation), the issuances it signed, and the
// deposit references of issuances mined before their deposit was on record.
// A vault that replaces another on the chain must number its deposits
// above it.
//
// Every one of these keys is the prefix, the chain and the deposit id (big
// endian), with anything else after it, so the last key under each prefix
// holds its highest id: one reverse seek per prefix, whatever the number of
// records (the status asks for it on every snapshot).
func (s *State) MaxDepositID(chain uint64) uint64 {
	var max uint64
	scan := func(prefix []byte) {
		iter := s.db.NewIterator(util.BytesPrefix(prefix), nil)
		defer iter.Release()
		if !iter.Last() {
			return
		}
		if k := iter.Key(); len(k) >= len(prefix)+8 {
			if id := binary.BigEndian.Uint64(k[len(prefix) : len(prefix)+8]); id > max {
				max = id
			}
		}
	}
	ch := be64(chain)
	for _, p := range []string{"i", pfxSettled, pfxSignedDep, pfxReplaced} {
		scan(append([]byte(p), ch...))
	}
	for v := 0; v < stateFormat; v++ {
		scan(append([]byte(pfxArchive+strconv.Itoa(v)+"/i"), ch...))
	}
	return max
}

// ---- the signing memory (issue.go) ----

// SignedIssue is one issuance transaction this guardian contributed a
// partial signature to: the custody coins it spends, the deposits it pays
// and the uARG it creates. A guardian signs another issuance for any of
// these deposits only if it spends one of the same coins (so at most one of
// them can ever confirm) or this one can no longer confirm.
type SignedIssue struct {
	TxID    string           `json:"txid"`
	Inputs  []string         `json:"inputs"` // custody outpoints, "txid:index"
	Inbound []InboundRef     `json:"inbound"`
	Amount  consensus.Amount `json:"amount"`
	Time    int64            `json:"time"`
}

func signedTxKey(txid string) []byte { return append([]byte(pfxSignedTx), txid...) }
func signedDepPrefix(ref InboundRef) []byte {
	return append(append([]byte(pfxSignedDep), be64(ref.Chain)...), be64(ref.ID)...)
}
func signedTimeKey(t int64, txid string) []byte {
	return append(append([]byte(pfxSignedTime), be64(uint64(t))...), txid...)
}

// RecordSignedIssue stores si durably (flushed to disk) before the partial
// signature leaves this guardian. Recording the same transaction again is a
// no-op.
func (s *State) RecordSignedIssue(si *SignedIssue) error {
	if s.has(signedTxKey(si.TxID)) {
		return nil
	}
	b, err := json.Marshal(si)
	if err != nil {
		return err
	}
	amt, _ := json.Marshal(si.Amount)
	ops := []kvOp{{key: signedTxKey(si.TxID), value: b}, {key: signedTimeKey(si.Time, si.TxID), value: amt}}
	for _, ref := range si.Inbound {
		ops = append(ops, kvOp{key: append(signedDepPrefix(ref), si.TxID...), value: []byte("1")})
	}
	return s.write(ops, true)
}

// SignedIssuesFor returns every issuance this guardian signed that pays ref.
func (s *State) SignedIssuesFor(ref InboundRef) []SignedIssue {
	prefix := signedDepPrefix(ref)
	var out []SignedIssue
	s.eachKey(prefix, func(k []byte) {
		var si SignedIssue
		if s.get(signedTxKey(string(k[len(prefix):])), &si) {
			out = append(out, si)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].TxID < out[j].TxID })
	return out
}

// SignedSince is what this guardian signed issuances for at or after unix
// time since, per deposit: a deposit in several of them (a round retried,
// an issuance built anew after one was dropped) counts once, with its
// amount on record. An issuance paying a deposit that is not on record here
// counts once with its whole amount, under a key of its own.
func (s *State) SignedSince(since int64) map[InboundRef]consensus.Amount {
	start := append([]byte(pfxSignedTime), be64(uint64(since))...)
	iter := s.db.NewIterator(&util.Range{Start: start, Limit: util.BytesPrefix([]byte(pfxSignedTime)).Limit}, nil)
	defer iter.Release()
	out := map[InboundRef]consensus.Amount{}
	whole := func(txid string, a consensus.Amount) {
		h := consensus.TaggedHash("Argonmine/bridge/signed-issue", []byte(txid))
		out[InboundRef{Chain: 0, ID: binary.BigEndian.Uint64(h[:8])}] = a // chain 0: no deposit's
	}
	for iter.Next() {
		k := iter.Key()
		if len(k) < len(pfxSignedTime)+8 {
			continue
		}
		txid := string(k[len(pfxSignedTime)+8:])
		var si SignedIssue
		if !s.get(signedTxKey(txid), &si) {
			var a consensus.Amount
			if json.Unmarshal(iter.Value(), &a) == nil {
				whole(txid, a)
			}
			continue
		}
		amounts := map[InboundRef]consensus.Amount{}
		complete := true
		for _, ref := range si.Inbound {
			d, ok := s.GetInbound(ref.Chain, ref.ID)
			if !ok {
				complete = false
				break
			}
			amounts[ref] = d.Amount
		}
		if !complete {
			whole(txid, si.Amount)
			continue
		}
		for ref, a := range amounts {
			out[ref] = a
		}
	}
	return out
}

// sumSigned adds up a SignedSince window, leaving out the deposits in except.
func sumSigned(m map[InboundRef]consensus.Amount, except map[InboundRef]bool) consensus.Amount {
	var sum consensus.Amount
	for ref, a := range m {
		if !except[ref] {
			sum += a
		}
	}
	return sum
}

// SignedBurn is a release this guardian signed: the burn it pays and
// exactly what it authorised. A guardian signs a burn once; asked again it
// returns the same signature for the same release and refuses any other
// (another vault, chain, token, recipient or amount), whatever the
// configuration says by then.
type SignedBurn struct {
	TxID   string `json:"txid"`
	Chain  uint64 `json:"chain_id"`
	Vault  string `json:"vault"`
	Token  string `json:"token"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Digest string `json:"digest"`
	Time   int64  `json:"time"`
}

func signedBurnKey(txid string) []byte { return append([]byte(pfxSignedBurn), txid...) }

func (s *State) SignedBurn(txid string) (*SignedBurn, bool) {
	var b SignedBurn
	if !s.get(signedBurnKey(txid), &b) {
		return nil, false
	}
	return &b, true
}

// RecordSignedBurn stores b durably before the signature leaves this guardian.
func (s *State) RecordSignedBurn(b *SignedBurn) error { return s.putSync(signedBurnKey(b.TxID), b) }

// Epoch is the vault a chain was last seen with, and the first Argonmine
// height whose burns it pays when this guardian saw the vault replaced
// (0 when it never was: the chain's l1_start_height alone decides).
//
// It also records the vault's deposit numbering (docs/BRIDGE.md,
// "Replacing a vault"). A deposit is named by its chain and id alone, so a
// vault that replaces another must number its deposits above every id on
// record for the chain: AboveID is the highest of those when this vault
// replaced Previous (0 for the first vault this guardian saw on the chain),
// and FirstID the vault's first deposit id (its firstDepositId()), recorded
// once it was read and found above AboveID.
//
// An epoch is recorded only once its vault has passed that check
// (checkVaultIdentity): until then it is a candidate, kept in memory
// (chainState.cand), and nothing of the vault is scanned, issued or paid,
// so a vault that is refused, or mistyped, never takes the place of the one
// in use. Rescan marks a candidate replacement whose EVM scan starts again
// from the chain's start_block when it passes; on record it is only found
// in an epoch written by the build of 2026-10-28 before its check (then
// with no FirstID: it never passed). History holds the chain's earlier
// epochs, oldest first, each as it was when the next one replaced it.
type Epoch struct {
	Vault    string  `json:"vault"`
	L1Start  uint32  `json:"l1_start"`
	Previous string  `json:"previous,omitempty"`
	Since    int64   `json:"since"`
	AboveID  uint64  `json:"above_id,omitempty"`
	FirstID  uint64  `json:"first_id,omitempty"`
	Rescan   bool    `json:"rescan,omitempty"`
	History  []Epoch `json:"history,omitempty"`
}

// passed reports whether the epoch's vault passed the identity check, so
// that something of it may have been scanned, issued or paid: every epoch
// this build records, and every one from before the check existed. Only an
// epoch recorded before its check (Rescan, no FirstID) never did.
func (e *Epoch) passed() bool { return e.FirstID != 0 || !e.Rescan }

// restorable is the earlier epoch of the chain whose vault is vault (with
// the history before it), when every epoch recorded after it never passed
// the identity check: nothing of their vaults was scanned, issued or paid,
// so going back to that vault takes its epoch back as it was (its L1Start,
// its FirstID, its EVM scan) instead of starting a new one. e is the epoch
// on record, whose vault is not vault.
func (e *Epoch) restorable(vault string) (*Epoch, bool) {
	if e.passed() {
		return nil, false
	}
	for i := len(e.History) - 1; i >= 0; i-- {
		h := e.History[i]
		if strings.EqualFold(h.Vault, vault) {
			h.History = append([]Epoch{}, e.History[:i]...)
			return &h, true
		}
		if h.passed() {
			return nil, false
		}
	}
	return nil, false
}

// succeeded is the history of an epoch that replaces e: e's own history and
// e itself.
func (e *Epoch) succeeded() []Epoch {
	prev := *e
	prev.History = nil
	return append(append([]Epoch{}, e.History...), prev)
}

func epochKey(chain uint64) []byte { return append([]byte(pfxEpoch), be64(chain)...) }

func (s *State) GetEpoch(chain uint64) (*Epoch, bool) {
	var e Epoch
	if !s.get(epochKey(chain), &e) {
		return nil, false
	}
	return &e, true
}

func (s *State) PutEpoch(chain uint64, e *Epoch) error { return s.putSync(epochKey(chain), e) }

// ---- memos ----

// Memo helpers. A redemption's NULLDATA memo names where the stablecoin is
// paid out:
//
//	"BRG1" || recipient (20 bytes)                                     24 bytes: the bridge's first chain and token
//	"BRG2" || chain id (u64 BE) || token (20 bytes) || recipient (20)   52 bytes: an explicit chain and token
var (
	memoPrefix   = []byte("BRG1")
	memoPrefixV2 = []byte("BRG2")
)

// RedeemMemo is a decoded redemption memo. ChainID 0 and a zero Token mean
// "the bridge's default chain and token" (a BRG1 memo).
type RedeemMemo struct {
	ChainID uint64
	Token   [20]byte
	To      [20]byte
}

// IsDefault reports whether the memo leaves the chain and token to the bridge.
func (m RedeemMemo) IsDefault() bool { return m.ChainID == 0 && m.Token == [20]byte{} }

// EncodeMemo builds the short memo naming evmAddr as the recipient of a
// redemption on the bridge's default chain and token.
func EncodeMemo(evmAddr [20]byte) []byte {
	return append(append([]byte{}, memoPrefix...), evmAddr[:]...)
}

// EncodeRedeemMemo builds the memo for a payout of the given token on the
// given chain to evmAddr.
func EncodeRedeemMemo(chainID uint64, token, evmAddr [20]byte) []byte {
	out := make([]byte, 0, 52)
	out = append(out, memoPrefixV2...)
	var id [8]byte
	binary.BigEndian.PutUint64(id[:], chainID)
	out = append(out, id[:]...)
	out = append(out, token[:]...)
	return append(out, evmAddr[:]...)
}

// DecodeRedeemMemo parses either memo form; ok is false if the payload is not
// a bridge memo.
func DecodeRedeemMemo(payload []byte) (RedeemMemo, bool) {
	var m RedeemMemo
	switch {
	case len(payload) == 24 && string(payload[:4]) == string(memoPrefix):
		copy(m.To[:], payload[4:])
		return m, true
	case len(payload) == 52 && string(payload[:4]) == string(memoPrefixV2):
		m.ChainID = binary.BigEndian.Uint64(payload[4:12])
		copy(m.Token[:], payload[12:32])
		copy(m.To[:], payload[32:52])
		return m, m.ChainID != 0
	}
	return m, false
}

// DecodeMemo parses a NULLDATA payload and returns the recipient; ok is false
// if it is not a bridge memo. Callers that route by chain and token use
// DecodeRedeemMemo.
func DecodeMemo(payload []byte) ([20]byte, bool) {
	m, ok := DecodeRedeemMemo(payload)
	return m.To, ok
}

// An issuance carries the deposits it pays in a NULLDATA output:
//
//	"BRI1" || (chain id varint || deposit id varint) per deposit, in output order
//
// with the minimal LEB128 varints of the consensus encoding, at most 80
// bytes in all (consensus.MaxNullDataLen). Every guardian checks it against
// the outputs before signing, and settles deposits from it, and from
// nothing else, when the issuance is mined.
var issueMemoPrefix = []byte("BRI1")

// EncodeIssueMemo builds the deposit references of an issuance. It fails
// when they do not fit in one NULLDATA output.
func EncodeIssueMemo(refs []InboundRef) ([]byte, error) {
	if len(refs) == 0 {
		return nil, errors.New("an issuance pays at least one deposit")
	}
	var w consensus.Writer
	w.Raw(issueMemoPrefix)
	for _, r := range refs {
		if r.Chain == 0 || r.ID == 0 || r.Chain > consensus.MaxVarint || r.ID > consensus.MaxVarint {
			return nil, fmt.Errorf("deposit %s cannot be referenced", r)
		}
		w.Varint(r.Chain)
		w.Varint(r.ID)
	}
	if w.Len() > consensus.MaxNullDataLen {
		return nil, fmt.Errorf("the references of %d deposits take %d bytes, more than %d", len(refs), w.Len(), consensus.MaxNullDataLen)
	}
	return w.Bytes(), nil
}

// DecodeIssueMemo parses an issuance's deposit references; ok is false for
// anything else (another memo, a non-minimal varint, a zero or repeated
// reference, trailing bytes).
func DecodeIssueMemo(payload []byte) ([]InboundRef, bool) {
	if len(payload) <= len(issueMemoPrefix) || len(payload) > consensus.MaxNullDataLen || !bytes.HasPrefix(payload, issueMemoPrefix) {
		return nil, false
	}
	r := consensus.NewReader(payload[len(issueMemoPrefix):])
	var out []InboundRef
	seen := map[InboundRef]bool{}
	for r.Remaining() > 0 {
		chain, err := r.Varint()
		if err != nil {
			return nil, false
		}
		id, err := r.Varint()
		if err != nil {
			return nil, false
		}
		ref := InboundRef{Chain: chain, ID: id}
		if chain == 0 || id == 0 || seen[ref] {
			return nil, false
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out, len(out) > 0
}
