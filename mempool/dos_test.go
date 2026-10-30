package mempool

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
)

// A copy of a valid transaction with a garbage witness has the same txid.
// Its rejection must not block the genuine transaction (review finding M3).
func TestGarbageWitnessDoesNotBlockGenuineTx(t *testing.T) {
	h := newHarness(t, 101)
	op, out := h.matureCoin(0)
	r := newKey(t)
	good := h.spend(h.k, op, out, r.out(49*coin))
	for _, wit := range [][]byte{make([]byte, 64), bytes.Repeat([]byte{7}, 100), make([]byte, 63)} {
		bad := *good
		bad.Witnesses = [][]byte{wit}
		if bad.TxID() != good.TxID() {
			t.Fatal("txid differs")
		}
		if err := h.pool.Accept(&bad); err == nil {
			t.Fatal("garbage witness accepted")
		}
		if h.pool.RecentlyRejected(good.TxID()) {
			t.Fatal("a witness failure blacklisted the txid for relay")
		}
		// The same bytes are still remembered, so a peer cannot make us
		// verify them over and over.
		if err := h.pool.Accept(&bad); !errors.Is(err, ErrRecentlyRejected) {
			t.Fatalf("identical bad copy re-verified: %v", err)
		}
	}
	if err := h.pool.Accept(good); err != nil {
		t.Fatalf("genuine tx refused after a garbage copy: %v", err)
	}

	// A bigger witness can also fail the fee check of a minimum-fee
	// transaction; that failure is witness-dependent too.
	h.pool = New(h.ch, DefaultConfig())
	tight := h.spend(h.k, op, out, r.out(1))
	fee := consensus.Amount(tight.SerializedSize())
	tight = h.spend(h.k, op, out, r.out(out.Amount-fee))
	fat := *tight
	fat.Witnesses = [][]byte{bytes.Repeat([]byte{1}, 100)}
	if err := h.pool.Accept(&fat); !errors.Is(err, ErrInsufficientFee) {
		t.Fatalf("fat copy: %v", err)
	}
	if err := h.pool.Accept(tight); err != nil {
		t.Fatalf("genuine minimum-fee tx refused after a fat copy: %v", err)
	}

	// Failures that do not depend on the witness are still cached by txid.
	h.pool = New(h.ch, DefaultConfig())
	dust := h.spend(h.k, op, out, r.out(49*coin), r.out(1))
	if err := h.pool.Accept(dust); !errors.Is(err, ErrDust) {
		t.Fatalf("dust: %v", err)
	}
	if !h.pool.RecentlyRejected(dust.TxID()) {
		t.Fatal("witness-independent failure not cached by txid")
	}
}

// A long chain of unconfirmed spends stops at the ancestor limit and block
// template selection stays fast (review finding M4).
func TestChainLimitAndFastSelect(t *testing.T) {
	h := newHarness(t, 102)
	op, out := h.matureCoin(0)
	amt := out.Amount
	var chain []consensus.Hash
	var refused error
	for i := 0; i < 2000; i++ {
		amt -= consensus.Amount(200 + i) // rising fee, so each child outranks its parent
		tx := h.spend(h.k, op, out, h.k.out(amt))
		if err := h.pool.Accept(tx); err != nil {
			refused = err
			break
		}
		chain = append(chain, tx.TxID())
		op, out = consensus.OutPoint{TxID: tx.TxID(), Index: 0}, tx.Outputs[0]
	}
	if len(chain) != MaxChainTxs || !errors.Is(refused, ErrTooLongChain) {
		t.Fatalf("accepted %d of a 2000-long chain (want %d), then: %v", len(chain), MaxChainTxs, refused)
	}
	start := time.Now()
	txs, _ := h.pool.Select(consensus.MaxBlockSize)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("Select took %s", el)
	}
	if len(txs) != MaxChainTxs {
		t.Fatalf("selected %d", len(txs))
	}
	for i, tx := range txs {
		if tx.TxID() != chain[i] {
			t.Fatalf("selection out of chain order at %d", i)
		}
	}
	if ids := h.pool.Ordered(-1); len(ids) != MaxChainTxs || ids[0] != chain[0] || ids[len(ids)-1] != chain[len(chain)-1] {
		t.Fatal("Ordered does not put parents first")
	}

	// Descendants: one parent with many children stops at the limit too.
	h.pool = New(h.ch, DefaultConfig())
	op, out = h.matureCoin(1)
	fan := h.spend(h.k, op, out)
	for i := 0; i < 30; i++ {
		fan.Outputs = append(fan.Outputs, h.k.out(coin))
	}
	fan = h.spend(h.k, op, out, fan.Outputs...)
	if err := h.pool.Accept(fan); err != nil {
		t.Fatal(err)
	}
	kids := 0
	for i := 0; i < 30; i++ {
		kid := h.spend(h.k, consensus.OutPoint{TxID: fan.TxID(), Index: uint32(i)}, fan.Outputs[i], h.k.out(coin-1000))
		if err := h.pool.Accept(kid); err != nil {
			if !errors.Is(err, ErrTooLongChain) {
				t.Fatalf("child %d: %v", i, err)
			}
			break
		}
		kids++
	}
	if kids != MaxChainTxs-1 {
		t.Fatalf("parent took %d children, want %d", kids, MaxChainTxs-1)
	}
}

