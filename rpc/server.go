// Package rpc exposes the node over JSON-RPC 2.0 (HTTP POST). It is the
// interface used by the wallet, the mining app and the explorer.
package rpc

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/miner"
	"argonmine/p2p"
	"argonmine/wallet"
)

// Config for the RPC server.
type Config struct {
	Listen string // e.g. 127.0.0.1:9332
	Token  string // bearer token; empty disables auth (localhost only!)
}

// Deps are the node components the RPC methods operate on.
type Deps struct {
	Params  *consensus.Params
	Chain   *chain.Chain
	Pool    *mempool.Pool
	P2P     *p2p.Server  // may be nil
	Miner   *miner.Miner // may be nil
	Version string
	Started time.Time
	Logf    func(string, ...any)
}

// Server is the HTTP JSON-RPC server.
type Server struct {
	cfg     Config
	d       Deps
	http    *http.Server
	methods map[string]method
	ln      net.Listener
}

type method func(params []json.RawMessage) (any, error)

// Error codes (JSON-RPC and Bitcoin-compatible where sensible).
const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeMethodMissing = -32601
	codeInvalidParams = -32602
	codeInternal      = -32603
	codeMisc          = -1
	codeNotFound      = -5
	codeVerify        = -26
)

// Error is a JSON-RPC error with a code.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func rpcErr(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

type request struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// New builds the server; call Start.
func New(cfg Config, d Deps) *Server {
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	if d.Version == "" {
		d.Version = "1.1.0"
	}
	s := &Server{cfg: cfg, d: d}
	s.methods = map[string]method{
		"help":                 s.help,
		"uptime":               s.uptime,
		"getblockchaininfo":    s.getblockchaininfo,
		"getblockcount":        s.getblockcount,
		"getbestblockhash":     s.getbestblockhash,
		"getdifficulty":        s.getdifficulty,
		"getblockhash":         s.getblockhash,
		"getblockheader":       s.getblockheader,
		"getblock":             s.getblock,
		"getrawtransaction":    s.getrawtransaction,
		"decoderawtransaction": s.decoderawtransaction,
		"gettxout":             s.gettxout,
		"getrawmempool":        s.getrawmempool,
		"getmempoolinfo":       s.getmempoolinfo,
		"sendrawtransaction":   s.sendrawtransaction,
		"getblocktemplate":     s.getblocktemplate,
		"submitblock":          s.submitblock,
		"getmininginfo":        s.getmininginfo,
		"getpeerinfo":          s.getpeerinfo,
		"getnetworkinfo":       s.getnetworkinfo,
		"validateaddress":      s.validateaddress,
		"generatetoaddress":    s.generatetoaddress,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.http = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second}
	return s
}

// Start listens and serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.d.Logf("rpc: serve: %v", err)
		}
	}()
	s.d.Logf("rpc: listening on %s", ln.Addr())
	return nil
}

// Addr is the bound address.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Stop shuts the server down.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(s.cfg.Token)) == 1
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Token)) == 1
	}
	return false
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="argonmine"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, response{JSONRPC: "2.0", Error: rpcErr(codeParse, "parse error: %v", err)})
		return
	}
	resp := s.dispatch(&req)
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func (s *Server) dispatch(req *request) response {
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if req.ID == nil {
		resp.ID = json.RawMessage("null")
	}
	m, ok := s.methods[req.Method]
	if !ok {
		resp.Error = rpcErr(codeMethodMissing, "method not found: %s", req.Method)
		return resp
	}
	result, err := m(req.Params)
	if err != nil {
		var re *Error
		if errors.As(err, &re) {
			resp.Error = re
		} else {
			resp.Error = rpcErr(codeMisc, "%s", err.Error())
		}
		return resp
	}
	if result == nil {
		resp.Result = json.RawMessage("null")
	} else {
		resp.Result = result
	}
	return resp
}

// ---- parameter helpers ----

func paramString(params []json.RawMessage, i int) (string, error) {
	if i >= len(params) {
		return "", rpcErr(codeInvalidParams, "missing parameter %d", i)
	}
	var s string
	if err := json.Unmarshal(params[i], &s); err != nil {
		return "", rpcErr(codeInvalidParams, "parameter %d must be a string", i)
	}
	return s, nil
}

