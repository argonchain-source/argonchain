package consensus

import (
	"bytes"
	"errors"
	"testing"
)

// regtestChain builds a small regtest chain with mature coinbases for tests.
type regtestChain struct {
	p       *Params
	view    MemView
	headers []Header // index = height
	coins   map[OutPoint]Coin
}

func newRegtestChain(t *testing.T) *regtestChain {
	t.Helper()
	c := &regtestChain{p: RegTest, view: MemView{}}
	g := RegTest.Genesis
	ov := NewOverlay(c.view)
	if err := ConnectBlock(c.p, g, 0, ov); err != nil {
		t.Fatalf("genesis connect: %v", err)
	}
	c.view.Apply(ov)
	c.headers = append(c.headers, g.Header)
	return c
}

func (c *regtestChain) tip() (*Header, uint32) {
	h := uint32(len(c.headers) - 1)
	return &c.headers[h], h
}

func (c *regtestChain) mtp() uint64 {
	times := make([]uint64, len(c.headers))
	for i := range c.headers {
		times[i] = c.headers[i].Time
	}
	return MedianTimePast(times)
}

// mine appends a block paying the coinbase to key, carrying txs, and
// validates it end to end (header context + connect).
func (c *regtestChain) mine(t *testing.T, key *testKey, txs []*Tx, fees Amount) *Block {
	t.Helper()
	parent, ph := c.tip()
	height := ph + 1
	cb := coinbaseTx(height, key, Subsidy(height)+fees, []byte("test"))
	all := append([]*Tx{cb}, txs...)
	blk := buildBlock(t, c.p, parent, ph, parent.Time+TargetSpacingU64(), all)
	if err := c.accept(blk); err != nil {
		t.Fatalf("mine at height %d: %v", height, err)
	}
	return blk
}

func TargetSpacingU64() uint64 { return uint64(TargetSpacing) }

// accept runs the full validation path a node would run on a new tip block.
func (c *regtestChain) accept(blk *Block) error {
	parent, ph := c.tip()
	if err := CheckHeaderStateless(c.p, &blk.Header); err != nil {
		return err
	}
	if err := CheckHeaderContext(c.p, &blk.Header, parent.Hash(), parent.Time, ph, c.mtp(), -1); err != nil {
		return err
	}
	ov := NewOverlay(c.view)
	if err := ConnectBlock(c.p, blk, ph+1, ov); err != nil {
		return err
	}
	c.view.Apply(ov)
	c.headers = append(c.headers, blk.Header)
	return nil
}

func (c *regtestChain) spend(t *testing.T, key *testKey, from OutPoint, outs []TxOut) *Tx {
	t.Helper()
	coin, ok := c.view.Lookup(from)
	if !ok {
		t.Fatalf("no coin %s", from)
	}
	tx := &Tx{Version: TxVersion, Inputs: []TxIn{{PrevOut: from}}, Outputs: outs, Witnesses: make([][]byte, 1)}
	key.sign(t, c.p.ChainTag, tx, []TxOut{coin.Out}, 0)
	return tx
}

func TestGenesisOutputsNotInUTXOSet(t *testing.T) {
	c := newRegtestChain(t)
	if len(c.view) != 0 {
		t.Fatal("genesis added coins to the UTXO set")
	}
}

