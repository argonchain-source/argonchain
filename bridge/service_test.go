package bridge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/rpc"
	"argonmine/wallet"
)

// fakeEVM behaves like one chain with its Vault contract: it records
// deposits of any token, taking the deposit fee and crediting the net,
// verifies threshold guardian signatures on release, pays the gross less
// the release fee, refuses to pay a burn twice, keeps a reserve and the
// obligations per token (the surplus between them is the fees) and
// confirms receipts instantly. With fail set, every call errors like a
// dead RPC.
//
// It also models what the hardening needs: block hashes that change from
// the height of a reorganisation up (reorgFrom), deposits that leave the
// chain or move to another block, a finalized tag trailing the head, token
// decimals (6 unless set), the vault's pause and release limits, a gas key
// with a balance, and, with hold set, release transactions that wait in a
// mempool until mined (mineRelease), with replacement by nonce.
type fakeEVM struct {
	mu          sync.Mutex
	chainID     uint64
	vault       [20]byte
	head        uint64
	guardians   map[[20]byte]bool
	threshold   int
	depositBps  uint16
	releaseBps  uint16
	deposits    []DepositEvent
	released    map[[32]byte]bool
	releases    []fakeRelease
	receipts    map[string]ReceiptStatus
	byTx        map[string]fakeRelease
	reserve     map[[20]byte]*big.Int
	obligations map[[20]byte]*big.Int
	logReads    int // ReleasedLog calls: the daemon finalising fees from receipts
	fail        bool
	rt          *fakeRouter // the chain's DepositRouter, if it has one (depositaddr_test.go)

	finalLag    uint64   // the finalized tag trails the head by this much
	forks       []uint64 // heights from which the chain was replaced: a block's hash changes with every fork at or below it
	decimals    map[[20]byte]int
	paused      bool
	maxRelease  map[[20]byte]*big.Int
	periodAvail map[[20]byte]*big.Int
	limitOnce   int // the next n sends fail with LimitExceeded (a limit reached between the check and the send)
	native      *big.Int
	sender      [20]byte
	owner       [20]byte
	hold        bool                // releases wait in pending until mineRelease
	pending     map[uint64]fakeTx   // nonce -> the transaction in the mempool
	sent        map[string][]byte   // hash -> call data, every release transaction broadcast
	nonce       uint64              // transactions of the gas key mined
	nextNonce   uint64              // the next nonce a new release takes
	txCount     int                 // hashes handed out
	sentFrom    map[[20]byte]int    // transactions per sender
	sends       []*TxSend           // every transaction broadcast, in order
	minedBy     map[[32]byte]string // burn id -> the transaction that paid it
	firstID     uint64              // the vault's firstDepositId() (0: 1)
	setup       bool                // the vault's finalized() is false
	noCode      bool                // no contract at the vault address (mistyped, or not deployed yet)
}

type fakeTx struct {
	hash string
	data []byte
}

type fakeRelease struct {
	token  [20]byte
	to     [20]byte
	amount *big.Int // gross, what release() was called with
	fee    *big.Int // kept by the vault
	paid   *big.Int // transferred to the recipient
	id     [32]byte
	sigs   int
}

func newFakeEVM(chainID uint64, vault [20]byte, guardians [][20]byte, threshold int, depositBps, releaseBps uint16) *fakeEVM {
	f := &fakeEVM{chainID: chainID, vault: vault, head: 100, guardians: map[[20]byte]bool{}, threshold: threshold, depositBps: depositBps, releaseBps: releaseBps,
		released: map[[32]byte]bool{}, receipts: map[string]ReceiptStatus{}, byTx: map[string]fakeRelease{}, reserve: map[[20]byte]*big.Int{}, obligations: map[[20]byte]*big.Int{},
		decimals: map[[20]byte]int{}, maxRelease: map[[20]byte]*big.Int{}, periodAvail: map[[20]byte]*big.Int{}, native: new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
		pending: map[uint64]fakeTx{}, sent: map[string][]byte{}, sentFrom: map[[20]byte]int{}, minedBy: map[[32]byte]string{}}
	for _, g := range guardians {
		f.guardians[g] = true
	}
	return f
}

var errRPCDown = errors.New("dial tcp: connection refused")

func (f *fakeEVM) ChainID() uint64  { return f.chainID }
func (f *fakeEVM) Vault() [20]byte  { return f.vault }
func (f *fakeEVM) Sender() [20]byte { f.mu.Lock(); defer f.mu.Unlock(); return f.sender }

// BlockNumber rejects a nil context the way a real HTTP client does, so the
// handlers are exercised with proper contexts.
func (f *fakeEVM) BlockNumber(ctx context.Context) (uint64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("net/http: nil Context")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errRPCDown
	}
	return f.head, nil
}

func (f *fakeEVM) Heads(ctx context.Context, finalized bool) (uint64, uint64, error) {
	if ctx == nil {
		return 0, 0, fmt.Errorf("net/http: nil Context")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, 0, errRPCDown
	}
	final := f.head
	if finalized {
		if f.finalLag > final {
			final = 0
		} else {
			final -= f.finalLag
		}
	}
	return f.head, final, nil
}

// hashAt is the hash of the block at a height in the current branch.
func (f *fakeEVM) hashAt(h uint64) string {
	n := 0
	for _, x := range f.forks {
		if x <= h {
			n++
		}
	}
	k := Keccak256(be64(f.chainID), be64(h), be64(uint64(n)))
	return "0x" + hex.EncodeToString(k[:])
}

func (f *fakeEVM) BlockHash(_ context.Context, h uint64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", errRPCDown
	}
	if h > f.head {
		return "", nil
	}
	return f.hashAt(h), nil
}

