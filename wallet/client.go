package wallet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"argonmine/consensus"
)

// Client is a minimal JSON-RPC client for argonmined.
type Client struct {
	url   string
	token string
	hc    *http.Client
}

// NewClient creates a client for url (e.g. http://127.0.0.1:9332) with a
// bearer token (contents of rpc.cookie).
func NewClient(url, token string) *Client {
	return &Client{url: url, token: token, hc: &http.Client{Timeout: 5 * time.Minute}}
}

// RPCError is an error returned by the node.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Call performs one request and returns the raw result.
func (c *Client) Call(method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("rpc: unauthorized (check the token / rpc.cookie)")
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		// The start of the body only: an answer that failed to parse may be
		// tens of megabytes.
		snippet := string(data[:min(len(data), 256)])
		if len(data) > 256 {
			snippet += "…"
		}
		return nil, fmt.Errorf("rpc: bad response (%d, %d bytes): %s", resp.StatusCode, len(data), snippet)
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

func (c *Client) call(out any, method string, params ...any) error {
	res, err := c.Call(method, params...)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res, out)
}

// ChainInfo mirrors getblockchaininfo.
type ChainInfo struct {
	Chain                string `json:"chain"`
	Blocks               uint32 `json:"blocks"`
	BestBlockHash        string `json:"bestblockhash"`
	InitialBlockDownload bool   `json:"initialblockdownload"`
	Genesis              string `json:"genesis"`
}

// TxInJSON / TxOutJSON / TxJSON / BlockJSON mirror the node's JSON.
// PrevOutJSON describes the output an input spends (present for confirmed
// and mempool transactions served by the node).
type PrevOutJSON struct {
	Amount  consensus.Amount `json:"amount"`
	Type    uint8            `json:"type"`
	Address string           `json:"address"`
	Asset   string           `json:"asset,omitempty"` // non-ARG asset id (hex); empty for ARG
}

type TxInJSON struct {
	TxID     string       `json:"txid"`
	Index    uint32       `json:"index"`
	Coinbase bool         `json:"coinbase"`
	Prevout  *PrevOutJSON `json:"prevout,omitempty"`
}

type TxOutJSON struct {
	N       int              `json:"n"`
	Amount  consensus.Amount `json:"amount"`
	Type    uint8            `json:"type"`
	Address string           `json:"address"`
	Payload string           `json:"payload"`
	Asset   string           `json:"asset,omitempty"` // non-ARG asset id (hex); empty for ARG
}

// IssuanceJSON mirrors one issuance of a version-2 transaction.
type IssuanceJSON struct {
	Asset  string           `json:"asset"`
	Issuer string           `json:"issuer"` // x-only public key, hex
	Symbol string           `json:"symbol"`
	Amount consensus.Amount `json:"amount"`
}

type TxJSON struct {
	TxID       string         `json:"txid"`
	LockHeight uint32         `json:"lock_height"`
	Coinbase   bool           `json:"coinbase"`
	Inputs     []TxInJSON     `json:"inputs"`
	Outputs    []TxOutJSON    `json:"outputs"`
	Issuances  []IssuanceJSON `json:"issuances,omitempty"`
	Hex        string         `json:"hex"`
}

type BlockJSON struct {
	Hash     string   `json:"hash"`
	Height   uint32   `json:"height"`
	PrevHash string   `json:"prevhash"`
	Time     uint64   `json:"time"`
	Tx       []TxJSON `json:"tx"`
}

func (c *Client) ChainInfo() (*ChainInfo, error) {
	var ci ChainInfo
	return &ci, c.call(&ci, "getblockchaininfo")
}

func (c *Client) BlockHash(height uint32) (string, error) {
	var h string
	return h, c.call(&h, "getblockhash", height)
}

func (c *Client) Block(hash string) (*BlockJSON, error) {
	var b BlockJSON
	return &b, c.call(&b, "getblock", hash, 2)
}

func (c *Client) SendRawTransaction(txHex string) (string, error) {
	var id string
	return id, c.call(&id, "sendrawtransaction", txHex)
}

func (c *Client) RawMempool() ([]string, error) {
	var ids []string
	return ids, c.call(&ids, "getrawmempool")
}

func (c *Client) MempoolTx(txid string) (*TxJSON, error) {
	var tx TxJSON
	return &tx, c.call(&tx, "getrawtransaction", txid)
}
