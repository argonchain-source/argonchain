package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"

	"argonmine/consensus"
)

// The bridge accepts dollar stablecoins (USDC, USDT) on several EVM chains:
// one vault per chain, holding one or more allowlisted tokens. uARG is a
// dollar with 8 decimals whatever the token's decimals; the conversions are
// UnitsFromToken and TokenFromUnits below. The chain set is a JSON file
// passed with -chains (docs/BRIDGE.md, "Configuration").

// Token is one stablecoin the vault of a chain accepts.
type Token struct {
	Symbol     string
	Address    [20]byte
	Decimals   int
	Mint       bool     // a test mock with a public mint(address,uint256): the page offers a faucet
	MinDeposit *big.Int // smallest deposit paid out, in token units (default 1 token)
	// MinRelease is the smallest payout (gross, in token units) a release is
	// sent for; a redemption below it is held as unreleasable, so dust paid
	// to swap addresses cannot make the gas key pay one release transaction
	// per output (docs/BRIDGE.md, "Minimum withdrawal"). Default MinDeposit,
	// or one token when that is 0.
	MinRelease *big.Int
	// MaxDeposit is the largest deposit (gross, in token units) issued
	// without a human; a larger one is held with the reason and its funds
	// stay in the vault (docs/BRIDGE.md, "Per-deposit cap"). nil: no cap.
	MaxDeposit *big.Int
}

// Finality modes: how a chain's final block is found.
const (
	// FinalityFinalized: the block the node calls "finalized" (the
	// eth_getBlockByNumber("finalized") tag), and at least Confirmations deep.
	FinalityFinalized = "finalized"
	// FinalityDepth: latest − Confirmations.
	FinalityDepth = "depth"
)

// Chain is one EVM chain with its vault and tokens.
type Chain struct {
	Name string
	ID   uint64
	RPC  string // the first RPC provider, what the daemon sends transactions through
	// RPCs are every provider the daemon reads the chain from, RPC first. A
	// deposit is acted on only when all of them agree on its block and its
	// event (docs/BRIDGE.md, "Finality and providers").
	RPCs []string
	// PublicRPC is what the page hands to browser wallets
	// (wallet_addEthereumChain) and what /api/status shows. It never
	// defaults to RPC, which may carry an API key: empty means none is shown.
	PublicRPC     string
	Explorer      string
	NativeSymbol  string
	Vault         [20]byte
	StartBlock    uint64 // the vault's deployment block; public RPCs prune old logs
	Confirmations uint64 // blocks a deposit's block must be below the head before it is final (and below the finalized block in FinalityFinalized)
	Finality      string // FinalityFinalized or FinalityDepth
	// MaxGasPrice caps the gas price (legacy) or fee cap (EIP-1559) of the
	// release transactions the gas key sends, in wei, bumps included; nil
	// for none. MinGasBalance is the gas key balance (wei) below which the
	// status warns.
	MaxGasPrice   *big.Int
	MinGasBalance *big.Int
	Tokens        []Token
	// Router is the chain's DepositRouter (contracts/DepositRouter.sol), or
	// zero when deposit addresses are off on this chain; RouterBlock is its
	// deployment block, where the leader starts looking for transfers into
	// deposit addresses (docs/BRIDGE.md, "Deposit addresses").
	Router      [20]byte
	RouterBlock uint64
	// L1StartHeight is the first Argonmine height whose burns the chain's
	// current vault pays; burns below it naming this chain are never paid
	// from it. Set it to the current height before replacing a vault
	// (docs/BRIDGE.md, "Replacing a vault").
	L1StartHeight uint32
}

// HasRouter reports whether deposit addresses are configured on the chain.
func (c *Chain) HasRouter() bool { return c.Router != [20]byte{} }

// Token finds a token of the chain by address.
func (c *Chain) Token(addr [20]byte) (*Token, bool) {
	for i := range c.Tokens {
		if c.Tokens[i].Address == addr {
			return &c.Tokens[i], true
		}
	}
	return nil, false
}

// chainsFile is the JSON document on disk.
type chainsFile struct {
	Chains []chainJSON `json:"chains"`
}