func (f *fakeEVM) Deposits(_ context.Context, from, to uint64) ([]DepositEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	var out []DepositEvent
	for _, d := range f.deposits {
		if d.Block >= from && d.Block <= to && d.Block <= f.head {
			d.BlockHash = f.hashAt(d.Block)
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeEVM) DepositReceipt(_ context.Context, txHash string) (*DepositReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	for _, d := range f.deposits {
		if strings.EqualFold(d.TxHash, txHash) && d.Block <= f.head {
			d.BlockHash = f.hashAt(d.Block)
			return &DepositReceipt{Status: ReceiptOK, Block: d.Block, BlockHash: d.BlockHash, Deposits: []DepositEvent{d}}, nil
		}
	}
	return nil, nil
}

func (f *fakeEVM) IsReleased(_ context.Context, id [32]byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return false, errRPCDown
	}
	return f.released[id], nil
}

func (f *fakeEVM) Reserve(_ context.Context, token [20]byte) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	r := f.reserve[token]
	if r == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(r), nil
}

func (f *fakeEVM) Obligations(_ context.Context, token [20]byte) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	o := f.obligations[token]
	if o == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(o), nil
}

func (f *fakeEVM) Fees(_ context.Context) (uint16, uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, 0, errRPCDown
	}
	if f.noCode {
		return 0, 0, fmt.Errorf("evm: the vault at %s has no depositFeeBps()", HexAddress(f.vault))
	}
	return f.depositBps, f.releaseBps, nil
}

func (f *fakeEVM) TokenDecimals(_ context.Context, token [20]byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errRPCDown
	}
	if d, ok := f.decimals[token]; ok {
		return d, nil
	}
	return 6, nil
}

func (f *fakeEVM) ReleaseLimits(_ context.Context, token [20]byte) (*ReleaseLimits, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	l := &ReleaseLimits{Paused: f.paused}
	if m := f.maxRelease[token]; m != nil {
		l.MaxRelease = new(big.Int).Set(m)
	}
	if p := f.periodAvail[token]; p != nil {
		l.PeriodAvailable = new(big.Int).Set(p)
	}
	return l, nil
}

func (f *fakeEVM) NativeBalance(_ context.Context, addr [20]byte) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	return new(big.Int).Set(f.native), nil
}

func (f *fakeEVM) VaultRoles(_ context.Context, addr [20]byte) (bool, [20]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.guardians[addr], f.owner, nil
}

func (f *fakeEVM) TxKnown(_ context.Context, h string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.receipts[h]; ok {
		return true, nil
	}
	for _, p := range f.pending {
		if p.hash == h {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeEVM) ConfirmedNonce(_ context.Context, addr [20]byte) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errRPCDown
	}
	return f.nonce, nil
}

// ReleasedLog is the Released event of a mined release, as a receipt holds it.
func (f *fakeEVM) ReleasedLog(_ context.Context, h string) (*ReleaseEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logReads++
	if f.fail {
		return nil, errRPCDown
	}
	r, ok := f.byTx[h]
	if !ok {
		return nil, nil
	}
	return &ReleaseEvent{BurnID: r.id, Token: r.token, To: r.to, Amount: new(big.Int).Set(r.amount), Fee: new(big.Int).Set(r.fee)}, nil
}

// check is the vault's release() up to the transfer: what eth_estimateGas
// would say.
func (f *fakeEVM) check(data []byte) (fakeRelease, error) {
	token, to, amount, id, sigs, err := DecodeReleaseCall(data)
	if err != nil {
		return fakeRelease{}, err
	}
	if f.released[id] {
		return fakeRelease{}, ErrAlreadyReleased
	}
	if f.paused {
		return fakeRelease{}, ErrVaultPaused
	}
	if f.limitOnce > 0 {
		f.limitOnce--
		return fakeRelease{}, ErrLimitExceeded
	}
	if m := f.maxRelease[token]; m != nil && amount.Cmp(m) > 0 {
		return fakeRelease{}, ErrLimitExceeded
	}
	if p := f.periodAvail[token]; p != nil && amount.Cmp(p) > 0 {
		return fakeRelease{}, ErrLimitExceeded
	}
	digest := ReleaseDigest(f.chainID, f.vault, token, to, amount, id)
	seen := map[[20]byte]bool{}
	var last [20]byte
	for _, s := range sigs {
		addr, err := RecoverSigner(digest, s)
		if err != nil || !f.guardians[addr] || seen[addr] {
			return fakeRelease{}, fmt.Errorf("BadSignatures")
		}
		if len(seen) > 0 && string(addr[:]) <= string(last[:]) {
			return fakeRelease{}, fmt.Errorf("BadSignatures: unordered")
		}
		seen[addr] = true
		last = addr
	}
	if len(seen) < f.threshold {
		return fakeRelease{}, fmt.Errorf("BadSignatures: %d < %d", len(seen), f.threshold)
	}
	// The vault keeps fee = amount × releaseFeeBps / 10000 and pays the rest;
	// the burn extinguishes the gross from its obligations.
	fee := FeeOf(amount, f.releaseBps)
	paid := new(big.Int).Sub(amount, fee)
	if f.reserve[token] == nil || f.reserve[token].Cmp(paid) < 0 {
		return fakeRelease{}, fmt.Errorf("insufficient reserve")
	}
	return fakeRelease{token, to, new(big.Int).Set(amount), fee, paid, id, len(seen)}, nil
}

// execute pays a checked release in transaction h.
func (f *fakeEVM) execute(rel fakeRelease, h string) {
	f.released[rel.id] = true
	f.reserve[rel.token].Sub(f.reserve[rel.token], rel.paid)
	if f.obligations[rel.token] == nil {
		f.obligations[rel.token] = new(big.Int)
	}
	f.obligations[rel.token].Sub(f.obligations[rel.token], rel.amount)
	if p := f.periodAvail[rel.token]; p != nil {
		p.Sub(p, rel.amount)
	}
	f.releases = append(f.releases, rel)
	f.receipts[h] = ReceiptOK
	f.byTx[h] = rel
	f.minedBy[rel.id] = h
	f.nonce++
	f.head++
}

