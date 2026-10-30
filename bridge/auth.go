package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"

	"argonmine/consensus"
)

// Guardian-to-guardian requests (docs/BRIDGE.md, "Security model").
//
// Only the leader ever asks the other guardians for anything: nonces and
// partial signatures for an issuance, signatures for a release. Every such
// request carries the time it was made, a random nonce, a BIP-340 signature
// by the leader's own guardian key and an HMAC with the shared secret, both
// over
//
//	network ‖ custody key ‖ path ‖ time ‖ nonce ‖ SHA-256(body)
//
// (the variable-length parts length-prefixed). A follower knows the
// leader's public key from its configuration (-leader-pubkey) and accepts
// nothing else: a request signed by another key, without the secret, more
// than 60 seconds away from its own clock, or with a nonce it has already
// seen is refused. The leader itself takes no guardian requests at all, so
// its public address never serves /guardian/*. The secret alone is worth
// nothing to an attacker; it is a second layer for the case where the
// leader's key leaks without it.

const (
	authTag         = "ArgonmineBridge/guardian-request/v1"
	authSkew        = 60 * time.Second
	authNonceLen    = 16
	replayCacheMax  = 100_000
	guardianBodyMax = 1 << 20

	// MinSecretLen is the shortest shared secret a guardian accepts.
	MinSecretLen = 32

	hdrGuardianTime  = "X-Guardian-Time"
	hdrGuardianNonce = "X-Guardian-Nonce"
	hdrGuardianAuth  = "X-Guardian-Auth"      // BIP-340 signature by the leader's guardian key
	hdrGuardianMAC   = "X-Guardian-Signature" // HMAC-SHA256 with the shared secret
)

// authPreimage is what both the leader's signature and the HMAC cover.
func authPreimage(network string, custody []byte, path string, ts int64, nonce, body []byte) []byte {
	bh := sha256.Sum256(body)
	var w consensus.Writer
	w.VarBytes([]byte(network))
	w.VarBytes(custody)
	w.VarBytes([]byte(path))
	w.I64(ts)
	w.VarBytes(nonce)
	w.Raw(bh[:])
	return w.Bytes()
}

func authMessage(pre []byte) [32]byte { return [32]byte(consensus.TaggedHash(authTag, pre)) }

func authMAC(secret string, pre []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(authTag))
	mac.Write(pre)
	return hex.EncodeToString(mac.Sum(nil))
}

// guardianHeaders authenticates one request of the leader to path with body.
func (s *Service) guardianHeaders(path string, body []byte, now time.Time) (http.Header, error) {
	nonce := make([]byte, authNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.guardianHeadersAt(path, body, now.Unix(), nonce)
}

func (s *Service) guardianHeadersAt(path string, body []byte, ts int64, nonce []byte) (http.Header, error) {
	pre := authPreimage(s.cfg.Params.Name, s.cfg.Custody.XOnly(), path, ts, nonce, body)
	sig, err := s.cfg.Custody.SignAuth(authMessage(pre))
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set(hdrGuardianTime, strconv.FormatInt(ts, 10))
	h.Set(hdrGuardianNonce, hex.EncodeToString(nonce))
	h.Set(hdrGuardianAuth, hex.EncodeToString(sig))
	h.Set(hdrGuardianMAC, authMAC(s.cfg.Secret, pre))
	return h, nil
}

var errUnauthorized = errors.New("unauthorized")

// authenticate checks a guardian request against the leader's key, the
// secret, the clock and the replay cache. When the request is refused it
// returns the HTTP status to answer with and the reason.
func (s *Service) authenticate(r *http.Request, body []byte, now time.Time) (int, error) {
	if s.cfg.Leader {
		return http.StatusForbidden, errors.New("the leader takes no guardian requests")
	}
	if s.leaderKey == nil || s.cfg.Secret == "" || !s.cfg.Custody.CanSign() {
		return http.StatusServiceUnavailable, errors.New("this guardian is not configured to sign")
	}
	ts, err := strconv.ParseInt(r.Header.Get(hdrGuardianTime), 10, 64)
	if err != nil {
		return http.StatusUnauthorized, errUnauthorized
	}
	if d := now.Sub(time.Unix(ts, 0)); d > authSkew || d < -authSkew {
		return http.StatusUnauthorized, fmt.Errorf("unauthorized: the request is %v away from this guardian's clock (at most %v)", d.Round(time.Second), authSkew)
	}
	nonce, err := hex.DecodeString(r.Header.Get(hdrGuardianNonce))
	if err != nil || len(nonce) != authNonceLen {
		return http.StatusUnauthorized, errUnauthorized
	}
	pre := authPreimage(s.cfg.Params.Name, s.cfg.Custody.XOnly(), r.URL.Path, ts, nonce, body)
	if !hmac.Equal([]byte(authMAC(s.cfg.Secret, pre)), []byte(strings.ToLower(r.Header.Get(hdrGuardianMAC)))) {
		return http.StatusUnauthorized, errUnauthorized
	}
	sigb, err := hex.DecodeString(r.Header.Get(hdrGuardianAuth))
	if err != nil {
		return http.StatusUnauthorized, errUnauthorized
	}
	sig, err := schnorr.ParseSignature(sigb)
	if err != nil {
		return http.StatusUnauthorized, errUnauthorized
	}
	msg := authMessage(pre)
	if !sig.Verify(msg[:], s.leaderKey) {
		return http.StatusUnauthorized, errUnauthorized
	}
	// Only now, with the request proven to come from the leader, is its nonce
	// remembered: nobody else can fill the cache.
	if err := s.replay.add(hex.EncodeToString(nonce), ts+int64(authSkew/time.Second)+1, now.Unix()); err != nil {
		return http.StatusUnauthorized, err
	}
	return 0, nil
}

// replayCache remembers the nonces of accepted guardian requests until their
// timestamps are too old to be accepted anyway.
type replayCache struct {
	mu  sync.Mutex
	m   map[string]int64 // nonce → unix time after which it can be forgotten
	max int
}

func newReplayCache(max int) *replayCache { return &replayCache{m: map[string]int64{}, max: max} }

func (c *replayCache) add(nonce string, until, now int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.m[nonce]; seen {
		return errors.New("unauthorized: replayed request")
	}
	if len(c.m) >= c.max {
		for k, exp := range c.m {
			if exp < now {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			return errors.New("too many recent guardian requests; try again in a minute")
		}
	}
	c.m[nonce] = until
	return nil
}

// parseLeaderKey checks the configured leader key: 33 bytes compressed, one
// of the guardians.
func parseLeaderKey(c *Custody, key []byte) (*btcec.PublicKey, error) {
	if len(key) != 33 {
		return nil, errors.New("bridge: the leader key must be a 33-byte compressed public key")
	}
	if !c.IsSigner(key) {
		return nil, errors.New("bridge: the leader key is not one of the guardians")
	}
	return btcec.ParsePubKey(key)
}

// callPeer sends one authenticated request to another guardian.
func (s *Service) callPeer(ctx context.Context, base, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hdr, err := s.guardianHeaders(path, body, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

// guardian wraps a guardian endpoint: POST only, on followers only, with an
// authenticated request from the leader.
func (s *Service) guardian(fn func(ctx context.Context, body []byte) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Leader {
			http.Error(w, "the leader takes no guardian requests", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, guardianBodyMax+1))
		if err != nil || len(body) > guardianBodyMax {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if code, err := s.authenticate(r, body, time.Now()); err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		out, err := fn(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, out)
	}
}
