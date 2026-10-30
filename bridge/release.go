package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
)

// Paying out burns (docs/BRIDGE.md, "Redemption", "Payout depth" and
// "Release transactions").

// burnID is the id the vault records a payout under, so it pays each
// redemption once: a burn's transaction id, or for a payment to a swap
// address ("txid:vout") the tagged hash
// H("Argonmine/bridge/swap-payment", txid || vout as 4 bytes big-endian),
// which no transaction id can equal.
func burnID(id string) [32]byte {
	var out [32]byte
	txid, vout, err := ParseRedemptionID(id)
	if err != nil {
		return out
	}
	b, _ := hex.DecodeString(txid)
	if vout == nil {
		copy(out[:], b)
		return out
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], *vout)
	return consensus.TaggedHash("Argonmine/bridge/swap-payment", b, n[:])
}

// ---- payout depth ----

// Tier is one step of the payout depth: a burn of at most UpTo uARG units
// waits Confirmations Argonmine blocks (and never fewer than
// -l1-confirmations). A burn above the last tier is held for a manual
// release.
type Tier struct {
	UpTo          consensus.Amount `json:"up_to"`
	Confirmations uint32           `json:"confirmations"`
}

// ParseTiers reads "1000:6,10000:30,100000:120": amounts in uARG (whole
// dollars or with decimals) and the blocks a burn up to that amount waits,
// both increasing. An empty string means no tiers.
func ParseTiers(s string) ([]Tier, error) {
	var out []Tier
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		a, b, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("tier %q: want amount:blocks", part)
		}
		amt, err := wallet.ParseAmount(strings.TrimSpace(a))
		if err != nil || amt <= 0 {
			return nil, fmt.Errorf("tier %q: the amount must be a positive amount of uARG", part)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(b), 10, 32)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("tier %q: the blocks must be a positive whole number", part)
		}
		if len(out) > 0 && (amt <= out[len(out)-1].UpTo || uint32(n) < out[len(out)-1].Confirmations) {
			return nil, fmt.Errorf("tier %q: amounts must increase and blocks must not decrease", part)
		}
		out = append(out, Tier{UpTo: amt, Confirmations: uint32(n)})
	}
	return out, nil
}

// burnDepth is the Argonmine depth a payout needs before it is paid, for
// amount uARG units: the larger of the bridge payout of its whole
// transaction (Redemption.depthAmount) and of the blocks up to it
// (payoutDepth), since a reorganisation undoes all of them together. ok is
// false above the last tier: such a payout is held for a manual release,
// unless an operator approved it (then the last tier's depth applies).
func (s *Service) burnDepth(txid string, amount consensus.Amount) (uint32, bool) {
	need := s.cfg.L1Confirmations
	tiers := s.cfg.ReleaseTiers
	if len(tiers) == 0 {
		return need, true
	}
	for _, t := range tiers {
		if amount <= t.UpTo {
			if t.Confirmations > need {
				need = t.Confirmations
			}
			return need, true
		}
	}
	if s.st.ReleaseApproved(txid) {
		if last := tiers[len(tiers)-1].Confirmations; last > need {
			need = last
		}
		return need, true
	}
	return 0, false
}

// payoutWindow is W, the number of blocks whose payouts are added up for
// the payout depth: the deepest depth a payout can wait (the last tier's,
// at least -l1-confirmations).
func (s *Service) payoutWindow() uint32 {
	w := s.cfg.L1Confirmations
	if n := len(s.cfg.ReleaseTiers); n > 0 && s.cfg.ReleaseTiers[n-1].Confirmations > w {
		w = s.cfg.ReleaseTiers[n-1].Confirmations
	}
	if w == 0 {
		w = 1
	}
	return w
}

// windowVolume is what the bridge pays out of the payoutWindow blocks up to
// and including height, from this guardian's own records: the sum of every
// redemption on record at a height in (height − W, height], each counted
// with its own amount, whatever its status.
func (s *Service) windowVolume(height uint32) consensus.Amount {
	w := s.payoutWindow()
	from := uint32(0)
	if height >= w {
		from = height - w + 1
	}
	return s.st.PayoutVolume(from, height)
}

