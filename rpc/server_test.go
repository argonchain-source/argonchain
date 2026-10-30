package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/wallet"
)

type client struct {
	t     *testing.T
	url   string
	token string
}

func (c *client) call(method string, params ...any) (json.RawMessage, *Error) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", c.url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		c.t.Fatalf("%s: decode: %v", method, err)
	}
	return r.Result, r.Error
}

func (c *client) must(method string, params ...any) json.RawMessage {
	c.t.Helper()
	res, err := c.call(method, params...)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return res
}

func TestRPCEndToEnd(t *testing.T) {
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	pool := mempool.New(ch, mempool.DefaultConfig())
	srv := New(Config{Listen: "127.0.0.1:0", Token: "secret"}, Deps{Params: consensus.RegTest, Chain: ch, Pool: pool, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	c := &client{t: t, url: "http://" + srv.Addr(), token: "secret"}

	// Auth required.
	bad := &client{t: t, url: c.url, token: "wrong"}
	req, _ := http.NewRequest("POST", bad.url, bytes.NewReader([]byte(`{"method":"getblockcount"}`)))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	var count uint32
	json.Unmarshal(c.must("getblockcount"), &count)
	if count != 0 {
		t.Fatal("count")
	}

	// Generate to an address.
	priv, _ := btcec.NewPrivateKey()
	xonly := schnorr.SerializePubKey(priv.PubKey())
	addr, _ := wallet.EncodeAddress("rarg", consensus.OutputTypeP2PK, xonly)
	var hashes []string
	json.Unmarshal(c.must("generatetoaddress", 101, addr), &hashes)
	if len(hashes) != 101 {
		t.Fatal("generate")
	}
	json.Unmarshal(c.must("getblockcount"), &count)
	if count != 101 {
		t.Fatal("count after generate")
	}

	// Block lookups.
	var h1 string
	json.Unmarshal(c.must("getblockhash", 1), &h1)
	if h1 != hashes[0] {
		t.Fatal("getblockhash")
	}
	var blk map[string]any
	json.Unmarshal(c.must("getblock", h1, 2), &blk)
	if blk["height"].(float64) != 1 || blk["confirmations"].(float64) != 101 {
		t.Fatalf("getblock: %v", blk)
	}
	txs := blk["tx"].([]any)
	cb := txs[0].(map[string]any)
	cbTxid := cb["txid"].(string)
	outs := cb["outputs"].([]any)
	if outs[0].(map[string]any)["address"].(string) != addr {
		t.Fatal("coinbase address")
	}

	// gettxout of the mature coinbase.
	var utxo map[string]any
	json.Unmarshal(c.must("gettxout", cbTxid, 0), &utxo)
	if utxo == nil || utxo["coinbase"] != true {
		t.Fatal("gettxout")
	}

	// Build, decode, send a transaction.
	txidHash, _ := consensus.HashFromHex(cbTxid)
	op := consensus.OutPoint{TxID: txidHash, Index: 0}
	coin, _ := ch.LookupCoin(op)
	tx := &consensus.Tx{Version: 1, Inputs: []consensus.TxIn{{PrevOut: op}}, Outputs: []consensus.TxOut{{Amount: 49 * consensus.UnitsPerCoin, Type: 0, Payload: xonly}}, Witnesses: make([][]byte, 1)}
	msg := consensus.SigHash(consensus.RegTest.ChainTag, tx, []consensus.TxOut{coin.Out}, 0)
	sig, _ := schnorr.Sign(priv, msg[:])
	tx.Witnesses[0] = sig.Serialize()
	rawHex := hexOf(tx.Serialize())
	var decoded map[string]any
	json.Unmarshal(c.must("decoderawtransaction", rawHex), &decoded)
	if decoded["txid"].(string) != tx.TxID().String() {
		t.Fatal("decode")
	}
	var sent string
	json.Unmarshal(c.must("sendrawtransaction", rawHex), &sent)
	if sent != tx.TxID().String() {
		t.Fatal("send")
	}
	var mp []string
	json.Unmarshal(c.must("getrawmempool"), &mp)
	if len(mp) != 1 {
		t.Fatal("mempool")
	}
	// Template includes it; mining via generatetoaddress confirms it.
	var tmpl map[string]any
	json.Unmarshal(c.must("getblocktemplate"), &tmpl)
	if len(tmpl["transactions"].([]any)) != 1 || tmpl["height"].(float64) != 102 {
		t.Fatalf("template: %v", tmpl)
	}
	if tmpl["coinbasevalue"].(float64) != float64(consensus.Subsidy(102)+1*consensus.UnitsPerCoin) {
		t.Fatal("coinbasevalue should include the fee")
	}
	c.must("generatetoaddress", 1, addr)
	json.Unmarshal(c.must("getrawmempool"), &mp)
	if len(mp) != 0 {
		t.Fatal("mempool not cleared")
	}
	// Spent output is gone.
	res, _ := c.call("gettxout", cbTxid, 0)
	if string(res) != "null" {
		t.Fatalf("spent txout should be null, got %s", res)
	}
	// Found in its block.
	var best string
	json.Unmarshal(c.must("getbestblockhash"), &best)
	var got map[string]any
	json.Unmarshal(c.must("getrawtransaction", tx.TxID().String(), best), &got)
	if got["blockhash"].(string) != best {
		t.Fatal("getrawtransaction by block")
	}
	// And via the transaction index, with no block hash.
	var indexed map[string]any
	json.Unmarshal(c.must("getrawtransaction", tx.TxID().String()), &indexed)
	if indexed["blockhash"].(string) != best || indexed["height"].(float64) != 102 {
		t.Fatalf("getrawtransaction by index: %v", indexed)
	}
	if _, e := c.call("getrawtransaction", consensus.TaggedHash("none", nil).String()); e == nil || e.Code != codeNotFound {
		t.Fatal("unknown txid should be not found")
	}

	// submitblock of a duplicate reports "duplicate".
	var raw string
	json.Unmarshal(c.must("getblock", best, 0), &raw)
	var dup string
	json.Unmarshal(c.must("submitblock", raw), &dup)
	if dup != "duplicate" {
		t.Fatalf("submitblock dup: %q", dup)
	}

	// Misc.
	var va map[string]any
	json.Unmarshal(c.must("validateaddress", addr), &va)
	if va["isvalid"] != true {
		t.Fatal("validateaddress")
	}
	json.Unmarshal(c.must("validateaddress", "arg1qbad"), &va)
	if va["isvalid"] != false {
		t.Fatal("validateaddress bad")
	}
	if _, e := c.call("nosuchmethod"); e == nil || e.Code != codeMethodMissing {
		t.Fatal("method missing")
	}
	var info map[string]any
	json.Unmarshal(c.must("getblockchaininfo"), &info)
	if info["chain"] != "regtest" || info["blocks"].(float64) != 102 {
		t.Fatalf("info: %v", info)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i] = digits[c>>4]
		out[2*i+1] = digits[c&15]
	}
	return string(out)
}