func paramUint(params []json.RawMessage, i int) (uint64, error) {
	if i >= len(params) {
		return 0, rpcErr(codeInvalidParams, "missing parameter %d", i)
	}
	var n float64
	if err := json.Unmarshal(params[i], &n); err != nil || n < 0 || n != float64(uint64(n)) {
		return 0, rpcErr(codeInvalidParams, "parameter %d must be a non-negative integer", i)
	}
	return uint64(n), nil
}

func paramHash(params []json.RawMessage, i int) (consensus.Hash, error) {
	s, err := paramString(params, i)
	if err != nil {
		return consensus.Hash{}, err
	}
	h, err := consensus.HashFromHex(s)
	if err != nil {
		return consensus.Hash{}, rpcErr(codeInvalidParams, "parameter %d: bad hash", i)
	}
	return h, nil
}

// ---- JSON shapes ----

func (s *Server) address(o *consensus.TxOut) string {
	if o.Type != consensus.OutputTypeP2PK {
		return ""
	}
	a, err := wallet.EncodeAddress(s.d.Params.HRP, o.Type, o.Payload)
	if err != nil {
		return ""
	}
	return a
}

func formatCoin(a consensus.Amount) string {
	neg := a < 0
	if neg {
		a = -a
	}
	whole := a / consensus.UnitsPerCoin
	frac := a % consensus.UnitsPerCoin
	s := fmt.Sprintf("%d.%08d", whole, frac)
	if neg {
		s = "-" + s
	}
	return s
}

func (s *Server) txJSON(tx *consensus.Tx, blockHash *consensus.Hash, height int64, prevouts map[consensus.OutPoint]consensus.TxOut) map[string]any {
	ins := make([]map[string]any, len(tx.Inputs))
	for i, in := range tx.Inputs {
		ins[i] = map[string]any{
			"txid":    in.PrevOut.TxID.String(),
			"index":   in.PrevOut.Index,
			"witness": hex.EncodeToString(tx.Witnesses[i]),
		}
		if in.PrevOut == consensus.NullOutPoint {
			ins[i]["coinbase"] = true
		}
		if po, ok := prevouts[in.PrevOut]; ok {
			pj := map[string]any{"amount": po.Amount, "coin": formatCoin(po.Amount), "type": po.Type}
			if addr := s.address(&po); addr != "" {
				pj["address"] = addr
			}
			if po.Asset != consensus.NativeAsset {
				pj["asset"] = po.Asset.String()
			}
			ins[i]["prevout"] = pj
		}
	}
	outs := make([]map[string]any, len(tx.Outputs))
	for i := range tx.Outputs {
		o := &tx.Outputs[i]
		outs[i] = map[string]any{
			"n":       i,
			"amount":  o.Amount,
			"coin":    formatCoin(o.Amount),
			"type":    o.Type,
			"payload": hex.EncodeToString(o.Payload),
		}
		if addr := s.address(o); addr != "" {
			outs[i]["address"] = addr
		}
		if o.Asset != consensus.NativeAsset {
			outs[i]["asset"] = o.Asset.String()
		}
	}
	var issuances []map[string]any
	for _, is := range tx.Issuances {
		issuances = append(issuances, map[string]any{"asset": is.Asset.String(), "issuer": hex.EncodeToString(is.Issuer[:]), "symbol": is.Symbol, "amount": is.Amount})
	}
	j := map[string]any{
		"txid":        tx.TxID().String(),
		"wtxid":       tx.WTxID().String(),
		"version":     tx.Version,
		"lock_height": tx.LockHeight,
		"size":        tx.SerializedSize(),
		"coinbase":    tx.IsCoinbase(),
		"inputs":      ins,
		"outputs":     outs,
		"hex":         hex.EncodeToString(tx.Serialize()),
	}
	if issuances != nil {
		j["issuances"] = issuances
	}
	if blockHash != nil {
		j["blockhash"] = blockHash.String()
		j["height"] = height
		if tipH := int64(s.d.Chain.Tip().Height); height >= 0 {
			j["confirmations"] = tipH - height + 1
		}
	}
	return j
}

