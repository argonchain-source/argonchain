package consensus

// Coin is an unspent transaction output together with the context needed to
// validate a spend of it.
type Coin struct {
	Out      TxOut
	Height   uint32 // height of the block that created it
	Coinbase bool   // subject to CoinbaseMaturity
}

// UTXOView is a read-only view of the unspent output set.
type UTXOView interface {
	// Lookup returns the coin and true if the outpoint is unspent.
	Lookup(op OutPoint) (Coin, bool)
}

// MemView is an in-memory UTXO set. Used for tests and as the base of
// regtest chains; the node's persistent set lives in the chain package.
type MemView map[OutPoint]Coin

func (m MemView) Lookup(op OutPoint) (Coin, bool) {
	c, ok := m[op]
	return c, ok
}

// Apply writes an overlay's changes into the map.
func (m MemView) Apply(o *Overlay) {
	for op := range o.spent {
		delete(m, op)
	}
	for op, c := range o.added {
		m[op] = c
	}
}

// Overlay is a copy-on-write layer over a base view. ConnectBlock works on an
// overlay so that a block that fails validation half-way leaves the base
// untouched, and so that undo information falls out for free.
type Overlay struct {
	base  UTXOView
	added map[OutPoint]Coin
	spent map[OutPoint]Coin // coins removed from base (or from added) with their data, for undo
}

// NewOverlay creates an empty overlay over base.
func NewOverlay(base UTXOView) *Overlay {
	return &Overlay{base: base, added: map[OutPoint]Coin{}, spent: map[OutPoint]Coin{}}
}

func (o *Overlay) Lookup(op OutPoint) (Coin, bool) {
	if c, ok := o.added[op]; ok {
		return c, true
	}
	if _, gone := o.spent[op]; gone {
		return Coin{}, false
	}
	return o.base.Lookup(op)
}

// Spend removes op. The caller must have confirmed it exists via Lookup.
func (o *Overlay) Spend(op OutPoint, c Coin) {
	if _, ok := o.added[op]; ok {
		// Created and spent inside the same block: no undo entry is needed
		// because disconnecting the block removes the creation as well.
		delete(o.added, op)
		return
	}
	o.spent[op] = c
}

// Add inserts a new coin.
func (o *Overlay) Add(op OutPoint, c Coin) { o.added[op] = c }

// Spent returns the coins removed from the base view (undo data).
func (o *Overlay) Spent() map[OutPoint]Coin { return o.spent }

// Added returns the coins created on top of the base view.
func (o *Overlay) Added() map[OutPoint]Coin { return o.added }