func TestMineAndSpendAfterMaturity(t *testing.T) {
	c := newRegtestChain(t)
	miner := newTestKey(t)
	first := c.mine(t, miner, nil, 0)
	cbOut := OutPoint{TxID: first.Txs[0].TxID(), Index: 0}
	if _, ok := c.view.Lookup(cbOut); !ok {
		t.Fatal("coinbase output missing from UTXO set")
	}

	// Spending at height 2 (immature) must fail.
	recv := newTestKey(t)
	tx := c.spend(t, miner, cbOut, []TxOut{recv.output(49 * UnitsPerCoin)})
	_, _, err := CheckTxInputs(tx, c.view, 2)
	if !errors.Is(err, ErrTxImmatureCoinbase) {
		t.Fatalf("immature spend: %v", err)
	}

	// Mine until height 100 (coinbase from height 1 matures at height 101).
	for c.headersLen() <= 100 {
		c.mine(t, miner, nil, 0)
	}
	// Height now 100; block 101 may spend it.
	_, ph := c.tip()
	if ph != 100 {
		t.Fatalf("height %d", ph)
	}
	fee := Amount(1 * UnitsPerCoin)
	tx = c.spend(t, miner, cbOut, []TxOut{recv.output(50*UnitsPerCoin - fee)})
	c.mine(t, miner, []*Tx{tx}, fee)

	if _, ok := c.view.Lookup(cbOut); ok {
		t.Fatal("spent coin still in set")
	}
	newOut := OutPoint{TxID: tx.TxID(), Index: 0}
	coin, ok := c.view.Lookup(newOut)
	if !ok || coin.Out.Amount != 50*UnitsPerCoin-fee || coin.Coinbase {
		t.Fatal("new coin wrong")
	}

	// Chain of spends inside a single block (tx2 spends tx1's output).
	key3 := newTestKey(t)
	tx1 := c.spend(t, recv, newOut, []TxOut{key3.output(48 * UnitsPerCoin)})
	tx2 := &Tx{Version: TxVersion, Inputs: []TxIn{{PrevOut: OutPoint{TxID: tx1.TxID(), Index: 0}}},
		Outputs: []TxOut{miner.output(47 * UnitsPerCoin)}, Witnesses: make([][]byte, 1)}
	key3.sign(t, c.p.ChainTag, tx2, []TxOut{tx1.Outputs[0]}, 0)
	c.mine(t, miner, []*Tx{tx1, tx2}, 2*UnitsPerCoin)

	// Reversed order must fail: tx2 spends an output created later in the block.
	parent, ph := c.tip()
	cb := coinbaseTx(ph+1, miner, Subsidy(ph+1), nil)
	key4 := newTestKey(t)
	out47 := OutPoint{TxID: tx2.TxID(), Index: 0}
	txA := c.spend(t, miner, out47, []TxOut{key4.output(46 * UnitsPerCoin)})
	txB := &Tx{Version: TxVersion, Inputs: []TxIn{{PrevOut: OutPoint{TxID: txA.TxID(), Index: 0}}},
		Outputs: []TxOut{miner.output(45 * UnitsPerCoin)}, Witnesses: make([][]byte, 1)}
	key4.sign(t, c.p.ChainTag, txB, []TxOut{txA.Outputs[0]}, 0)
	bad := buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb, txB, txA})
	if err := c.accept(bad); !errors.Is(err, ErrTxMissingInput) {
		t.Fatalf("out-of-order spend accepted: %v", err)
	}
}

func (c *regtestChain) headersLen() int { return len(c.headers) }

func TestCoinbaseRules(t *testing.T) {
	c := newRegtestChain(t)
	miner := newTestKey(t)
	parent, ph := c.tip()

	// Overpaying coinbase.
	cb := coinbaseTx(1, miner, Subsidy(1)+1, nil)
	blk := buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb})
	if err := c.accept(blk); !errors.Is(err, ErrBlockBadCoinbaseValue) {
		t.Fatalf("overpay: %v", err)
	}
	// Wrong lock_height (BIP-34 equivalent).
	cb = coinbaseTx(2, miner, Subsidy(1), nil)
	blk = buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb})
	if err := c.accept(blk); !errors.Is(err, ErrTxCoinbaseHeight) {
		t.Fatalf("lock height: %v", err)
	}
	// Tag too long.
	cb = coinbaseTx(1, miner, Subsidy(1), make([]byte, 101))
	if err := CheckTxStateless(cb); !errors.Is(err, ErrTxCoinbaseTagLen) {
		t.Fatalf("tag: %v", err)
	}
	// Two coinbases.
	cb = coinbaseTx(1, miner, Subsidy(1), nil)
	cb2 := coinbaseTx(1, miner, 0, []byte{1})
	blk = buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb, cb2})
	if err := c.accept(blk); !errors.Is(err, ErrBlockExtraCoinbase) {
		t.Fatalf("two coinbases: %v", err)
	}
	// First tx not coinbase.
	fake := &Tx{Version: TxVersion, Inputs: []TxIn{{PrevOut: OutPoint{Index: 0}}}, Outputs: []TxOut{miner.output(1)}, Witnesses: [][]byte{{}}}
	blk = buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{fake})
	if err := c.accept(blk); !errors.Is(err, ErrBlockFirstNotCoinbase) {
		t.Fatalf("no coinbase: %v", err)
	}
	// Underpaying is allowed (burns the difference).
	cb = coinbaseTx(1, miner, 1, nil)
	blk = buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb})
	if err := c.accept(blk); err != nil {
		t.Fatalf("underpay should be fine: %v", err)
	}
}