// blockPrevouts returns the output spent by every input of a connected
// block: from undo data for coins that existed before the block, and from
// earlier transactions of the same block otherwise.
func (s *Server) blockPrevouts(blk *consensus.Block, hash consensus.Hash) map[consensus.OutPoint]consensus.TxOut {
	if blk == nil {
		return nil
	}
	out := map[consensus.OutPoint]consensus.TxOut{}
	if undo, err := s.d.Chain.Undo(hash); err == nil {
		for op, coin := range undo {
			out[op] = coin.Out
		}
	}
	byID := make(map[consensus.Hash]*consensus.Tx, len(blk.Txs))
	for _, tx := range blk.Txs {
		byID[tx.TxID()] = tx
	}
	for _, tx := range blk.Txs {
		for _, in := range tx.Inputs {
			if _, ok := out[in.PrevOut]; ok || in.PrevOut == consensus.NullOutPoint {
				continue
			}
			if prev, ok := byID[in.PrevOut.TxID]; ok && int(in.PrevOut.Index) < len(prev.Outputs) {
				out[in.PrevOut] = prev.Outputs[in.PrevOut.Index]
			}
		}
	}
	return out
}

// mempoolPrevouts resolves the inputs of an unconfirmed transaction against
// the UTXO set and the mempool.
func (s *Server) mempoolPrevouts(tx *consensus.Tx) map[consensus.OutPoint]consensus.TxOut {
	out := map[consensus.OutPoint]consensus.TxOut{}
	for _, in := range tx.Inputs {
		if coin, ok := s.d.Chain.LookupCoin(in.PrevOut); ok {
			out[in.PrevOut] = coin.Out
			continue
		}
		if ptx, ok := s.d.Pool.Get(in.PrevOut.TxID); ok && int(in.PrevOut.Index) < len(ptx.Outputs) {
			out[in.PrevOut] = ptx.Outputs[in.PrevOut.Index]
		}
	}
	return out
}

func (s *Server) headerJSON(n *chain.Node) map[string]any {
	h := &n.Header
	target, _ := consensus.CompactToTarget(h.Bits)
	j := map[string]any{
		"hash":          n.Hash.String(),
		"height":        n.Height,
		"version":       h.Version,
		"prevhash":      h.PrevHash.String(),
		"txroot":        h.TxRoot.String(),
		"witnessroot":   h.WitnessRoot.String(),
		"time":          h.Time,
		"mediantime":    chain.MedianTimePast(n),
		"bits":          fmt.Sprintf("%08x", h.Bits),
		"target":        fmt.Sprintf("%064x", target),
		"nonce":         h.Nonce,
		"difficulty":    s.difficulty(target),
		"chainwork":     fmt.Sprintf("%064x", n.Work),
		"powhash":       consensus.PowHash(h.Serialize()).String(),
		"onmainchain":   s.d.Chain.IsMainChain(n),
		"confirmations": -1,
	}
	if s.d.Chain.IsMainChain(n) {
		j["confirmations"] = s.d.Chain.Tip().Height - n.Height + 1
	}
	if next, ok := s.d.Chain.NodeAtHeight(n.Height + 1); ok && s.d.Chain.IsMainChain(n) {
		j["nextblockhash"] = next.Hash.String()
	}
	return j
}

// difficulty = powLimit / target (difficulty 1 is the easiest allowed block).
func (s *Server) difficulty(target *big.Int) float64 {
	if target.Sign() <= 0 {
		return 0
	}
	f := new(big.Float).SetInt(s.d.Params.PowLimit)
	f.Quo(f, new(big.Float).SetInt(target))
	d, _ := f.Float64()
	return d
}

// ---- methods ----

func (s *Server) help(params []json.RawMessage) (any, error) {
	names := make([]string, 0, len(s.methods))
	for n := range s.methods {
		names = append(names, n)
	}
	return names, nil
}

func (s *Server) uptime(params []json.RawMessage) (any, error) {
	return int64(time.Since(s.d.Started).Seconds()), nil
}

func (s *Server) getblockchaininfo(params []json.RawMessage) (any, error) {
	tip := s.d.Chain.Tip()
	target, _ := consensus.CompactToTarget(tip.Header.Bits)
	syncing := false
	if s.d.P2P != nil {
		syncing = s.d.P2P.Syncing()
	}
	return map[string]any{
		"chain":                s.d.Params.Name,
		"blocks":               tip.Height,
		"bestblockhash":        tip.Hash.String(),
		"difficulty":           s.difficulty(target),
		"nextbits":             fmt.Sprintf("%08x", s.d.Chain.NextBits()),
		"time":                 tip.Header.Time,
		"mediantime":           chain.MedianTimePast(tip),
		"chainwork":            fmt.Sprintf("%064x", tip.Work),
		"initialblockdownload": syncing,
		"genesis":              s.d.Params.GenesisHash.String(),
		"subsidy":              consensus.Subsidy(tip.Height + 1),
	}, nil
}