// payoutDepth is the depth r needs (burnDepth) for the larger of what its
// whole transaction pays out (depthAmount) and what the bridge pays out of
// the payoutWindow blocks up to its height (windowVolume). A reorganisation
// of those blocks undoes all of their payouts together, so payouts split
// across transactions, each under a tier, add up as one: once the window
// passes a tier, later payouts in it wait deeper, and above the last tier
// they are held for a manual release (docs/BRIDGE.md, "Payout depth").
// basis is the amount the depth was chosen for and window the window's
// volume. Without tiers every payout waits -l1-confirmations.
func (s *Service) payoutDepth(r *Redemption) (need uint32, ok bool, basis, window consensus.Amount) {
	basis = r.depthAmount()
	if len(s.cfg.ReleaseTiers) > 0 {
		if window = s.windowVolume(r.Height); window > basis {
			basis = window
		}
	}
	need, ok = s.burnDepth(r.ID(), basis)
	return need, ok, basis, window
}

// aboveTiers is why r is held for a manual release; window is what the
// bridge pays out of the blocks up to its height (payoutDepth).
func (s *Service) aboveTiers(r *Redemption, window consensus.Amount) string {
	last := s.cfg.ReleaseTiers[len(s.cfg.ReleaseTiers)-1]
	if window > r.depthAmount() {
		from := uint32(0)
		if w := s.payoutWindow(); r.Height >= w {
			from = r.Height - w + 1
		}
		return fmt.Sprintf("the bridge pays out %s %s in Argonmine blocks %d to %d (this payout %s, at height %d), above the largest automatic payout (%s): held for a manual release (argonmine-bridge approve-release %s on every guardian)",
			wallet.FormatAmount(window), s.cfg.AssetSymbol, from, r.Height, wallet.FormatAmount(r.Amount), r.Height, wallet.FormatAmount(last.UpTo), r.ID())
	}
	if r.TxPayout > r.Amount {
		return fmt.Sprintf("its transaction pays out %s %s in all (this payout %s), above the largest automatic payout (%s): held for a manual release (argonmine-bridge approve-release %s on every guardian)",
			wallet.FormatAmount(r.TxPayout), s.cfg.AssetSymbol, wallet.FormatAmount(r.Amount), wallet.FormatAmount(last.UpTo), r.ID())
	}
	return fmt.Sprintf("the burn of %s %s is above the largest automatic payout (%s): held for a manual release (argonmine-bridge approve-release on every guardian)",
		wallet.FormatAmount(r.Amount), s.cfg.AssetSymbol, wallet.FormatAmount(last.UpTo))
}

// guardianRole is a cached answer of the vault's isGuardian.
type guardianRole struct {
	yes bool
	at  time.Time
}

// isVaultGuardian reports whether addr is a guardian of c's vault, from the
// vault itself (cached for an hour). A client that cannot read roles, or a
// read that fails, answers yes: the vault's own check still decides, and an
// RPC hiccup must not stop payouts.
func (s *Service) isVaultGuardian(ctx context.Context, c *chainState, addr [20]byte) bool {
	rr, ok := c.evm.(RoleReader)
	if !ok {
		return true
	}
	c.mu.Lock()
	if g, ok := c.guardianSeen[addr]; ok && time.Since(g.at) < time.Hour {
		c.mu.Unlock()
		return g.yes
	}
	c.mu.Unlock()
	yes, _, err := rr.VaultRoles(ctx, addr)
	if err != nil {
		return true
	}
	c.mu.Lock()
	if c.guardianSeen == nil {
		c.guardianSeen = map[[20]byte]guardianRole{}
	}
	if len(c.guardianSeen) > 1000 {
		c.guardianSeen = map[[20]byte]guardianRole{}
	}
	c.guardianSeen[addr] = guardianRole{yes, time.Now()}
	c.mu.Unlock()
	return yes
}

// ---- the leader's round ----

// holdRedemption records why a pending redemption waits (once).
func (s *Service) holdRedemption(r *Redemption, why string) {
	if r.Error == why {
		return
	}
	r.Error, r.Updated = why, time.Now().Unix()
	s.st.PutRedemption(r)
	s.cfg.Logf("bridge: burn %s (%s %s on %s) waits: %s", r.ID(), FormatBigTokenAmount(mustBig(r.TokenAmount), r.Decimals), r.TokenSymbol, r.ChainName, why)
}

