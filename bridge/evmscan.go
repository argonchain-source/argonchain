package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"argonmine/consensus"
)

// Scanning the EVM chains (docs/BRIDGE.md, "Finality and providers").
//
// A deposit is acted on only once its block is final: below the block the
// node calls finalized (on the chains that serve that tag) and at least
// `confirmations` below the head, or only the latter (FinalityDepth). The
// scan's cursor never passes the final block, and every block it stops at
// is recorded with its hash. Each round the hash at the cursor is read
// again: when it changed, the chain reorganised below its final block (a
// provider fault, or a reorganisation deeper than the configured depth), and
// the scan goes back to the last recorded block that is still in the chain.
// Every deposit above it is looked up again by its transaction: one that
// moved to another block is re-located, one whose transaction is gone is
// marked reorged and never issued, and one that was already issued raises
// an alert and pauses issuance on that chain until an operator resumes it.
//
// With several RPC providers configured for a chain, the heads, the block
// hashes, the deposit logs and the receipts are read from each of them and
// must agree; when they do not, the chain is not acted on and the status
// says so. Above the final block the first provider's logs are previewed:
// such a deposit is recorded as pending so wallets can follow it, and
// nothing is ever signed for it until the final scan and the receipt check
// below confirm it.

// errDisagree is wrapped by every error that comes from the providers of a
// chain disagreeing.
var errDisagree = errors.New("the chain's RPC providers disagree")

// depositGoneErr: the deposit's event is not in the chain any more.
type depositGoneErr struct{ msg string }

func (e depositGoneErr) Error() string { return e.msg }

// depositMovedErr: the deposit's transaction is in another block now.
type depositMovedErr struct{ ev DepositEvent }

func (e depositMovedErr) Error() string {
	return fmt.Sprintf("the deposit moved to block %d", e.ev.Block)
}

// disagreement records that the providers of c disagree (logged once per
// distinct reason) and returns the error for it.
func (s *Service) disagreement(c *chainState, what string) error {
	c.mu.Lock()
	first := c.disagree != what
	c.disagree = what
	c.mu.Unlock()
	if first {
		s.cfg.Logf("bridge: ALERT %s: %s; nothing on this chain is acted on until they agree", c.Name, what)
	}
	return fmt.Errorf("%w: %s", errDisagree, what)
}

func (c *chainState) clearDisagree() {
	c.mu.Lock()
	c.disagree = ""
	c.mu.Unlock()
}

// chainHeads reads every provider's head and final block and returns the
// lowest of each: the final block is the lower of the finalized tag (in
// FinalityFinalized) and latest − confirmations.
func (s *Service) chainHeads(ctx context.Context, c *chainState) (latest, final uint64, err error) {
	for i, p := range c.providers {
		l, f, err := p.Heads(ctx, c.Finality == FinalityFinalized)
		if err != nil {
			if len(c.providers) > 1 {
				return 0, 0, fmt.Errorf("provider %d: %w", i+1, err)
			}
			return 0, 0, err
		}
		fin := uint64(0)
		if l >= c.Confirmations {
			fin = l - c.Confirmations
		}
		if c.Finality == FinalityFinalized && f < fin {
			fin = f
		}
		if i == 0 || l < latest {
			latest = l
		}
		if i == 0 || fin < final {
			final = fin
		}
	}
	return latest, final, nil
}

// actionableFinal is the highest block whose deposits may be issued on c
// now: final on every provider and passed by this guardian's final scan.
func (s *Service) actionableFinal(ctx context.Context, c *chainState) (uint64, error) {
	_, final, err := s.chainHeads(ctx, c)
	if err != nil {
		return 0, err
	}
	cur, _, ok := s.st.Cursor(EVMCursor(c.ID))
	if !ok {
		return 0, nil
	}
	if cur < final {
		final = cur
	}
	return final, nil
}

// agreedBlockHash is the hash of the block at a height on every provider.
func (s *Service) agreedBlockHash(ctx context.Context, c *chainState, height uint64) (string, error) {
	var first string
	for i, p := range c.providers {
		h, err := p.BlockHash(ctx, height)
		if err != nil {
			return "", fmt.Errorf("evm: block %d: %w", height, err)
		}
		if i == 0 {
			first = h
		} else if h != first {
			return "", s.disagreement(c, fmt.Sprintf("the providers return different blocks at height %d", height))
		}
	}
	return first, nil
}