func TestHeaderContextRules(t *testing.T) {
	c := newRegtestChain(t)
	miner := newTestKey(t)
	for i := 0; i < 12; i++ {
		c.mine(t, miner, nil, 0)
	}
	parent, ph := c.tip()
	cb := coinbaseTx(ph+1, miner, Subsidy(ph+1), nil)

	// Timestamp equal to MTP is too old.
	blk := buildBlock(t, c.p, parent, ph, c.mtp(), []*Tx{cb})
	if err := c.accept(blk); !errors.Is(err, ErrTimeTooOld) {
		t.Fatalf("mtp: %v", err)
	}
	// Timestamp just above MTP is fine even though below parent time.
	blk = buildBlock(t, c.p, parent, ph, c.mtp()+1, []*Tx{cb})
	if err := c.accept(blk); err != nil {
		t.Fatalf("mtp+1: %v", err)
	}
	parent, ph = c.tip()
	cb = coinbaseTx(ph+1, miner, Subsidy(ph+1), nil)

	// Wrong prev hash.
	blk = buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb})
	blk.Header.PrevHash[0] ^= 1
	mineHeader(t, c.p, &blk.Header)
	if err := c.accept(blk); !errors.Is(err, ErrBadPrevHash) {
		t.Fatalf("prev: %v", err)
	}
	// Wrong version.
	blk = buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb})
	blk.Header.Version = 2
	mineHeader(t, c.p, &blk.Header)
	if err := c.accept(blk); !errors.Is(err, ErrBadHeaderVersion) {
		t.Fatalf("version: %v", err)
	}
	// Future timestamp with a clock.
	blk = buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb})
	now := int64(parent.Time)
	if err := CheckHeaderContext(c.p, &blk.Header, parent.Hash(), parent.Time, ph, c.mtp(), now-7200); !errors.Is(err, ErrTimeTooNew) {
		t.Fatalf("future: %v", err)
	}
	if err := CheckHeaderContext(c.p, &blk.Header, parent.Hash(), parent.Time, ph, c.mtp(), now-6600); err != nil {
		t.Fatalf("within window: %v", err)
	}
	// Bits that do not match (regtest: anything but the limit).
	blk.Header.Bits = 0x1F7FFFFF
	mineHeader(t, c.p, &blk.Header)
	if err := c.accept(blk); !errors.Is(err, ErrBadBits) {
		t.Fatalf("bits: %v", err)
	}
	// Bits easier than the limit are rejected by the stateless check.
	blk.Header.Bits = 0x2100ffff // 2^256 - 2^240: canonical, > regtest limit
	if err := CheckHeaderStateless(c.p, &blk.Header); !errors.Is(err, ErrTargetAbovePowLimit) {
		t.Fatalf("above limit: %v", err)
	}
}

func TestMerkleRootsEnforced(t *testing.T) {
	c := newRegtestChain(t)
	miner := newTestKey(t)
	parent, ph := c.tip()
	cb := coinbaseTx(1, miner, Subsidy(1), []byte("a"))
	blk := buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb})
	// Change the witness only: txid root still matches, witness root does not.
	blk.Txs[0].Witnesses[0] = []byte("b")
	ov := NewOverlay(c.view)
	if err := ConnectBlock(c.p, blk, 1, ov); !errors.Is(err, ErrBlockBadWitnessRoot) {
		t.Fatalf("witness root: %v", err)
	}
	// Change the tx itself.
	blk.Txs[0].Witnesses[0] = []byte("a")
	blk.Txs[0].Outputs[0].Amount--
	ov = NewOverlay(c.view)
	if err := ConnectBlock(c.p, blk, 1, ov); !errors.Is(err, ErrBlockBadTxRoot) {
		t.Fatalf("tx root: %v", err)
	}
}