// releaseBlocker is why the vault of c cannot pay r now, from what it says
// at this moment ("" when it can): paused, above its per-release limit,
// past its 24-hour window, or short of the token. None of these fails the
// redemption: it waits and is tried again every round.
func (s *Service) releaseBlocker(ctx context.Context, c *chainState, r *Redemption, token [20]byte, amount *big.Int) (string, error) {
	if why := c.tokenRefused(token); why != "" {
		return why, nil
	}
	if why := c.gasRefused(); why != "" {
		return why, nil
	}
	lim, err := c.evm.ReleaseLimits(ctx, token)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.limits[token] = lim
	c.mu.Unlock()
	if lim.Paused {
		return fmt.Sprintf("the vault on %s is paused: the payout waits until its owner lifts the pause", c.Name), nil
	}
	if lim.MaxRelease != nil && amount.Cmp(lim.MaxRelease) > 0 {
		return fmt.Sprintf("above the vault's per-release limit of %s %s on %s: waits until the limit is raised", FormatBigTokenAmount(lim.MaxRelease, r.Decimals), r.TokenSymbol, c.Name), nil
	}
	if lim.PeriodAvailable != nil && amount.Cmp(lim.PeriodAvailable) > 0 {
		return fmt.Sprintf("waits for room in the vault's 24-hour release window on %s (%s %s available now)", c.Name, FormatBigTokenAmount(lim.PeriodAvailable, r.Decimals), r.TokenSymbol), nil
	}
	bal, err := c.evm.Reserve(ctx, token)
	if err != nil {
		return "", err
	}
	// What the vault holds less what releases already sent will take from it.
	free := new(big.Int).Set(bal)
	for _, o := range s.st.Redemptions() {
		if o.Status == RedemptionReleased && o.ID() != r.ID() && o.Chain == c.ID && strings.EqualFold(o.Token, r.Token) {
			free.Sub(free, mustBig(o.TokenPaid))
		}
	}
	_, releaseBps := c.fees()
	if paid := new(big.Int).Sub(amount, FeeOf(amount, releaseBps)); free.Cmp(paid) < 0 {
		return fmt.Sprintf("the vault on %s holds %s %s beyond the releases in flight, less than the %s this payout needs: waits for liquidity", c.Name, FormatBigTokenAmount(free, r.Decimals), r.TokenSymbol, FormatBigTokenAmount(paid, r.Decimals)), nil
	}
	return "", nil
}

// processRedemptions pays confirmed burns from the vault of the chain each
// one names and follows the release transactions to their end.
func (s *Service) processRedemptions(ctx context.Context) error {
	if p, ok := s.st.GetPause(pauseL1); ok {
		s.cfg.Logf("bridge: releases are paused: %s", p.Reason)
		return nil
	}
	info, err := s.cfg.Node.ChainInfo()
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range s.st.Redemptions() {
		if r.Status != RedemptionPending && r.Status != RedemptionReleased {
			continue
		}
		c, ok := s.byID[r.Chain]
		if !ok {
			continue // the chain was removed from the configuration; it stays as it is
		}
		if why := c.disagreeReason(); why != "" {
			continue // nothing is sent on a chain whose providers disagree
		}
		switch r.Status {
		case RedemptionPending:
			if _, err := s.payableFrom(c); err != nil {
				s.holdRedemption(&r, err.Error()) // the configured vault is not in use yet: nothing is paid from it
				continue
			}
			need, ok, _, window := s.payoutDepth(&r)
			if !ok {
				s.holdRedemption(&r, s.aboveTiers(&r, window))
				continue
			}
			if r.Confirmations != need {
				r.Confirmations = need
				s.st.PutRedemption(&r)
			}
			if info.Blocks+1 < r.Height+need {
				continue
			}
			if err := s.releaseRedemption(ctx, &r, c); err != nil {
				var refused errReleaseRefused
				switch {
				case errors.As(err, &refused):
					r.Status, r.Error, r.Updated = RedemptionUnreleasable, refused.msg, time.Now().Unix()
					s.st.PutRedemption(&r)
					s.cfg.Logf("bridge: burn %s is not released: %s", r.ID(), refused.msg)
				case errors.Is(err, ErrLimitExceeded):
					s.holdRedemption(&r, fmt.Sprintf("waits for room in the vault's release limits on %s", c.Name))
				case errors.Is(err, ErrVaultPaused):
					s.holdRedemption(&r, fmt.Sprintf("the vault on %s is paused: the payout waits until its owner lifts the pause", c.Name))
				default:
					r.Attempts++
					r.Error = publicErr(err.Error())
					r.Updated = time.Now().Unix()
					s.st.PutRedemption(&r)
					errs = append(errs, fmt.Errorf("release %s on %s: %w", r.ID(), c.Name, err))
				}
			}
		case RedemptionReleased:
			if err := s.followRelease(ctx, &r, c); err != nil {
				errs = append(errs, fmt.Errorf("%s: release %s: %w", c.Name, r.ID(), err))
			}
		}
	}
	return errors.Join(errs...)
}