func sortEvents(evs []DepositEvent) {
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].Block != evs[j].Block {
			return evs[i].Block < evs[j].Block
		}
		return evs[i].LogIndex < evs[j].LogIndex
	})
}

func sameEvents(a, b []DepositEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if !sameDeposit(x, y) || x.Block != y.Block || x.LogIndex != y.LogIndex || !strings.EqualFold(x.TxHash, y.TxHash) || !strings.EqualFold(x.BlockHash, y.BlockHash) {
			return false
		}
	}
	return true
}

// agreedDeposits reads the vault's Deposited events of a range from every
// provider; they must be the same events in the same blocks.
func (s *Service) agreedDeposits(ctx context.Context, c *chainState, lo, hi uint64) ([]DepositEvent, error) {
	var first []DepositEvent
	for i, p := range c.providers {
		evs, err := p.Deposits(ctx, lo, hi)
		if err != nil {
			return nil, fmt.Errorf("evm logs: %w", err)
		}
		sortEvents(evs)
		if i == 0 {
			first = evs
		} else if !sameEvents(first, evs) {
			return nil, s.disagreement(c, fmt.Sprintf("the providers return different deposits in blocks %d to %d", lo, hi))
		}
	}
	return first, nil
}

func sameReceipt(a, b *DepositReceipt) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Status == b.Status && a.Block == b.Block && strings.EqualFold(a.BlockHash, b.BlockHash) && sameEvents(a.Deposits, b.Deposits)
}

// agreedReceipt reads a deposit transaction's receipt from every provider.
// Receipts that differ in a block above the final one are a transient
// error (the providers are at different heads); below it, a disagreement.
func (s *Service) agreedReceipt(ctx context.Context, c *chainState, txHash string, final uint64) (*DepositReceipt, error) {
	var first *DepositReceipt
	for i, p := range c.providers {
		rc, err := p.DepositReceipt(ctx, txHash)
		if err != nil {
			return nil, fmt.Errorf("evm receipt %s: %w", txHash, err)
		}
		if rc != nil {
			sortEvents(rc.Deposits)
		}
		if i == 0 {
			first = rc
			continue
		}
		if !sameReceipt(first, rc) {
			if (first == nil || first.Block > final) && (rc == nil || rc.Block > final) {
				return nil, fmt.Errorf("the providers do not agree yet on transaction %s, which is not final", txHash)
			}
			return nil, s.disagreement(c, fmt.Sprintf("the providers return different receipts for transaction %s", txHash))
		}
	}
	return first, nil
}

// recordMatches reports whether an event is the deposit a record describes.
func recordMatches(d *Inbound, ev DepositEvent) bool {
	return ev.ID == d.ID && HexAddress(ev.Token) == strings.ToLower(d.Token) && HexAddress(ev.From) == strings.ToLower(d.From) &&
		ev.Amount != nil && ev.Amount.String() == d.TokenAmount && ev.Fee != nil && ev.Fee.String() == d.TokenFee &&
		strings.ToLower(strings.TrimSpace(ev.To)) == d.To
}

// findInReceipt returns the record's event in a receipt, or nil when the
// receipt (nil: the transaction is not mined) does not hold it any more.
func findInReceipt(rc *DepositReceipt, d *Inbound) *DepositEvent {
	if rc == nil || rc.Status != ReceiptOK {
		return nil
	}
	for i := range rc.Deposits {
		if recordMatches(d, rc.Deposits[i]) {
			ev := rc.Deposits[i]
			if ev.BlockHash == "" {
				ev.BlockHash = rc.BlockHash
			}
			if ev.Block == 0 {
				ev.Block = rc.Block
			}
			return &ev
		}
	}
	return nil
}

// checkDeposit is what every guardian does with a deposit right before it
// signs an issuance paying it: the transaction's receipt, read from every
// provider, says it succeeded in exactly the block the record names (same
// hash, same log), that block is at or below final, and the vault's event in
// it is exactly the recorded deposit (id, token, depositor, amount, fee,
// destination). A record from before block hashes were kept takes the hash
// of the receipt's block.
func (s *Service) checkDeposit(ctx context.Context, c *chainState, d *Inbound, final uint64) error {
	rc, err := s.agreedReceipt(ctx, c, d.EVMTx, final)
	if err != nil {
		return err
	}
	ev := findInReceipt(rc, d)
	if ev == nil {
		return depositGoneErr{fmt.Sprintf("deposit %d#%d is not in block %d of %s any more: its transaction is not in the chain with this event", d.Chain, d.ID, d.Block, c.Name)}
	}
	if ev.Block != d.Block || (d.BlockHash != "" && (!strings.EqualFold(ev.BlockHash, d.BlockHash) || ev.LogIndex != d.LogIndex)) {
		return depositMovedErr{*ev}
	}
	if ev.Block > final {
		return fmt.Errorf("deposit %d#%d in block %d is not final on %s (final block %d)", d.Chain, d.ID, d.Block, c.Name, final)
	}
	return nil
}

