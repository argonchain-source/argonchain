package bridge

// Review F (second pass, 2026-10-28): release signature gathering.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// vaultStrictEVM is the fake vault with the signature encoding rules of the
// real Vault._recover, written out here on their own: v must be 27 or 28 (0
// and 1 are taken as 27 and 28), s at most N/2.
type vaultStrictEVM struct{ *fakeEVM }

func vaultAccepts(sig []byte) bool {
	if len(sig) != 65 {
		return false
	}
	v := sig[64]
	if v < 27 {
		v += 27
	}
	return (v == 27 || v == 28) && new(big.Int).SetBytes(sig[32:64]).Cmp(secpHalfN) <= 0
}

func (f vaultStrictEVM) Release(ctx context.Context, req ReleaseRequest) (*TxSend, error) {
	data := req.Data
	if req.Replace != nil {
		data, _ = hex.DecodeString(req.Replace.Data)
	}
	if _, _, _, _, sigs, err := DecodeReleaseCall(data); err == nil {
		for _, s := range sigs {
			if !vaultAccepts(s) {
				return nil, errors.New("execution reverted: BadSignatures")
			}
		}
	}
	return f.fakeEVM.Release(ctx, req)
}

// TestReviewF_NonCanonicalReleaseSignature (fixed 2026-10-28): the leader
// counted a peer's release signature when RecoverSigner gave a vault
// guardian, but RecoverSigner (decred's RecoverCompact) also accepted
// recovery codes 31..34 (the "compressed key" flag) and a high s, which the
// vault refuses (Vault._recover: v in {27, 28}, s <= N/2). One compromised
// follower that re-encoded its own, otherwise valid, signature (v + 4) was
// counted as the second signature, the leader stopped asking, and every
// release reverted in simulation, round after round: one guardian of three
// stopped every payout.
//
// Now a signature counts only in the encoding the vault accepts
// (VaultSignatureEncoding, which RecoverSigner applies too): the re-encoded
// one is ignored, the leader asks the next guardian, and the payout is made.
// This guardian's own signatures are always in that encoding.
func TestReviewF_NonCanonicalReleaseSignature(t *testing.T) {
	t.Parallel()
	// The encoding itself: the vault refuses both variants, and so do
	// VaultSignatureEncoding and RecoverSigner
	// (ReviewFVetoTest.test_nonCanonicalReleaseSignaturesRevert).
	k, _ := secp256k1.GeneratePrivateKey()
	var digest [32]byte
	digest[0] = 7
	good := SignDigest(k, digest)
	plus4 := append([]byte{}, good...)
	plus4[64] += 4
	highS := append([]byte{}, good...)
	s := new(big.Int).Sub(secpN, new(big.Int).SetBytes(good[32:64]))
	s.FillBytes(highS[32:64])
	highS[64] = 55 - good[64] // 27 <-> 28
	for name, sg := range map[string][]byte{"v+4": plus4, "high s": highS, "64 bytes": good[:64], "v=29": append(append([]byte{}, good[:64]...), 29)} {
		a, err := RecoverSigner(digest, sg)
		if err == nil || VaultSignatureEncoding(sg) == nil || vaultAccepts(sg) {
			t.Fatalf("%s: RecoverSigner -> %s (err %v), the vault accepts it: %v; want all three to refuse it", name, HexAddress(a), err, vaultAccepts(sg))
		}
	}
	if !bytes.Equal(lowS(highS), good) || !bytes.Equal(lowS(good), good) {
		t.Fatal("lowS does not turn a high s into the low one")
	}
	// v as 0 or 1 is what the vault takes as 27 or 28.
	zeroV := append(append([]byte{}, good[:64]...), good[64]-27)
	if a, err := RecoverSigner(digest, zeroV); err != nil || a != AddressOf(k.PubKey()) || !vaultAccepts(zeroV) {
		t.Fatalf("v %d: %s %v", zeroV[64], HexAddress(a), err)
	}
	// This guardian's own signatures: always in the vault's encoding.
	for i := 0; i < 300; i++ {
		key, _ := secp256k1.GeneratePrivateKey()
		d := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		sg := SignDigest(key, d)
		if a, err := RecoverSigner(d, sg); err != nil || a != AddressOf(key.PubKey()) || !vaultAccepts(sg) || sg[64] < 27 {
			t.Fatalf("own signature %d: %x (%v)", i, sg, err)
		}
	}

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

	// Follower 1 turns bad: it forwards each sign-release to its own
	// honest daemon and returns that signature with v + 4.
	var asked atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		body, _ := io.ReadAll(r.Body)
		req, _ := http.NewRequest(r.Method, e.urls[1]+r.URL.Path, bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		var ans struct {
			Signature string `json:"signature"`
		}
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &ans) != nil {
			w.WriteHeader(resp.StatusCode)
			w.Write(raw)
			return
		}
		s, _ := hex.DecodeString(ans.Signature)
		if len(s) == 65 {
			s[64] += 4
		}
		json.NewEncoder(w).Encode(map[string]string{"signature": hex.EncodeToString(s)})
	}))
	defer evil.Close()

	e.leader.Stop()
	strict := vaultStrictEVM{e.evm}
	cfg := Config{
		Params: e.leader.cfg.Params, Node: e.client, Chains: e.chains, EVMs: map[uint64]EVM{e.chains[0].ID: strict}, Custody: e.cs[0], EVMKey: e.evmKeys[0],
		Leader: true, Peers: []string{evil.URL, e.urls[2]}, Secret: testSecret, LeaderKey: e.cs[0].LocalKey(), Threshold: 2, L1Confirmations: 3,
		StatePath: filepath.Join(e.dir, "g0"), Logf: t.Logf,
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Stop)
	e.leader = l

	var recipient [20]byte
	recipient[19] = 0xCD
	burn := e.burnFirstCoin(EncodeMemo(recipient), 9)
	e.mine(4, e.uAddr)
	for i := 0; i < 5; i++ {
		e.round()
	}
	r, _ := e.leader.st.GetRedemption(burn)
	_, f2Signed := e.f2.st.SignedBurn(burn)
	t.Logf("after 5 rounds: status=%s attempts=%d error=%q; bad follower asked %d times; honest follower 2 signed: %v; releases %d",
		r.Status, r.Attempts, r.Error, asked.Load(), f2Signed, len(e.evm.releases))
	if asked.Load() == 0 || !f2Signed {
		t.Fatalf("the leader did not ask both followers: bad one %d times, honest one signed %v", asked.Load(), f2Signed)
	}
	if len(e.evm.releases) != 1 || r.Status != RedemptionConfirmed {
		t.Fatalf("one follower re-encoding its signature stopped the payout: %d releases, status %s (%s)", len(e.evm.releases), r.Status, r.Error)
	}
}