// gasProblem reports whether a node rejected a transaction because the
// sender cannot pay for it.
func gasProblem(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "insufficient funds") || strings.Contains(m, "insufficient balance") || strings.Contains(m, "intrinsic gas")
}

func (s *Service) noGas(c *chainState, what string, err error) {
	sender := "the gas key"
	if a := gasSender(c); a != ([20]byte{}) {
		sender = "the gas key " + HexAddress(a)
	}
	c.setGasErr(fmt.Sprintf("no gas: %s needs %s on %s to send %s (%v)", sender, c.NativeSymbol, c.Name, what, err))
}

// markReleased records that the vault paid r: in the transaction hash
// (one of ours, found by its receipt) when it is known.
func (s *Service) markReleased(ctx context.Context, r *Redemption, c *chainState, hash string) error {
	r.Status, r.Error, r.Updated = RedemptionConfirmed, "", time.Now().Unix()
	if hash != "" {
		r.EVMTx = hash
	}
	s.finaliseRelease(ctx, r, c)
	return s.st.PutRedemption(r)
}

// releaseRedemption checks that the vault can pay the burn now, gathers
// threshold signatures and sends the release from the gas key.
func (s *Service) releaseRedemption(ctx context.Context, r *Redemption, c *chainState) error {
	id := burnID(r.ID())
	done, err := c.evm.IsReleased(ctx, id)
	if err != nil {
		return err
	}
	if done {
		return s.markReleased(ctx, r, c, "")
	}
	to, err := ParseAddress(r.To)
	if err != nil {
		return err
	}
	token, err := ParseAddress(r.Token)
	if err != nil {
		return err
	}
	amount, ok := new(big.Int).SetString(r.TokenAmount, 10)
	if !ok || amount.Sign() <= 0 {
		return errors.New("bad token amount on record")
	}
	why, err := s.releaseBlocker(ctx, c, r, token, amount)
	if err != nil {
		return err
	}
	if why != "" {
		s.holdRedemption(r, why)
		return nil
	}
	digest := ReleaseDigest(c.ID, c.Vault, token, to, amount, id)
	own, err := s.signRelease(r, c, token, to, amount)
	if err != nil {
		return err
	}

	type signed struct {
		addr [20]byte
		sig  []byte
	}
	var sigs []signed
	seen := map[[20]byte]bool{}
	// Only a signature over this digest by one of the vault's guardians, in
	// the encoding the vault accepts, counts: anything else (another digest,
	// another key, a recovery byte or an s the vault refuses) would make the
	// release revert, and must not stop the leader asking the others.
	add := func(sig []byte, from string) {
		if err := VaultSignatureEncoding(sig); err != nil {
			s.cfg.Logf("bridge: %s answered release %s with a signature the vault would refuse (%v); ignored", from, r.ID(), err)
			return
		}
		addr, err := RecoverSigner(digest, sig)
		if err != nil || seen[addr] {
			return
		}
		if !s.isVaultGuardian(ctx, c, addr) {
			s.cfg.Logf("bridge: %s answered release %s with a signature from %s, which is not a guardian of the vault on %s; ignored", from, r.ID(), HexAddress(addr), c.Name)
			return
		}
		seen[addr] = true
		sigs = append(sigs, signed{addr, sig})
	}
	add(own, "this guardian")
	for _, peer := range s.cfg.Peers {
		if len(sigs) >= s.cfg.Threshold {
			break
		}
		var resp struct {
			Signature string `json:"signature"`
		}
		if err := s.callPeer(ctx, peer, "/guardian/sign-release", map[string]any{"txid": r.ID()}, &resp); err != nil {
			s.cfg.Logf("bridge: %s declined to sign release %s: %v", peer, r.ID(), err)
			continue
		}
		if sig, err := hex.DecodeString(resp.Signature); err == nil {
			add(sig, peer)
		}
	}
	if len(sigs) < s.cfg.Threshold {
		return fmt.Errorf("only %d of %d guardian signatures", len(sigs), s.cfg.Threshold)
	}
	sort.Slice(sigs, func(i, j int) bool { return bytes.Compare(sigs[i].addr[:], sigs[j].addr[:]) < 0 })
	raw := make([][]byte, len(sigs))
	for i, sg := range sigs {
		raw[i] = sg.sig
	}
	tx, err := c.evm.Release(ctx, ReleaseRequest{Data: EncodeReleaseCall(token, to, amount, id, raw), MaxGasPrice: c.MaxGasPrice})
	if err != nil {
		if errors.Is(err, ErrAlreadyReleased) {
			return s.markReleased(ctx, r, c, "")
		}
		if gasProblem(err) {
			s.noGas(c, "release transactions", err)
		}
		return err
	}
	c.setGasErr("")
	now := s.now().Unix()
	tx.Sent, tx.Last = now, now
	r.Status, r.Send, r.EVMTx, r.Error = RedemptionReleased, tx, tx.Hash(), ""
	r.applyReleaseFee(c) // the vault takes the fee at its rate when the release is mined; this is the latest read of it
	r.Updated = time.Now().Unix()
	s.cfg.Logf("bridge: released %s %s on %s for burn %s: pays %s, fee %s (%s, nonce %d)", FormatBigTokenAmount(amount, r.Decimals), r.TokenSymbol, c.Name, r.ID(), FormatBigTokenAmount(mustBig(r.TokenPaid), r.Decimals), FormatBigTokenAmount(mustBig(r.TokenFee), r.Decimals), tx.Hash(), tx.Nonce)
	return s.st.PutRedemption(r)
}