// relocate moves a deposit's record to where its transaction is now.
func (s *Service) relocate(c *chainState, d *Inbound, ev DepositEvent) error {
	if d.Block == ev.Block && strings.EqualFold(d.BlockHash, ev.BlockHash) && d.LogIndex == ev.LogIndex {
		return nil
	}
	if d.BlockHash != "" {
		s.cfg.Logf("bridge: deposit %s#%d moved from block %d to block %d (same transaction %s): re-located", c.Name, d.ID, d.Block, ev.Block, d.EVMTx)
	}
	d.Block, d.BlockHash, d.LogIndex, d.Updated = ev.Block, strings.ToLower(ev.BlockHash), ev.LogIndex, time.Now().Unix()
	return s.st.PutInbound(d)
}

// everSigned reports whether this guardian took part in an issuance paying
// a deposit (its pending record on a follower does not say so).
func (s *Service) everSigned(d *Inbound) bool {
	return len(s.st.SignedIssuesFor(InboundRef{Chain: d.Chain, ID: d.ID})) > 0
}

// depositGone handles a deposit whose event left the chain: never issued,
// it is marked reorged; issued (or signed by this guardian), it raises an
// alert and pauses issuance on the chain until an operator resumes it.
func (s *Service) depositGone(c *chainState, d *Inbound, why string) error {
	if d.Status == InboundReorged {
		return nil
	}
	now := time.Now().Unix()
	issued := d.Status == InboundIssued || d.Status == InboundConfirmed || s.everSigned(d)
	if !issued {
		d.Status, d.Updated = InboundReorged, now
		d.Error = fmt.Sprintf("the deposit is no longer in block %d of %s: its transaction left the chain in a reorganisation, so it is never issued", d.Block, c.Name)
		s.cfg.Logf("bridge: deposit %s#%d (%s) %s: marked reorged (%s)", c.Name, d.ID, d.EVMTx, why, d.Error)
		return s.st.PutInbound(d)
	}
	alert := fmt.Sprintf("deposit %s#%d was issued (%s) but its event left %s in a reorganisation (%s): issuance on this chain is paused until an operator resumes it", c.Name, d.ID, d.IssueTx, c.Name, why)
	if d.Error != alert {
		d.Error, d.Updated = alert, now
		if err := s.st.PutInbound(d); err != nil {
			return err
		}
	}
	return s.pauseChain(c, alert)
}

// recheckDeposit looks a recorded deposit up again by its transaction and
// re-locates it, or handles it as gone.
func (s *Service) recheckDeposit(ctx context.Context, c *chainState, d *Inbound, final uint64) error {
	rc, err := s.agreedReceipt(ctx, c, d.EVMTx, final)
	if err != nil {
		return err
	}
	ev := findInReceipt(rc, d)
	if ev == nil {
		why := "its transaction is not in the chain"
		if rc != nil {
			why = fmt.Sprintf("its transaction is in block %d without this event", rc.Block)
		}
		return s.depositGone(c, d, why)
	}
	return s.relocate(c, d, *ev)
}

