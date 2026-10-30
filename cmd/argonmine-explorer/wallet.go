package main

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
)

// What a wallet without a node of its own needs (the browser wallet,
// package litewallet): the coins of its address, which of them a waiting
// transaction already spends, its unconfirmed outputs, and a way to hand a
// signed transaction to the network. Transactions carry their own proof
// (every input is signed), so anyone may broadcast through any node; the
// explorer only refuses malformed ones and ones that would replace a
// payment still waiting, so a second payment can never quietly undo the
// first.

const (
	maxCoinsListed = 1000
	maxTxHexBytes  = 2*consensus.MaxTxSize + 64
)

// pendingCoin is an unconfirmed output paying the address. Own: every
// input of its transaction was the address's own coin (its change), which
// the wallet may spend before it confirms.
type pendingCoin struct {
	TxID   string           `json:"txid"`
	Index  uint32           `json:"index"`
	Amount consensus.Amount `json:"amount"`
	Asset  string           `json:"asset,omitempty"`
	Own    bool             `json:"own"`
}

// apiUTXOs serves GET /api/utxos/{address}.
func (s *server) apiUTXOs(w http.ResponseWriter, r *http.Request) {
	addr := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/utxos/"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := wallet.DecodeAddress(s.params.HRP, addr); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": "not a valid address for this network"})
		return
	}
	h, tip, publicErr := s.ix.status()
	coins, all := s.ix.coins(addr, maxCoinsListed)
	mp := s.mempoolView()
	for i := range coins {
		coins[i].SpentBy = mp.SpentBy[outpointKey(coins[i].TxID, coins[i].Index)]
	}
	pending := []pendingCoin{}
	touching := []string{}
	for _, rw := range mp.All {
		mine := false
		own := len(rw.Spends) > 0
		for _, c := range rw.Spends {
			if c.Addr == addr {
				mine = true
			} else {
				own = false
			}
		}
		for _, o := range rw.Pays {
			if o.Addr != addr {
				continue
			}
			mine = true
			if mp.SpentBy[outpointKey(rw.TxID, o.Index)] != "" {
				continue
			}
			pending = append(pending, pendingCoin{TxID: rw.TxID, Index: o.Index, Amount: o.Amount, Asset: o.Asset, Own: own})
		}
		if mine {
			touching = append(touching, rw.TxID)
		}
	}
	if mp.Error != "" && publicErr == "" {
		publicErr = mp.Error
	}
	json.NewEncoder(w).Encode(map[string]any{
		"address": addr, "indexedHeight": h, "nodeHeight": tip, "error": publicErr,
		"coins": coins, "complete": all, "pending": pending, "mempool": touching, "mempoolComplete": mp.Complete && mp.Error == "",
	})
}

// apiTx serves POST /api/tx: {"hex": signed transaction} -> {"txid"}.
func (s *server) apiTx(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "POST, OPTIONS")
		fail(http.StatusMethodNotAllowed, "use POST")
		return
	}
	if !s.limits.tx.allow(s.clientIP(r), time.Now()) {
		w.Header().Set("Retry-After", "2")
		fail(http.StatusTooManyRequests, "Too many payments at once. Wait a moment and try again.")
		return
	}
	var req struct {
		Hex string `json:"hex"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTxHexBytes+1024))
	if err != nil || json.Unmarshal(body, &req) != nil {
		fail(http.StatusBadRequest, "The payment could not be read.")
		return
	}
	raw, err := hex.DecodeString(strings.TrimSpace(req.Hex))
	if err != nil || len(raw) == 0 || len(raw) > consensus.MaxTxSize {
		fail(http.StatusBadRequest, "The payment could not be read.")
		return
	}
	tx, err := consensus.DecodeTx(raw)
	if err != nil || consensus.CheckTxStateless(tx) != nil || tx.IsCoinbase() {
		fail(http.StatusBadRequest, "The payment is not a valid transaction.")
		return
	}
	txid := tx.TxID().String()
	mp := s.mempoolView()
	for _, in := range tx.Inputs {
		if by := mp.SpentBy[outpointKey(in.PrevOut.TxID.String(), in.PrevOut.Index)]; by != "" && by != txid {
			fail(http.StatusConflict, "These coins are already being spent by a payment that is waiting to confirm. Wait for it, then try again.")
			return
		}
	}
	got, err := s.client.SendRawTransaction(hex.EncodeToString(raw))
	if err != nil {
		s.logNode("broadcast", err)
		fail(http.StatusUnprocessableEntity, broadcastReason(err))
		return
	}
	s.mpCache.expire()
	json.NewEncoder(w).Encode(map[string]string{"txid": got})
}

// broadcastReason says in plain words why the node refused a transaction,
// without passing on its internal detail.
func broadcastReason(err error) string {
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "unavailable") || strings.Contains(m, "connect") || strings.Contains(m, "timeout") || strings.Contains(m, "refused"):
		return nodeUnavailable
	case strings.Contains(m, "already") || strings.Contains(m, "spent") || strings.Contains(m, "missing") || strings.Contains(m, "unknown input") || strings.Contains(m, "conflict"):
		return "Some of these coins are already spent. Refresh your wallet and try again."
	case strings.Contains(m, "fee"):
		return "The network fee is too low for this payment."
	case strings.Contains(m, "immature") || strings.Contains(m, "maturity"):
		return "Mining rewards can be spent 100 blocks after they are mined."
	case strings.Contains(m, "chain"):
		return "Too many payments are waiting to confirm. Wait for the next block, then try again."
	}
	return "The network refused this payment."
}
