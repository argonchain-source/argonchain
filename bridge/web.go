package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"argonmine/consensus"
	"argonmine/ui"
	"argonmine/wallet"
)

const (
	apiListMax   = 200             // deposits or redemptions in one answer
	apiFilterMax = 100             // addresses or transaction ids in one filter
	snapshotTTL  = 3 * time.Second // how long /api/status and the unfiltered lists are served from memory
)

// Handler serves the public bridge page and API, and the authenticated
// guardian-to-guardian endpoints. Every /api/* response carries
// Access-Control-Allow-Origin: * so the desktop app and the website can read
// the status from another origin, and every response the security headers
// (securityHeaders).
func (s *Service) Handler() http.Handler {
	return securityHeaders(s.mux(), s.pageCSP())
}

func (s *Service) mux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/assets/", ui.Handler())
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.snapshot().status) })
	// ?to=addr1,addr2 keeps only deposits paid to those addresses (a wallet
	// following its own incoming bridge transfers), from the in-memory index.
	mux.HandleFunc("/api/deposits", func(w http.ResponseWriter, r *http.Request) {
		var d []Inbound
		if want := csvList(r.URL.Query().Get("to")); want != nil {
			if len(want) > apiFilterMax {
				apiError(w, http.StatusBadRequest, "at most 100 addresses")
				return
			}
			d = s.st.InboundsTo(want, apiListMax)
		} else {
			d = s.snapshot().deposits
		}
		out := make([]Inbound, len(d))
		for i := range d {
			out[i] = publicInbound(d[i])
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/redemption/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/redemption/"))
		x, ok := s.st.GetRedemption(id)
		if !ok {
			apiError(w, http.StatusNotFound, "no redemption with that transaction id")
			return
		}
		writeJSON(w, publicRedemption(*x))
	})
	// ?txids=a,b keeps only the redemptions for those burn transactions (a
	// wallet following the payouts of its own burns), read by key.
	mux.HandleFunc("/api/redemptions", func(w http.ResponseWriter, r *http.Request) {
		var x []Redemption
		if want := csvList(r.URL.Query().Get("txids")); want != nil {
			if len(want) > apiFilterMax {
				apiError(w, http.StatusBadRequest, "at most 100 transaction ids")
				return
			}
			for _, id := range want {
				if y, ok := s.st.GetRedemption(id); ok {
					x = append(x, *y)
				}
			}
			sort.Slice(x, func(i, j int) bool {
				if x[i].Height != x[j].Height {
					return x[i].Height > x[j].Height
				}
				return x[i].TxID > x[j].TxID
			})
		} else {
			x = s.snapshot().redemptions
		}
		out := make([]Redemption, len(x))
		for i := range x {
			out[i] = publicRedemption(x[i])
		}
		writeJSON(w, out)
	})
	// /api/deposit/{chain_id}/{id}, /api/deposit/{0x tx hash}, or
	// /api/deposit/{id} on the first chain.
	mux.HandleFunc("/api/deposit/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.ToLower(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/deposit/"), "/"))
		if strings.HasPrefix(key, "0x") {
			if d, ok := s.InboundByTx(key); ok {
				writeJSON(w, publicInbound(*d))
				return
			}
		} else if parts := strings.Split(key, "/"); len(parts) == 2 {
			chain, err1 := strconv.ParseUint(parts[0], 10, 64)
			id, err2 := strconv.ParseUint(parts[1], 10, 64)
			if err1 == nil && err2 == nil {
				if d, ok := s.Inbound(chain, id); ok {
					writeJSON(w, publicInbound(*d))
					return
				}
			}
		} else if id, err := strconv.ParseUint(key, 10, 64); err == nil {
			if d, ok := s.Inbound(s.chains[0].ID, id); ok {
				writeJSON(w, publicInbound(*d))
				return
			}
		}
		apiError(w, http.StatusNotFound, "unknown deposit")
	})
	// Hand-off: a phone (which holds no keys) prepares a redemption and the
	// desktop app, which holds the keys, picks it up with a short code. The
	// request carries nothing secret: amount, chain, token and payout address.
	mux.HandleFunc("/api/handoff", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
		default:
			apiError(w, http.StatusMethodNotAllowed, "POST a redemption to get a code")
			return
		}
		ip := s.clientIP(r)
		if !s.handoffPost.allow(ip, time.Now()) {
			w.Header().Set("Retry-After", "20")
			apiError(w, http.StatusTooManyRequests, "too many hand-offs; wait a little")
			return
		}
		var req struct {
			Kind    string          `json:"kind"`  // "redeem" (default) or "send"
			Asset   string          `json:"asset"` // send: "uARG" (default) or "ARG"
			Amount  string          `json:"amount"`
			ChainID json.RawMessage `json:"chain_id"`
			Token   string          `json:"token"`
			To      string          `json:"to"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "the body must be JSON with amount, chain_id, token and to")
			return
		}
		units, err := wallet.ParseAmount(req.Amount)
		if err != nil || units <= 0 {
			apiError(w, http.StatusBadRequest, "amount must be a positive number with at most 8 decimals")
			return
		}
		// A plain transfer of uARG or ARG to another address on this chain:
		// the phone prepares it, the desktop app signs it. Only the coin, the
		// amount and the destination travel. Never to a swap address: sends
		// are between people; swaps go through Swap.
		if req.Kind == "send" {
			to := strings.ToLower(strings.TrimSpace(req.To))
			if _, err := wallet.DecodeAddress(s.cfg.Params.HRP, to); err != nil {
				apiError(w, http.StatusBadRequest, "to must be an address on this network ("+s.cfg.Params.HRP+"1…)")
				return
			}
			if IsSwapAddress(s.cfg.Params.HRP, to) {
				apiError(w, http.StatusBadRequest, "to is a swap address; use Swap")
				return
			}
			asset := "uARG"
			switch strings.ToUpper(strings.TrimSpace(req.Asset)) {
			case "", "UARG":
			case "ARG":
				asset = "ARG"
			default:
				apiError(w, http.StatusBadRequest, "asset must be uARG or ARG")
				return
			}
			h, err := s.handoffs.put(Handoff{Kind: "send", Asset: asset, Amount: wallet.FormatAmount(units), To: to}, ip)
			if err != nil {
				writeAPIErr(w, err)
				return
			}
			writeJSON(w, h)
			return
		}
		if req.Kind != "" && req.Kind != "redeem" {
			apiError(w, http.StatusBadRequest, "kind must be redeem or send")
			return
		}
		chainID, err := strconv.ParseUint(strings.Trim(string(req.ChainID), "\" "), 10, 64)
		if err != nil || chainID == 0 {
			apiError(w, http.StatusBadRequest, "chain_id must be a chain id")
			return
		}
		token, err := ParseAddress(req.Token)
		if err != nil {
			apiError(w, http.StatusBadRequest, "token must be a 0x token address")
			return
		}
		to, err := ParseAddress(req.To)
		if err != nil || to == [20]byte{} {
			apiError(w, http.StatusBadRequest, "to must be a 0x EVM address")
			return
		}
		c, t, err := s.resolve(RedeemMemo{ChainID: chainID, Token: token, To: to})
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		h, err := s.handoffs.put(Handoff{Kind: "redeem", Amount: wallet.FormatAmount(units), ChainID: c.ID, Chain: c.Name, Token: HexAddress(t.Address), TokenSymbol: t.Symbol, To: HexAddress(to)}, ip)
		if err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, h)
	})
	// The desktop app reads a hand-off once: a code is gone after the first
	// successful read.
	mux.HandleFunc("/api/handoff/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		if !s.handoffGet.allow(s.clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "10")
			apiError(w, http.StatusTooManyRequests, "too many requests; wait a few seconds")
			return
		}
		h, ok := s.handoffs.take(strings.TrimPrefix(r.URL.Path, "/api/handoff/"))
		if !ok {
			apiError(w, http.StatusNotFound, "unknown, used or expired code")
			return
		}
		writeJSON(w, h)
	})

	// Deposit addresses (depositaddr.go): the address of an Argonmine address
	// on a chain, registered for the leader to watch, and its progress.
	mux.HandleFunc("/api/deposit-address", s.handleDepositAddress)
	mux.HandleFunc("/api/deposit-address/status", s.handleDepositAddressStatus)
	// Swap addresses (swapaddr.go): the Argonmine address that pays uARG out
	// as a token on a chain to an EVM address. Computed, never stored.
	mux.HandleFunc("/api/swap-address", s.handleSwapAddress)
	// Withdrawals (withdraw.go): a payout's swap address with a memo of its
	// own, and the progress of what is paid to it.
	mux.HandleFunc("/api/withdrawals", s.handleWithdrawals)
	mux.HandleFunc("/api/withdrawal/", s.handleWithdrawal)
	// The QR of a deposit address (or of an Argonmine address) for the page.
	mux.HandleFunc("/qr.svg", serveQR)

	// /api/memo?to=0x…[&chain=<id>&token=0x…]: the burn memo for a payout to
	// that address, on the first chain's first token when chain and token are
	// omitted (BRG1), or on the named chain and token (BRG2).
	mux.HandleFunc("/api/memo", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		addr, err := ParseAddress(q.Get("to"))
		if err != nil {
			apiError(w, http.StatusBadRequest, "to must be a 0x EVM address")
			return
		}
		out := map[string]any{"asset": s.asset.String(), "to": HexAddress(addr)}
		chainQ, tokenQ := strings.TrimSpace(q.Get("chain")), strings.TrimSpace(q.Get("token"))
		if chainQ == "" && tokenQ == "" {
			c, t := s.chains[0], s.chains[0].Tokens[0]
			out["memo"] = hex.EncodeToString(EncodeMemo(addr))
			out["chain_id"], out["chain"], out["token"], out["token_symbol"] = c.ID, c.Name, HexAddress(t.Address), t.Symbol
			writeJSON(w, out)
			return
		}
		chainID, err := strconv.ParseUint(chainQ, 10, 64)
		if err != nil || chainID == 0 {
			apiError(w, http.StatusBadRequest, "chain must be a chain id")
			return
		}
		token, err := ParseAddress(tokenQ)
		if err != nil {
			apiError(w, http.StatusBadRequest, "token must be a 0x token address")
			return
		}
		c, t, err := s.resolve(RedeemMemo{ChainID: chainID, Token: token, To: addr})
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		out["memo"] = hex.EncodeToString(EncodeRedeemMemo(c.ID, t.Address, addr))
		out["chain_id"], out["chain"], out["token"], out["token_symbol"] = c.ID, c.Name, HexAddress(t.Address), t.Symbol
		writeJSON(w, out)
	})

	// Operator commands (ops.go): from this host only, signed with this
	// guardian's own key.
	mux.HandleFunc("/admin/", s.handleAdmin)

	// Guardian API (auth.go): followers only, requests signed by the
	// leader's guardian key, with the shared secret, fresh and never replayed.
	mux.HandleFunc("/guardian/sign-release", s.guardian(func(ctx context.Context, body []byte) (any, error) {
		var req struct {
			TxID string `json:"txid"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		sig, err := s.SignReleaseLocally(ctx, strings.ToLower(req.TxID))
		if err != nil {
			return nil, err
		}
		return map[string]string{"signature": hex.EncodeToString(sig)}, nil
	}))
	mux.HandleFunc("/guardian/nonces", s.guardian(func(ctx context.Context, body []byte) (any, error) {
		var req SignRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		out, err := s.followerNonces(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"nonces": out}, nil
	}))
	mux.HandleFunc("/guardian/sign", s.guardian(func(ctx context.Context, body []byte) (any, error) {
		var req struct {
			ID     string     `json:"id"`
			Nonces [][]string `json:"nonces"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		if len(req.Nonces) > maxIssueInputs+1 {
			return nil, errors.New("too many messages")
		}
		nonces := make([][][]byte, len(req.Nonces))
		for i := range req.Nonces {
			if len(req.Nonces[i]) > s.cfg.Custody.N() {
				return nil, errors.New("too many nonces")
			}
			for _, nh := range req.Nonces[i] {
				nb, err := hex.DecodeString(nh)
				if err != nil {
					return nil, err
				}
				nonces[i] = append(nonces[i], nb)
			}
		}
		partials, err := s.followerSign(ctx, req.ID, nonces)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(partials))
		for i, p := range partials {
			out[i] = hex.EncodeToString(p)
		}
		return map[string]any{"partials": out}, nil
	}))

	mux.HandleFunc("/owner", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write([]byte(strings.NewReplacer(
			"{{HEAD}}", ui.Head(ui.Brand+" vault ownership"),
			"{{BRAND}}", ui.Brand, "{{MARK}}", ui.Mark, "{{UIJS}}", ui.AssetPath("ui.js"),
			"{{TREASURY}}", s.treasuryAddress(),
		).Replace(ownerPage)))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		page := strings.NewReplacer(
			"{{HEAD}}", ui.Head(ui.Brand+" bridge · "+s.cfg.Params.Name),
			"{{BRAND}}", ui.Brand, "{{ARG}}", ui.Coin, "{{MARK}}", ui.Mark, "{{UIJS}}", ui.AssetPath("ui.js"),
			"{{NETWORK}}", s.cfg.Params.Name, "{{HRP}}", s.cfg.Params.HRP,
			"{{ASSET}}", ui.DisplaySymbol(s.cfg.AssetSymbol),
			"{{SEL_APPROVE}}", hex.EncodeToString(SelApprove[:]), "{{SEL_DEPOSIT}}", hex.EncodeToString(SelDeposit[:]),
			"{{SEL_MINT}}", hex.EncodeToString(SelMint[:]),
			"{{TOK_UARG}}", ui.AssetPath("tokens/uarg.svg"), "{{SEL_BALANCE}}", hex.EncodeToString(selBalanceOf[:]), "{{SEL_ALLOWANCE}}", hex.EncodeToString(selAllowance[:]),
			"{{TOPIC_DEPOSIT}}", hex.EncodeToString(topicDeposit[:]),
		).Replace(bridgePage)
		w.Write([]byte(page))
	})
	return mux
}

// treasuryAddress is the address of the network's treasury (the 1% rule,
// consensus/treasury.go), or "" on a network without one.
func (s *Service) treasuryAddress() string {
	k := s.cfg.Params.TreasuryKey
	if len(k) != consensus.P2PKPayloadLen {
		return ""
	}
	a, err := wallet.EncodeAddress(s.cfg.Params.HRP, consensus.OutputTypeP2PK, k)
	if err != nil {
		return ""
	}
	return a
}

// securityHeaders sets on every response what keeps it from being framed,
// sniffed as another type or leaking the page's address (a withdrawal's
// memo rides in ?w=) to the sites it links to. The pages (/ and /owner) get pageCSP; the
// API, the guardian endpoints and the assets get a policy that allows
// nothing, since none of them is a document.
func securityHeaders(next http.Handler, pageCSP string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/" || r.URL.Path == "/owner" {
			h.Set("Content-Security-Policy", pageCSP)
		} else {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// pageCSP is the page's Content-Security-Policy. The page runs its own
// inline script and style, loads /assets/ and /qr.svg from here, shows
// wallet icons (data: or https: images, EIP-6963), and talks to this
// bridge's API and to each chain's public RPC (/api/status chains[].rpc) and
// whatever RPC a wallet names, hence https: in connect-src; a public RPC on
// plain http (a local chain) is allowed by its origin.
func (s *Service) pageCSP() string {
	connect := []string{"'self'", "https:"}
	seen := map[string]bool{}
	for _, c := range s.chains {
		u, err := url.Parse(strings.TrimSpace(c.PublicRPC))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "wss" && u.Scheme != "ws") || strings.ContainsAny(u.Host, " ;,'\"") {
			continue
		}
		if o := u.Scheme + "://" + u.Host; !seen[o] {
			seen[o] = true
			connect = append(connect, o)
		}
	}
	return "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self' data:; " +
		"connect-src " + strings.Join(connect, " ") + "; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

// selAllowance lets the page read allowance(owner, vault) to decide whether the
// next step is the approval or the deposit itself.
var selAllowance = Selector("allowance(address,address)")

// ---- the public API's cache ----

// apiSnapshot is what /api/status and the unfiltered lists serve: computed
// at most once every snapshotTTL, by one request while the others wait, so
// no request decodes the whole state on its own.
type apiSnapshot struct {
	at          time.Time
	status      Status
	deposits    []Inbound    // newest first, at most apiListMax
	redemptions []Redemption // newest first, at most apiListMax
}

func (s *Service) snapshot() *apiSnapshot {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if s.snap != nil && time.Since(s.snap.at) < snapshotTTL {
		return s.snap
	}
	deps, rs := s.st.Inbounds(), s.st.Redemptions()
	snap := &apiSnapshot{at: time.Now(), status: s.statusOf(deps, rs)}
	if len(deps) > apiListMax {
		deps = deps[:apiListMax]
	}
	if len(rs) > apiListMax {
		rs = rs[:apiListMax]
	}
	snap.deposits = append([]Inbound{}, deps...)
	snap.redemptions = append([]Redemption{}, rs...)
	s.snap = snap
	return snap
}

// ---- what errors may say in public ----

var (
	urlInText    = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s"'<>]+`)
	secretInText = regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|access[_-]?token|token|key|secret|auth|password|pass)=[^&\s"'<>]+`)
)

// publicErr strips from an error what the public must not see: a URL (an
// RPC URL often carries an API key in its path or query string) is cut to
// its scheme and host, anything that looks like a credential parameter is
// blanked, and the message is bounded.
func publicErr(msg string) string {
	if msg == "" {
		return ""
	}
	msg = urlInText.ReplaceAllStringFunc(msg, func(u string) string {
		trail := ""
		for len(u) > 0 && strings.ContainsRune(".,;:)]}", rune(u[len(u)-1])) {
			trail = u[len(u)-1:] + trail
			u = u[:len(u)-1]
		}
		pu, err := url.Parse(u)
		if err != nil || pu.Host == "" {
			return "[url]" + trail
		}
		return pu.Scheme + "://" + pu.Host + trail
	})
	msg = secretInText.ReplaceAllString(msg, "$1=[redacted]")
	if r := []rune(msg); len(r) > 400 {
		msg = string(r[:400]) + "…"
	}
	return msg
}

func publicInbound(d Inbound) Inbound          { d.Error = publicErr(d.Error); return d }
func publicRedemption(r Redemption) Redemption { r.Error = publicErr(r.Error); return r }

// ---- hand-offs ----

// Handoff is a redemption (or a send) prepared on one device for another to
// sign: the bridge keeps it for a quarter of an hour behind a ten-character
// code, and hands it out once.
type Handoff struct {
	Code        string `json:"code"`            // ten characters in two groups, "K7M2Q-X9RT4"
	Expires     int64  `json:"expires"`         // unix seconds
	Kind        string `json:"kind"`            // "redeem": burn for a payout on an EVM chain; "send": transfer to an address on this chain
	Asset       string `json:"asset,omitempty"` // send: "uARG" or "ARG"
	Amount      string `json:"amount"`          // decimal, of Asset (uARG for redeem)
	ChainID     uint64 `json:"chain_id,omitempty"`
	Chain       string `json:"chain,omitempty"`
	Token       string `json:"token,omitempty"`
	TokenSymbol string `json:"token_symbol,omitempty"`
	To          string `json:"to"` // 0x payout address (redeem) or an address on this chain (send)

	ip string // who made it, for the per-client limit
}

const (
	handoffTTL     = 15 * time.Minute
	handoffMax     = 5000 // pending across all clients
	handoffPerIP   = 10   // pending per client
	handoffCodeLen = 10
	// handoffAlphabet is base32 without the characters that read alike
	// (0/O, 1/I): 32 symbols, five bits each, fifty bits a code.
	handoffAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
)

type handoffStore struct {
	mu    sync.Mutex
	m     map[string]Handoff // by the code without its separator
	perIP map[string]int
}

func newHandoffStore() *handoffStore {
	return &handoffStore{m: map[string]Handoff{}, perIP: map[string]int{}}
}

func (st *handoffStore) drop(code string) {
	h, ok := st.m[code]
	if !ok {
		return
	}
	delete(st.m, code)
	if st.perIP[h.ip]--; st.perIP[h.ip] <= 0 {
		delete(st.perIP, h.ip)
	}
}

func (st *handoffStore) put(h Handoff, ip string) (Handoff, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for code, old := range st.m {
		if old.Expires <= now.Unix() {
			st.drop(code)
		}
	}
	if len(st.m) >= handoffMax {
		return Handoff{}, &apiErr{http.StatusServiceUnavailable, "too many pending hand-offs; try again in a few minutes"}
	}
	if st.perIP[ip] >= handoffPerIP {
		return Handoff{}, &apiErr{http.StatusTooManyRequests, "too many pending hand-offs from this address; use one or wait for it to expire"}
	}
	for try := 0; try < 20; try++ {
		code, err := newHandoffCode()
		if err != nil {
			return Handoff{}, err
		}
		if _, taken := st.m[code]; taken {
			continue
		}
		h.Code, h.Expires, h.ip = code[:5]+"-"+code[5:], now.Add(handoffTTL).Unix(), ip
		st.m[code] = h
		st.perIP[ip]++
		return h, nil
	}
	return Handoff{}, errors.New("could not allocate a code")
}

// take returns a hand-off and forgets it: a code works once.
func (st *handoffStore) take(code string) (Handoff, bool) {
	code = normalizeHandoffCode(code)
	if code == "" {
		return Handoff{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	h, ok := st.m[code]
	if !ok {
		return Handoff{}, false
	}
	st.drop(code)
	if h.Expires <= time.Now().Unix() {
		return Handoff{}, false
	}
	return h, true
}

func newHandoffCode() (string, error) {
	var b [handoffCodeLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	out := make([]byte, handoffCodeLen)
	for i, x := range b {
		out[i] = handoffAlphabet[x&31]
	}
	return string(out), nil
}

// normalizeHandoffCode accepts a code as typed (any case, with or without
// the separator or spaces) and returns it bare, or "" if it cannot be one.
func normalizeHandoffCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
	if len(s) != handoffCodeLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(handoffAlphabet, s[i]) < 0 {
			return ""
		}
	}
	return s
}

// csvList parses a comma-separated query value into distinct lowercase
// values; nil when the value is empty (no filter).
func csvList(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": publicErr(msg)})
}

// SwapAddressInfo is the answer of GET /api/swap-address.
type SwapAddressInfo struct {
	Address       string `json:"address"` // <hrp>1qswap…
	ChainID       uint64 `json:"chain_id"`
	Chain         string `json:"chain"`
	Token         string `json:"token"`         // symbol
	TokenAddress  string `json:"token_address"` // 0x lowercase
	Decimals      int    `json:"decimals"`
	To            string `json:"to"` // EIP-55
	ReleaseFeeBps uint16 `json:"release_fee_bps"`
}

// handleSwapAddress answers GET /api/swap-address?chain=<id>&token=<symbol>&to=<0x…>.
// Anyone can compute the same address from the three values (SwapAddress);
// the bridge only checks that it pays the token out on that chain and that
// the recipient is not one of its own contracts.
func (s *Service) handleSwapAddress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		apiError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	id, err := strconv.ParseUint(strings.TrimSpace(q.Get("chain")), 10, 64)
	if err != nil || id == 0 {
		apiError(w, http.StatusBadRequest, "chain must be a chain id")
		return
	}
	c, ok := s.byID[id]
	if !ok {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("chain %d is not served by this bridge", id))
		return
	}
	t, err := c.tokenBySymbol(strings.TrimSpace(q.Get("token")))
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := ParseAddress(q.Get("to"))
	if err != nil || to == ([20]byte{}) {
		apiError(w, http.StatusBadRequest, "to must be an EVM address (0x and 40 hex digits)")
		return
	}
	if to == c.Vault || to == c.Router || to == t.Address {
		apiError(w, http.StatusBadRequest, "to is one of the bridge's own contracts; enter your own address")
		return
	}
	addr, err := SwapAddress(s.cfg.Params.HRP, SwapDest{ChainID: c.ID, Symbol: t.Symbol, To: to})
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, bps := c.fees()
	writeJSON(w, SwapAddressInfo{Address: addr, ChainID: c.ID, Chain: c.Name, Token: strings.ToUpper(t.Symbol), TokenAddress: HexAddress(t.Address),
		Decimals: t.Decimals, To: ChecksumAddress(to), ReleaseFeeBps: bps})
}