func TestDuplicateInputsRejected(t *testing.T) {
	// CVE-2018-17144 class, within a tx and across a block.
	c := newRegtestChain(t)
	miner := newTestKey(t)
	first := c.mine(t, miner, nil, 0)
	for c.headersLen() <= 100 {
		c.mine(t, miner, nil, 0)
	}
	op := OutPoint{TxID: first.Txs[0].TxID(), Index: 0}
	coin, _ := c.view.Lookup(op)

	// Same outpoint twice in one tx.
	dup := &Tx{Version: TxVersion, Inputs: []TxIn{{PrevOut: op}, {PrevOut: op}},
		Outputs: []TxOut{miner.output(99 * UnitsPerCoin)}, Witnesses: make([][]byte, 2)}
	if err := CheckTxStateless(dup); !errors.Is(err, ErrTxDuplicateInput) {
		t.Fatalf("dup in tx: %v", err)
	}

	// Two txs in one block spending the same outpoint.
	recv := newTestKey(t)
	txA := c.spend(t, miner, op, []TxOut{recv.output(49 * UnitsPerCoin)})
	txB := c.spend(t, miner, op, []TxOut{recv.output(48 * UnitsPerCoin)})
	parent, ph := c.tip()
	cb := coinbaseTx(ph+1, miner, Subsidy(ph+1)+3*UnitsPerCoin, nil)
	blk := buildBlock(t, c.p, parent, ph, parent.Time+600, []*Tx{cb, txA, txB})
	if err := c.accept(blk); !errors.Is(err, ErrBlockDoubleSpend) {
		t.Fatalf("double spend in block: %v", err)
	}
	_ = coin
}

