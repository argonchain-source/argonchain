package bridge

// Swap addresses end to end (swapaddr.go): uARG paid to one from any
// wallet, with no memo, is paid out as the token, chain and recipient the
// address names; every guardian reads the payout from the address itself.

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/consensus"
	"argonmine/wallet"
)

// paySwap spends the user's newest uARG coin and the coinbase of block
// cbHeight into outs, with the rest of the uARG and ARG back to the user,
// and broadcasts it. The change outputs follow outs, uARG first.
func (e *pocEnv) paySwap(outs []consensus.TxOut, cbHeight uint32) string {
	t := e.t
	t.Helper()
	asset := e.leader.Asset()
	info, _ := e.client.ChainInfo()
	var ucoin wallet.TxOutJSON
	var utx string
	for h := info.Blocks; h > 0 && utx == ""; h-- {
		bh, _ := e.client.BlockHash(h)
		blk, _ := e.client.Block(bh)
		for _, tx := range blk.Tx {
			for _, o := range tx.Outputs {
				if o.Asset == asset.String() && o.Address == e.uAddr {
					ucoin, utx = o, tx.TxID
				}
			}
		}
	}
	if utx == "" {
		t.Fatal("the user has no uARG")
	}
	var spentAsset consensus.Amount
	for _, o := range outs {
		if o.Asset == asset {
			spentAsset += o.Amount
		}
	}
	change := e.user.Output(ucoin.Amount - spentAsset)
	change.Asset = asset
	cbBlock, _ := e.client.Block(mustBlockHash(t, e.client, cbHeight))
	cb := cbBlock.Tx[0]
	var argOut consensus.Amount
	for _, o := range outs {
		if o.Asset != asset {
			argOut += o.Amount
		}
	}
	tx := &consensus.Tx{Version: consensus.TxVersionAssets,
		Inputs:    []consensus.TxIn{{PrevOut: consensus.OutPoint{TxID: mustHash(t, utx), Index: uint32(ucoin.N)}}, {PrevOut: consensus.OutPoint{TxID: mustHash(t, cb.TxID), Index: 0}}},
		Outputs:   append(append([]consensus.TxOut{}, outs...), change, e.user.Output(cb.Outputs[0].Amount-argOut-20000)),
		Witnesses: make([][]byte, 2)}
	spent := []consensus.TxOut{{Amount: ucoin.Amount, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly, Asset: asset}, {Amount: cb.Outputs[0].Amount, Type: consensus.OutputTypeP2PK, Payload: e.user.XOnly}}
	for i := range tx.Inputs {
		if err := e.user.Sign(consensus.RegTest.ChainTag, tx, spent, i); err != nil {
			t.Fatal(err)
		}
	}
	id, err := e.client.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSwapAddressPayout(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()
	c := e.chains[0]
	usdc, usdt := c.Tokens[0], c.Tokens[1]
	e.issueDeposit(usdc.Address, usd(100, 6))
	e.issueDeposit(usdt.Address, usd(20, 6)) // the newest coin, 19.8 uARG, pays the swaps

	var r1, r2 [20]byte
	r1[19], r2[19] = 0xA1, 0xA2
	payload := func(d SwapDest) []byte {
		p, err := SwapPayload(d)
		if err != nil {
			t.Fatal(err)
		}
		return p[:]
	}
	asset := e.leader.Asset()
	unit := consensus.Amount(consensus.UnitsPerCoin)
	pay := e.paySwap([]consensus.TxOut{
		{Amount: 10 * unit, Type: consensus.OutputTypeP2PK, Payload: payload(SwapDest{ChainID: c.ID, Symbol: "USDC", To: r1}), Asset: asset},
		{Amount: 5 * unit, Type: consensus.OutputTypeP2PK, Payload: payload(SwapDest{ChainID: c.ID, Symbol: "USDT", To: r2}), Asset: asset},
		{Amount: 1 * unit, Type: consensus.OutputTypeP2PK, Payload: payload(SwapDest{ChainID: 999, Symbol: "USDC", To: r1}), Asset: asset},
		{Amount: 1 * unit, Type: consensus.OutputTypeP2PK, Payload: payload(SwapDest{ChainID: c.ID, Symbol: "DAI", To: r1}), Asset: asset},
		// ARG sent to a swap address is lost, never a payout.
		{Amount: 50_000, Type: consensus.OutputTypeP2PK, Payload: payload(SwapDest{ChainID: c.ID, Symbol: "USDC", To: r1})},
	}, 10)
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}

	// Every guardian records the payments from the addresses alone.
	for i, s := range []*Service{e.leader, e.f1, e.f2} {
		want := []struct {
			id, status, sym string
			to              [20]byte
			units           consensus.Amount
		}{
			{pay + ":0", RedemptionPending, "USDC", r1, 10 * unit},
			{pay + ":1", RedemptionPending, "USDT", r2, 5 * unit},
			{pay + ":2", RedemptionUnreleasable, "USDC", r1, unit},
			{pay + ":3", RedemptionUnreleasable, "DAI", r1, unit},
		}
		for _, w := range want {
			r, ok := s.st.GetRedemption(w.id)
			if !ok {
				t.Fatalf("guardian %d did not record %s", i, w.id)
			}
			if r.Status != w.status || r.TokenSymbol != w.sym || r.To != HexAddress(w.to) || r.Amount != w.units || r.ID() != w.id || !IsSwapAddress(consensus.RegTest.HRP, r.Address) {
				t.Fatalf("guardian %d recorded %s as %+v", i, w.id, r)
			}
		}
		if r, _ := s.st.GetRedemption(pay + ":2"); !strings.Contains(r.Error, "not served") {
			t.Fatalf("unserved chain: %q", r.Error)
		}
		if r, _ := s.st.GetRedemption(pay + ":3"); !strings.Contains(r.Error, "not paid out") {
			t.Fatalf("unknown token: %q", r.Error)
		}
		for _, id := range []string{pay, pay + ":4", pay + ":5", pay + ":6"} {
			if _, ok := s.st.GetRedemption(id); ok {
				t.Fatalf("guardian %d recorded %s, which pays no uARG to a swap address", i, id)
			}
		}
	}

	// Below the depth a follower refuses to sign.
	if _, err := e.f1.SignReleaseLocally(ctx, pay+":0"); err == nil || !strings.Contains(err.Error(), "confirmations") {
		t.Fatalf("signed below the depth: %v", err)
	}
	e.mine(4, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.leader.Once(ctx)

	// Two payouts, each under the id of its output.
	if len(e.evm.releases) != 2 {
		t.Fatalf("releases: %d", len(e.evm.releases))
	}
	got := map[[32]byte]fakeRelease{}
	for _, rel := range e.evm.releases {
		got[rel.id] = rel
	}
	for _, w := range []struct {
		id    string
		token [20]byte
		to    [20]byte
		gross int64
	}{{pay + ":0", usdc.Address, r1, 10_000_000}, {pay + ":1", usdt.Address, r2, 5_000_000}} {
		rel, ok := got[burnID(w.id)]
		if !ok || rel.token != w.token || rel.to != w.to || rel.amount.Int64() != w.gross {
			t.Fatalf("release of %s: %+v (have %v)", w.id, rel, ok)
		}
	}
	if burnID(pay+":0") == burnID(pay) || burnID(pay+":0") == burnID(pay+":1") {
		t.Fatal("payout ids collide")
	}

	// A follower checks the output itself: not the change, not ARG, not a
	// missing output, not the transaction as a burn; and signs once.
	for id, why := range map[string]string{
		pay + ":4": "does not pay " + e.f1.cfg.AssetSymbol,
		pay + ":5": "does not pay a swap address",
		pay + ":9": "has no output",
		pay:        "burns no",
		pay + ":2": "not served",
		"zz:1":     "64 hex digits",
		pay + ":x": "whole number",
	} {
		if _, err := e.f1.SignReleaseLocally(ctx, id); err == nil || !strings.Contains(err.Error(), why) {
			t.Fatalf("%s: %v, want %q", id, err, why)
		}
	}
	s1, err := e.f2.SignReleaseLocally(ctx, pay+":0")
	if err != nil {
		t.Fatal(err)
	}
	if sb, ok := e.f2.st.SignedBurn(pay + ":0"); !ok || sb.To != HexAddress(r1) || len(s1) == 0 {
		t.Fatalf("signing record: %+v", sb)
	}

	// The status lists them by id; the API computes the same address.
	get := func(path string) (int, string) {
		resp, err := http.Get(e.urls[0] + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/api/redemption/" + pay + ":1"); code != 200 || !strings.Contains(body, `"vout":1`) || !strings.Contains(body, `"address":"`+consensus.RegTest.HRP+"1qswap") {
		t.Fatalf("redemption by id: %d %s", code, body)
	}
	want, _ := SwapAddress(consensus.RegTest.HRP, SwapDest{ChainID: c.ID, Symbol: "USDC", To: r1})
	if code, body := get(fmt.Sprintf("/api/swap-address?chain=%d&token=usdc&to=%s", c.ID, HexAddress(r1))); code != 200 || !strings.Contains(body, `"address":"`+want+`"`) || !strings.Contains(body, `"token":"USDC"`) {
		t.Fatalf("swap-address: %d %s", code, body)
	}
	for q, why := range map[string]string{
		fmt.Sprintf("chain=%d&token=DAI&to=%s", c.ID, HexAddress(r1)):          "not paid out",
		fmt.Sprintf("chain=999&token=USDC&to=%s", HexAddress(r1)):              "not served",
		fmt.Sprintf("chain=%d&token=USDC&to=%s", c.ID, HexAddress(c.Vault)):    "own contracts",
		fmt.Sprintf("chain=%d&token=USDC&to=0x12", c.ID):                       "EVM address",
		fmt.Sprintf("chain=%d&token=USDC&to=%s", c.ID, HexAddress([20]byte{})): "EVM address",
	} {
		if code, body := get("/api/swap-address?" + q); code != 400 || !strings.Contains(body, why) {
			t.Fatalf("%s: %d %s, want %q", q, code, body, why)
		}
	}
	// A swap address is never a deposit's recipient.
	if _, _, err := e.leader.depositChain(fmt.Sprint(c.ID), want); err == nil || !strings.Contains(err.Error(), "swap address") {
		t.Fatalf("deposit to a swap address: %v", err)
	}
}

// TestAnvilSwapAddress: on a real EVM node with the hardened Vault, uARG
// paid to a swap address is released under the output's hashed id, which
// the vault records as paid, and the recipient receives the token.
func TestAnvilSwapAddress(t *testing.T) {
	t.Parallel()
	an := startAnvil(t)
	e := newPocEnv(t)
	ctx := context.Background()
	e.fund()

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
	an.send(anvilAccount0, vaultHex, call("finalizeSetup()"), nil)
	an.send(anvilAccount1, usdcHex, call("mint(address,uint256)", addrWord(anvilAccount1), wordUint(usd(1000, 6))), nil)
	an.send(anvilAccount1, usdcHex, call("approve(address,uint256)", word(vault[:]), wordUint(usd(1000, 6))), nil)
	start := an.blockNumber()
	gasKey, _ := secp256k1.GeneratePrivateKey()
	an.send(anvilAccount0, HexAddress(AddressOf(gasKey.PubKey())), nil, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	chains, err := ParseChains([]byte(fmt.Sprintf(`{"chains":[{"name":"Anvil","chain_id":31337,"rpc":%q,"vault":%q,"start_block":%d,"confirmations":2,
	  "tokens":[{"symbol":"USDC","address":%q,"decimals":6}]}]}`, an.url, vaultHex, start, usdcHex)))
	if err != nil {
		t.Fatal(err)
	}
	e.chains = chains
	e.stop()
	e.startRPC("s", chains, gasKey)

	// 100 USDC in: 99 uARG to the user.
	an.send(anvilAccount1, vaultHex, append(call("deposit(address,uint256,string)", word(usdc[:]), wordUint(usd(100, 6)), wordUint(big.NewInt(96))), abiString(e.uAddr)...), nil)
	an.call("anvil_mine", "0x3")
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.leader.Inbound(31337, 1); d == nil || d.Status != InboundConfirmed {
		t.Fatalf("deposit: %+v", d)
	}

	// 25 uARG out to a swap address, from an ordinary transfer.
	var recipient [20]byte
	recipient[0], recipient[19] = 0x5A, 0xB1
	p, err := SwapPayload(SwapDest{ChainID: 31337, Symbol: "USDC", To: recipient})
	if err != nil {
		t.Fatal(err)
	}
	pay := e.paySwap([]consensus.TxOut{{Amount: 25 * consensus.UnitsPerCoin, Type: consensus.OutputTypeP2PK, Payload: p[:], Asset: e.leader.Asset()}}, 9)
	e.mine(3, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.leader.Once(ctx)
	id := pay + ":0"
	r, ok := e.leader.st.GetRedemption(id)
	if !ok || r.Status != RedemptionConfirmed {
		t.Fatalf("swap payout: %+v", r)
	}
	if got := an.erc20Balance(usdc, HexAddress(recipient)); got.String() != "24750000" {
		t.Fatalf("recipient holds %s, want 24.75 USDC", got)
	}
	bid := burnID(id)
	if got := an.str("eth_call", map[string]string{"to": vaultHex, "data": "0x" + hex.EncodeToString(call("released(bytes32)", bid[:]))}, "latest"); got != "0x"+strings.Repeat("0", 63)+"1" {
		t.Fatal("the vault does not record the swap payment as paid")
	}
}
