package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
)

// Operations (docs/BRIDGE.md, "Pauses", "Stuck items" and "Operator
// commands"): what stops the daemon from acting until a human looks, what it
// reports as stuck, and the operator's commands.

const (
	// pauseL1 stops every issuance and every release on this guardian: set
	// when Argonmine reorganised deeper than -l1-reorg-pause.
	pauseL1 = "l1"
	// adminTag domain-separates the operator's signed requests.
	adminTag = "ArgonmineBridge/admin-request/v1"
	// stuckMax bounds the stuck items in the status.
	stuckMax = 100
)

// pauseScopeEVM is the scope that stops issuance of one chain's deposits.
func pauseScopeEVM(chain uint64) string { return "evm/" + strconv.FormatUint(chain, 10) }

func (s *State) GetPause(scope string) (*Pause, bool) {
	var p Pause
	if !s.get(pauseKey(scope), &p) {
		return nil, false
	}
	return &p, true
}

// pauseChain stops issuance on c until an operator resumes it.
func (s *Service) pauseChain(c *chainState, reason string) error {
	scope := pauseScopeEVM(c.ID)
	if p, ok := s.st.GetPause(scope); ok && p.Reason == reason {
		return nil
	}
	s.cfg.Logf("bridge: ALERT %s", reason)
	return s.st.PutPause(&Pause{Scope: scope, Reason: reason, Since: time.Now().Unix()})
}

// pauseAll stops every issuance and release until an operator resumes them.
func (s *Service) pauseAll(reason string) error {
	if p, ok := s.st.GetPause(pauseL1); ok && p.Reason == reason {
		return nil
	}
	s.cfg.Logf("bridge: ALERT %s", reason)
	return s.st.PutPause(&Pause{Scope: pauseL1, Reason: reason, Since: time.Now().Unix()})
}

func (c *chainState) disagreeReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disagree
}

// issuancePaused is why deposits of c are not issued now ("" when they may
// be): a pause of everything, a pause of the chain, its providers
// disagreeing, or its vault's deposit ids refused or not checked yet.
func (s *Service) issuancePaused(c *chainState) string {
	if p, ok := s.st.GetPause(pauseL1); ok {
		return "issuance and releases are paused on this guardian: " + p.Reason
	}
	if p, ok := s.st.GetPause(pauseScopeEVM(c.ID)); ok {
		return "issuance on " + c.Name + " is paused on this guardian: " + p.Reason
	}
	if why := c.disagreeReason(); why != "" {
		return c.Name + ": its RPC providers disagree (" + why + "); nothing on it is acted on until they agree"
	}
	// A vault whose deposit ids are refused, or not checked yet, has none of
	// its deposits issued (checkVaultIdentity).
	if why := c.identityErr(); why != "" {
		return why
	}
	if s.vaultFirstID(c) == 0 {
		return "the deposit ids of the vault of " + c.Name + " are not checked yet: its deposits are issued once its first scan has checked them"
	}
	return ""
}

// warnOnce reports whether a warning under key may be logged now: at most
// once per interval.
func (s *Service) warnOnce(key string, every time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if last, ok := s.warned[key]; ok && now.Sub(last) < every {
		return false
	}
	s.warned[key] = now
	return true
}

// ---- stuck items ----

// StuckItem is something that has waited longer than -stuck-after: a
// deposit not issued, an issuance not mined, a burn not paid or a release
// transaction not mined.
type StuckItem struct {
	Kind    string `json:"kind"` // "deposit", "issuance", "redemption", "release"
	ID      string `json:"id"`   // "<chain id>#<deposit id>", or the burn's transaction id
	ChainID uint64 `json:"chain_id,omitempty"`
	Since   int64  `json:"since"`  // unix seconds: when it was seen, or sent
	Reason  string `json:"reason"` // why it waits, stripped of URLs and secrets
}

