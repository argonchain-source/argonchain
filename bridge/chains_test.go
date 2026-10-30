package bridge

import (
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"argonmine/consensus"
)

const testChainsJSON = `{"chains":[
  {"name":"Local A","chain_id":31337,"rpc":"http://a","explorer":"https://a.example","native_symbol":"ETH","vault":"0x0000000000000000000000000000000000000077","start_block":0,"confirmations":5,
   "tokens":[{"symbol":"USDC","address":"0x0000000000000000000000000000000000000055","decimals":6,"mint":true},
             {"symbol":"USDT","address":"0x0000000000000000000000000000000000000056","decimals":6,"mint":true,"min_deposit":"2000000"}]},
  {"name":"Local B","chain_id":31338,"rpc":"http://b","native_symbol":"BNB","vault":"0x0000000000000000000000000000000000000088","confirmations":2,
   "tokens":[{"symbol":"USDT","address":"0x0000000000000000000000000000000000000066","decimals":18}]},
  {"name":"Local C","chain_id":31339,"rpc":"http://c","vault":"0x0000000000000000000000000000000000000099","confirmations":2,
   "tokens":[{"symbol":"USDC","address":"0x0000000000000000000000000000000000000057","decimals":6}]}
]}`

func TestParseChains(t *testing.T) {
	chains, err := ParseChains([]byte(testChainsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 3 || chains[0].ID != 31337 || chains[1].ID != 31338 || chains[2].ID != 31339 || len(chains[0].Tokens) != 2 || len(chains[1].Tokens) != 1 || len(chains[2].Tokens) != 1 {
		t.Fatalf("parsed %+v", chains)
	}
	a := chains[0]
	// public_rpc never defaults to rpc, which may carry an API key.
	if a.Name != "Local A" || a.Confirmations != 5 || a.NativeSymbol != "ETH" || a.PublicRPC != "" || a.RPC != "http://a" || len(a.RPCs) != 1 || HexAddress(a.Vault) != "0x0000000000000000000000000000000000000077" {
		t.Fatalf("chain A: %+v", a)
	}
	if a.Tokens[0].Symbol != "USDC" || a.Tokens[0].Decimals != 6 || !a.Tokens[0].Mint || a.Tokens[0].MinDeposit.String() != "1000000" {
		t.Fatalf("USDC defaults: %+v", a.Tokens[0])
	}
	if a.Tokens[1].MinDeposit.String() != "2000000" {
		t.Fatalf("USDT min_deposit: %+v", a.Tokens[1])
	}
	if chains[1].Tokens[0].MinDeposit.String() != "1000000000000000000" || chains[1].Confirmations != 2 {
		t.Fatalf("18-decimal defaults: %+v", chains[1])
	}
	if chains[0].L1StartHeight != 0 {
		t.Fatal("l1_start_height defaults to 0")
	}
	started, err := ParseChains([]byte(strings.Replace(testChainsJSON, `"confirmations":2,`, `"confirmations":2,"l1_start_height":4321,`, 1)))
	if err != nil || started[1].L1StartHeight != 4321 || started[2].L1StartHeight != 0 {
		t.Fatalf("l1_start_height: %v %+v", err, started)
	}
	bad := map[string]string{
		"no chains":       `{"chains":[]}`,
		"dup chain":       strings.Replace(testChainsJSON, "31338", "31337", 1),
		"dup token":       strings.Replace(testChainsJSON, "0x0000000000000000000000000000000000000056", "0x0000000000000000000000000000000000000055", 1),
		"decimals":        strings.Replace(testChainsJSON, `"decimals":18`, `"decimals":19`, 1),
		"no tokens":       `{"chains":[{"name":"x","chain_id":1,"rpc":"r","vault":"0x0000000000000000000000000000000000000077","tokens":[]}]}`,
		"bad vault":       strings.Replace(testChainsJSON, "0x0000000000000000000000000000000000000077", "0x77", 1),
		"bad min":         strings.Replace(testChainsJSON, `"min_deposit":"2000000"`, `"min_deposit":"two"`, 1),
		"unknown field":   strings.Replace(testChainsJSON, `"mint":true,"min_deposit"`, `"faucet":true,"min_deposit"`, 1),
		"missing chainid": strings.Replace(testChainsJSON, `"chain_id":31338,`, ``, 1),
	}
	for name, doc := range bad {
		if _, err := ParseChains([]byte(doc)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestUnitConversion(t *testing.T) {
	// 6 decimals: 25 USDC -> 25 uARG; 0.000001 USDC -> 100 units.
	if u, ok := UnitsFromToken(big.NewInt(25_000_000), 6); !ok || u != 25*consensus.UnitsPerCoin {
		t.Fatalf("6 dec: %d %v", u, ok)
	}
	if u, ok := UnitsFromToken(big.NewInt(1), 6); !ok || u != 100 {
		t.Fatalf("6 dec dust: %d %v", u, ok)
	}
	// 18 decimals: 2.123456789012345678 -> 2.12345678 (rounded down).
	v, _ := new(big.Int).SetString("2123456789012345678", 10)
	if u, ok := UnitsFromToken(v, 18); !ok || u != 212_345_678 {
		t.Fatalf("18 dec: %d %v", u, ok)
	}
	// 8 decimals: identity. 0 decimals: whole dollars.
	if u, ok := UnitsFromToken(big.NewInt(123), 8); !ok || u != 123 {
		t.Fatal("8 dec")
	}
	if u, ok := UnitsFromToken(big.NewInt(3), 0); !ok || u != 3*consensus.UnitsPerCoin {
		t.Fatal("0 dec")
	}
	// Zero, negative and out-of-range amounts are refused.
	if _, ok := UnitsFromToken(big.NewInt(0), 6); ok {
		t.Fatal("zero accepted")
	}
	if _, ok := UnitsFromToken(new(big.Int).Lsh(big.NewInt(1), 70), 6); ok {
		t.Fatal("huge accepted")
	}
	// Payouts: 10 uARG -> 10 USDC (6), 5 uARG -> 5e18 (18), 1 unit -> 0 on 6 decimals (dust burned), 1 unit -> 1e10 on 18.
	if TokenFromUnits(10*consensus.UnitsPerCoin, 6).String() != "10000000" {
		t.Fatal("payout 6")
	}
	if TokenFromUnits(5*consensus.UnitsPerCoin, 18).String() != "5000000000000000000" {
		t.Fatal("payout 18")
	}
	if TokenFromUnits(1, 6).Sign() != 0 || TokenFromUnits(99, 6).Sign() != 0 || TokenFromUnits(100, 6).String() != "1" {
		t.Fatal("payout dust")
	}
	if TokenFromUnits(1, 18).String() != "10000000000" {
		t.Fatal("payout 18 unit")
	}
	if TokenFromUnits(12_345_678_900, 0).String() != "123" {
		t.Fatal("payout 0 dec")
	}
	if FormatBigTokenAmount(v, 18) != "2.123456789012345678" || FormatBigTokenAmount(big.NewInt(1_000_000), 6) != "1" || FormatBigTokenAmount(big.NewInt(0), 6) != "0" {
		t.Fatal("format")
	}
}

// TestStateFormatRebuild: a format change never loses a record of money
// moved or promised. Format 3 is migrated in place (only the custody coins,
// the address-and-amount records and the Argonmine cursor go, to be rebuilt
// by a rescan); anything older is moved under a/<format>/ and kept; a newer
// format is refused.
func TestStateFormatRebuild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := openState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Rebuilt {
		t.Fatal("a new directory should count as rebuilt")
	}
	fill := func(st *State) {
		st.PutInbound(&Inbound{Chain: 1, ID: 7, Status: InboundPending, To: "rarg1x", EVMTx: "0xAB"})
		st.PutRedemption(&Redemption{TxID: "bb", Status: RedemptionConfirmed, EVMTx: "0xee"})
		st.PutIssue(&Issue{TxID: "cc", Inbound: []InboundRef{{1, 7}}})
		st.RecordSignedBurn(&SignedBurn{TxID: "bb", Digest: "d1"})
		st.RecordSignedIssue(&SignedIssue{TxID: "cc", Inputs: []string{"aa:0"}, Inbound: []InboundRef{{1, 7}}, Amount: 5, Time: 1})
		st.PutUTXO(&UTXO{TxID: "aa", Index: 0, Amount: 9, Height: 3})
		st.SetCursor(EVMCursor(1), 99, "")
		st.SetCursor("l1", 50, "h")
	}
	kept := func(st *State, what string) {
		t.Helper()
		if len(st.Inbounds()) != 1 || len(st.Redemptions()) != 1 || len(st.Issues()) != 1 {
			t.Fatalf("%s: deposits, redemptions or issuances lost", what)
		}
		if _, ok := st.SignedBurn("bb"); !ok {
			t.Fatalf("%s: signed burns lost", what)
		}
		if len(st.SignedIssuesFor(InboundRef{1, 7})) != 1 {
			t.Fatalf("%s: signing memory lost", what)
		}
		if h, _, ok := st.Cursor(EVMCursor(1)); !ok || h != 99 {
			t.Fatalf("%s: EVM cursor lost", what)
		}
		if d, ok := st.InboundByEVMTx("0xab"); !ok || d.ID != 7 || len(st.InboundsTo([]string{"RARG1X"}, 0)) != 1 {
			t.Fatalf("%s: deposit indexes not rebuilt", what)
		}
	}
	fill(st)
	st.Close()
	// Same format: kept.
	st, _ = openState(dir)
	if st.Rebuilt || st.MigratedFrom != 0 {
		t.Fatal("state of the current format was not kept")
	}
	kept(st, "format 4")
	if len(st.UTXOs()) != 1 {
		t.Fatal("custody coins lost")
	}
	// Format 3: migrated in place.
	st.put([]byte("format"), 3)
	st.Close()
	st, _ = openState(dir)
	if st.Rebuilt || st.MigratedFrom != 3 {
		t.Fatalf("format 3: rebuilt %v, from %d", st.Rebuilt, st.MigratedFrom)
	}
	kept(st, "format 3")
	if len(st.UTXOs()) != 0 {
		t.Fatal("format 3: the custody coins are rebuilt by a rescan")
	}
	if _, _, ok := st.Cursor("l1"); ok {
		t.Fatal("format 3: the Argonmine cursor must go so that Argonmine is rescanned")
	}
	// An older format: archived, nothing deleted.
	st.put([]byte("format"), 2)
	st.Close()
	st, _ = openState(dir)
	if !st.Rebuilt || st.MigratedFrom != 2 || len(st.Inbounds()) != 0 || len(st.Redemptions()) != 0 {
		t.Fatal("format 2 was not set aside")
	}
	if r, ok := st.ArchivedRedemption("bb"); !ok || r.Status != RedemptionConfirmed {
		t.Fatal("format 2: the redemptions were not archived")
	}
	var d Inbound
	if !st.get(append([]byte("a/2/"), inboundKey(1, 7)...), &d) || d.ID != 7 {
		t.Fatal("format 2: the deposits were not archived")
	}
	if _, ok := st.SignedBurn("bb"); !ok || len(st.SignedIssuesFor(InboundRef{1, 7})) != 1 {
		t.Fatal("format 2: the signed burns and the signing memory must stay active")
	}
	// A newer format than this daemon's: refused, untouched.
	st.put([]byte("format"), stateFormat+1)
	st.Close()
	if _, err := openState(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a newer format was opened: %v", err)
	}
}