func TestSignatureRules(t *testing.T) {
	c := newRegtestChain(t)
	miner := newTestKey(t)
	first := c.mine(t, miner, nil, 0)
	for c.headersLen() <= 100 {
		c.mine(t, miner, nil, 0)
	}
	op := OutPoint{TxID: first.Txs[0].TxID(), Index: 0}
	coin, _ := c.view.Lookup(op)
	recv := newTestKey(t)
	spent := []TxOut{coin.Out}

	good := c.spend(t, miner, op, []TxOut{recv.output(49 * UnitsPerCoin)})
	if err := VerifyTxSignatures(c.p.ChainTag, good, spent); err != nil {
		t.Fatalf("good sig: %v", err)
	}

	// Wrong key.
	bad := c.spend(t, recv, op, []TxOut{recv.output(49 * UnitsPerCoin)})
	if err := VerifyTxSignatures(c.p.ChainTag, bad, spent); !errors.Is(err, ErrTxBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	// Signature over a different chain tag (replay from another network).
	replay := &Tx{Version: TxVersion, Inputs: good.Inputs, Outputs: good.Outputs, Witnesses: make([][]byte, 1)}
	miner.sign(t, TestNet.ChainTag, replay, spent, 0)
	if err := VerifyTxSignatures(c.p.ChainTag, replay, spent); !errors.Is(err, ErrTxBadSignature) {
		t.Fatalf("replay: %v", err)
	}
	// Outputs changed after signing.
	tampered := &Tx{Version: TxVersion, Inputs: good.Inputs, Outputs: []TxOut{recv.output(49*UnitsPerCoin + 1)}, Witnesses: good.Witnesses}
	if err := VerifyTxSignatures(c.p.ChainTag, tampered, spent); !errors.Is(err, ErrTxBadSignature) {
		t.Fatalf("tamper: %v", err)
	}
	// Spent amount lied about (hardware-wallet fee attack): sighash differs.
	lied := []TxOut{{Amount: coin.Out.Amount + 1, Type: OutputTypeP2PK, Payload: coin.Out.Payload}}
	if err := VerifyTxSignatures(c.p.ChainTag, good, lied); !errors.Is(err, ErrTxBadSignature) {
		t.Fatalf("amount lie: %v", err)
	}
	// Wrong witness length.
	short := &Tx{Version: TxVersion, Inputs: good.Inputs, Outputs: good.Outputs, Witnesses: [][]byte{good.Witnesses[0][:63]}}
	if err := VerifyTxSignatures(c.p.ChainTag, short, spent); !errors.Is(err, ErrTxBadWitnessLen) {
		t.Fatalf("short: %v", err)
	}
	// txid does not depend on the witness (no malleability); wtxid does.
	if good.TxID() != bad.TxID() {
		t.Fatal("txid changed with witness")
	}
	if good.WTxID() == bad.WTxID() {
		t.Fatal("wtxid ignores witness")
	}
	// Invalid x-only key in the spent output makes it unspendable.
	notOnCurve := make([]byte, 32)
	for i := range notOnCurve {
		notOnCurve[i] = 0xff
	}
	badKey := []TxOut{{Amount: coin.Out.Amount, Type: OutputTypeP2PK, Payload: notOnCurve}}
	if err := VerifyTxSignatures(c.p.ChainTag, good, badKey); !errors.Is(err, ErrTxBadPubKey) {
		t.Fatalf("bad key: %v", err)
	}
}

func TestStatelessTxRules(t *testing.T) {
	k := newTestKey(t)
	base := func() *Tx {
		return &Tx{Version: TxVersion, Inputs: []TxIn{{PrevOut: OutPoint{Index: 1}}}, Outputs: []TxOut{k.output(1)}, Witnesses: [][]byte{make([]byte, 64)}}
	}
	if err := CheckTxStateless(base()); err != nil {
		t.Fatalf("base: %v", err)
	}
	cases := map[string]struct {
		mut func(*Tx)
		err error
	}{
		"version":       {func(tx *Tx) { tx.Version = 3 }, ErrTxVersion},
		"no inputs":     {func(tx *Tx) { tx.Inputs = nil; tx.Witnesses = nil }, ErrTxNoInputs},
		"no outputs":    {func(tx *Tx) { tx.Outputs = nil }, ErrTxNoOutputs},
		"witness count": {func(tx *Tx) { tx.Witnesses = nil }, ErrTxWitnessCount},
		"null prevout": {func(tx *Tx) {
			tx.Inputs[0].PrevOut = NullOutPoint
			tx.Inputs = append(tx.Inputs, TxIn{})
			tx.Witnesses = append(tx.Witnesses, nil)
		}, ErrTxNullPrevout},
		"negative out":   {func(tx *Tx) { tx.Outputs[0].Amount = -1 }, ErrAmountOutOfRange},
		"over cap out":   {func(tx *Tx) { tx.Outputs[0].Amount = MaxMoney + 1 }, ErrAmountOutOfRange},
		"sum over cap":   {func(tx *Tx) { tx.Outputs = []TxOut{k.output(MaxMoney), k.output(1)} }, ErrAmountOverflow},
		"unknown type":   {func(tx *Tx) { tx.Outputs[0].Type = 2 }, ErrTxBadOutputType},
		"short pubkey":   {func(tx *Tx) { tx.Outputs[0].Payload = tx.Outputs[0].Payload[:31] }, ErrTxBadPayloadLen},
		"nulldata value": {func(tx *Tx) { tx.Outputs[0] = TxOut{Amount: 1, Type: OutputTypeNullData} }, ErrTxNullDataAmount},
		"nulldata long":  {func(tx *Tx) { tx.Outputs[0] = TxOut{Type: OutputTypeNullData, Payload: make([]byte, 81)} }, ErrTxBadPayloadLen},
		"too many in": {func(tx *Tx) {
			tx.Inputs = make([]TxIn, MaxTxInputs+1)
			for i := range tx.Inputs {
				tx.Inputs[i].PrevOut.Index = uint32(i)
			}
			tx.Witnesses = make([][]byte, MaxTxInputs+1)
		}, ErrTxTooManyInputs},
		"too many out": {func(tx *Tx) { tx.Outputs = make([]TxOut, MaxTxOutputs+1) }, ErrTxTooManyOutputs},
	}
	for name, c := range cases {
		tx := base()
		c.mut(tx)
		if err := CheckTxStateless(tx); !errors.Is(err, c.err) {
			t.Errorf("%s: got %v want %v", name, err, c.err)
		}
	}
	// NULLDATA with amount 0 and <= 80 bytes is fine, alongside a value output.
	tx := base()
	tx.Outputs = append(tx.Outputs, TxOut{Type: OutputTypeNullData, Payload: make([]byte, 80)})
	if err := CheckTxStateless(tx); err != nil {
		t.Fatalf("nulldata ok: %v", err)
	}
}

func TestTxSerializationRoundTrip(t *testing.T) {
	k := newTestKey(t)
	tx := &Tx{Version: 1, LockHeight: 77,
		Inputs:    []TxIn{{PrevOut: OutPoint{TxID: TaggedHash("x", nil), Index: 3}}, {PrevOut: OutPoint{Index: 0xFFFFFFFE}}},
		Outputs:   []TxOut{k.output(123456789), {Type: OutputTypeNullData, Payload: []byte("hello")}},
		Witnesses: [][]byte{make([]byte, 64), {}},
	}
	b := tx.Serialize()
	back, err := DecodeTx(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Serialize(), b) {
		t.Fatal("round trip differs")
	}
	if back.TxID() != tx.TxID() || back.WTxID() != tx.WTxID() {
		t.Fatal("ids differ")
	}
	// Trailing byte rejected.
	if _, err := DecodeTx(append(append([]byte{}, b...), 0)); !errors.Is(err, ErrTrailingBytes) {
		t.Fatalf("trailing: %v", err)
	}
	// Truncation rejected.
	if _, err := DecodeTx(b[:len(b)-1]); !errors.Is(err, ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
	// Oversized payload length rejected before allocation.
	var w Writer
	w.U32(1)
	w.U32(0)
	w.Varint(1)
	w.Hash(Hash{})
	w.U32(0)
	w.Varint(1)
	w.I64(0)
	w.U8(1)
	w.Varint(1 << 30)
	if _, err := DecodeTx(w.Bytes()); !errors.Is(err, ErrLengthExceedsMax) {
		t.Fatalf("payload len: %v", err)
	}
	// Non-minimal varint inside a tx is rejected.
	var w2 Writer
	w2.U32(1)
	w2.U32(0)
	w2.Raw([]byte{0x81, 0x00}) // "1" encoded in two bytes
	if _, err := DecodeTx(w2.Bytes()); !errors.Is(err, ErrVarintNonCanonical) {
		t.Fatalf("non-canonical varint: %v", err)
	}
}

func TestBlockSerializationRoundTrip(t *testing.T) {
	c := newRegtestChain(t)
	miner := newTestKey(t)
	blk := c.mine(t, miner, nil, 0)
	b := blk.Serialize()
	back, err := DecodeBlock(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Serialize(), b) {
		t.Fatal("block round trip differs")
	}
	if back.Header.Hash() != blk.Header.Hash() {
		t.Fatal("hash differs")
	}
	if _, err := DecodeBlock(make([]byte, MaxBlockSize+1)); !errors.Is(err, ErrBlockTooLarge) {
		t.Fatal("oversize block accepted")
	}
	hb := blk.Header.Serialize()
	if len(hb) != HeaderSize {
		t.Fatalf("header size %d", len(hb))
	}
	if _, err := DecodeHeader(hb[:119]); !errors.Is(err, ErrBadHeaderLength) {
		t.Fatal("short header accepted")
	}
}

func TestOverlayUndoSemantics(t *testing.T) {
	base := MemView{}
	op1 := OutPoint{Index: 1}
	base[op1] = Coin{Out: TxOut{Amount: 5, Type: OutputTypeP2PK, Payload: make([]byte, 32)}}
	ov := NewOverlay(base)
	// spend base coin
	c, _ := ov.Lookup(op1)
	ov.Spend(op1, c)
	if _, ok := ov.Lookup(op1); ok {
		t.Fatal("spent coin visible")
	}
	if _, ok := ov.Spent()[op1]; !ok {
		t.Fatal("undo entry missing")
	}
	// add then spend inside the same overlay leaves no trace
	op2 := OutPoint{Index: 2}
	ov.Add(op2, Coin{})
	ov.Spend(op2, Coin{})
	if _, ok := ov.Added()[op2]; ok {
		t.Fatal("intra-block coin lingered")
	}
	if _, ok := ov.Spent()[op2]; ok {
		t.Fatal("intra-block coin produced undo entry")
	}
	// base untouched until Apply
	if _, ok := base[op1]; !ok {
		t.Fatal("base mutated early")
	}
	base.Apply(ov)
	if _, ok := base[op1]; ok {
		t.Fatal("apply did not remove")
	}
}