// scanChain brings one chain up to date: its heads, the decimals of its
// tokens, the final scan (with reorganisation handling), the preview above
// the final block, and the vault's fees, reserves and limits.
func (s *Service) scanChain(ctx context.Context, c *chainState) error {
	latest, final, err := s.chainHeads(ctx, c)
	if err != nil {
		return fmt.Errorf("evm: %w", err)
	}
	c.mu.Lock()
	c.latest, c.final = latest, final
	c.mu.Unlock()
	if err := s.checkTokens(ctx, c); err != nil {
		return err
	}
	if err := s.checkVaultIdentity(ctx, c); err != nil {
		s.readVault(ctx, c) // the chain's fees, reserves and limits stay in the status
		return err
	}
	cur := EVMCursor(c.ID)
	from := c.StartBlock
	if h, hash, ok := s.st.Cursor(cur); ok {
		if err := s.checkEVMCursor(ctx, c, h, hash, latest, final); err != nil {
			return err
		}
		if h, _, ok := s.st.Cursor(cur); ok && h+1 > from {
			from = h + 1
		}
	}
	// Scan in chunks so provider limits on eth_getLogs ranges are respected.
	for lo := from; lo <= final; lo += 2000 {
		hi := lo + 1999
		if hi > final {
			hi = final
		}
		evs, err := s.agreedDeposits(ctx, c, lo, hi)
		if err != nil {
			return err
		}
		if err := s.applyFinal(ctx, c, lo, hi, final, evs); err != nil {
			return err
		}
		bh, err := s.agreedBlockHash(ctx, c, hi)
		if err != nil {
			return err
		}
		if bh == "" {
			return fmt.Errorf("evm: the provider has no block %d", hi)
		}
		if err := s.st.SetCursor(cur, hi, bh); err != nil {
			return err
		}
		if err := s.st.PutCheckpoint(c.ID, hi, bh); err != nil {
			return err
		}
	}
	c.clearDisagree() // a full round in which every provider agreed
	if err := s.preview(ctx, c, final, latest); err != nil {
		return err
	}
	return s.readVault(ctx, c)
}

// checkVaultIdentity keeps every deposit the scan records named apart from
// every deposit on record (docs/BRIDGE.md, "Replacing a vault"): deposits
// are keyed by chain and id alone. While the configured vault is not the
// one in use, it is a candidate epoch in memory (vaultCandidate), checked
// here every round: the address must hold a contract that answers as a
// vault (its fees and its first deposit id, read from every provider), and
// a vault that replaces another must number its deposits above every id on
// record for the chain. Only when it passes is its epoch recorded, its
// predecessor kept in the epoch's history, and, for a replacement, its scan
// started again from the chain's start_block (the old vault's cursor says
// nothing about it), all in one write. Until then nothing of the vault is
// scanned, issued or paid, the epoch on record stays as it was, and the
// chain's status says why.
func (s *Service) checkVaultIdentity(ctx context.Context, c *chainState) error {
	cur := HexAddress(c.Vault)
	ep, err := s.vaultCandidate(c, func() (uint32, error) {
		info, err := s.cfg.Node.ChainInfo()
		if err != nil {
			return 0, fmt.Errorf("the vault of %s changed to %s; checking it needs the Argonmine node: %w", c.Name, cur, err)
		}
		return info.Blocks, nil
	})
	if err != nil {
		return err
	}
	if ep == nil {
		c.setIDErr("")
		return nil
	}
	refuse := func(why string) error {
		if c.setIDErr(why) {
			s.cfg.Logf("bridge: ALERT %s", why)
		}
		return errors.New(why)
	}
	first, err := s.agreedFirstDepositID(ctx, c)
	if errors.Is(err, ErrNoVaultCode) {
		return refuse(fmt.Sprintf("there is no contract at the vault address %s of %s (a mistyped address, or the vault is not deployed yet): nothing on this chain is scanned, issued or paid until the chains file names the vault", cur, c.Name))
	}
	if err != nil {
		return err
	}
	if _, _, err := c.evm.Fees(ctx); err != nil {
		return fmt.Errorf("the fees of the vault %s of %s could not be read, so nothing on this chain is scanned yet (a contract that is not a vault never answers them): %w", cur, c.Name, err)
	}
	if ep.Rescan {
		// A replacement: every deposit id on record counts, those named by
		// issuances the Argonmine scan has not reached yet included, so
		// the check waits until it has indexed the height the change was
		// seen at, and takes the highest id on record now.
		if h, _, ok := s.st.Cursor("l1"); !ok || h+1 < uint64(ep.L1Start) {
			return fmt.Errorf("the vault of %s changed to %s: its deposit ids are checked once the Argonmine scan has reached height %d", c.Name, cur, ep.L1Start-1)
		}
		if m := s.st.MaxDepositID(c.ID); m > ep.AboveID {
			c.mu.Lock()
			ep.AboveID = m
			c.mu.Unlock()
		}
	}
	if first > consensus.MaxVarint {
		return refuse(fmt.Sprintf("the vault %s of %s numbers its deposits from %d, above the largest id an issuance can name (%d): nothing on this chain is scanned or issued", cur, c.Name, first, uint64(consensus.MaxVarint)))
	}
	if ep.FirstID != 0 && first != ep.FirstID {
		return refuse(fmt.Sprintf("the vault %s of %s now answers firstDepositId() %d, but numbered its deposits from %d when this guardian used it: nothing on this chain is scanned or issued", cur, c.Name, first, ep.FirstID))
	}
	if ep.Previous != "" && first <= ep.AboveID {
		why := fmt.Sprintf("the vault %s of %s numbers its deposits from %d, not above deposit %d#%d already on record for the chain (a deposit is named by its chain and id): nothing on this chain is scanned or issued until the vault is redeployed with firstDepositId %d or more (docs/BRIDGE.md, \"Replacing a vault\")",
			cur, c.Name, first, c.ID, ep.AboveID, ep.AboveID+1)
		for _, h := range ep.History[:max(len(ep.History)-1, 0)] {
			if strings.EqualFold(h.Vault, cur) {
				why += fmt.Sprintf("; it was the chain's vault before %s, which has been in use since, so its ids would meet that vault's", ep.Previous)
				break
			}
		}
		return refuse(why)
	}
	// Passed: recorded at last, in one write with the restart of the scan
	// for a replacement.
	rescan := ep.Rescan
	rec := *ep
	rec.FirstID, rec.Rescan = first, false
	b, err := json.Marshal(&rec)
	if err != nil {
		return err
	}
	ops := []kvOp{{key: epochKey(c.ID), value: b}}
	if rescan {
		// The cursor and the block hashes belong to the vault this one
		// replaced: the scan starts again from the chain's start_block.
		s.st.eachKey(append([]byte(pfxCheckpoint), be64(c.ID)...), func(k []byte) { ops = append(ops, kvOp{key: k}) })
		ops = append(ops, kvOp{key: cursorKey(EVMCursor(c.ID))})
	}
	c.mu.Lock()
	if c.cand != ep {
		c.mu.Unlock()
		return fmt.Errorf("the candidate vault of %s was reset by an operator; it is checked again", c.Name)
	}
	if err := s.st.write(ops, true); err != nil {
		c.mu.Unlock()
		return err
	}
	c.cand = nil
	if rescan {
		c.previewTo = 0
	}
	c.mu.Unlock()
	c.setIDErr("")
	if rescan {
		s.cfg.Logf("bridge: %s: the replacement vault %s is in use and scanned from block %d; it pays burns from Argonmine height %d", c.Name, cur, c.StartBlock, rec.L1Start)
	}
	s.cfg.Logf("bridge: %s: vault %s numbers its deposits from %d", c.Name, cur, first)
	return nil
}