func (s *Server) getblockcount(params []json.RawMessage) (any, error) {
	return s.d.Chain.Tip().Height, nil
}

func (s *Server) getbestblockhash(params []json.RawMessage) (any, error) {
	return s.d.Chain.Tip().Hash.String(), nil
}

func (s *Server) getdifficulty(params []json.RawMessage) (any, error) {
	target, _ := consensus.CompactToTarget(s.d.Chain.NextBits())
	return s.difficulty(target), nil
}

func (s *Server) getblockhash(params []json.RawMessage) (any, error) {
	h, err := paramUint(params, 0)
	if err != nil {
		return nil, err
	}
	if h > uint64(s.d.Chain.Tip().Height) {
		return nil, rpcErr(codeNotFound, "block height out of range")
	}
	n, ok := s.d.Chain.NodeAtHeight(uint32(h))
	if !ok {
		return nil, rpcErr(codeNotFound, "block not found")
	}
	return n.Hash.String(), nil
}

func (s *Server) getblockheader(params []json.RawMessage) (any, error) {
	h, err := paramHash(params, 0)
	if err != nil {
		return nil, err
	}
	n, ok := s.d.Chain.Node(h)
	if !ok {
		return nil, rpcErr(codeNotFound, "block not found")
	}
	return s.headerJSON(n), nil
}

func (s *Server) getblock(params []json.RawMessage) (any, error) {
	h, err := paramHash(params, 0)
	if err != nil {
		return nil, err
	}
	verbosity := uint64(1)
	if len(params) > 1 {
		if verbosity, err = paramUint(params, 1); err != nil {
			return nil, err
		}
	}
	n, ok := s.d.Chain.Node(h)
	if !ok {
		return nil, rpcErr(codeNotFound, "block not found")
	}
	blk, err := s.d.Chain.Block(h)
	if err != nil {
		return nil, rpcErr(codeNotFound, "block data not available")
	}
	if verbosity == 0 {
		return hex.EncodeToString(blk.Serialize()), nil
	}
	j := s.headerJSON(n)
	j["size"] = blk.SerializedSize()
	j["txcount"] = len(blk.Txs)
	if verbosity == 1 {
		ids := make([]string, len(blk.Txs))
		for i, tx := range blk.Txs {
			ids[i] = tx.TxID().String()
		}
		j["tx"] = ids
	} else {
		prev := s.blockPrevouts(blk, n.Hash)
		txs := make([]map[string]any, len(blk.Txs))
		for i, tx := range blk.Txs {
			txs[i] = s.txJSON(tx, &n.Hash, int64(n.Height), prev)
		}
		j["tx"] = txs
	}
	return j, nil
}

func (s *Server) getrawtransaction(params []json.RawMessage) (any, error) {
	txid, err := paramHash(params, 0)
	if err != nil {
		return nil, err
	}
	if tx, ok := s.d.Pool.Get(txid); ok {
		return s.txJSON(tx, nil, -1, s.mempoolPrevouts(tx)), nil
	}
	if len(params) > 1 {
		bh, err := paramHash(params, 1)
		if err != nil {
			return nil, err
		}
		n, ok := s.d.Chain.Node(bh)
		if !ok {
			return nil, rpcErr(codeNotFound, "block not found")
		}
		blk, err := s.d.Chain.Block(bh)
		if err != nil {
			return nil, rpcErr(codeNotFound, "block data not available")
		}
		for _, tx := range blk.Txs {
			if tx.TxID() == txid {
				return s.txJSON(tx, &n.Hash, int64(n.Height), s.blockPrevouts(blk, n.Hash)), nil
			}
		}
		return nil, rpcErr(codeNotFound, "transaction not in block")
	}
	tx, n, err := s.d.Chain.Transaction(txid)
	if err != nil {
		return nil, rpcErr(codeNotFound, "transaction not found")
	}
	blk, _ := s.d.Chain.Block(n.Hash)
	return s.txJSON(tx, &n.Hash, int64(n.Height), s.blockPrevouts(blk, n.Hash)), nil
}

func (s *Server) decoderawtransaction(params []json.RawMessage) (any, error) {
	hs, err := paramString(params, 0)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(hs)
	if err != nil {
		return nil, rpcErr(codeInvalidParams, "bad hex")
	}
	tx, err := consensus.DecodeTx(raw)
	if err != nil {
		return nil, rpcErr(codeInvalidParams, "decode: %v", err)
	}
	return s.txJSON(tx, nil, -1, s.mempoolPrevouts(tx)), nil
}