// followRelease follows a sent release to its end. The vault's released()
// says whether the burn was paid, whichever of our transactions did it. A
// release that reverted goes back to pending. One that is not mined after
// the release timeout is sent again with the same nonce at higher fees (up
// to the chain's cap); one whose nonce went to another transaction is sent
// again with a new nonce. The vault pays a burn once, so none of this can
// pay twice.
func (s *Service) followRelease(ctx context.Context, r *Redemption, c *chainState) error {
	id := burnID(r.ID())
	hashes := []string{r.EVMTx}
	if r.Send != nil && len(r.Send.Hashes) > 0 {
		hashes = r.Send.Hashes
	}
	done, err := c.evm.IsReleased(ctx, id)
	if err != nil {
		return err
	}
	if done {
		for i := len(hashes) - 1; i >= 0; i-- {
			if st, _, err := c.evm.Receipt(ctx, hashes[i]); err == nil && st == ReceiptOK {
				if hashes[i] != r.Send.Hash() && r.Send != nil {
					s.cfg.Logf("bridge: release %s on %s was paid by an earlier transaction of it, %s", r.ID(), c.Name, hashes[i])
				}
				return s.markReleased(ctx, r, c, hashes[i])
			}
		}
		// Paid, but none of our receipts is readable yet (or another sender
		// paid it with the same signatures): released() is the authority.
		return s.markReleased(ctx, r, c, "")
	}
	for _, h := range hashes {
		st, _, err := c.evm.Receipt(ctx, h)
		if err != nil {
			return err
		}
		if st == ReceiptFailed {
			r.Status, r.Send = RedemptionPending, nil // retried; the vault refuses double releases
			r.Error, r.Updated = "release transaction failed; retrying", time.Now().Unix()
			r.Attempts++
			s.cfg.Logf("bridge: release %s on %s reverted (%s); it is tried again", r.ID(), c.Name, h)
			return s.st.PutRedemption(r)
		}
	}
	if r.Send == nil {
		return nil // sent by an older daemon: followed by its receipt only
	}
	sender, err := ParseAddress(r.Send.From)
	if err != nil {
		return err
	}
	n, err := c.evm.ConfirmedNonce(ctx, sender)
	if err != nil {
		return err
	}
	if n > r.Send.Nonce {
		// The nonce went to a transaction that is none of ours and the burn
		// is not paid: replaced or dropped. Send the same call again.
		s.cfg.Logf("bridge: release %s on %s: nonce %d was taken by another transaction; sending it again", r.ID(), c.Name, r.Send.Nonce)
		return s.resend(ctx, r, c, nil)
	}
	if s.now().Sub(time.Unix(r.Send.Last, 0)) < s.cfg.ReleaseTimeout {
		return nil
	}
	return s.resend(ctx, r, c, r.Send)
}