type chainJSON struct {
	Name          string      `json:"name"`
	ChainID       uint64      `json:"chain_id"`
	RPC           string      `json:"rpc,omitempty"`
	RPCs          []string    `json:"rpcs,omitempty"`               // optional: more providers; deposits need all of them to agree
	PublicRPC     string      `json:"public_rpc,omitempty"`         // optional: shown to browsers and in the status; never defaults to rpc
	Finality      string      `json:"finality,omitempty"`           // "finalized" or "depth"; default by chain id
	MaxGasGwei    json.Number `json:"max_gas_price_gwei,omitempty"` // cap on release gas prices, gwei (default by chain id)
	MinGasBalance string      `json:"min_gas_balance,omitempty"`    // gas key balance warning threshold, in the native coin ("0.01")
	Explorer      string      `json:"explorer"`
	NativeSymbol  string      `json:"native_symbol"`
	Vault         string      `json:"vault"`
	StartBlock    uint64      `json:"start_block"`
	Confirmations uint64      `json:"confirmations"`
	Tokens        []tokenJSON `json:"tokens"`
	Router        string      `json:"router,omitempty"`          // optional: the DepositRouter; none = no deposit addresses on this chain
	RouterBlock   uint64      `json:"router_block,omitempty"`    // its deployment block (default start_block)
	L1StartHeight uint32      `json:"l1_start_height,omitempty"` // first Argonmine height whose burns this vault pays (default 0)
}

type tokenJSON struct {
	Symbol     string `json:"symbol"`
	Address    string `json:"address"`
	Decimals   int    `json:"decimals"`
	Mint       bool   `json:"mint"`
	MinDeposit string `json:"min_deposit,omitempty"` // token units, decimal; default 10^decimals
	MinRelease string `json:"min_release,omitempty"` // token units, decimal, gross; default min_deposit (one token when that is 0)
	MaxDeposit string `json:"max_deposit,omitempty"` // token units, decimal; default none
}

// maxProviders bounds the RPC providers of one chain.
const maxProviders = 4

// defaultFinality is the finality mode of a chain when the file names none:
// the finalized tag on the chains known to serve it, depth elsewhere
// (a local anvil serves the tag too, 64 blocks behind; depth suits it).
func defaultFinality(chainID uint64) string {
	switch chainID {
	case 1, 11155111, 17000, 560048, // Ethereum, Sepolia, Holesky, Hoodi
		8453, 84532, // Base, Base Sepolia
		10, 11155420, // OP Mainnet, OP Sepolia
		42161, 421614, // Arbitrum One, Arbitrum Sepolia
		56, 97, // BNB Chain and its testnet (fast finality)
		137, 80002: // Polygon PoS, Amoy (milestones)
		return FinalityFinalized
	}
	return FinalityDepth
}

// defaultMaxGasGwei caps a release's gas price where the file does not:
// generous for each chain's usual prices, so a spike waits instead of
// draining the gas key.
func defaultMaxGasGwei(chainID uint64) int64 {
	switch chainID {
	case 1, 11155111, 17000, 560048:
		return 300
	case 56, 97:
		return 30
	case 137, 80002:
		return 3000
	case 8453, 84532, 10, 11155420, 42161, 421614:
		return 20
	}
	return 500
}

// defaultMinGasWei is the gas key balance below which the status warns.
func defaultMinGasWei(chainID uint64) *big.Int {
	switch chainID {
	case 137, 80002:
		return pow10(18) // 1 POL
	}
	return pow10(16) // 0.01 ETH or BNB
}

// UsesLegacyTx reports whether release transactions on a chain are sent as
// type-0 (EIP-155) transactions with a gas price: BNB Chain, whose
// validators order by gas price. Elsewhere they are EIP-1559 transactions
// when the chain has a base fee.
func UsesLegacyTx(chainID uint64) bool { return chainID == 56 || chainID == 97 }

// parseDecimalUnits parses a non-negative decimal ("0.5", "30") into an
// integer with the given number of decimals.
func parseDecimalUnits(s string, decimals int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return nil, errors.New("not a non-negative number")
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	if len(frac) > decimals || (whole == "" && frac == "") {
		return nil, fmt.Errorf("at most %d decimals", decimals)
	}
	for len(frac) < decimals {
		frac += "0"
	}
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return new(big.Int), nil
	}
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, errors.New("not a number")
	}
	return v, nil
}

