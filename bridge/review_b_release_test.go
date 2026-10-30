package bridge

// Review B (custody and payout core): releases.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestReviewB_ForeignSignatureBlocksRelease: the leader counts any
// signature that recovers to *some* address towards the threshold, without
// checking that the address is a guardian of the vault. One follower that
// answers sign-release with a signature over another digest (compromised,
// or honest with a different chains file) is counted as the second
// signature, the leader stops asking the other followers, and the release
// reverts in simulation every round: one guardian freezes every payout
// although the other two are honest and online (2 of 3 should tolerate one
// faulty guardian). When the same follower merely refuses, the leader asks
// the next one and the burn is paid.
func TestForeignSignatureDoesNotBlockRelease(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	e.fund()
	usdc := e.chains[0].Tokens[0].Address
	var from [20]byte
	e.evm.addDeposit(usdc, from, usd(100, 6), e.uAddr)
	e.evm.advance(10)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}
	e.mine(1, e.uAddr)
	if err := e.round(); err != nil {
		t.Fatal(err)
	}

	// Follower 1 turns bad: it answers every sign-release with a well-formed
	// signature of its own EVM key over a digest of its choosing.
	var refuse atomic.Bool
	var asked atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if refuse.Load() {
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		var other [32]byte
		rand.Read(other[:])
		json.NewEncoder(w).Encode(map[string]string{"signature": hex.EncodeToString(SignDigest(e.evmKeys[1], other))})
	}))
	defer evil.Close()
	e.leader.Stop()
	e.leader = e.mk(0, true, []string{evil.URL, e.urls[2]}, "g0", e.chains, e.evm)

	var recipient [20]byte
	recipient[19] = 0xCD
	burn := e.burnFirstCoin(EncodeMemo(recipient), 9)
	e.mine(4, e.uAddr)
	for i := 0; i < 5; i++ {
		e.round()
	}
	r, _ := e.leader.st.GetRedemption(burn)
	t.Logf("after 5 rounds: status=%s attempts=%d error=%q; bad follower asked %d times; releases %d", r.Status, r.Attempts, r.Error, asked.Load(), len(e.evm.releases))
	// Fixed 2026-10-28: a signature from a key that is not a vault guardian
	// is ignored and the leader asks the next follower, so one faulty
	// guardian cannot stop payouts.
	if len(e.evm.releases) != 1 || r.Status != RedemptionConfirmed {
		t.Fatalf("one bad follower stopped the payout: %d releases, redemption %+v", len(e.evm.releases), r)
	}
	if _, ok := e.f2.st.SignedBurn(burn); !ok {
		t.Fatalf("the honest follower was never asked")
	}
}

// TestReviewB_GuardianRequestReplaysAcrossFollowers: the request preimage
// (network, custody key, path, time, nonce, body) does not name the
// follower it is for, and each follower keeps its own replay cache, so a
// request the leader made for follower 1 is accepted by follower 2 within
// the 60-second window. Whoever sees the leader's traffic to one follower
// (a compromised follower, a TLS-terminating proxy) can replay it to
// another: it runs a /guardian/nonces round under the leader's request id
// (the leader's own call then fails as "id already in use") or consumes a
// /guardian/sign session first (the leader's then fails as unknown).
//
// Left open (2026-10-28, low): binding a request to its follower needs the
// leader to learn each follower's key authentically (a signed identity
// exchange, or peer keys in the configuration) and changes the request
// format that TestGuardianRequestAuth pins. A replayed request gets nothing
// signed that the leader did not ask for (every request is checked again,
// and a release is signed once per burn), so the impact is a failed round,
// retried with a new id.
func TestReviewB_GuardianRequestReplaysAcrossFollowers(t *testing.T) {
	t.Parallel()
	e := newPocEnv(t)
	body := []byte(`{"txid":"` + strings.Repeat("00", 32) + `"}`)
	nonce := make([]byte, authNonceLen)
	rand.Read(nonce)
	h, err := e.leader.guardianHeadersAt("/guardian/sign-release", body, time.Now().Unix(), nonce)
	if err != nil {
		t.Fatal(err)
	}
	for i, u := range e.urls[1:] {
		code, ans := postGuardian(t, u+"/guardian/sign-release", body, h)
		t.Logf("follower %d answered the same signed request with %d %s", i+1, code, strings.TrimSpace(ans))
		if code != http.StatusBadRequest || !strings.Contains(ans, "not found") {
			t.Fatalf("follower %d refused the replayed request: %d %s", i+1, code, ans)
		}
	}
}
