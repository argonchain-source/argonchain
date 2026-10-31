// Package names is the rule book of Argonmine names (docs/NAMES.md):
// what a label may look like, what it costs, how an operation is written
// into a transaction's NULLDATA output, and how one operation moves a
// name from one state to the next. The explorer indexes with it and the
// wallets build with it, so the two can never disagree.
package names

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"argonmine/consensus"
	"argonmine/wallet"
)

const (
	// Suffix is what follows a label when people write it: alice.arg.
	Suffix = "arg"
	// Prefix opens every name memo.
	Prefix = "NMS1"

	MinLen        = 3
	MaxLen        = 32
	BlocksPerYear = 52_560 // 10-minute blocks
	GraceBlocks   = 4_320  // 30 days after expiry in which only the owner may renew
	YearsMax      = 5      // per registration or renewal
)

// Operations.
const (
	OpRegister = "register"
	OpRenew    = "renew"
	OpTransfer = "transfer"
	OpPrimary  = "primary"
)

// Reserved labels can be registered only by the treasury itself.
var Reserved = []string{"argonmine", "treasury", "pool", "bridge", "swap", "explorer", "docs", "app", "www", "admin", "support", "team", "official"}

// Prices per year, in whole ARG, by label length; the last entry is for
// every longer label.
var pricesARG = map[int]int64{3: 250, 4: 50, 5: 10}

var (
	ErrLength  = fmt.Errorf("a name is %d to %d characters", MinLen, MaxLen)
	ErrChars   = errors.New("a name uses only lowercase letters, digits and hyphens")
	ErrHyphen  = errors.New("a name cannot start or end with a hyphen")
	ErrUpper   = errors.New("a name is lowercase")
	ErrSuffix  = errors.New("write the name without .arg")
	errNoTreas = errors.New("no treasury on this network")
)

// Normalize turns what a person typed into a label: trimmed, lowercased,
// without a ".arg" suffix. It does not validate.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "."+Suffix)
	return s
}

// Check reports why label is not a valid label, or nil.
func Check(label string) error {
	if strings.HasSuffix(label, "."+Suffix) {
		return ErrSuffix
	}
	if label != strings.ToLower(label) {
		return ErrUpper
	}
	if len(label) < MinLen || len(label) > MaxLen {
		return ErrLength
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(label)-1 {
				return ErrHyphen
			}
		default:
			return ErrChars
		}
	}
	return nil
}

// Valid reports whether label is a valid label.
func Valid(label string) bool { return Check(label) == nil }

// IsReserved reports whether label is one only the treasury may register.
func IsReserved(label string) bool {
	for _, r := range Reserved {
		if r == label {
			return true
		}
	}
	return false
}

// HasTreasury reports whether names cost anything on this network.
func HasTreasury(p *consensus.Params) bool {
	return p != nil && p.TreasuryHeight != 0 && len(p.TreasuryKey) == consensus.P2PKPayloadLen
}

// TreasuryAddress is the address name fees are paid to, or "" on a network
// without a treasury.
func TreasuryAddress(p *consensus.Params) string {
	if !HasTreasury(p) {
		return ""
	}
	a, err := wallet.EncodeAddress(p.HRP, consensus.OutputTypeP2PK, p.TreasuryKey)
	if err != nil {
		return ""
	}
	return a
}

// PricePerYear is what a year of label costs, in units; 0 on a network
// without a treasury. The label is not validated.
func PricePerYear(p *consensus.Params, label string) consensus.Amount {
	if !HasTreasury(p) {
		return 0
	}
	n := len(label)
	if n > 5 {
		n = 5
	}
	if n < MinLen {
		n = MinLen
	}
	return consensus.Amount(pricesARG[n]) * consensus.UnitsPerCoin
}

// Price is what years of label cost, in units.
func Price(p *consensus.Params, label string, years int) consensus.Amount {
	if years < 1 {
		return 0
	}
	return PricePerYear(p, label) * consensus.Amount(years)
}

// Prices lists the price per year by length ("3", "4", "5" for five or
// more), in units.
func Prices(p *consensus.Params) map[string]consensus.Amount {
	out := map[string]consensus.Amount{}
	for n := range pricesARG {
		out[strconv.Itoa(n)] = PricePerYear(p, strings.Repeat("a", n))
	}
	return out
}

// Op is one name operation.
type Op struct {
	Kind  string `json:"op"`
	Label string `json:"name"`
	Years int    `json:"years,omitempty"` // register and renew
}