// agreedFirstDepositID reads the vault's first deposit id from every
// provider; they must agree. A client that cannot read it says 1. An
// address without a contract on any provider is ErrNoVaultCode.
func (s *Service) agreedFirstDepositID(ctx context.Context, c *chainState) (uint64, error) {
	var first uint64
	for i, p := range c.providers {
		v := uint64(1)
		if r, ok := p.(VaultInfoReader); ok {
			var err error
			if v, err = r.FirstDepositID(ctx); err != nil {
				if errors.Is(err, ErrNoVaultCode) {
					return 0, err
				}
				return 0, fmt.Errorf("vault firstDepositId(): %w", err)
			}
		}
		if i == 0 {
			first = v
		} else if v != first {
			return 0, s.disagreement(c, fmt.Sprintf("the providers return different first deposit ids for the vault (%d and %d)", first, v))
		}
	}
	return first, nil
}

// belowFirst reports a Deposited event whose id is below the vault's own
// first deposit id: a vault never emits one, and such an id may belong to a
// deposit of an earlier vault of the chain, so it is never recorded.
func (s *Service) belowFirst(c *chainState, ev DepositEvent, first uint64) {
	if s.warnOnce(fmt.Sprintf("below/%d/%d", c.ID, ev.ID), time.Hour) {
		s.cfg.Logf("bridge: ALERT %s: the vault reports deposit id %d (transaction %s) below its first deposit id %d; it is not recorded", c.Name, ev.ID, ev.TxHash, first)
	}
}

// checkEVMCursor compares the cursor's recorded hash with the chain's and
// goes back to the last recorded block still in the chain when it changed.
func (s *Service) checkEVMCursor(ctx context.Context, c *chainState, h uint64, hash string, latest, final uint64) error {
	if h > latest {
		return nil // a provider is behind the cursor: checked when it catches up
	}
	now, err := s.agreedBlockHash(ctx, c, h)
	if err != nil {
		return err
	}
	if hash == "" { // a cursor from before hashes were kept
		if now == "" {
			return nil
		}
		if err := s.st.SetCursor(EVMCursor(c.ID), h, now); err != nil {
			return err
		}
		return s.st.PutCheckpoint(c.ID, h, now)
	}
	if strings.EqualFold(now, hash) {
		return nil
	}
	return s.rewindEVM(ctx, c, h, latest, final)
}