// Ordering is O(n log n): even a pool far beyond the chain limits (built
// directly, not through Accept) is ordered quickly and correctly.
func TestTopologicalScales(t *testing.T) {
	p := &Pool{txs: map[consensus.Hash]*Entry{}, spends: map[consensus.OutPoint]consensus.Hash{}}
	const n = 20_000
	now := time.Now()
	add := func(i int, fee consensus.Amount, parent *consensus.Hash) consensus.Hash {
		id := consensus.TaggedHash("t", []byte{byte(i), byte(i >> 8), byte(i >> 16), byte(fee)})
		e := &Entry{Tx: &consensus.Tx{}, TxID: id, Fee: fee, Size: 100, Added: now, parents: map[consensus.Hash]struct{}{}, children: map[consensus.Hash]struct{}{}}
		if parent != nil {
			e.parents[*parent] = struct{}{}
			p.txs[*parent].children[id] = struct{}{}
		}
		p.txs[id] = e
		p.bytes += e.Size
		return id
	}
	var prev *consensus.Hash
	pos := map[consensus.Hash]int{}
	var chainIDs []consensus.Hash
	for i := 0; i < n; i++ {
		id := add(i, consensus.Amount(100+i), prev) // children pay more than parents
		chainIDs = append(chainIDs, id)
		prev = &chainIDs[len(chainIDs)-1]
	}
	for i := 0; i < n; i++ {
		add(n+i, consensus.Amount(50+i%100), nil)
	}
	start := time.Now()
	order := p.topological()
	t.Logf("ordered %d entries in %s", 2*n, time.Since(start))
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("ordering %d entries took %s", 2*n, el)
	}
	if len(order) != 2*n {
		t.Fatalf("ordered %d of %d", len(order), 2*n)
	}
	for i, e := range order {
		pos[e.TxID] = i
	}
	for i := 1; i < n; i++ {
		if pos[chainIDs[i-1]] > pos[chainIDs[i]] {
			t.Fatal("child before parent")
		}
	}
	start = time.Now()
	txs, _ := p.Select(consensus.MaxBlockSize)
	if el := time.Since(start); el > 2*time.Second || len(txs) == 0 {
		t.Fatalf("Select: %d txs in %s", len(txs), el)
	}

	// Announcement order for a capped count: the best-paying entries with
	// every in-pool parent before its child.
	start = time.Now()
	ids := p.Ordered(1000)
	t.Logf("top 1000 of %d in %s", 2*n, time.Since(start))
	if len(ids) != 1000 {
		t.Fatalf("Ordered(1000) gave %d", len(ids))
	}
	at := map[consensus.Hash]int{}
	for i, id := range ids {
		at[id] = i
	}
	for i, id := range ids {
		for parent := range p.txs[id].parents {
			if j, ok := at[parent]; !ok || j > i {
				t.Fatalf("entry %d announced without its parent first", i)
			}
		}
	}
}

// When a block confirms a parent but not its child, the child stays.
func TestChildSurvivesParentConfirmation(t *testing.T) {
	h := newHarness(t, 101)
	op, out := h.matureCoin(0)
	a := newKey(t)
	parent := h.spend(h.k, op, out, a.out(49*coin))
	child := h.spend(a, consensus.OutPoint{TxID: parent.TxID()}, parent.Outputs[0], a.out(48*coin))
	for _, tx := range []*consensus.Tx{parent, child} {
		if err := h.pool.Accept(tx); err != nil {
			t.Fatal(err)
		}
	}
	h.mine([]*consensus.Tx{parent}, coin)
	if h.pool.Has(parent.TxID()) || !h.pool.Has(child.TxID()) {
		t.Fatalf("after confirming the parent: parent in pool %v, child in pool %v", h.pool.Has(parent.TxID()), h.pool.Has(child.TxID()))
	}
	sel, fees := h.pool.Select(consensus.MaxBlockSize)
	if len(sel) != 1 || sel[0].TxID() != child.TxID() {
		t.Fatal("child not selected")
	}
	h.mine(sel, fees)
	if h.pool.Count() != 0 {
		t.Fatal("pool not cleared")
	}
}