func (s *Server) gettxout(params []json.RawMessage) (any, error) {
	txid, err := paramHash(params, 0)
	if err != nil {
		return nil, err
	}
	n, err := paramUint(params, 1)
	if err != nil {
		return nil, err
	}
	coin, ok := s.d.Chain.LookupCoin(consensus.OutPoint{TxID: txid, Index: uint32(n)})
	if !ok {
		return nil, nil
	}
	tip := s.d.Chain.Tip()
	j := map[string]any{
		"bestblock":     tip.Hash.String(),
		"confirmations": tip.Height - coin.Height + 1,
		"amount":        coin.Out.Amount,
		"coin":          formatCoin(coin.Out.Amount),
		"type":          coin.Out.Type,
		"payload":       hex.EncodeToString(coin.Out.Payload),
		"height":        coin.Height,
		"coinbase":      coin.Coinbase,
	}
	if addr := s.address(&coin.Out); addr != "" {
		j["address"] = addr
	}
	if coin.Out.Asset != consensus.NativeAsset {
		j["asset"] = coin.Out.Asset.String()
	}
	return j, nil
}

func (s *Server) getrawmempool(params []json.RawMessage) (any, error) {
	ids := s.d.Pool.TxIDs()
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out, nil
}

func (s *Server) getmempoolinfo(params []json.RawMessage) (any, error) {
	return map[string]any{"size": s.d.Pool.Count(), "bytes": s.d.Pool.Bytes()}, nil
}

func (s *Server) sendrawtransaction(params []json.RawMessage) (any, error) {
	hs, err := paramString(params, 0)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(hs)
	if err != nil {
		return nil, rpcErr(codeInvalidParams, "bad hex")
	}
	tx, err := consensus.DecodeTx(raw)
	if err != nil {
		return nil, rpcErr(codeInvalidParams, "decode: %v", err)
	}
	if err := s.d.Pool.Accept(tx); err != nil && !errors.Is(err, mempool.ErrAlreadyInPool) {
		return nil, rpcErr(codeVerify, "%v", err)
	}
	if s.d.P2P != nil {
		s.d.P2P.BroadcastTx(tx)
	}
	return tx.TxID().String(), nil
}

func (s *Server) getblocktemplate(params []json.RawMessage) (any, error) {
	tip := s.d.Chain.Tip()
	// Build a template with a dummy payout to learn fees and txs; the miner
	// substitutes its own coinbase output.
	dummy := consensus.TxOut{Type: consensus.OutputTypeP2PK, Payload: make([]byte, 32)}
	blk, err := miner.BuildTemplate(s.d.Chain, s.d.Pool, dummy, nil, time.Now().Unix())
	if err != nil {
		return nil, rpcErr(codeInternal, "%v", err)
	}
	target, _ := consensus.CompactToTarget(blk.Header.Bits)
	txs := make([]map[string]any, 0, len(blk.Txs)-1)
	for _, tx := range blk.Txs[1:] {
		txs = append(txs, map[string]any{"data": hex.EncodeToString(tx.Serialize()), "txid": tx.TxID().String()})
	}
	return map[string]any{
		"version":           blk.Header.Version,
		"previousblockhash": tip.Hash.String(),
		"height":            tip.Height + 1,
		"bits":              fmt.Sprintf("%08x", blk.Header.Bits),
		"target":            fmt.Sprintf("%064x", target),
		"curtime":           blk.Header.Time,
		"mintime":           chain.MedianTimePast(tip) + 1,
		"coinbasevalue":     blk.Txs[0].Outputs[0].Amount,
		"transactions":      txs,
		"sizelimit":         consensus.MaxBlockSize,
	}, nil
}

// SubmitSideChain is submitblock's verdict for a valid block that did not
// become the tip: it extends a side branch (its parent is no longer the tip,
// or another block at its height arrived first). null means the block is
// the new tip; any other string is a rejection reason, "duplicate" or an
// "inconclusive-" verdict.
const SubmitSideChain = "inconclusive-side-chain"