// resend sends a release again: with replace, the same nonce at higher fees;
// without, the same call with a new nonce.
func (s *Service) resend(ctx context.Context, r *Redemption, c *chainState, replace *TxSend) error {
	if why := c.gasRefused(); why != "" {
		s.holdRedemption(r, why)
		return nil
	}
	req := ReleaseRequest{Replace: replace, MaxGasPrice: c.MaxGasPrice}
	if replace == nil {
		data, err := hex.DecodeString(r.Send.Data)
		if err != nil {
			return err
		}
		req.Data = data
	}
	tx, err := c.evm.Release(ctx, req)
	now := s.now().Unix()
	switch {
	case errors.Is(err, ErrAlreadyReleased):
		return nil // paid meanwhile: the next round reads released() and finds which transaction did it
	case errors.Is(err, ErrNonceUsed):
		r.Send.Last = 0 // the next round sees the nonce taken and sends with a new one
		return s.st.PutRedemption(r)
	case errors.Is(err, ErrGasCap):
		why := fmt.Sprintf("not mined at the chain's gas price cap of %s gwei: waits (nonce %d, %d fee raises)", FormatBigTokenAmount(c.MaxGasPrice, 9), replace.Nonce, replace.Bumps)
		r.Send.Last = now
		if r.Error != why {
			s.cfg.Logf("bridge: release %s on %s: %s", r.ID(), c.Name, why)
		}
		r.Error, r.Updated = why, time.Now().Unix()
		return s.st.PutRedemption(r)
	case errors.Is(err, ErrUnderpriced) && tx != nil:
		tx.Last = now // the fees tried are kept: the next raise starts from them
		r.Send = tx
		return s.st.PutRedemption(r)
	case errors.Is(err, ErrLimitExceeded), errors.Is(err, ErrVaultPaused):
		r.Send.Last = now
		r.Error = publicErr(err.Error()) + ": the transaction in flight waits"
		return s.st.PutRedemption(r)
	case err != nil:
		if gasProblem(err) {
			s.noGas(c, "release transactions", err)
		}
		r.Send.Last = now
		r.Error, r.Updated = publicErr(err.Error()), time.Now().Unix()
		s.st.PutRedemption(r)
		return err
	}
	c.setGasErr("")
	tx.Last = now
	if replace == nil {
		tx.Sent = now
		tx.Hashes = append(append([]string{}, r.Send.Hashes...), tx.Hashes...)
		s.cfg.Logf("bridge: release %s on %s sent again with nonce %d (%s)", r.ID(), c.Name, tx.Nonce, tx.Hash())
	} else {
		s.cfg.Logf("bridge: release %s on %s not mined after %v: sent again at higher fees (nonce %d, %s)", r.ID(), c.Name, s.cfg.ReleaseTimeout, tx.Nonce, tx.Hash())
	}
	r.Send, r.EVMTx, r.Error, r.Updated = tx, tx.Hash(), "", time.Now().Unix()
	return s.st.PutRedemption(r)
}

// finaliseRelease replaces a confirmed redemption's computed fee and payout
// with what the vault's Released event says it did, when the client can
// read it. The gross must agree; if the receipt cannot be read, the
// computed figures stand (they are the contract's own arithmetic).
func (s *Service) finaliseRelease(ctx context.Context, r *Redemption, c *chainState) {
	rl, ok := c.evm.(ReleaseLogReader)
	if !ok || r.EVMTx == "" {
		return
	}
	ev, err := rl.ReleasedLog(ctx, r.EVMTx)
	if err != nil || ev == nil || ev.Amount == nil || ev.Fee == nil {
		return
	}
	if ev.BurnID != burnID(r.ID()) || ev.Amount.String() != r.TokenAmount {
		s.cfg.Logf("bridge: release %s on %s: the Released event (burn %x, gross %s) does not match the redemption (gross %s)", r.EVMTx, c.Name, ev.BurnID[:4], ev.Amount, r.TokenAmount)
		return
	}
	r.TokenFee = ev.Fee.String()
	r.TokenPaid = new(big.Int).Sub(ev.Amount, ev.Fee).String()
}

func mustBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

// ---- every guardian's checks ----

// verifyRedemptionLocally is what every guardian does before signing a
// release: find the burn on its own node and recompute the facts, resolving
// the memo's chain and token against its own configuration (its minimum
// release included), with the depth its own payout tiers ask for what the
// whole transaction pays out of the bridge (txPayout).
//
// A swap payment ("txid:vout") is checked the same way: the output must pay
// the bridge's own uARG (by asset id) to a swap address, and the payout is
// read from that address, never from the request.
func (s *Service) verifyRedemptionLocally(id string) (*Redemption, error) {
	txid, vout, err := ParseRedemptionID(id)
	if err != nil {
		return nil, err
	}
	raw, err := s.cfg.Node.Call("getrawtransaction", txid)
	if err != nil {
		return nil, fmt.Errorf("burn %s not found: %w", txid, err)
	}
	var tx struct {
		wallet.TxJSON
		BlockHash     string `json:"blockhash"`
		Height        uint32 `json:"height"`
		Confirmations uint32 `json:"confirmations"`
	}
	if err := json.Unmarshal(raw, &tx); err != nil {
		return nil, err
	}
	if tx.BlockHash == "" || tx.Confirmations < s.cfg.L1Confirmations {
		return nil, fmt.Errorf("burn has %d of %d confirmations", tx.Confirmations, s.cfg.L1Confirmations)
	}
	assetHex := s.asset.String()
	if vout != nil {
		var o *wallet.TxOutJSON
		for i := range tx.Outputs {
			if tx.Outputs[i].N == int(*vout) {
				o = &tx.Outputs[i]
			}
		}
		if o == nil {
			return nil, fmt.Errorf("transaction %s has no output %d", txid, *vout)
		}
		if o.Type != consensus.OutputTypeP2PK || o.Asset != assetHex {
			return nil, fmt.Errorf("output %d of %s does not pay %s", *vout, txid, s.cfg.AssetSymbol)
		}
		d, ok := ParseSwapAddress(s.cfg.Params.HRP, o.Address)
		if !ok {
			return nil, fmt.Errorf("output %d of %s does not pay a swap address", *vout, txid)
		}
		r := &Redemption{TxID: txid, Vout: vout, Address: o.Address, Height: tx.Height, Amount: o.Amount, TxPayout: s.txPayout(&tx.TxJSON)}
		if err := s.checkLocalDepth(r, tx.Confirmations, "payment"); err != nil {
			return nil, err
		}
		s.classifySwap(r, d)
		if r.Status != RedemptionPending {
			return nil, errors.New(r.Error)
		}
		return r, nil
	}
	var inAsset, outAsset consensus.Amount
	for _, in := range tx.Inputs {
		if in.Prevout != nil && in.Prevout.Asset == assetHex {
			inAsset += in.Prevout.Amount
		}
	}
	var memo []byte
	for _, o := range tx.Outputs {
		if o.Type == consensus.OutputTypeNullData {
			memo, _ = hex.DecodeString(o.Payload)
		}
		if o.Asset == assetHex {
			outAsset += o.Amount
		}
	}
	if inAsset <= outAsset {
		return nil, errors.New("transaction burns no " + s.cfg.AssetSymbol)
	}
	r := &Redemption{TxID: txid, Height: tx.Height, Amount: inAsset - outAsset, TxPayout: s.txPayout(&tx.TxJSON)}
	if err := s.checkLocalDepth(r, tx.Confirmations, "burn"); err != nil {
		return nil, err
	}
	s.classifyRedemption(r, memo)
	if r.Status != RedemptionPending {
		return nil, errors.New(r.Error)
	}
	return r, nil
}