// stuckItems lists what has waited longer than the threshold, oldest first.
func (s *Service) stuckItems(deps []Inbound, rs []Redemption) []StuckItem {
	now := s.now().Unix()
	limit := int64(s.cfg.StuckAfter / time.Second)
	var out []StuckItem
	for _, d := range deps {
		id := fmt.Sprintf("%d#%d", d.Chain, d.ID)
		switch {
		case d.Status == InboundPending && now-d.Created > limit:
			why := d.Error
			if why == "" {
				why = "waits for its block to be final and for the next issuance"
			}
			out = append(out, StuckItem{Kind: "deposit", ID: id, ChainID: d.Chain, Since: d.Created, Reason: publicErr(why)})
		case d.Status == InboundIssued && now-d.Updated > limit:
			out = append(out, StuckItem{Kind: "issuance", ID: id, ChainID: d.Chain, Since: d.Updated, Reason: "issuance " + d.IssueTx + " is not mined yet"})
		}
	}
	for _, r := range rs {
		switch {
		case r.Status == RedemptionPending && now-r.Created > limit:
			why := r.Error
			if why == "" {
				why = fmt.Sprintf("waits for %d Argonmine confirmations", r.Confirmations)
			}
			out = append(out, StuckItem{Kind: "redemption", ID: r.ID(), ChainID: r.Chain, Since: r.Created, Reason: publicErr(why)})
		case r.Status == RedemptionReleased:
			sent := r.Updated
			if r.Send != nil && r.Send.Sent > 0 {
				sent = r.Send.Sent
			}
			if now-sent <= limit {
				continue
			}
			why := r.Error
			if why == "" {
				why = "release transaction " + r.EVMTx + " is not mined yet"
				if r.Send != nil {
					why += fmt.Sprintf(" (nonce %d, %d fee raises)", r.Send.Nonce, r.Send.Bumps)
				}
			}
			out = append(out, StuckItem{Kind: "release", ID: r.ID(), ChainID: r.Chain, Since: sent, Reason: publicErr(why)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > stuckMax {
		out = out[:stuckMax]
	}
	return out
}

// warnStuck logs every stuck item at warn level, once an hour each. It
// looks at most once a minute.
func (s *Service) warnStuck() {
	if !s.warnOnce("stuck-scan", time.Minute) {
		return
	}
	for _, it := range s.stuckItems(s.st.Inbounds(), s.st.Redemptions()) {
		if s.warnOnce("stuck/"+it.Kind+"/"+it.ID, time.Hour) {
			s.cfg.Logf("bridge: WARN stuck %s %s since %s: %s", it.Kind, it.ID, time.Unix(it.Since, 0).UTC().Format(time.RFC3339), it.Reason)
		}
	}
}

// ---- operator commands ----
//
// An operator acts on a running guardian through POST /admin/<action>: only
// from the loopback interface, never through a proxy, and signed with that
// guardian's own L1 key over
//
//	TaggedHash("ArgonmineBridge/admin-request/v1", network ‖ key ‖ path ‖ time ‖ nonce ‖ SHA-256(body))
//
// (`argonmine-bridge resume` and the other commands build it from the key
// file). With the daemon stopped, the same commands open the state
// directory directly. Actions:
//
//	resume             {"scope": "all" | "l1" | "evm/<chain id>"}: lift pauses
//	approve-release    {"txid": "<burn>"}: pay a burn above the last payout tier
//	deposit-unclaimable {"chain_id", "id", "reason"}: a held deposit refunded by hand is never issued
//	reset-vault        {"chain_id"}: drop the chain's candidate vault epoch (pending or refused), built again next round

// AdminResult is the answer of an operator command.
type AdminResult struct {
	Done    string  `json:"done"`
	Pauses  []Pause `json:"pauses"` // what is still paused afterwards
	Message string  `json:"message,omitempty"`
}

type adminRequest struct {
	Scope   string `json:"scope,omitempty"`
	TxID    string `json:"txid,omitempty"`
	ChainID uint64 `json:"chain_id,omitempty"`
	ID      uint64 `json:"id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// adminApply performs one operator action on a state.
func adminApply(st *State, action string, body []byte, logf func(string, ...any)) (*AdminResult, error) {
	var req adminRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("bad request: %w", err)
		}
	}
	res := &AdminResult{}
	switch action {
	case "resume":
		scope := strings.TrimSpace(req.Scope)
		if scope == "" {
			scope = "all"
		}
		var lifted []string
		for _, p := range st.Pauses() {
			if scope == "all" || p.Scope == scope {
				if err := st.DelPause(p.Scope); err != nil {
					return nil, err
				}
				lifted = append(lifted, p.Scope)
				logf("bridge: operator resumed %s (paused since %s: %s)", p.Scope, time.Unix(p.Since, 0).UTC().Format(time.RFC3339), p.Reason)
			}
		}
		if len(lifted) == 0 {
			res.Done = "nothing was paused under " + scope
		} else {
			res.Done = "resumed " + strings.Join(lifted, ", ")
		}
	case "approve-release":
		txid := strings.ToLower(strings.TrimSpace(req.TxID))
		if _, _, err := ParseRedemptionID(txid); err != nil {
			return nil, errors.New("txid must be a burn's transaction id (64 hex digits), or txid:output for a swap address payment")
		}
		if err := st.ApproveRelease(txid); err != nil {
			return nil, err
		}
		logf("bridge: operator approved the release of burn %s above the payout tiers", txid)
		res.Done = "approved the release of burn " + txid + " on this guardian (every guardian that signs must approve it)"
	case "deposit-unclaimable":
		d, ok := st.GetInbound(req.ChainID, req.ID)
		if !ok {
			return nil, fmt.Errorf("deposit %d#%d is not on record", req.ChainID, req.ID)
		}
		if d.Status != InboundPending && d.Status != InboundUnclaimable {
			return nil, fmt.Errorf("deposit %d#%d is %s: only a pending deposit can be marked unclaimable", req.ChainID, req.ID, d.Status)
		}
		if len(st.SignedIssuesFor(InboundRef{Chain: d.Chain, ID: d.ID})) > 0 {
			return nil, fmt.Errorf("deposit %d#%d is in an issuance this guardian signed; it cannot be marked unclaimable", req.ChainID, req.ID)
		}
		why := strings.TrimSpace(req.Reason)
		if why == "" {
			why = "settled by hand"
		}
		d.Status, d.Error, d.Updated = InboundUnclaimable, "marked unclaimable by an operator: "+why, time.Now().Unix()
		if err := st.PutInbound(d); err != nil {
			return nil, err
		}
		logf("bridge: operator marked deposit %d#%d unclaimable: %s", d.Chain, d.ID, why)
		res.Done = fmt.Sprintf("deposit %d#%d is unclaimable on this guardian: it is never issued", d.Chain, d.ID)
	case "reset-vault":
		// A running guardian handles it itself (Service.resetVault); a
		// stopped one keeps no candidate.
		if req.ChainID == 0 {
			return nil, errors.New("chain_id must name the chain")
		}
		ep, ok := st.GetEpoch(req.ChainID)
		res.Done = fmt.Sprintf("this guardian is stopped, and a candidate vault epoch lives only in a running guardian's memory: the next start builds it afresh for chain %d", req.ChainID)
		if ok {
			res.Done += fmt.Sprintf(" (the vault in use on record is %s, paying burns from Argonmine height %d)", ep.Vault, ep.L1Start)
		}
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
	res.Pauses = st.Pauses()
	if res.Pauses == nil {
		res.Pauses = []Pause{}
	}
	return res, nil
}

func releaseApprovalKey(txid string) []byte { return []byte("y/" + txid) }

// ApproveRelease lets a burn above the last payout tier be paid (at the
// last tier's depth) by this guardian.
func (s *State) ApproveRelease(txid string) error {
	return s.putSync(releaseApprovalKey(txid), time.Now().Unix())
}

// ReleaseApproved reports whether an operator approved paying the burn.
func (s *State) ReleaseApproved(txid string) bool { return s.has(releaseApprovalKey(txid)) }

// AdminOffline performs an operator action on a stopped guardian's state
// directory (the daemon holds a lock on it while it runs).
func AdminOffline(statePath, action string, body []byte, logf func(string, ...any)) (*AdminResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	st, err := openState(statePath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return adminApply(st, action, body, logf)
}

func adminPreimage(network string, key []byte, path string, ts int64, nonce, body []byte) []byte {
	return authPreimage(network, key, path, ts, nonce, body)
}

func adminMessage(pre []byte) [32]byte { return [32]byte(consensus.TaggedHash(adminTag, pre)) }

// AdminHeaders signs an operator request to path with the guardian's own
// L1 key, for the daemon of network that holds that key.
func AdminHeaders(network string, key *btcec.PrivateKey, path string, body []byte, now time.Time) (http.Header, error) {
	nonce := make([]byte, authNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	pub := key.PubKey().SerializeCompressed()
	pre := adminPreimage(network, pub, path, now.Unix(), nonce, body)
	msg := adminMessage(pre)
	sig, err := schnorr.Sign(key, msg[:])
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set(hdrGuardianTime, strconv.FormatInt(now.Unix(), 10))
	h.Set(hdrGuardianNonce, hex.EncodeToString(nonce))
	h.Set(hdrGuardianAuth, hex.EncodeToString(sig.Serialize()))
	return h, nil
}

// fromLoopback reports whether a request came straight from this host:
// from a loopback address and not forwarded by a proxy.
func fromLoopback(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip", "X-Forwarded-Host"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleAdmin serves POST /admin/<action>.
func (s *Service) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !fromLoopback(r) {
		http.Error(w, "operator commands are taken from this host only", http.StatusForbidden)
		return
	}
	if !s.cfg.Custody.CanSign() {
		http.Error(w, "this guardian has no key to check operator commands against", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16+1))
	if err != nil || len(body) > 1<<16 {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now()
	ts, err := strconv.ParseInt(r.Header.Get(hdrGuardianTime), 10, 64)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if d := now.Sub(time.Unix(ts, 0)); d > authSkew || d < -authSkew {
		http.Error(w, "unauthorized: the request's time is too far from this host's clock", http.StatusUnauthorized)
		return
	}
	nonce, err := hex.DecodeString(r.Header.Get(hdrGuardianNonce))
	sigb, err2 := hex.DecodeString(r.Header.Get(hdrGuardianAuth))
	if err != nil || err2 != nil || len(nonce) != authNonceLen {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sig, err := schnorr.ParseSignature(sigb)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	own, err := btcec.ParsePubKey(s.cfg.Custody.LocalKey())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	msg := adminMessage(adminPreimage(s.cfg.Params.Name, s.cfg.Custody.LocalKey(), r.URL.Path, ts, nonce, body))
	if !sig.Verify(msg[:], own) {
		http.Error(w, "unauthorized: not signed with this guardian's key", http.StatusUnauthorized)
		return
	}
	if err := s.adminReplay.add(hex.EncodeToString(nonce), ts+int64(authSkew/time.Second)+1, now.Unix()); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/admin/")
	var res *AdminResult
	if action == "reset-vault" {
		res, err = s.resetVault(body)
	} else {
		res, err = adminApply(s.st, action, body, s.cfg.Logf)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.snapMu.Lock()
	s.snap = nil // the next status shows the change
	s.snapMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// resetVault serves reset-vault on a running guardian: it drops the chain's
// candidate epoch (vaultCandidate), pending or refused, with its reason, so
// the next round builds it again from the Argonmine tip and the deposit ids
// on record then, and checks it again. The epoch in use is not touched.
func (s *Service) resetVault(body []byte) (*AdminResult, error) {
	var req adminRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("bad request: %w", err)
		}
	}
	c, ok := s.byID[req.ChainID]
	if !ok {
		return nil, fmt.Errorf("chain %d is not served by this guardian", req.ChainID)
	}
	c.mu.Lock()
	cand, why := c.cand, c.idErr
	var vault string
	var start uint32
	var above uint64
	if cand != nil {
		vault, start, above = cand.Vault, cand.L1Start, cand.AboveID
	}
	c.cand, c.idErr = nil, ""
	c.mu.Unlock()
	res := &AdminResult{Pauses: s.st.Pauses()}
	if res.Pauses == nil {
		res.Pauses = []Pause{}
	}
	if cand == nil {
		res.Done = fmt.Sprintf("no candidate vault waits on %s: %s is the vault in use", c.Name, HexAddress(c.Vault))
		return res, nil
	}
	s.cfg.Logf("bridge: operator reset the candidate epoch of vault %s on %s (burns from Argonmine height %d, deposit ids above %d; %s)", vault, c.Name, start, above, orNone(why))
	res.Done = fmt.Sprintf("dropped the candidate epoch of vault %s on %s (burns from Argonmine height %d, deposit ids above %d): the next round builds it again from the Argonmine tip and the deposit ids on record then, and checks it again", vault, c.Name, start, above)
	if why != "" {
		res.Message = "it was refused: " + publicErr(why)
	}
	return res, nil
}

func orNone(s string) string {
	if s == "" {
		return "not refused"
	}
	return "refused: " + s
}