func (s *Server) submitblock(params []json.RawMessage) (any, error) {
	hs, err := paramString(params, 0)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(hs)
	if err != nil {
		return nil, rpcErr(codeInvalidParams, "bad hex")
	}
	blk, err := consensus.DecodeBlock(raw)
	if err != nil {
		return nil, rpcErr(codeInvalidParams, "decode: %v", err)
	}
	tipChanged, err := s.d.Chain.ProcessBlock(blk, time.Now().Unix())
	switch {
	case err == nil && tipChanged:
		// The block is the new tip. The chain subscription relays it.
		return nil, nil
	case err == nil:
		// Valid, but on a side branch with no more work than the tip (the
		// chain moved on while it was mined): stored, not the best chain, no
		// reward for now. Callers that only test for null keep treating it as
		// "not accepted as the tip".
		return SubmitSideChain, nil
	case errors.Is(err, chain.ErrDuplicateBlock):
		return "duplicate", nil
	case errors.Is(err, consensus.ErrBlockUnknownParent):
		return "inconclusive-unknown-parent", nil
	default:
		return err.Error(), nil
	}
}

func (s *Server) getmininginfo(params []json.RawMessage) (any, error) {
	target, _ := consensus.CompactToTarget(s.d.Chain.NextBits())
	j := map[string]any{
		"blocks":       s.d.Chain.Tip().Height,
		"difficulty":   s.difficulty(target),
		"pooledtx":     s.d.Pool.Count(),
		"chain":        s.d.Params.Name,
		"mining":       false,
		"hashrate":     0.0,
		"threads":      0,
		"blocksfound":  0,
		"powalgorithm": "argon2id(t=1,m=8MiB,p=1)",
	}
	if s.d.Miner != nil {
		j["mining"] = s.d.Miner.Running()
		j["hashrate"] = s.d.Miner.HashRate()
		j["threads"] = s.d.Miner.Threads()
		j["blocksfound"] = s.d.Miner.BlocksFound()
	}
	return j, nil
}

func (s *Server) getpeerinfo(params []json.RawMessage) (any, error) {
	if s.d.P2P == nil {
		return []any{}, nil
	}
	peers := s.d.P2P.Peers()
	out := make([]map[string]any, len(peers))
	for i, p := range peers {
		out[i] = map[string]any{
			"id": p.ID, "addr": p.Addr, "inbound": p.Inbound, "useragent": p.UserAgent, "version": p.Version,
			"startheight": p.StartHeight, "banscore": p.BanScore, "pingms": p.PingMs,
			"conntime": p.ConnTime.Unix(), "bytesin": p.BytesIn, "bytesout": p.BytesOut,
		}
	}
	return out, nil
}

func (s *Server) getnetworkinfo(params []json.RawMessage) (any, error) {
	j := map[string]any{
		"version":         s.d.Version,
		"protocolversion": p2p.ProtocolVersion,
		"network":         s.d.Params.Name,
		"connections":     0,
		"listening":       "",
	}
	if s.d.P2P != nil {
		j["connections"] = s.d.P2P.PeerCount()
		j["listening"] = s.d.P2P.ListenAddr()
	}
	return j, nil
}

func (s *Server) validateaddress(params []json.RawMessage) (any, error) {
	a, err := paramString(params, 0)
	if err != nil {
		return nil, err
	}
	out, err := wallet.DecodeAddress(s.d.Params.HRP, a)
	if err != nil {
		return map[string]any{"isvalid": false, "error": err.Error()}, nil
	}
	return map[string]any{"isvalid": true, "type": out.Type, "payload": hex.EncodeToString(out.Payload)}, nil
}

func (s *Server) generatetoaddress(params []json.RawMessage) (any, error) {
	if !s.d.Params.ConstantDifficulty {
		return nil, rpcErr(codeMisc, "generatetoaddress is only available on regtest")
	}
	n, err := paramUint(params, 0)
	if err != nil {
		return nil, err
	}
	if n > 1000 {
		return nil, rpcErr(codeInvalidParams, "at most 1000 blocks per call")
	}
	a, err := paramString(params, 1)
	if err != nil {
		return nil, err
	}
	payout, err := wallet.DecodeAddress(s.d.Params.HRP, a)
	if err != nil {
		return nil, rpcErr(codeInvalidParams, "address: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	hashes := make([]string, 0, n)
	for i := uint64(0); i < n; i++ {
		blk, err := miner.MineOne(ctx, s.d.Chain, s.d.Pool, payout, 1)
		if err != nil {
			return nil, rpcErr(codeInternal, "%v", err)
		}
		hashes = append(hashes, blk.Header.Hash().String())
	}
	return hashes, nil
}