// LoadChains reads and validates a chains file.
func LoadChains(path string) ([]Chain, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	chains, err := ParseChains(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return chains, nil
}

// ParseChains parses and validates the chains document: at least one chain
// with one token, unique chain ids, unique token addresses per chain,
// decimals 0..18, valid addresses. The first chain's first token is the
// default a BRG1 memo pays out.
func ParseChains(data []byte) ([]Chain, error) {
	var f chainsFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("chains: %w", err)
	}
	if len(f.Chains) == 0 {
		return nil, errors.New("chains: at least one chain is required")
	}
	seenChain := map[uint64]bool{}
	var out []Chain
	for i, cj := range f.Chains {
		where := fmt.Sprintf("chains[%d]", i)
		if cj.Name != "" {
			where = fmt.Sprintf("chain %q", cj.Name)
		}
		if cj.ChainID == 0 {
			return nil, fmt.Errorf("%s: chain_id is required", where)
		}
		if seenChain[cj.ChainID] {
			return nil, fmt.Errorf("%s: chain id %d listed twice", where, cj.ChainID)
		}
		seenChain[cj.ChainID] = true
		if strings.TrimSpace(cj.Name) == "" {
			return nil, fmt.Errorf("%s: name is required", where)
		}
		var rpcs []string
		seenRPC := map[string]bool{}
		for _, u := range append([]string{cj.RPC}, cj.RPCs...) {
			if u = strings.TrimSpace(u); u != "" && !seenRPC[u] {
				seenRPC[u] = true
				rpcs = append(rpcs, u)
			}
		}
		if len(rpcs) == 0 {
			return nil, fmt.Errorf("%s: rpc (or rpcs) is required", where)
		}
		if len(rpcs) > maxProviders {
			return nil, fmt.Errorf("%s: at most %d RPC providers", where, maxProviders)
		}
		vault, err := ParseAddress(cj.Vault)
		if err != nil || vault == ([20]byte{}) {
			return nil, fmt.Errorf("%s: vault must be a 0x address", where)
		}
		if len(cj.Tokens) == 0 {
			return nil, fmt.Errorf("%s: at least one token is required", where)
		}
		c := Chain{Name: strings.TrimSpace(cj.Name), ID: cj.ChainID, RPC: rpcs[0], RPCs: rpcs, PublicRPC: strings.TrimSpace(cj.PublicRPC),
			Explorer: strings.TrimRight(strings.TrimSpace(cj.Explorer), "/"), NativeSymbol: strings.TrimSpace(cj.NativeSymbol), Vault: vault, StartBlock: cj.StartBlock, Confirmations: cj.Confirmations, L1StartHeight: cj.L1StartHeight}
		if c.NativeSymbol == "" {
			c.NativeSymbol = "ETH"
		}
		if c.Confirmations == 0 {
			c.Confirmations = 20
		}
		switch f := strings.TrimSpace(cj.Finality); f {
		case "":
			c.Finality = defaultFinality(c.ID)
		case FinalityFinalized, FinalityDepth:
			c.Finality = f
		default:
			return nil, fmt.Errorf("%s: finality must be %q or %q", where, FinalityFinalized, FinalityDepth)
		}
		if g := strings.TrimSpace(cj.MaxGasGwei.String()); g != "" {
			v, err := parseDecimalUnits(g, 9)
			if err != nil || v.Sign() == 0 {
				return nil, fmt.Errorf("%s: max_gas_price_gwei must be a positive number of gwei", where)
			}
			c.MaxGasPrice = v
		} else {
			c.MaxGasPrice = new(big.Int).Mul(big.NewInt(defaultMaxGasGwei(c.ID)), pow10(9))
		}
		if m := strings.TrimSpace(cj.MinGasBalance); m != "" {
			v, err := parseDecimalUnits(m, 18)
			if err != nil {
				return nil, fmt.Errorf("%s: min_gas_balance must be an amount of %s", where, c.NativeSymbol)
			}
			c.MinGasBalance = v
		} else {
			c.MinGasBalance = defaultMinGasWei(c.ID)
		}
		if r := strings.TrimSpace(cj.Router); r != "" {
			router, err := ParseAddress(r)
			if err != nil || router == ([20]byte{}) {
				return nil, fmt.Errorf("%s: router must be a 0x address", where)
			}
			if router == vault {
				return nil, fmt.Errorf("%s: router must be the DepositRouter, not the vault", where)
			}
			c.Router, c.RouterBlock = router, cj.RouterBlock
			if c.RouterBlock == 0 {
				c.RouterBlock = c.StartBlock
			}
		} else if cj.RouterBlock != 0 {
			return nil, fmt.Errorf("%s: router_block without router", where)
		}
		seenToken := map[[20]byte]bool{}
		for j, tj := range cj.Tokens {
			tw := fmt.Sprintf("%s token %d", where, j)
			if tj.Symbol != "" {
				tw = fmt.Sprintf("%s %s", where, tj.Symbol)
			}
			if strings.TrimSpace(tj.Symbol) == "" {
				return nil, fmt.Errorf("%s: symbol is required", tw)
			}
			addr, err := ParseAddress(tj.Address)
			if err != nil || addr == ([20]byte{}) {
				return nil, fmt.Errorf("%s: address must be a 0x address", tw)
			}
			if seenToken[addr] {
				return nil, fmt.Errorf("%s: token %s listed twice", tw, HexAddress(addr))
			}
			seenToken[addr] = true
			if tj.Decimals < 0 || tj.Decimals > 18 {
				return nil, fmt.Errorf("%s: decimals must be 0..18", tw)
			}
			t := Token{Symbol: strings.TrimSpace(tj.Symbol), Address: addr, Decimals: tj.Decimals, Mint: tj.Mint, MinDeposit: pow10(tj.Decimals)}
			if s := strings.TrimSpace(tj.MinDeposit); s != "" {
				v, ok := new(big.Int).SetString(s, 10)
				if !ok || v.Sign() < 0 {
					return nil, fmt.Errorf("%s: min_deposit must be a non-negative integer in token units", tw)
				}
				t.MinDeposit = v
			}
			t.MinRelease = t.MinDeposit
			if t.MinRelease.Sign() == 0 {
				t.MinRelease = pow10(tj.Decimals)
			}
			if s := strings.TrimSpace(tj.MinRelease); s != "" {
				v, ok := new(big.Int).SetString(s, 10)
				if !ok || v.Sign() <= 0 {
					return nil, fmt.Errorf("%s: min_release must be a positive integer in token units", tw)
				}
				t.MinRelease = v
			}
			if s := strings.TrimSpace(tj.MaxDeposit); s != "" {
				v, ok := new(big.Int).SetString(s, 10)
				if !ok || v.Sign() <= 0 {
					return nil, fmt.Errorf("%s: max_deposit must be a positive integer in token units", tw)
				}
				if v.Cmp(t.MinDeposit) < 0 {
					return nil, fmt.Errorf("%s: max_deposit is below min_deposit", tw)
				}
				t.MaxDeposit = v
			}
			c.Tokens = append(c.Tokens, t)
		}
		out = append(out, c)
	}
	return out, nil
}

