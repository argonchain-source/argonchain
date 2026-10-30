package bridge

// The hardened release path against a real EVM node: the test starts its
// own anvil on a free port (Foundry's anvil on PATH, or BRIDGE_ANVIL_BIN),
// deploys the hardened contracts/Vault.sol and MockUSDC from the forge
// artifacts in contracts/out, and runs three guardians over JSON-RPC with a
// regtest Argonmine node. It is skipped when anvil or the artifacts are
// missing.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/consensus"
)

// anvilNode is an anvil process this test started, and its JSON-RPC.
type anvilNode struct {
	t   *testing.T
	url string
}

const (
	anvilAccount0 = "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266" // deployer and vault owner
	anvilAccount1 = "0x70997970c51812dc3a010c7d01b50e0d17dc79c8" // the depositor
)

func startAnvil(t *testing.T) *anvilNode {
	t.Helper()
	bin := os.Getenv("BRIDGE_ANVIL_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("anvil")
	}
	if bin == "" {
		t.Skip("anvil not found: add Foundry to PATH or set BRIDGE_ANVIL_BIN")
	}
	for _, a := range []string{"Vault.sol/Vault.json", "MockUSDC.sol/MockUSDC.json"} {
		if _, err := os.Stat(filepath.Join("..", "contracts", "out", a)); err != nil {
			t.Skip("contract artifacts missing: run forge build in contracts/")
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(bin, "--port", strconv.Itoa(port), "--silent")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) // only the process this test started
	a := &anvilNode{t: t, url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	for i := 0; ; i++ {
		if _, err := a.try("eth_chainId"); err == nil {
			break
		}
		if i > 100 {
			t.Fatal("anvil did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return a
}

func (a *anvilNode) try(method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(a.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
	}
	return r.Result, nil
}

func (a *anvilNode) call(method string, params ...any) json.RawMessage {
	a.t.Helper()
	r, err := a.try(method, params...)
	if err != nil {
		a.t.Fatal(err)
	}
	return r
}

func (a *anvilNode) str(method string, params ...any) string {
	a.t.Helper()
	var s string
	json.Unmarshal(a.call(method, params...), &s)
	return s
}

func (a *anvilNode) blockNumber() uint64 {
	n, _ := hexQuantity(a.call("eth_blockNumber"))
	return n
}

// send sends a transaction from an unlocked account (automine on) and
// returns its receipt's contract address when it deployed one.
func (a *anvilNode) send(from, to string, data []byte, value *big.Int) (string, string) {
	a.t.Helper()
	tx := map[string]any{"from": from, "data": "0x" + hex.EncodeToString(data), "gas": "0x7a1200"}
	if to != "" {
		tx["to"] = to
	}
	if value != nil {
		tx["value"] = "0x" + value.Text(16)
	}
	h := a.str("eth_sendTransaction", tx)
	var rc struct {
		Status   string `json:"status"`
		Contract string `json:"contractAddress"`
	}
	for i := 0; i < 100; i++ { // automine answers before the block is in
		raw := a.call("eth_getTransactionReceipt", h)
		if string(raw) != "null" {
			if err := json.Unmarshal(raw, &rc); err != nil || rc.Status != "0x1" {
				a.t.Fatalf("transaction %s: %+v %v", h, rc, err)
			}
			return h, rc.Contract
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.t.Fatalf("transaction %s was not mined", h)
	return h, rc.Contract
}

func (a *anvilNode) erc20Balance(token [20]byte, who string) *big.Int {
	w, _ := ParseAddress(who)
	var s string
	json.Unmarshal(a.call("eth_call", map[string]any{"to": HexAddress(token), "data": "0x" + hex.EncodeToString(append(append([]byte{}, selBalanceOf[:]...), word(w[:])...))}, "latest"), &s)
	b, _ := hexBytes(s)
	return new(big.Int).SetBytes(b)
}

func artifact(t *testing.T, name string) []byte {
	b, err := os.ReadFile(filepath.Join("..", "contracts", "out", name))
	if err != nil {
		t.Fatal(err)
	}
	var art struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(b, &art); err != nil {
		t.Fatal(err)
	}
	code, err := hexBytes(art.Bytecode.Object)
	if err != nil || len(code) == 0 {
		t.Fatalf("artifact %s: %v", name, err)
	}
	return code
}

func call(sig string, words ...[]byte) []byte {
	sel := Selector(sig)
	out := append([]byte{}, sel[:]...)
	for _, w := range words {
		out = append(out, w...)
	}
	return out
}

func addrWord(s string) []byte { a, _ := ParseAddress(s); return word(a[:]) }

// startRPC replaces the env's guardians with three that talk to the chain
// over JSON-RPC: the leader sends from gasKey, the followers send nothing.
func (e *pocEnv) startRPC(prefix string, chains []Chain, gasKey *secp256k1.PrivateKey) {
	mk := func(i int, leader bool, peers []string) *Service {
		var key *secp256k1.PrivateKey
		if leader {
			key = gasKey
		}
		cfg := Config{
			Params: consensus.RegTest, Node: e.client, Chains: chains, EVMs: map[uint64]EVM{chains[0].ID: NewRPCEVM(chains[0].RPC, chains[0].ID, chains[0].Vault, key)},
			Custody: e.cs[i], EVMKey: e.evmKeys[i], Leader: leader, Peers: peers, Secret: testSecret, LeaderKey: e.cs[0].LocalKey(), Threshold: 2, L1Confirmations: 3,
			StatePath: filepath.Join(e.dir, fmt.Sprintf("%s%d", prefix, i)), Logf: e.t.Logf,
		}
		for _, o := range e.opts {
			o(&cfg)
		}
		s, err := New(cfg)
		if err != nil {
			e.t.Fatal(err)
		}
		e.t.Cleanup(s.Stop)
		return s
	}
	e.f1, e.f2 = mk(1, false, nil), mk(2, false, nil)
	w1, w2 := newTestServer(e.t, e.f1), newTestServer(e.t, e.f2)
	e.leader = mk(0, true, []string{w1, w2})
	e.urls = []string{newTestServer(e.t, e.leader), w1, w2}
}

func newTestServer(t *testing.T, s *Service) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler()}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return "http://" + l.Addr().String()
}

// unspentUARG finds an unspent uARG coin of the user of exactly amount.
func (e *pocEnv) unspentUARG(amount consensus.Amount) (string, uint32) {
	e.t.Helper()
	info, _ := e.client.ChainInfo()
	asset := e.leader.Asset().String()
	for h := uint32(1); h <= info.Blocks; h++ {
		bh, _ := e.client.BlockHash(h)
		blk, _ := e.client.Block(bh)
		for _, tx := range blk.Tx {
			for _, o := range tx.Outputs {
				if o.Asset != asset || o.Address != e.uAddr || o.Amount != amount {
					continue
				}
				if raw, err := e.client.Call("gettxout", tx.TxID, o.N); err == nil && len(raw) > 0 && string(raw) != "null" {
					return tx.TxID, uint32(o.N)
				}
			}
		}
	}
	e.t.Fatalf("no unspent uARG coin of %d", amount)
	return "", 0
}

// burnCoin burns the user's unspent uARG coin of amount with memo, paying
// the fee from the coinbase of block cbHeight, and mines it.
func (e *pocEnv) burnCoin(amount consensus.Amount, memo []byte, cbHeight uint32) string {
	t := e.t
	t.Helper()
	txid, n := e.unspentUARG(amount)
	cbBlock, _ := e.client.Block(mustBlockHash(t, e.client, cbHeight))
	cb := cbBlock.Tx[0]
	tx := &consensus.Tx{Version: consensus.TxVersionAssets,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: mustHash(t, txid), Index: n}}, {PrevOut: consensus.OutPoint{TxID: mustHash(t, cb.TxID), Index: 0}}},
		Outputs:   []consensus.TxOut{{Type: consensus.OutputTypeNullData, Payload: memo}, e.user.Output(cb.Outputs[0].Amount - 5000)},
		Witnesses: make([][]byte, 2)}
	spent := []consensus.TxOut{{Amount: amount, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly, Asset: e.leader.Asset()}, {Amount: cb.Outputs[0].Amount, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly}}
	for i := range tx.Inputs {
		if err := e.user.Sign(consensus.RegTest.ChainTag, tx, spent, i); err != nil {
			t.Fatal(err)
		}
	}
	burn, err := e.client.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
	if err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	return burn
}

// TestAnvilHardenedVault runs the guardians against the hardened Vault on a
// real EVM node: the finalized tag and a reorganisation made with
// evm_snapshot/evm_revert (the deposit that left is marked reorged, and its
// id is taken over by the next deposit); releases sent from the gas key and
// never from a guardian key; a release not mined in time sent again with
// the same nonce at fees 12.5 percent higher, and the first transaction
// landing late paid once and confirmed from it; the vault's 24-hour limit
// making a burn wait (never fail) until the window has room; and a paused
// vault holding payouts until its owner lifts the pause.
func TestAnvilHardenedVault(t *testing.T) {
	t.Parallel()
	an := startAnvil(t)
	clock := newTestClock()
	e := newPocEnv(t, func(c *Config) { c.Now = clock.now; c.ReleaseTimeout = 5 * time.Minute })
	ctx := context.Background()
	e.fund()

	// Contracts: MockUSDC and the hardened Vault (three guardians, two
	// signatures, a one-day timelock), set up and finalized like
	// deploy/railway/bridge-chain.sh does it.
	_, usdcHex := an.send(anvilAccount0, "", artifact(t, "MockUSDC.sol/MockUSDC.json"), nil)
	// constructor(address[] guardians, uint256 threshold, uint256 delay,
	// uint256 firstDepositId): the array's offset after the four head words.
	ctor := append(wordUint(big.NewInt(128)), wordUint(big.NewInt(2))...)
	ctor = append(ctor, wordUint(big.NewInt(86400))...)
	ctor = append(ctor, wordUint(big.NewInt(1))...)
	ctor = append(ctor, wordUint(big.NewInt(3))...)
	for _, g := range e.evmAddrs {
		ctor = append(ctor, word(g[:])...)
	}
	_, vaultHex := an.send(anvilAccount0, "", append(artifact(t, "Vault.sol/Vault.json"), ctor...), nil)
	usdc, _ := ParseAddress(usdcHex)
	vault, _ := ParseAddress(vaultHex)
	an.send(anvilAccount0, vaultHex, call("setToken(address,bool,uint256)", word(usdc[:]), wordUint(big.NewInt(1)), wordUint(usd(1, 6))), nil)
	an.send(anvilAccount0, vaultHex, call("setFees(uint16,uint16)", wordUint(big.NewInt(100)), wordUint(big.NewInt(100))), nil)
	an.send(anvilAccount0, vaultHex, call("setLimits(address,uint256,uint256)", word(usdc[:]), wordUint(new(big.Int)), wordUint(usd(300, 6))), nil)
	an.send(anvilAccount0, vaultHex, call("finalizeSetup()"), nil)
	an.send(anvilAccount1, usdcHex, call("mint(address,uint256)", addrWord(anvilAccount1), wordUint(usd(10000, 6))), nil)
	an.send(anvilAccount1, usdcHex, call("approve(address,uint256)", word(vault[:]), wordUint(usd(10000, 6))), nil)
	start := an.blockNumber()

	gasKey, _ := secp256k1.GeneratePrivateKey()
	gasAddr := HexAddress(AddressOf(gasKey.PubKey()))
	an.send(anvilAccount0, gasAddr, nil, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	doc := fmt.Sprintf(`{"chains":[{"name":"Anvil","chain_id":31337,"rpc":%q,"vault":%q,"start_block":%d,"confirmations":2,"finality":"finalized",
	  "tokens":[{"symbol":"USDC","address":%q,"decimals":6}]}]}`, an.url, vaultHex, start, usdcHex)
	chains, err := ParseChains([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	e.chains = chains
	e.stop()
	e.startRPC("a", chains, gasKey)
	deposit := func(gross *big.Int) {
		an.send(anvilAccount1, vaultHex, append(call("deposit(address,uint256,string)", word(usdc[:]), wordUint(gross), wordUint(big.NewInt(96))), abiString(e.uAddr)...), nil)
	}

	// 1. A deposit in a branch that is then reverted: final on the first
	// branch (64 blocks under anvil's finalized tag), gone on the second.
	snap := an.str("evm_snapshot")
	deposit(usd(70, 6))
	an.call("anvil_mine", "0x46")
	e.scanAll()
	x, _ := e.f1.Inbound(31337, 1)
	if x == nil || x.Status != InboundPending || x.BlockHash == "" {
		t.Fatalf("deposit on the first branch: %+v", x)
	}
	if !jsonBool(an.call("evm_revert", snap)) {
		t.Fatal("evm_revert failed")
	}
	an.send(anvilAccount0, usdcHex, call("mint(address,uint256)", addrWord(anvilAccount0), wordUint(big.NewInt(1))), nil) // another block at that height
	an.call("anvil_mine", "0x50")
	e.scanAll()
	for i, s := range []*Service{e.leader, e.f1, e.f2} {
		if x, _ := s.Inbound(31337, 1); x.Status != InboundReorged {
			t.Fatalf("guardian %d: the reverted deposit: %+v", i, x)
		}
	}

	// 2. Deposits on the branch that stays (ids from 1 again: the first takes
	// over the reorged record), issued once final.
	for _, v := range []int64{100, 60, 200, 40} {
		deposit(usd(v, 6))
	}
	an.call("anvil_mine", "0x46")
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	for id := uint64(1); id <= 4; id++ {
		if d, _ := e.leader.Inbound(31337, id); d == nil || d.Status != InboundConfirmed {
			t.Fatalf("deposit %d: %+v", id, d)
		}
	}
	if d, _ := e.leader.Inbound(31337, 1); d.TokenAmount != "99000000" {
		t.Fatalf("deposit 1 after the reorganisation: %+v", d)
	}

	// 3. A release, sent from the gas key and never from a guardian key.
	var recipient [20]byte
	recipient[19] = 0xCD
	memo := EncodeMemo(recipient)
	b1 := e.burnCoin(99*consensus.UnitsPerCoin, memo, 9)
	e.mine(3, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.leader.Once(ctx)
	r1, _ := e.leader.st.GetRedemption(b1)
	if r1.Status != RedemptionConfirmed {
		t.Fatalf("first release: %+v", r1)
	}
	var tx1 struct {
		From string `json:"from"`
	}
	json.Unmarshal(an.call("eth_getTransactionByHash", r1.EVMTx), &tx1)
	if !strings.EqualFold(tx1.From, gasAddr) {
		t.Fatalf("the release was sent from %s, not the gas key %s", tx1.From, gasAddr)
	}
	for _, g := range e.evmAddrs {
		if n, _ := hexQuantity(an.call("eth_getTransactionCount", HexAddress(g), "latest")); n != 0 {
			t.Fatalf("guardian key %s sent %d transactions", HexAddress(g), n)
		}
	}
	paid1 := an.erc20Balance(usdc, HexAddress(recipient))
	if paid1.String() != "98010000" {
		t.Fatalf("recipient holds %s after the first release", paid1)
	}
	if st := e.leader.Status(); st.GasAddress != gasAddr || st.Chains[0].GasBalance == "" || st.Chains[0].Finality != FinalityFinalized {
		t.Fatalf("status: %+v", st.Chains[0])
	}

	// 4. Not mined in time: sent again with the same nonce and higher fees;
	// the first transaction lands late and pays once.
	an.call("evm_setAutomine", false)
	b2 := e.burnCoin(5_940_000_000, memo, 10) // 59.4 uARG
	e.mine(3, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	r2, _ := e.leader.st.GetRedemption(b2)
	if r2.Status != RedemptionReleased || r2.Send == nil || r2.Send.Type != 2 {
		t.Fatalf("second release: %+v", r2)
	}
	first, nonce := r2.Send.Hash(), r2.Send.Nonce
	raw1 := an.str("eth_getRawTransactionByHash", first)
	fee1, tip1 := mustBig(r2.Send.FeeCap), mustBig(r2.Send.TipCap)
	clock.add(6 * time.Minute)
	e.leader.Once(ctx)
	r2, _ = e.leader.st.GetRedemption(b2)
	second := r2.Send.Hash()
	if second == first || r2.Send.Nonce != nonce || r2.Send.Bumps != 1 {
		t.Fatalf("not sent again: %+v", r2.Send)
	}
	var pooled struct {
		Nonce  string `json:"nonce"`
		FeeCap string `json:"maxFeePerGas"`
		Tip    string `json:"maxPriorityFeePerGas"`
	}
	json.Unmarshal(an.call("eth_getTransactionByHash", second), &pooled)
	pf, _ := hexBytes(pooled.FeeCap)
	pt, _ := hexBytes(pooled.Tip)
	if new(big.Int).SetBytes(pf).Cmp(mulDivUp(fee1, 1125, 1000)) < 0 || new(big.Int).SetBytes(pt).Cmp(mulDivUp(tip1, 1125, 1000)) < 0 {
		t.Fatalf("fees of the replacement: %s/%s after %s/%s", pooled.Tip, pooled.FeeCap, tip1, fee1)
	}
	if known, _ := e.leader.byID[31337].evm.TxKnown(ctx, first); known {
		t.Fatal("the node kept the replaced transaction")
	}
	an.call("anvil_dropTransaction", second)
	an.call("eth_sendRawTransaction", raw1)
	an.call("evm_mine")
	e.leader.Once(ctx)
	r2, _ = e.leader.st.GetRedemption(b2)
	if r2.Status != RedemptionConfirmed || r2.EVMTx != first {
		t.Fatalf("after the first transaction landed: %+v", r2)
	}
	paid2 := new(big.Int).Sub(an.erc20Balance(usdc, HexAddress(recipient)), paid1)
	if paid2.String() != "58806000" {
		t.Fatalf("the late transaction paid %s", paid2)
	}
	clock.add(time.Hour)
	e.leader.Once(ctx)
	if n, _ := hexQuantity(an.call("eth_getTransactionCount", gasAddr, "pending")); n != nonce+1 {
		t.Fatalf("the gas key sent %d transactions, want %d", n, nonce+1)
	}
	an.call("evm_setAutomine", true)

	// 5. The 24-hour window (300 USDC, 158.4 used): a 198 USDC burn waits,
	// and is paid once the window has room.
	b3 := e.burnCoin(19_800_000_000, memo, 11)
	e.mine(3, e.uAddr)
	e.round()
	r3, _ := e.leader.st.GetRedemption(b3)
	if r3.Status != RedemptionPending || !strings.Contains(r3.Error, "24-hour release window") {
		t.Fatalf("burn over the window: %+v", r3)
	}
	if tok := e.leader.Status().Chains[0].Tokens[0]; tok.PeriodAvailable != "141600000" || tok.AvailableNow != "141600000" {
		t.Fatalf("token status: %+v", tok)
	}
	an.call("evm_increaseTime", 86401)
	an.call("evm_mine")
	e.round()
	e.leader.Once(ctx)
	if r3, _ = e.leader.st.GetRedemption(b3); r3.Status != RedemptionConfirmed {
		t.Fatalf("burn after the window reset: %+v", r3)
	}

	// 6. The owner pauses the vault: payouts wait; lifted through the
	// timelock, they go.
	an.send(anvilAccount0, vaultHex, call("pause()"), nil)
	b4 := e.burnCoin(3_960_000_000, memo, 12)
	e.mine(3, e.uAddr)
	e.round()
	r4, _ := e.leader.st.GetRedemption(b4)
	if r4.Status != RedemptionPending || !strings.Contains(r4.Error, "paused") {
		t.Fatalf("burn on a paused vault: %+v", r4)
	}
	if st := e.leader.Status(); !st.Chains[0].VaultPaused || st.Chains[0].Tokens[0].AvailableNow != "0" {
		t.Fatalf("paused vault status: %+v", st.Chains[0])
	}
	an.send(anvilAccount0, vaultHex, call("proposeUnpause()"), nil)
	an.call("evm_increaseTime", 86401)
	an.call("evm_mine")
	an.send(anvilAccount0, vaultHex, call("executeUnpause()"), nil)
	e.round()
	e.leader.Once(ctx)
	if r4, _ = e.leader.st.GetRedemption(b4); r4.Status != RedemptionConfirmed {
		t.Fatalf("burn after the pause was lifted: %+v", r4)
	}
	total := an.erc20Balance(usdc, HexAddress(recipient))
	if want := new(big.Int).Add(new(big.Int).Add(paid1, paid2), big.NewInt(196_020_000+39_204_000)); total.Cmp(want) != 0 {
		t.Fatalf("recipient holds %s, want %s", total, want)
	}
}

func jsonBool(raw json.RawMessage) bool {
	var b bool
	json.Unmarshal(raw, &b)
	return b
}

// TestAnvilVaultInfo: the guardians read the hardened Vault's deposit
// numbering and setup state over JSON-RPC: firstDepositId() as deployed,
// the first deposit's id equal to it, finalized() before and after the
// setup; a contract without those views reads as numbering from 1 with
// its setup state unknown.
func TestAnvilVaultInfo(t *testing.T) {
	t.Parallel()
	an := startAnvil(t)
	ctx := context.Background()
	var gs [][20]byte
	for i := 0; i < 3; i++ {
		k, _ := secp256k1.GeneratePrivateKey()
		gs = append(gs, AddressOf(k.PubKey()))
	}
	_, usdcHex := an.send(anvilAccount0, "", artifact(t, "MockUSDC.sol/MockUSDC.json"), nil)
	usdc, _ := ParseAddress(usdcHex)
	ctor := append(wordUint(big.NewInt(128)), wordUint(big.NewInt(2))...)
	ctor = append(ctor, wordUint(big.NewInt(86400))...)
	ctor = append(ctor, wordUint(big.NewInt(1_000_001))...)
	ctor = append(ctor, wordUint(big.NewInt(3))...)
	for _, g := range gs {
		ctor = append(ctor, word(g[:])...)
	}
	_, vaultHex := an.send(anvilAccount0, "", append(artifact(t, "Vault.sol/Vault.json"), ctor...), nil)
	vault, _ := ParseAddress(vaultHex)
	ev := NewRPCEVM(an.url, 31337, vault, nil)
	if id, err := ev.FirstDepositID(ctx); err != nil || id != 1_000_001 {
		t.Fatalf("firstDepositId(): %d %v", id, err)
	}
	if fin, ok, err := ev.SetupFinalized(ctx); err != nil || !ok || fin {
		t.Fatalf("finalized() of a vault in setup: %v %v %v", fin, ok, err)
	}
	an.send(anvilAccount0, vaultHex, call("setToken(address,bool,uint256)", word(usdc[:]), wordUint(big.NewInt(1)), wordUint(usd(1, 6))), nil)
	an.send(anvilAccount0, vaultHex, call("finalizeSetup()"), nil)
	if fin, ok, err := ev.SetupFinalized(ctx); err != nil || !ok || !fin {
		t.Fatalf("finalized() after finalizeSetup: %v %v %v", fin, ok, err)
	}
	an.send(anvilAccount1, usdcHex, call("mint(address,uint256)", addrWord(anvilAccount1), wordUint(usd(10, 6))), nil)
	an.send(anvilAccount1, usdcHex, call("approve(address,uint256)", word(vault[:]), wordUint(usd(10, 6))), nil)
	an.send(anvilAccount1, vaultHex, append(call("deposit(address,uint256,string)", word(usdc[:]), wordUint(usd(10, 6)), wordUint(big.NewInt(96))), abiString("targ1qexample")...), nil)
	evs, err := ev.Deposits(ctx, 0, an.blockNumber())
	if err != nil || len(evs) != 1 || evs[0].ID != 1_000_001 {
		t.Fatalf("the first deposit: %+v %v", evs, err)
	}
	// An address without a contract (a mistyped vault): an error, never 1.
	if _, err := NewRPCEVM(an.url, 31337, [20]byte{19: 0x99}, nil).FirstDepositID(ctx); !errors.Is(err, ErrNoVaultCode) {
		t.Fatalf("firstDepositId() of an address without a contract: %v", err)
	}
	// A contract without the views (a vault from before them).
	old := NewRPCEVM(an.url, 31337, usdc, nil)
	if id, err := old.FirstDepositID(ctx); err != nil || id != 1 {
		t.Fatalf("firstDepositId() of a contract without it: %d %v", id, err)
	}
	if _, ok, err := old.SetupFinalized(ctx); err != nil || ok {
		t.Fatalf("finalized() of a contract without it: %v %v", ok, err)
	}
}