func (f *fakeEVM) Release(_ context.Context, req ReleaseRequest) (*TxSend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errRPCDown
	}
	data := req.Data
	if req.Replace != nil {
		data, _ = hex.DecodeString(req.Replace.Data)
	}
	rel, err := f.check(data)
	if err != nil {
		return nil, err
	}
	var tx *TxSend
	if req.Replace != nil {
		tx = req.Replace.clone()
		pt, pc := mustBig(tx.TipCap), mustBig(tx.FeeCap)
		tip, fc, err := Bump1559(pt, pc, big.NewInt(1_000_000_000), big.NewInt(1_000_000_000), req.MaxGasPrice)
		if err != nil {
			return nil, err
		}
		if tx.Nonce < f.nonce {
			return nil, ErrNonceUsed
		}
		tx.TipCap, tx.FeeCap = tip.String(), fc.String()
		tx.Bumps++
	} else {
		fc := big.NewInt(3_000_000_000)
		if req.MaxGasPrice != nil && fc.Cmp(req.MaxGasPrice) > 0 {
			fc = new(big.Int).Set(req.MaxGasPrice)
		}
		if f.nextNonce < f.nonce {
			f.nextNonce = f.nonce
		}
		tx = &TxSend{From: HexAddress(f.sender), Nonce: f.nextNonce, Type: 2, TipCap: "1000000000", FeeCap: fc.String(), Gas: 120_000, Data: hex.EncodeToString(data)}
		f.nextNonce++
	}
	f.txCount++
	h := fmt.Sprintf("0x%064x", 0xe000+f.txCount)
	tx.Hashes = append(tx.Hashes, h)
	f.sent[h] = data
	f.sentFrom[f.sender]++
	f.sends = append(f.sends, tx.clone())
	if f.hold {
		f.pending[tx.Nonce] = fakeTx{hash: h, data: data} // a replacement takes the nonce's place
		return tx, nil
	}
	f.execute(rel, h)
	return tx, nil
}

// mineRelease mines a release transaction broadcast earlier (even one that
// was replaced in the mempool: a miner may still have it). It reverts if
// the burn was paid meanwhile. The nonce's other transaction is dropped.
func (f *fakeEVM) mineRelease(h string) ReceiptStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.sent[h]
	if !ok {
		panic("unknown transaction " + h)
	}
	for n, p := range f.pending {
		if p.hash == h {
			delete(f.pending, n)
		}
	}
	for _, s := range f.sends {
		if s.Hash() == h {
			delete(f.pending, s.Nonce)
		}
	}
	rel, err := f.check(data)
	if err != nil {
		f.receipts[h] = ReceiptFailed
		f.nonce++
		f.head++
		return ReceiptFailed
	}
	f.execute(rel, h)
	return ReceiptOK
}

// takeNonce mines another transaction of the gas key with the next nonce:
// whatever release was pending with it is gone.
func (f *fakeEVM) takeNonce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pending, f.nonce)
	f.nonce++
	f.head++
}

func (f *fakeEVM) Receipt(_ context.Context, h string) (ReceiptStatus, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return ReceiptPending, 0, errRPCDown
	}
	return f.receipts[h], f.head, nil
}

func (f *fakeEVM) FirstDepositID(_ context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errRPCDown
	}
	if f.noCode {
		return 0, fmt.Errorf("%w %s on chain %d", ErrNoVaultCode, HexAddress(f.vault), f.chainID)
	}
	if f.firstID == 0 {
		return 1, nil
	}
	return f.firstID, nil
}

func (f *fakeEVM) SetupFinalized(_ context.Context) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return false, false, errRPCDown
	}
	return !f.setup, true, nil
}

// addDeposit is a user's deposit(token, gross, to): the vault takes the
// whole gross, keeps fee = gross × depositFeeBps / 10000 and credits the rest.
// Its id is the vault's next: firstID (default 1) for the first.
func (f *fakeEVM) addDeposit(token, from [20]byte, gross *big.Int, to string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head++
	id := f.firstID
	if id == 0 {
		id = 1
	}
	for _, d := range f.deposits {
		if d.ID >= id {
			id = d.ID + 1
		}
	}
	fee := FeeOf(gross, f.depositBps)
	net := new(big.Int).Sub(gross, fee)
	f.deposits = append(f.deposits, DepositEvent{ID: id, Token: token, From: from, Amount: net, Fee: fee, To: to, Block: f.head, TxHash: fmt.Sprintf("0x%064x", f.chainID*0x10000+id)})
	if f.reserve[token] == nil {
		f.reserve[token] = new(big.Int)
		f.obligations[token] = new(big.Int)
	}
	f.reserve[token].Add(f.reserve[token], gross)
	f.obligations[token].Add(f.obligations[token], net)
	return id
}

// reorgFrom replaces the chain from height h up: every block from there has
// another hash.
func (f *fakeEVM) reorgFrom(h uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forks = append(f.forks, h)
}

// dropDeposit takes a deposit's transaction out of the chain in a
// reorganisation from its block.
func (f *fakeEVM) dropDeposit(id uint64) {
	f.mu.Lock()
	var block uint64
	for i, d := range f.deposits {
		if d.ID == id {
			block = d.Block
			f.deposits = append(f.deposits[:i], f.deposits[i+1:]...)
			break
		}
	}
	f.mu.Unlock()
	f.reorgFrom(block)
}

// moveDeposit puts a deposit's transaction into another block in a
// reorganisation from the lower of the two.
func (f *fakeEVM) moveDeposit(id, block uint64) {
	f.mu.Lock()
	from := block
	for i := range f.deposits {
		if f.deposits[i].ID == id {
			if f.deposits[i].Block < from {
				from = f.deposits[i].Block
			}
			f.deposits[i].Block = block
		}
	}
	f.mu.Unlock()
	f.reorgFrom(from)
}