// ---- amount math: token units <-> uARG units (8-decimal dollars) ----

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// UnitsFromToken converts a token amount with the given decimals into uARG
// units: units = amount × 10^8 / 10^decimals, rounded down when the token
// has more than 8 decimals (the remainder stays in the vault). ok is false
// when the result is zero or exceeds what the chain can hold in one output.
func UnitsFromToken(amount *big.Int, decimals int) (consensus.Amount, bool) {
	if amount == nil || amount.Sign() <= 0 {
		return 0, false
	}
	var u *big.Int
	if decimals > 8 {
		u = new(big.Int).Quo(amount, pow10(decimals-8))
	} else {
		u = new(big.Int).Mul(amount, pow10(8-decimals))
	}
	if !u.IsInt64() || !consensus.AssetRange(consensus.Amount(u.Int64())) {
		return 0, false
	}
	return consensus.Amount(u.Int64()), true
}

// TokenFromUnits converts uARG units into a token amount with the given
// decimals: amount = units × 10^decimals / 10^8, rounded down when the token
// has fewer than 8 decimals (a 6-decimal token cannot pay fractions of a
// cent; that dust was burned and is not paid). The result may be zero.
func TokenFromUnits(units consensus.Amount, decimals int) *big.Int {
	if units <= 0 {
		return new(big.Int)
	}
	u := new(big.Int).SetInt64(int64(units))
	if decimals >= 8 {
		return u.Mul(u, pow10(decimals-8))
	}
	return u.Quo(u, pow10(8-decimals))
}

// FormatBigTokenAmount renders token units (any size) as a decimal string.
func FormatBigTokenAmount(units *big.Int, decimals int) string {
	if units == nil {
		return "0"
	}
	s := units.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for len(s) <= decimals {
		s = "0" + s
	}
	if decimals > 0 {
		s = strings.TrimRight(strings.TrimRight(s[:len(s)-decimals]+"."+s[len(s)-decimals:], "0"), ".")
	}
	if neg {
		s = "-" + s
	}
	return s
}