// Memo is the NULLDATA text of op.
func (o Op) Memo() string {
	switch o.Kind {
	case OpRegister, OpRenew:
		return fmt.Sprintf("%s %s %s %d", Prefix, o.Kind, o.Label, o.Years)
	default:
		return fmt.Sprintf("%s %s %s", Prefix, o.Kind, o.Label)
	}
}

// Parse reads a name operation from NULLDATA text. ok is false when the
// text is not a name memo at all, or is one that breaks the rules (those
// are ignored by the index, so the two cases are the same to a caller).
func Parse(memo []byte) (op Op, ok bool) {
	f := strings.Split(string(memo), " ")
	if len(f) < 3 || f[0] != Prefix {
		return Op{}, false
	}
	op.Kind, op.Label = f[1], f[2]
	if !Valid(op.Label) {
		return Op{}, false
	}
	switch op.Kind {
	case OpRegister, OpRenew:
		if len(f) != 4 {
			return Op{}, false
		}
		y, err := strconv.Atoi(f[3])
		if err != nil || y < 1 || y > YearsMax || strconv.Itoa(y) != f[3] {
			return Op{}, false
		}
		op.Years = y
	case OpTransfer, OpPrimary:
		if len(f) != 3 {
			return Op{}, false
		}
	default:
		return Op{}, false
	}
	return op, true
}

// FindMemo returns the first name operation among a transaction's
// NULLDATA payloads, in output order.
func FindMemo(payloads [][]byte) (Op, bool) {
	for _, p := range payloads {
		if op, ok := Parse(p); ok {
			return op, true
		}
	}
	return Op{}, false
}

// Record is the state of one name.
type Record struct {
	Label      string `json:"name"`
	Owner      string `json:"owner"`
	Registered uint32 `json:"registered_height"` // the block of the registration this ownership started with
	RegTxID    string `json:"txid"`
	Expires    uint32 `json:"expires_height"` // the first block at which the name is expired
	Primary    bool   `json:"primary"`        // the owner chose this name to be shown for their address
}

// Name statuses.
const (
	StatusAvailable  = "available"
	StatusRegistered = "registered"
	StatusGrace      = "grace"
)

// Status is the state of r at height: nil or expired past grace is
// available, in grace is grace, else registered.
func Status(r *Record, height uint32) string {
	switch {
	case r == nil, height >= r.Expires+GraceBlocks:
		return StatusAvailable
	case height >= r.Expires:
		return StatusGrace
	default:
		return StatusRegistered
	}
}

// Active reports whether r resolves at height.
func Active(r *Record, height uint32) bool { return Status(r, height) == StatusRegistered }

// Tx is what Apply needs to know about the transaction carrying an
// operation: who signed it (the address of the coin input 0 spends), the
// address of output 0 (the recipient of a transfer), and what it paid the
// treasury in ARG over all its outputs.
type Tx struct {
	Signer string
	Out0   string
	Paid   consensus.Amount
	TxID   string
}

// Apply works out the state after op at height, from the current record
// (nil when the name has never been registered, or was dropped). ok is
// false when the operation is invalid in this state and changes nothing.
func Apply(p *consensus.Params, cur *Record, op Op, tx Tx, height uint32) (next Record, ok bool) {
	if tx.Signer == "" {
		return Record{}, false
	}
	status := Status(cur, height)
	switch op.Kind {
	case OpRegister:
		if status != StatusAvailable {
			return Record{}, false
		}
		if IsReserved(op.Label) && tx.Signer != TreasuryAddress(p) {
			return Record{}, false
		}
		if tx.Paid < Price(p, op.Label, op.Years) {
			return Record{}, false
		}
		return Record{Label: op.Label, Owner: tx.Signer, Registered: height, RegTxID: tx.TxID, Expires: height + uint32(op.Years)*BlocksPerYear}, true
	case OpRenew:
		if status == StatusAvailable || cur.Owner != tx.Signer {
			return Record{}, false
		}
		if tx.Paid < Price(p, op.Label, op.Years) {
			return Record{}, false
		}
		next = *cur
		from := cur.Expires
		if status == StatusGrace {
			from = height
		}
		next.Expires = from + uint32(op.Years)*BlocksPerYear
		return next, true
	case OpTransfer:
		if status != StatusRegistered || cur.Owner != tx.Signer || tx.Out0 == "" {
			return Record{}, false
		}
		next = *cur
		next.Owner, next.Primary = tx.Out0, false
		return next, true
	case OpPrimary:
		if status != StatusRegistered || cur.Owner != tx.Signer {
			return Record{}, false
		}
		next = *cur
		next.Primary = true
		return next, true
	}
	return Record{}, false
}

// Display is how a label is written for people.
func Display(label string) string { return label + "." + Suffix }