// rewindEVM handles a reorganisation below the cursor: back to the newest
// recorded block that is still in the chain, every deposit above it looked
// up again.
func (s *Service) rewindEVM(ctx context.Context, c *chainState, h, latest, final uint64) error {
	fork, forkHash, found := uint64(0), "", false
	for _, cp := range s.st.Checkpoints(c.ID, h) {
		if cp.Height > latest {
			continue
		}
		now, err := s.agreedBlockHash(ctx, c, cp.Height)
		if err != nil {
			return err
		}
		if strings.EqualFold(now, cp.Hash) {
			fork, forkHash, found = cp.Height, cp.Hash, true
			break
		}
	}
	if found {
		s.cfg.Logf("bridge: ALERT %s reorganised below the final block %d it had scanned: back to block %d, every deposit above it is looked up again", c.Name, h, fork)
		if err := s.st.DropCheckpointsAbove(c.ID, fork); err != nil {
			return err
		}
		if err := s.st.SetCursor(EVMCursor(c.ID), fork, forkHash); err != nil {
			return err
		}
	} else {
		s.cfg.Logf("bridge: ALERT %s reorganised below every block this guardian recorded (cursor %d): rescanning from block %d", c.Name, h, c.StartBlock)
		if c.StartBlock > 0 {
			fork = c.StartBlock - 1
		}
		if err := s.st.DropCheckpointsAbove(c.ID, fork); err != nil {
			return err
		}
		if err := s.st.del(cursorKey(EVMCursor(c.ID))); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.previewTo = 0
	c.mu.Unlock()
	var errs []error
	first := s.vaultFirstID(c)
	for _, d := range s.st.InboundsInBlocks(c.ID, fork+1, math.MaxUint64) {
		if d.ID < first {
			continue // a deposit of an earlier vault: this vault's chain view says nothing about it
		}
		if err := s.recheckDeposit(ctx, c, &d, final); err != nil {
			errs = append(errs, err) // left for the final scan, which looks it up again
		}
	}
	return errors.Join(errs...)
}

// applyFinal records the deposits of a final range: new ones are
// classified; known ones are checked against what the chain says now (a
// deposit re-mined elsewhere is re-located, an id that went to another
// deposit replaces a record never issued, and alerts on one that was); and
// every record in the range that the range no longer holds is looked up
// again by its transaction.
func (s *Service) applyFinal(ctx context.Context, c *chainState, lo, hi, final uint64, evs []DepositEvent) error {
	seen := map[uint64]string{}
	first := s.vaultFirstID(c)
	for _, ev := range evs {
		if ev.ID < first {
			s.belowFirst(c, ev, first)
			continue
		}
		seen[ev.ID] = strings.ToLower(ev.TxHash)
		old, exists := s.st.GetInbound(c.ID, ev.ID)
		switch {
		case !exists:
			d := s.classifyDeposit(c, ev)
			if err := s.st.PutInbound(d); err != nil {
				return err
			}
			s.logDeposit(c, d, ev, "")
		case strings.EqualFold(old.EVMTx, ev.TxHash) && recordMatches(old, ev):
			if old.Status == InboundReorged {
				d := s.classifyDeposit(c, ev) // back in the chain: what it says now
				d.Created = old.Created
				if err := s.st.PutInbound(d); err != nil {
					return err
				}
				s.logDeposit(c, d, ev, " (back in the chain after a reorganisation)")
				continue
			}
			if err := s.relocate(c, old, ev); err != nil {
				return err
			}
		case old.Status == InboundReorged || ((old.Status == InboundPending || old.Status == InboundUnclaimable) && !s.everSigned(old)):
			// The id went to another deposit in a reorganisation; the
			// record it replaces was never issued and is kept aside.
			if err := s.st.ArchiveReplaced(old); err != nil {
				return err
			}
			d := s.classifyDeposit(c, ev)
			if err := s.st.PutInbound(d); err != nil {
				return err
			}
			s.cfg.Logf("bridge: deposit %s#%d is now transaction %s (it was %s, which was never issued and is kept aside)", c.Name, ev.ID, ev.TxHash, old.EVMTx)
			s.logDeposit(c, d, ev, "")
		default:
			if err := s.depositGone(c, old, fmt.Sprintf("its id now belongs to transaction %s", ev.TxHash)); err != nil {
				return err
			}
		}
	}
	for _, d := range s.st.InboundsInBlocks(c.ID, lo, hi) {
		if tx, ok := seen[d.ID]; (ok && strings.EqualFold(tx, d.EVMTx)) || d.ID < first {
			continue // as the chain says, or a deposit of an earlier vault of the chain
		}
		if err := s.recheckDeposit(ctx, c, &d, final); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) logDeposit(c *chainState, d *Inbound, ev DepositEvent, note string) {
	s.cfg.Logf("bridge: deposit %s#%d %s %s (fee %s) -> %s (%s)%s", c.Name, d.ID, FormatBigTokenAmount(ev.Amount, d.Decimals), d.TokenSymbol, FormatBigTokenAmount(ev.Fee, d.Decimals), d.To, d.Status, note)
}

// preview records the deposits above the final block, from the first
// provider, so wallets can follow them. Nothing is signed for them before
// the final scan has passed their block and their receipt checks out.
func (s *Service) preview(ctx context.Context, c *chainState, final, latest uint64) error {
	c.mu.Lock()
	from := c.previewTo + 1
	c.mu.Unlock()
	if from <= final {
		from = final + 1
	}
	for lo := from; lo <= latest; lo += 2000 {
		hi := lo + 1999
		if hi > latest {
			hi = latest
		}
		evs, err := c.evm.Deposits(ctx, lo, hi)
		if err != nil {
			return fmt.Errorf("evm logs: %w", err)
		}
		first := s.vaultFirstID(c)
		for _, ev := range evs {
			if ev.ID < first {
				s.belowFirst(c, ev, first)
				continue
			}
			if _, exists := s.st.GetInbound(c.ID, ev.ID); exists {
				continue
			}
			d := s.classifyDeposit(c, ev)
			if err := s.st.PutInbound(d); err != nil {
				return err
			}
			s.logDeposit(c, d, ev, " (not final yet)")
		}
		c.mu.Lock()
		c.previewTo = hi
		c.mu.Unlock()
	}
	return nil
}

// checkTokens reads decimals() of every token of the chain that has not
// been checked since the daemon started. A token whose decimals differ from
// the configuration (or that has none) is refused: none of its deposits is
// issued and none of its burns is paid until the chains file is fixed.
func (s *Service) checkTokens(ctx context.Context, c *chainState) error {
	for i := range c.Tokens {
		t := &c.Tokens[i]
		c.mu.Lock()
		_, done := c.tokenErr[t.Address]
		c.mu.Unlock()
		if done {
			continue
		}
		dec, err := c.evm.TokenDecimals(ctx, t.Address)
		why := ""
		switch {
		case errors.Is(err, ErrNoDecimals):
			why = fmt.Sprintf("%s (%s) answers no decimals(): the token is refused", t.Symbol, HexAddress(t.Address))
		case err != nil:
			return fmt.Errorf("decimals of %s: %w", t.Symbol, err)
		case dec != t.Decimals:
			why = fmt.Sprintf("%s (%s) has %d decimals on chain but %d in the configuration: the token is refused until the chains file is fixed", t.Symbol, HexAddress(t.Address), dec, t.Decimals)
		}
		c.mu.Lock()
		c.tokenErr[t.Address] = why
		c.mu.Unlock()
		if why != "" {
			s.cfg.Logf("bridge: ALERT %s: %s", c.Name, why)
		}
	}
	return nil
}

// tokenRefused is why a token of c is not acted on: its decimals are not
// checked yet, or they differ from the configuration ("" when it is fine).
func (c *chainState) tokenRefused(addr [20]byte) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	why, done := c.tokenErr[addr]
	if !done {
		return "the token's decimals() has not been checked yet"
	}
	return why
}

// readVault reads the vault's fee rates and, per token, its reserve, its
// obligations and its release limits, and on the leader the gas key's
// balance. Read with the scan, so the status never waits on an RPC.
func (s *Service) readVault(ctx context.Context, c *chainState) error {
	dep, rel, err := c.evm.Fees(ctx)
	if err != nil {
		return fmt.Errorf("vault fees: %w", err)
	}
	c.setFees(dep, rel)
	if vr, ok := c.evm.(VaultInfoReader); ok {
		fin, known, err := vr.SetupFinalized(ctx)
		if err != nil {
			return fmt.Errorf("vault finalized(): %w", err)
		}
		c.mu.Lock()
		c.setup = known && !fin
		c.mu.Unlock()
		if known && !fin && s.warnOnce("setup/"+fmt.Sprint(c.ID), time.Hour) {
			s.cfg.Logf("bridge: WARN the vault %s on %s is still in setup mode (finalized() is false): its owner can change its tokens, fees and limits at once, without the timelock; call finalizeSetup() before it holds real funds", HexAddress(c.Vault), c.Name)
		}
	}
	for i := range c.Tokens {
		t := &c.Tokens[i]
		r, err := c.evm.Reserve(ctx, t.Address)
		if err != nil {
			return fmt.Errorf("reserve of %s: %w", t.Symbol, err)
		}
		o, err := c.evm.Obligations(ctx, t.Address)
		if err != nil {
			return fmt.Errorf("obligations of %s: %w", t.Symbol, err)
		}
		c.setReserve(t.Address, r, o)
		lim, err := c.evm.ReleaseLimits(ctx, t.Address)
		if err != nil {
			return fmt.Errorf("release limits of %s: %w", t.Symbol, err)
		}
		c.mu.Lock()
		c.limits[t.Address] = lim
		c.mu.Unlock()
	}
	if !s.cfg.Leader {
		return nil
	}
	return s.readGas(ctx, c)
}

// gasSender is the address the chain's client sends releases and sweeps
// from (zero when it sends nothing).
func gasSender(c *chainState) [20]byte {
	if sp, ok := c.evm.(SenderEVM); ok {
		return sp.Sender()
	}
	return [20]byte{}
}

// readGas reads the gas key's balance on c, and once per vault checks that
// the gas key is neither one of the vault's guardians nor its owner.
func (s *Service) readGas(ctx context.Context, c *chainState) error {
	sender := gasSender(c)
	if sender == ([20]byte{}) {
		return nil
	}
	bal, err := c.evm.NativeBalance(ctx, sender)
	if err != nil {
		return fmt.Errorf("gas key balance: %w", err)
	}
	c.mu.Lock()
	c.gasBalance = bal
	checked := c.rolesChecked
	c.mu.Unlock()
	if low := c.MinGasBalance != nil && bal.Cmp(c.MinGasBalance) < 0; low && s.warnOnce("gas/"+fmt.Sprint(c.ID), time.Hour) {
		s.cfg.Logf("bridge: WARN the gas key %s holds %s %s on %s, below %s: fund it", HexAddress(sender), FormatBigTokenAmount(bal, 18), c.NativeSymbol, c.Name, FormatBigTokenAmount(c.MinGasBalance, 18))
	}
	if checked {
		return nil
	}
	rr, ok := c.evm.(RoleReader)
	if !ok {
		return nil
	}
	isGuardian, owner, err := rr.VaultRoles(ctx, sender)
	if err != nil {
		return fmt.Errorf("vault roles: %w", err)
	}
	why := ""
	switch {
	case isGuardian:
		why = fmt.Sprintf("the gas key %s is a guardian of the vault on %s: use a dedicated key (-gas-key); no release is sent from it", HexAddress(sender), c.Name)
	case owner == sender:
		why = fmt.Sprintf("the gas key %s owns the vault on %s: use a dedicated key (-gas-key); no release is sent from it", HexAddress(sender), c.Name)
	}
	c.mu.Lock()
	c.rolesChecked, c.gasRoleErr = true, why
	c.mu.Unlock()
	if why != "" {
		s.cfg.Logf("bridge: ALERT %s", why)
	}
	return nil
}

// gasReady is why releases and sweeps are not sent on c from its gas key
// ("" when they may be).
func (c *chainState) gasRefused() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gasRoleErr
}

// availableNow is the most of token that one release can pay on c now, in
// gross token units: what the vault holds less what releases already sent
// will take, within the vault's per-release and 24-hour limits; 0 when the
// vault is paused. nil when it is not known yet.
func (c *chainState) availableNow(token [20]byte, inFlight *big.Int) *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, lim := c.reserves[token], c.limits[token]
	if r == nil || lim == nil {
		return nil
	}
	if lim.Paused {
		return new(big.Int)
	}
	avail := new(big.Int).Sub(r, inFlight)
	if lim.PeriodAvailable != nil {
		p := new(big.Int).Sub(lim.PeriodAvailable, inFlight)
		if p.Cmp(avail) < 0 {
			avail = p
		}
	}
	if lim.MaxRelease != nil && lim.MaxRelease.Cmp(avail) < 0 {
		avail = new(big.Int).Set(lim.MaxRelease)
	}
	if avail.Sign() < 0 {
		avail.SetInt64(0)
	}
	return avail
}