func (f *fakeEVM) setDecimals(token [20]byte, d int) {
	f.mu.Lock()
	f.decimals[token] = d
	f.mu.Unlock()
}
func (f *fakeEVM) advance(n uint64)  { f.mu.Lock(); f.head += n; f.mu.Unlock() }
func (f *fakeEVM) setFail(fail bool) { f.mu.Lock(); f.fail = fail; f.mu.Unlock() }

func usd(whole int64, decimals int) *big.Int {
	return new(big.Int).Mul(big.NewInt(whole), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
}

func TestBridgeEndToEnd(t *testing.T) {
	// Node.
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	mp := mempool.New(ch, mempool.DefaultConfig())
	srv := rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: "t"}, rpc.Deps{Params: consensus.RegTest, Chain: ch, Pool: mp, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	client := wallet.NewClient("http://"+srv.Addr(), "t")
	mine := func(n int, addr string) {
		if _, err := client.Call("generatetoaddress", n, addr); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	// Guardians: 3 custody shares (the uARG issuer), 3 EVM keys; release threshold 2.
	cs, _ := guardians(t, 3)
	evmKeys := make([]*secp256k1.PrivateKey, 3)
	var evmAddrs [][20]byte
	for i := range evmKeys {
		evmKeys[i], _ = secp256k1.GeneratePrivateKey()
		evmAddrs = append(evmAddrs, AddressOf(evmKeys[i].PubKey()))
	}

	// Three chains from the same JSON an operator would write: A holds USDC
	// and USDT with 6 decimals and its vault takes no fee; B holds an
	// 18-decimal USDT and C a 6-decimal USDC, both with 1% fees on deposits
	// and on payouts.
	chains, err := ParseChains([]byte(testChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	chainA, chainB, chainC := chains[0], chains[1], chains[2]
	usdcA, usdtA, usdtB, usdcC := chainA.Tokens[0].Address, chainA.Tokens[1].Address, chainB.Tokens[0].Address, chainC.Tokens[0].Address
	evmA := newFakeEVM(chainA.ID, chainA.Vault, evmAddrs, 2, 0, 0)
	evmB := newFakeEVM(chainB.ID, chainB.Vault, evmAddrs, 2, 100, 100)
	evmC := newFakeEVM(chainC.ID, chainC.Vault, evmAddrs, 2, 100, 100)
	evmB.setDecimals(usdtB, 18) // what decimals() answers on chain B; the daemon checks it against the file
	evms := map[uint64]EVM{chainA.ID: evmA, chainB.ID: evmB, chainC.ID: evmC}

	dir := t.TempDir()
	mk := func(i int, leader bool, peers []string) *Service {
		s, err := New(Config{
			Params: consensus.RegTest, Node: client, Chains: chains, EVMs: evms, Custody: cs[i], EVMKey: evmKeys[i],
			Leader: leader, Peers: peers, Secret: testSecret, LeaderKey: cs[0].LocalKey(), Threshold: 2, L1Confirmations: 3,
			StatePath: filepath.Join(dir, fmt.Sprintf("g%d", i)), Logf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	f1, f2 := mk(1, false, nil), mk(2, false, nil)
	w1, w2 := httptest.NewServer(f1.Handler()), httptest.NewServer(f2.Handler())
	defer w1.Close()
	defer w2.Close()
	leader := mk(0, true, []string{w1.URL, w2.URL})
	defer leader.Stop()
	defer f1.Stop()
	defer f2.Stop()
	asset := leader.Asset()
	if asset != f1.Asset() || asset == consensus.NativeAsset {
		t.Fatal("guardians disagree on the asset id")
	}

	// Followers scan as their own loops would (EVM chains first, then this
	// chain); a chain that is down is reported, not fatal.
	followersScan := func() {
		for _, f := range []*Service{f1, f2} {
			f.scanEVM(ctx)
			if err := f.scanL1(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Custody needs a little ARG to pay issuance fees: mine it some blocks.
	// A user key receives the uARG and also mines blocks for its own fees.
	custody := cs[0].Address()
	mine(101, custody) // heights 1..101
	priv, _ := btcec.NewPrivateKey()
	user := &wallet.Key{Priv: priv, XOnly: schnorr.SerializePubKey(priv.PubKey())}
	uAddr := user.Address(consensus.RegTest.HRP)
	mine(101, uAddr) // heights 102..202

	// Deposits land on every chain: on A a good USDC one, one below the USDC
	// minimum, one to an address that is not valid here, one in a token the
	// vault does not accept, and a good USDT one; on B two 18-decimal USDT
	// deposits, one of them with more precision than uARG carries, each
	// less B's 1% fee; on C 100 USDC of which the vault keeps 1.
	var depositor, alien [20]byte
	depositor[19], alien[19] = 0xAB, 0xEE
	a1 := evmA.addDeposit(usdcA, depositor, usd(25, 6), uAddr)          // 25 USDC
	a2 := evmA.addDeposit(usdcA, depositor, big.NewInt(500_000), uAddr) // 0.5 USDC
	a3 := evmA.addDeposit(usdcA, depositor, usd(10, 6), "notanaddress")
	a4 := evmA.addDeposit(alien, depositor, usd(10, 6), uAddr)  // not allowlisted
	a5 := evmA.addDeposit(usdtA, depositor, usd(7, 6), uAddr)   // 7 USDT (A)
	b1 := evmB.addDeposit(usdtB, depositor, usd(35, 17), uAddr) // 3.5 USDT (B, 18 decimals): 3.465 net
	b2raw, _ := new(big.Int).SetString("2123456789012345678", 10)
	b2 := evmB.addDeposit(usdtB, depositor, b2raw, uAddr)       // 2.123456789012345678 USDT (B): 2.102222221122222222 net
	c1 := evmC.addDeposit(usdcC, depositor, usd(100, 6), uAddr) // 100 USDC (C): 99 net

	// Seen but not confirmed enough: recorded, nothing issued.
	if err := leader.Once(ctx); err != nil {
		t.Fatalf("once: %v", err)
	}
	want := func(chainID, id uint64, status string, units consensus.Amount, tokenAmount, tokenFee, symbol string) *Inbound {
		t.Helper()
		d, ok := leader.Inbound(chainID, id)
		if !ok || d.Status != status || d.Amount != units || d.TokenAmount != tokenAmount || d.TokenFee != tokenFee || d.TokenSymbol != symbol {
			t.Fatalf("deposit %d#%d: %+v (want %s %d %s fee %s %s)", chainID, id, d, status, units, tokenAmount, tokenFee, symbol)
		}
		return d
	}
	want(chainA.ID, a1, InboundPending, 25*consensus.UnitsPerCoin, "25000000", "0", "USDC")
	want(chainA.ID, a2, InboundUnclaimable, 0, "500000", "0", "USDC")
	want(chainA.ID, a3, InboundUnclaimable, 0, "10000000", "0", "USDC")
	want(chainA.ID, a4, InboundUnclaimable, 0, "10000000", "0", "")
	want(chainA.ID, a5, InboundPending, 7*consensus.UnitsPerCoin, "7000000", "0", "USDT")
	want(chainB.ID, b1, InboundPending, 346_500_000, "3465000000000000000", "35000000000000000", "USDT")
	want(chainB.ID, b2, InboundPending, 210_222_222, "2102222221122222222", "21234567890123456", "USDT")
	want(chainC.ID, c1, InboundPending, 99*consensus.UnitsPerCoin, "99000000", "1000000", "USDC")
	if d, _ := leader.Inbound(chainA.ID, a1); d.Chain != chainA.ID || d.ChainName != "Local A" || d.Token != HexAddress(usdcA) || d.Decimals != 6 || d.To != uAddr {
		t.Fatalf("deposit fields: %+v", d)
	}
	// A deposit of exactly the minimum is paid even though the net is below
	// it: the minimum is measured on what the user deposited.
	cMin := evmC.addDeposit(usdcC, depositor, usd(1, 6), uAddr)
	leader.scanEVM(ctx)
	want(chainC.ID, cMin, InboundPending, 99_000_000, "990000", "10000", "USDC")
	if len(leader.st.Issues()) != 0 {
		t.Fatal("issued too early")
	}
	// Ids are per chain: A#1 and B#1 are different deposits.
	if da, _ := leader.Inbound(chainA.ID, 1); da == nil || da.Chain != chainA.ID {
		t.Fatal("A#1")
	}
	if db, _ := leader.Inbound(chainB.ID, 1); db == nil || db.Chain != chainB.ID || db.TokenSymbol != "USDT" {
		t.Fatal("B#1")
	}

	// Confirm chain B only (2 confirmations): its deposits are issued, A's
	// and C's wait.
	evmB.advance(2)
	followersScan()
	if err := leader.Once(ctx); err != nil {
		t.Fatalf("issue round B: %v", err)
	}
	if d, _ := leader.Inbound(chainB.ID, b1); d.Status != InboundIssued {
		t.Fatalf("B#1 after issue: %+v", d)
	}
	if d, _ := leader.Inbound(chainA.ID, a1); d.Status != InboundPending {
		t.Fatalf("A#1 issued without confirmations: %+v", d)
	}
	if d, _ := leader.Inbound(chainC.ID, c1); d.Status != InboundPending {
		t.Fatalf("C#1 issued without confirmations: %+v", d)
	}
	// Now A and C too: one more issuance pays both A deposits and the C
	// ones, across tokens and chains, each for its net amount.
	evmA.advance(5)
	evmC.advance(2)
	followersScan()
	if err := leader.Once(ctx); err != nil {
		t.Fatalf("issue round A: %v", err)
	}
	d1, _ := leader.Inbound(chainA.ID, a1)
	d5, _ := leader.Inbound(chainA.ID, a5)
	dc, _ := leader.Inbound(chainC.ID, c1)
	if d1.Status != InboundIssued || d5.Status != InboundIssued || dc.Status != InboundIssued || d1.IssueTx == "" || d1.IssueTx != d5.IssueTx || d1.IssueTx != dc.IssueTx {
		t.Fatalf("after issue: %+v %+v %+v", d1, d5, dc)
	}
	issueA := d1.IssueTx
	dB, _ := leader.Inbound(chainB.ID, b1)
	issueB := dB.IssueTx
	if issueA == issueB {
		t.Fatal("A and B were issued in the same batch despite different confirmation depths")
	}
	if !mp.Has(mustHash(t, issueA)) || !mp.Has(mustHash(t, issueB)) {
		t.Fatal("issuance not in mempool")
	}
	// Idempotent while unconfirmed.
	leader.Once(ctx)
	if n := len(leader.st.Issues()); n != 2 {
		t.Fatalf("%d issuances, want 2", n)
	}

	mine(1, custody) // height 203 holds both issuances
	followersScan()
	if err := leader.Once(ctx); err != nil {
		t.Fatalf("confirm round: %v", err)
	}
	for _, ref := range []InboundRef{{chainA.ID, a1}, {chainA.ID, a5}, {chainB.ID, b1}, {chainB.ID, b2}, {chainC.ID, c1}, {chainC.ID, cMin}} {
		d, _ := leader.Inbound(ref.Chain, ref.ID)
		if d.Status != InboundConfirmed {
			t.Fatalf("not confirmed: %+v", d)
		}
		// Followers settled the same deposit from the chain, so none of them
		// would sign it again.
		if fd, _ := f1.Inbound(ref.Chain, ref.ID); fd == nil || fd.Status != InboundConfirmed || fd.IssueTx != d.IssueTx {
			t.Fatalf("follower view: %+v", fd)
		}
	}

	// The uARG coins are on chain, at the user's address, with the asset id.
	bh, _ := client.BlockHash(203)
	blk, err := client.Block(bh)
	if err != nil {
		t.Fatal(err)
	}
	type coin struct {
		txid string
		idx  uint32
		amt  consensus.Amount
	}
	coins := map[consensus.Amount]coin{}
	for _, tx := range blk.Tx {
		for _, o := range tx.Outputs {
			if o.Asset == asset.String() && o.Address == uAddr {
				coins[o.Amount] = coin{tx.TxID, uint32(o.N), o.Amount}
			}
		}
	}
	for _, amt := range []consensus.Amount{25 * consensus.UnitsPerCoin, 7 * consensus.UnitsPerCoin, 346_500_000, 210_222_222, 99 * consensus.UnitsPerCoin, 99_000_000} {
		if _, ok := coins[amt]; !ok {
			t.Fatalf("uARG output of %d units not found", amt)
		}
	}
	st := leader.Status()
	issued := consensus.Amount(25*consensus.UnitsPerCoin + 7*consensus.UnitsPerCoin + 346_500_000 + 210_222_222 + 99*consensus.UnitsPerCoin + 99_000_000)
	if st.Issued != issued || st.Pending != 0 {
		t.Fatalf("status: %+v", st)
	}
	// Reserves per chain and token, and their sum in dollars: A holds
	// 35.5 USDC and 7 USDT, B holds 5.623456789012345678 USDT (the gross:
	// the fee stays in the vault), C holds 101 USDC.
	if len(st.Chains) != 3 || st.Chains[0].Tokens[0].Reserve != "35500000" || st.Chains[0].Tokens[1].Reserve != "7000000" || st.Chains[1].Tokens[0].Reserve != "5623456789012345678" || st.Chains[2].Tokens[0].Reserve != "101000000" {
		t.Fatalf("reserves: %+v", st.Chains)
	}
	if st.Chains[1].Tokens[0].ReserveUnits != 562_345_678 || st.ReserveUnits != 3_550_000_000+700_000_000+562_345_678+10_100_000_000 {
		t.Fatalf("reserve units: %+v", st)
	}
	// Fees per chain, and per token what backs uARG against what the vault
	// has collected: on A everything backs; on B the two deposit fees
	// (0.056234567890123456 USDT) are surplus; on C 1.01 USDC is.
	if st.Chains[0].DepositFeeBps != 0 || st.Chains[0].ReleaseFeeBps != 0 || st.Chains[1].DepositFeeBps != 100 || st.Chains[1].ReleaseFeeBps != 100 || st.Chains[2].DepositFeeBps != 100 || st.Chains[2].ReleaseFeeBps != 100 {
		t.Fatalf("fees: %+v", st.Chains)
	}
	if st.Chains[0].Tokens[0].Obligations != "35500000" || st.Chains[0].Tokens[0].Surplus != "0" ||
		st.Chains[1].Tokens[0].Obligations != "5567222221122222222" || st.Chains[1].Tokens[0].Surplus != "56234567890123456" ||
		st.Chains[2].Tokens[0].Obligations != "99990000" || st.Chains[2].Tokens[0].Surplus != "1010000" {
		t.Fatalf("obligations and surplus: %+v", st.Chains)
	}
	if st.BackingUnits != 3_550_000_000+700_000_000+556_722_222+9_999_000_000 || st.SurplusUnits != 5_623_456+101_000_000 || st.BackingUnits+st.SurplusUnits > st.ReserveUnits {
		t.Fatalf("backing and surplus units: %+v", st)
	}
	if st.Chains[0].Error != "" || st.Chains[1].Error != "" || !st.Chains[0].Tokens[0].Mint || st.Chains[1].Tokens[0].Mint || st.Chains[0].Tokens[1].MinDeposit != "2000000" {
		t.Fatalf("chain status: %+v", st.Chains)
	}

	// Four burns: a BRG1 memo (default: USDC on A, no fee) of 10 uARG, a
	// BRG2 memo for USDT on B of 5 uARG (B pays 4.95 and keeps 0.05), a BRG2
	// memo naming a chain this bridge does not serve, and a BRG2 memo for
	// USDC on C of 50 uARG (C pays 49.5 and keeps 0.5). Each spends one uARG
	// coin, pays some back to the user, and pays its fee from a mined coin.
	var recipient [20]byte
	recipient[19] = 0xCD
	burn := func(c coin, back consensus.Amount, memo []byte, cbHeight uint32) string {
		t.Helper()
		cbBlock, _ := client.Block(mustBlockHash(t, client, cbHeight))
		cb := cbBlock.Tx[0]
		fee := consensus.Amount(5000)
		tx := &consensus.Tx{
			Version: consensus.TxVersionAssets,
			Inputs: []consensus.TxIn{
				{PrevOut: consensus.OutPoint{TxID: mustHash(t, c.txid), Index: c.idx}},
				{PrevOut: consensus.OutPoint{TxID: mustHash(t, cb.TxID), Index: 0}},
			},
			Outputs: []consensus.TxOut{
				{Amount: 0, Type: consensus.OutputTypeNullData, Payload: memo},
				user.Output(cb.Outputs[0].Amount - fee),
			},
			Witnesses: make([][]byte, 2),
		}
		if back > 0 {
			tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: back, Type: consensus.OutputTypeP2PK, Payload: user.XOnly, Asset: asset})
		}
		spent := []consensus.TxOut{
			{Amount: c.amt, Type: consensus.OutputTypeP2PK, Payload: user.XOnly, Asset: asset},
			{Amount: cb.Outputs[0].Amount, Type: consensus.OutputTypeP2PK, Payload: user.XOnly},
		}
		for i := range tx.Inputs {
			if err := user.Sign(consensus.RegTest.ChainTag, tx, spent, i); err != nil {
				t.Fatal(err)
			}
		}
		id, err := client.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
		if err != nil {
			t.Fatalf("broadcast burn: %v", err)
		}
		return id
	}
	burn1 := burn(coins[25*consensus.UnitsPerCoin], 15*consensus.UnitsPerCoin, EncodeMemo(recipient), 102)
	burn2 := burn(coins[7*consensus.UnitsPerCoin], 2*consensus.UnitsPerCoin, EncodeRedeemMemo(chainB.ID, usdtB, recipient), 103)
	burn3 := burn(coins[346_500_000], 300_000_000, EncodeRedeemMemo(99, usdtB, recipient), 104)
	mine(1, custody) // height 204: the fourth burn's coinbase (height 105) matures
	burn4 := burn(coins[99*consensus.UnitsPerCoin], 49*consensus.UnitsPerCoin, EncodeRedeemMemo(chainC.ID, usdcC, recipient), 105)
	mine(1, custody) // height 205 holds the burns
	followersScan()
	if err := leader.Once(ctx); err != nil {
		t.Fatalf("burns seen: %v", err)
	}
	r1, ok := leader.st.GetRedemption(burn1)
	if !ok || r1.Status != RedemptionPending || r1.Amount != 10*consensus.UnitsPerCoin || r1.TokenAmount != "10000000" || r1.TokenFee != "0" || r1.TokenPaid != "10000000" || r1.To != HexAddress(recipient) ||
		r1.Chain != chainA.ID || r1.ChainName != "Local A" || r1.Token != HexAddress(usdcA) || r1.TokenSymbol != "USDC" || r1.Decimals != 6 {
		t.Fatalf("redemption 1: %+v", r1)
	}
	r2, _ := leader.st.GetRedemption(burn2)
	if r2 == nil || r2.Status != RedemptionPending || r2.Amount != 5*consensus.UnitsPerCoin || r2.TokenAmount != "5000000000000000000" || r2.TokenFee != "50000000000000000" || r2.TokenPaid != "4950000000000000000" || r2.Chain != chainB.ID || r2.TokenSymbol != "USDT" || r2.Decimals != 18 {
		t.Fatalf("redemption 2: %+v", r2)
	}
	r3, _ := leader.st.GetRedemption(burn3)
	if r3 == nil || r3.Status != RedemptionUnreleasable || !strings.Contains(r3.Error, "chain 99") || r3.To != HexAddress(recipient) {
		t.Fatalf("redemption 3: %+v", r3)
	}
	r4, _ := leader.st.GetRedemption(burn4)
	if r4 == nil || r4.Status != RedemptionPending || r4.Amount != 50*consensus.UnitsPerCoin || r4.TokenAmount != "50000000" || r4.TokenFee != "500000" || r4.TokenPaid != "49500000" || r4.Chain != chainC.ID || r4.TokenSymbol != "USDC" {
		t.Fatalf("redemption 4: %+v", r4)
	}
	if len(evmA.releases)+len(evmB.releases)+len(evmC.releases) != 0 {
		t.Fatal("released before the burns had enough confirmations")
	}
	// A follower refuses to sign what it cannot verify.
	if _, err := f1.SignReleaseLocally(ctx, "00"); err == nil {
		t.Fatal("follower signed an unknown burn")
	}
	if _, err := f1.SignReleaseLocally(ctx, burn1); err == nil {
		t.Fatal("follower signed an unconfirmed burn")
	}

	mine(2, custody) // height 207: 3 confirmations
	followersScan()
	if _, err := f1.SignReleaseLocally(ctx, burn3); err == nil {
		t.Fatal("follower signed a burn for an unknown chain")
	}
	if err := leader.Once(ctx); err != nil {
		t.Fatalf("release round: %v", err)
	}
	// Every release is called with the gross; the vault pays the gross less
	// its fee and keeps the fee.
	if len(evmA.releases) != 1 || evmA.releases[0].token != usdcA || evmA.releases[0].to != recipient || evmA.releases[0].amount.String() != "10000000" || evmA.releases[0].paid.String() != "10000000" || evmA.releases[0].fee.Sign() != 0 || evmA.releases[0].sigs < 2 || evmA.releases[0].id != burnID32(burn1) {
		t.Fatalf("releases on A: %+v", evmA.releases)
	}
	if len(evmB.releases) != 1 || evmB.releases[0].token != usdtB || evmB.releases[0].to != recipient || evmB.releases[0].amount.String() != "5000000000000000000" || evmB.releases[0].paid.String() != "4950000000000000000" || evmB.releases[0].fee.String() != "50000000000000000" || evmB.releases[0].id != burnID32(burn2) {
		t.Fatalf("releases on B: %+v", evmB.releases)
	}
	if len(evmC.releases) != 1 || evmC.releases[0].token != usdcC || evmC.releases[0].to != recipient || evmC.releases[0].amount.String() != "50000000" || evmC.releases[0].paid.String() != "49500000" || evmC.releases[0].fee.String() != "500000" || evmC.releases[0].sigs < 2 || evmC.releases[0].id != burnID32(burn4) {
		t.Fatalf("releases on C: %+v", evmC.releases)
	}
	leader.Once(ctx)
	for _, id := range []string{burn1, burn2, burn4} {
		r, _ := leader.st.GetRedemption(id)
		if r.Status != RedemptionConfirmed {
			t.Fatalf("after receipt: %s %s", id, r.Status)
		}
	}
	// The confirmed figures were read back from the Released events.
	if evmC.logReads == 0 || evmB.logReads == 0 {
		t.Fatal("release receipts were not read for the fee")
	}
	if r, _ := leader.st.GetRedemption(burn4); r.TokenAmount != "50000000" || r.TokenFee != "500000" || r.TokenPaid != "49500000" || r.EVMTx == "" {
		t.Fatalf("redemption 4 confirmed: %+v", r)
	}
	// Idempotent.
	leader.Once(ctx)
	if len(evmA.releases) != 1 || len(evmB.releases) != 1 || len(evmC.releases) != 1 {
		t.Fatal("double release")
	}
	st = leader.Status()
	if st.Redeemed != 65*consensus.UnitsPerCoin || st.Chains[0].Tokens[0].Reserve != "25500000" || st.Chains[1].Tokens[0].Reserve != "673456789012345678" || st.Chains[2].Tokens[0].Reserve != "51500000" {
		t.Fatalf("final status: %+v", st)
	}
	// The release fees joined the surplus; the burns left the obligations.
	if st.Chains[1].Tokens[0].Obligations != "567222221122222222" || st.Chains[1].Tokens[0].Surplus != "106234567890123456" || st.Chains[2].Tokens[0].Obligations != "49990000" || st.Chains[2].Tokens[0].Surplus != "1510000" {
		t.Fatalf("final obligations and surplus: %+v", st.Chains)
	}
	if n := len(st.Chains[1].Tokens); n != 1 {
		t.Fatalf("chain B tokens: %d", n)
	}

	// Chain B's RPC goes down. A new deposit on A is still issued, B's error
	// shows in its status, and the whole thing recovers when B is back.
	evmB.setFail(true)
	a6 := evmA.addDeposit(usdtA, depositor, usd(4, 6), uAddr)
	evmA.advance(5)
	followersScan()
	err = leader.Once(ctx)
	if err == nil || !strings.Contains(err.Error(), "Local B") {
		t.Fatalf("expected a Local B error, got %v", err)
	}
	if d, _ := leader.Inbound(chainA.ID, a6); d == nil || d.Status != InboundIssued {
		t.Fatalf("A#6 not issued while B is down: %+v", d)
	}
	st = leader.Status()
	if st.Chains[1].Error == "" || st.Chains[0].Error != "" || st.LastError == "" {
		t.Fatalf("chain errors: %+v", st.Chains)
	}
	evmB.setFail(false)
	mine(1, custody)
	followersScan()
	if err := leader.Once(ctx); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if st = leader.Status(); st.Chains[1].Error != "" || st.LastError != "" {
		t.Fatalf("error did not clear: %+v", st.Chains)
	}
	if d, _ := leader.Inbound(chainA.ID, a6); d.Status != InboundConfirmed {
		t.Fatalf("A#6: %+v", d)
	}

	// The public API shapes.
	ws := httptest.NewServer(leader.Handler())
	defer ws.Close()
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := ws.Client().Get(ws.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: no CORS header", path)
		}
		var b strings.Builder
		buf := make([]byte, 1<<16)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	if code, body := get(fmt.Sprintf("/api/deposit/%d/%d", chainB.ID, b1)); code != 200 || !strings.Contains(body, `"chain":"Local B"`) || !strings.Contains(body, `"token_amount":"3465000000000000000"`) || !strings.Contains(body, `"token_fee":"35000000000000000"`) {
		t.Fatalf("deposit by chain and id: %d %s", code, body)
	}
	if code, body := get("/api/deposit/" + fmt.Sprintf("0x%064x", chainA.ID*0x10000+a1)); code != 200 || !strings.Contains(body, `"id":1`) || !strings.Contains(body, `"chain_id":31337`) {
		t.Fatalf("deposit by hash: %d %s", code, body)
	}
	if code, _ := get("/api/deposit/31337/999"); code != 404 {
		t.Fatalf("unknown deposit: %d", code)
	}
	if code, body := get("/api/memo?to=" + HexAddress(recipient)); code != 200 || !strings.Contains(body, `"memo":"`+hex.EncodeToString(EncodeMemo(recipient))+`"`) || !strings.Contains(body, `"token_symbol":"USDC"`) {
		t.Fatalf("memo v1: %d %s", code, body)
	}
	if code, body := get(fmt.Sprintf("/api/memo?to=%s&chain=%d&token=%s", HexAddress(recipient), chainB.ID, HexAddress(usdtB))); code != 200 || !strings.Contains(body, `"memo":"`+hex.EncodeToString(EncodeRedeemMemo(chainB.ID, usdtB, recipient))+`"`) {
		t.Fatalf("memo v2: %d %s", code, body)
	}
	if code, body := get(fmt.Sprintf("/api/memo?to=%s&chain=%d&token=%s", HexAddress(recipient), chainB.ID, HexAddress(usdcA))); code != 400 || !strings.Contains(body, "not accepted") {
		t.Fatalf("memo with a wrong token: %d %s", code, body)
	}
	if code, body := get("/api/status"); code != 200 || !strings.Contains(body, `"chains":[{"name":"Local A"`) || !strings.Contains(body, `"reserve_units":`) || strings.Contains(body, `"token_symbol"`) ||
		!strings.Contains(body, `"deposit_fee_bps":100,"release_fee_bps":100`) || !strings.Contains(body, `"obligations":"49990000","surplus":"1510000"`) || !strings.Contains(body, `"backing_units":`) || !strings.Contains(body, `"surplus_units":`) {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body := get("/api/redemptions"); code != 200 || !strings.Contains(body, `"chain":"Local B"`) || !strings.Contains(body, `"token_amount":"5000000000000000000","token_fee":"50000000000000000","token_paid":"4950000000000000000"`) {
		t.Fatalf("redemptions: %d %s", code, body)
	}
	if code, body := get("/api/deposits"); code != 200 || !strings.Contains(body, `"token_amount":"99000000","token_fee":"1000000"`) {
		t.Fatalf("deposits: %d %s", code, body)
	}
}

func burnID32(txid string) [32]byte { return burnID(txid) }

func mustBlockHash(t *testing.T, c *wallet.Client, h uint32) string {
	t.Helper()
	bh, err := c.BlockHash(h)
	if err != nil {
		t.Fatal(err)
	}
	return bh
}

func mustHash(t *testing.T, s string) consensus.Hash {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad hash %q", s)
	}
	var h consensus.Hash
	copy(h[:], b)
	return h
}