// checkLocalDepth is a guardian's own payout depth check before it signs r,
// found in its node with confs confirmations: the depth its own tiers ask
// for the larger of what r's transaction pays out and what its own records
// say the bridge pays out of the blocks up to r's height (payoutDepth). The
// window needs those blocks indexed: until this guardian's Argonmine scan
// has reached r's height, it does not sign.
func (s *Service) checkLocalDepth(r *Redemption, confs uint32, what string) error {
	if len(s.cfg.ReleaseTiers) > 0 {
		if h, _, ok := s.st.Cursor("l1"); !ok || h < uint64(r.Height) {
			return fmt.Errorf("this guardian has not indexed Argonmine up to the %s's height %d yet, so it cannot add up the payouts of the blocks before it; ask again later", what, r.Height)
		}
	}
	need, ok, basis, window := s.payoutDepth(r)
	if !ok {
		return errors.New(s.aboveTiers(r, window))
	}
	if confs < need {
		return fmt.Errorf("%s has %d of %d confirmations (the depth for %s %s)", what, confs, need, wallet.FormatAmount(basis), s.cfg.AssetSymbol)
	}
	return nil
}

// SignReleaseLocally is the follower's answer to a sign-release request.
func (s *Service) SignReleaseLocally(ctx context.Context, id string) ([]byte, error) {
	if s.cfg.EVMKey == nil {
		return nil, errors.New("this guardian has no EVM key")
	}
	r, err := s.verifyRedemptionLocally(id)
	if err != nil {
		return nil, err
	}
	c, ok := s.byID[r.Chain]
	if !ok {
		return nil, fmt.Errorf("chain %d is not served by this guardian", r.Chain)
	}
	to, err := ParseAddress(r.To)
	if err != nil {
		return nil, err
	}
	token, err := ParseAddress(r.Token)
	if err != nil {
		return nil, err
	}
	if why := c.tokenRefused(token); why != "" {
		return nil, errors.New(why)
	}
	amount, ok := new(big.Int).SetString(r.TokenAmount, 10)
	if !ok || amount.Sign() <= 0 {
		return nil, errors.New("bad token amount")
	}
	return s.signRelease(r, c, token, to, amount)
}

// errReleaseRefused is a burn this guardian will never sign a release for
// as things are: it predates the chain's vault, or it was signed for
// another release already. It needs a human.
type errReleaseRefused struct{ msg string }

func (e errReleaseRefused) Error() string { return e.msg }

// signRelease is the only place a guardian signs a release, the leader for
// itself included. Releases must not be paused on this guardian, the payout
// must be at least the token's minimum release, the burn must be payable
// from the chain's current vault (its height at or above the vault's epoch
// and l1_start_height), and a burn is signed for one
// release only: the first signature is recorded on disk before it is
// returned, and a later request for a different release of the same burn
// (another vault, chain, token, recipient or amount) is refused, whatever
// the configuration says by then.
func (s *Service) signRelease(r *Redemption, c *chainState, token, to [20]byte, amount *big.Int) ([]byte, error) {
	if s.cfg.EVMKey == nil {
		return nil, errors.New("this guardian has no EVM key")
	}
	if p, ok := s.st.GetPause(pauseL1); ok {
		return nil, fmt.Errorf("releases are paused on this guardian: %s", p.Reason)
	}
	if t, ok := c.Token(token); !ok {
		return nil, errReleaseRefused{fmt.Sprintf("token %s is not paid out on %s", HexAddress(token), c.Name)}
	} else if belowMinRelease(t, amount) {
		return nil, errReleaseRefused{minReleaseReason(c, t)}
	}
	start, err := s.payableFrom(c)
	if err != nil {
		return nil, err
	}
	if r.Height < start {
		return nil, errReleaseRefused{predatesVault(c, r.Height, start)}
	}
	digest := ReleaseDigest(c.ID, c.Vault, token, to, amount, burnID(r.ID()))
	dh := hex.EncodeToString(digest[:])
	s.signMu.Lock()
	defer s.signMu.Unlock()
	if prev, ok := s.st.SignedBurn(r.ID()); ok {
		if prev.Digest != dh {
			return nil, errReleaseRefused{fmt.Sprintf("this guardian already signed burn %s for a release of %s on chain %d from vault %s to %s; it signs a burn once", r.ID(), prev.Amount, prev.Chain, prev.Vault, prev.To)}
		}
	} else if err := s.st.RecordSignedBurn(&SignedBurn{TxID: r.ID(), Chain: c.ID, Vault: HexAddress(c.Vault), Token: HexAddress(token), To: HexAddress(to),
		Amount: amount.String(), Digest: dh, Time: time.Now().Unix()}); err != nil {
		return nil, fmt.Errorf("recording the release before signing it: %w", err)
	}
	return SignDigest(s.cfg.EVMKey, digest), nil
}