// Making room for a child never evicts the child's own parent.
func TestEvictionKeepsAncestors(t *testing.T) {
	h := newHarness(t, 102)
	r := newKey(t)
	op0, out0 := h.matureCoin(0)
	op1, out1 := h.matureCoin(1)
	parent := h.spend(h.k, op0, out0, r.out(50*coin-1000))                                                    // about 6 units/byte
	child := h.spend(r, consensus.OutPoint{TxID: parent.TxID()}, parent.Outputs[0], r.out(50*coin-1000-coin)) // far more
	cheap := h.spend(h.k, op1, out1, r.out(50*coin-300))                                                      // about 2 units/byte
	for _, withOther := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.MaxBytes = parent.SerializedSize() + child.SerializedSize() - 1
		if withOther {
			cfg.MaxBytes += cheap.SerializedSize()
		}
		h.pool = New(h.ch, cfg)
		if err := h.pool.Accept(parent); err != nil {
			t.Fatal(err)
		}
		if withOther {
			if err := h.pool.Accept(cheap); err != nil {
				t.Fatal(err)
			}
		}
		err := h.pool.Accept(child)
		if !h.pool.Has(parent.TxID()) {
			t.Fatalf("withOther=%v: parent evicted to make room for its child (%v)", withOther, err)
		}
		if withOther && err != nil {
			t.Fatalf("child refused although an unrelated cheaper tx could go: %v", err)
		}
		if !withOther && !errors.Is(err, ErrPoolFull) {
			t.Fatalf("child: %v", err)
		}
		sel, _ := h.pool.Select(consensus.MaxBlockSize)
		seen := map[consensus.Hash]bool{}
		for _, tx := range sel {
			for _, in := range tx.Inputs {
				if h.pool.Has(in.PrevOut.TxID) && !seen[in.PrevOut.TxID] {
					t.Fatal("template spends a pool tx before it")
				}
			}
			seen[tx.TxID()] = true
		}
	}
}

// Asset outputs beyond FreeAssetOutputs pay AssetOutputFee each (review
// finding L3; relay policy only).
func TestAssetOutputSurcharge(t *testing.T) {
	h := newHarness(t, 102)
	r := newKey(t)
	var issuer [32]byte
	copy(issuer[:], h.k.xonly)
	asset := consensus.AssetID(issuer, "SPAM")
	build := func(i, nAsset int, extra consensus.Amount) *consensus.Tx {
		op, out := h.matureCoin(i)
		tx := &consensus.Tx{Version: consensus.TxVersionAssets, Inputs: []consensus.TxIn{{PrevOut: op}},
			Issuances: []consensus.Issuance{{Asset: asset, Issuer: issuer, Symbol: "SPAM", Amount: consensus.Amount(nAsset)}}}
		for j := 0; j < nAsset; j++ {
			tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: 1, Type: consensus.OutputTypeP2PK, Payload: r.xonly, Asset: asset})
		}
		tx.Outputs = append(tx.Outputs, h.k.out(1))
		tx.Witnesses = [][]byte{make([]byte, 64), make([]byte, 64)}
		tx.Outputs[nAsset].Amount = out.Amount - consensus.Amount(tx.SerializedSize()) - extra
		spent := []consensus.TxOut{out}
		for k, idx := range []uint32{0, consensus.IssuanceSigIndex} {
			msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, spent, idx)
			sig, err := schnorr.Sign(h.k.priv, msg[:])
			if err != nil {
				t.Fatal(err)
			}
			tx.Witnesses[k] = sig.Serialize()
		}
		return tx
	}
	cfg := DefaultConfig()
	// Two asset outputs (a payment and its change) pay only for their bytes.
	if err := h.pool.Accept(build(0, FreeAssetOutputs, 0)); err != nil {
		t.Fatalf("payment-shaped asset tx: %v", err)
	}
	h.pool = New(h.ch, cfg)
	spam := build(0, 10, 0)
	if err := h.pool.Accept(spam); !errors.Is(err, ErrInsufficientFee) {
		t.Fatalf("10 asset outputs at the byte fee: %v", err)
	}
	want := MinRelayFee(cfg, spam) - consensus.Amount(spam.SerializedSize())
	if want != 8*cfg.AssetOutputFee {
		t.Fatalf("surcharge %d", want)
	}
	if err := h.pool.Accept(build(1, 10, want)); err != nil {
		t.Fatalf("10 asset outputs paying the surcharge: %v", err)
	}
}
