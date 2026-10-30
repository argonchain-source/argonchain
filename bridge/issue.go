package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
)

// Issuing uARG (docs/BRIDGE.md, "Deposit" and "Security model").
//
// The leader batches confirmed deposits into one issuance transaction and
// runs MuSig2 with every guardian. Each guardian, the leader included,
// checks the transaction on its own before it signs (verifyIssue): every
// input is a custody coin its node knows, at most maxIssueInputs of them;
// every uARG output pays a deposit it saw itself, pending, with the chain's
// confirmations counted against a head it has just read and its event
// still in its block; the only other outputs are at most one ARG change
// output back to custody and the NULLDATA deposit references; the issuance
// is under the limits; and it conflicts with every issuance it signed
// before for any of those deposits (the signing memory), so that at most
// one of them can ever confirm. The memory is on disk before a partial
// signature leaves the guardian.

const (
	maxIssueInputs     = 20 // custody coins one issuance may spend
	maxIssueBatch      = 50 // deposits one issuance may pay (the 80-byte memo usually stops it first)
	maxSigningSessions = 4  // signing sessions a follower keeps in flight
	signingSessionTTL  = 2 * time.Minute
	issuedStale        = 30 * time.Minute // an issuance neither mined nor in the mempool after this is built again
	issueDay           = 24 * time.Hour
	// issueDeadDepth is how deep (at least) the spend of one of its custody
	// coins by another transaction must be before an issuance signed earlier
	// counts as unable ever to confirm (issueDead): a reorganisation above it
	// could bring that issuance back next to a later one for the same
	// deposits. Never less than -l1-confirmations.
	issueDeadDepth = 100
)

var errNothingToIssue = errors.New("nothing to issue")

// assetOutputFee and freeAssetOutputs mirror the nodes' default relay
// policy (mempool.DefaultConfig().AssetOutputFee, mempool.FreeAssetOutputs):
// a transaction pays this much more fee for every asset output beyond the
// first two, so an issuance paying many deposits pays for each of them.
const (
	assetOutputFee   consensus.Amount = 546
	freeAssetOutputs                  = 2
)

// assetSurcharge is the relay fee owed for tx's asset outputs on top of its
// bytes.
func assetSurcharge(tx *consensus.Tx) consensus.Amount {
	n := 0
	for i := range tx.Outputs {
		if tx.Outputs[i].Asset != consensus.NativeAsset {
			n++
		}
	}
	if n <= freeAssetOutputs {
		return 0
	}
	return assetOutputFee * consensus.Amount(n-freeAssetOutputs)
}

// SignRequest is an issuance transaction the leader asks guardians to sign.
type SignRequest struct {
	ID      string       `json:"id"`
	TxHex   string       `json:"tx"`
	Spent   []string     `json:"spent"`   // hex-serialized TxOut per input (custody ARG coins)
	Inbound []InboundRef `json:"inbound"` // deposits paid, in output order
}

type pendingSigning struct {
	req      SignRequest
	sessions []*Session // one per input, then one for the issuance
	nonces   [][]byte
	created  time.Time
	ready    bool // false while a follower is still checking the request
}

// issueFacts is what verifyIssue established about an issuance.
type issueFacts struct {
	txid   string
	inputs []string // custody outpoints
	refs   []InboundRef
	amount consensus.Amount
}

// processInbound issues uARG for deposits whose block is final on their
// chain, batching them across chains into one transaction.
func (s *Service) processInbound(ctx context.Context) error {
	if p, ok := s.st.GetPause(pauseL1); ok {
		s.cfg.Logf("bridge: issuance is paused: %s", p.Reason)
		return nil
	}
	finals := map[uint64]uint64{}
	for _, c := range s.chains {
		if s.issuancePaused(c) != "" {
			continue // its deposits wait for the operator or for the providers to agree
		}
		f, err := s.actionableFinal(ctx, c)
		if err != nil {
			continue // reported by the scan; its deposits wait
		}
		finals[c.ID] = f
	}
	info, err := s.cfg.Node.ChainInfo()
	if err != nil {
		return err
	}
	all := s.st.Inbounds()
	s.reissueDropped(all)
	var pending, ready []Inbound
	for _, d := range s.st.Inbounds() {
		if d.Status != InboundPending {
			continue
		}
		pending = append(pending, d)
		_, ok := s.byID[d.Chain]
		final, seen := finals[d.Chain]
		if ok && seen && d.Block <= final {
			ready = append(ready, d)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Created != ready[j].Created {
			return ready[i].Created < ready[j].Created
		}
		if ready[i].Chain != ready[j].Chain {
			return ready[i].Chain < ready[j].Chain
		}
		return ready[i].ID < ready[j].ID
	})
	batch := s.selectBatch(ctx, ready, finals)
	if len(batch) == 0 {
		return nil
	}
	tx, spent, batch, err := s.buildIssue(batch, pending, info.Blocks)
	if errors.Is(err, errNothingToIssue) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("issue: %w", err)
	}
	var idb [8]byte
	rand.Read(idb[:])
	refs := make([]InboundRef, len(batch))
	for i, d := range batch {
		refs[i] = InboundRef{Chain: d.Chain, ID: d.ID}
	}
	req := SignRequest{ID: hex.EncodeToString(idb[:]), TxHex: hex.EncodeToString(tx.Serialize()), Inbound: refs}
	for _, o := range spent {
		req.Spent = append(req.Spent, encodeTxOut(o))
	}
	if err := s.signWithGuardians(ctx, req, tx, spent); err != nil {
		return fmt.Errorf("issue signing: %w", err)
	}
	if err := consensus.VerifyTxSignatures(s.cfg.Params.ChainTag, tx, spent); err != nil {
		return fmt.Errorf("issue signature invalid: %w", err)
	}
	txid, err := s.cfg.Node.SendRawTransaction(hex.EncodeToString(tx.Serialize()))
	if err != nil {
		return fmt.Errorf("broadcast issue: %w", err)
	}
	now := time.Now().Unix()
	for _, d := range batch {
		d.Status = InboundIssued
		d.IssueTx = txid
		d.Error = ""
		d.Updated = now
		s.st.PutInbound(&d)
	}
	for _, in := range tx.Inputs {
		s.st.DelUTXO(in.PrevOut.TxID.String(), in.PrevOut.Index)
	}
	for i, o := range tx.Outputs {
		if o.Asset == consensus.NativeAsset && s.cfg.Custody.PaysCustody(&o) {
			s.st.PutUTXO(&UTXO{TxID: txid, Index: uint32(i), Amount: o.Amount, Height: 0})
		}
	}
	s.st.PutIssue(&Issue{TxID: txid, Inbound: refs, Created: now, Hex: hex.EncodeToString(tx.Serialize())})
	s.cfg.Logf("bridge: issued %s for %d deposits in %s", s.cfg.AssetSymbol, len(batch), txid)
	return nil
}

// selectBatch picks the deposits of the next issuance, oldest first: under
// the per-issuance and daily limits and the per-deposit caps, with
// references that fit the memo, and each one's receipt saying it is final
// exactly where it was recorded (checkDeposit). A deposit above a limit or
// a cap waits with the reason in its error; one that moved is re-located
// and one whose event is gone is marked reorged (or, if it was signed
// already, raises an alert and pauses its chain).
func (s *Service) selectBatch(ctx context.Context, ready []Inbound, finals map[uint64]uint64) []Inbound {
	// The daily limit counts each deposit once, however many issuances were
	// signed for it: one signed again is in dayUsed already.
	window := s.st.SignedSince(time.Now().Add(-issueDay).Unix())
	dayUsed := sumSigned(window, nil)
	var batch []Inbound
	var refs []InboundRef
	var total, fresh consensus.Amount
	for _, d := range ready {
		if len(batch) >= maxIssueBatch {
			break
		}
		if s.cfg.MaxIssue > 0 && d.Amount > s.cfg.MaxIssue {
			s.holdDeposit(d, fmt.Sprintf("above the per-issuance limit of %s %s: waits for the guardians", wallet.FormatAmount(s.cfg.MaxIssue), s.cfg.AssetSymbol))
			continue
		}
		if s.cfg.MaxIssue > 0 && total+d.Amount > s.cfg.MaxIssue {
			continue // the next batch
		}
		extra := d.Amount
		if _, signed := window[InboundRef{Chain: d.Chain, ID: d.ID}]; signed {
			extra = 0
		}
		if s.cfg.MaxIssueDay > 0 && dayUsed+fresh+extra > s.cfg.MaxIssueDay {
			s.holdDeposit(d, fmt.Sprintf("the daily issuance limit of %s %s is reached: waits until it frees up", wallet.FormatAmount(s.cfg.MaxIssueDay), s.cfg.AssetSymbol))
			continue
		}
		ref := InboundRef{Chain: d.Chain, ID: d.ID}
		if _, err := EncodeIssueMemo(append(refs[:len(refs):len(refs)], ref)); err != nil {
			continue // does not fit this issuance's memo: the next batch
		}
		c := s.byID[d.Chain]
		if why := s.depositBlocker(c, &d); why != "" {
			s.holdDeposit(d, why)
			continue
		}
		err := s.checkDeposit(ctx, c, &d, finals[d.Chain])
		var gone depositGoneErr
		var moved depositMovedErr
		switch {
		case errors.As(err, &gone):
			s.depositGone(c, &d, gone.msg)
			continue
		case errors.As(err, &moved):
			s.relocate(c, &d, moved.ev) // issued from where it is now once that is final
			continue
		case err != nil:
			continue // the chain cannot be read now, or the providers disagree
		}
		batch = append(batch, d)
		refs = append(refs, ref)
		total += d.Amount
		fresh += extra
	}
	return batch
}

// holdDeposit records why a pending deposit waits (once).
func (s *Service) holdDeposit(d Inbound, why string) {
	if d.Error == why {
		return
	}
	d.Error, d.Updated = why, time.Now().Unix()
	s.st.PutInbound(&d)
	s.cfg.Logf("bridge: deposit %s#%d (%s) waits: %s", d.ChainName, d.ID, wallet.FormatAmount(d.Amount), why)
}

// reissueDropped looks after issuances that are neither mined nor in the
// node's mempool: after -issue-rebroadcast (10 minutes) the same signed
// transaction is broadcast again, at most once per that interval; after 30
// minutes its deposits go back to pending, to be issued again. The next
// issuance for them must spend one of that issuance's coins (the signing
// memory), so the two can never both confirm.
func (s *Service) reissueDropped(all []Inbound) {
	now := s.now()
	gone := map[string]bool{}
	missing := func(txid string) bool {
		g, ok := gone[txid]
		if !ok {
			_, err := s.cfg.Node.Call("getrawtransaction", txid)
			var re *wallet.RPCError
			g = errors.As(err, &re) && re.Code == -5
			gone[txid] = g
		}
		return g
	}
	rebroadcast := map[string]bool{}
	for _, d := range all {
		if d.Status != InboundIssued || d.IssueTx == "" {
			continue
		}
		age := now.Sub(time.Unix(d.Updated, 0))
		if age >= issuedStale {
			if missing(d.IssueTx) {
				d.Status, d.Error, d.Updated = InboundPending, "", now.Unix()
				s.st.PutInbound(&d)
				s.cfg.Logf("bridge: deposit %s#%d: issuance %s is neither mined nor in the mempool after %v; issuing it again", d.ChainName, d.ID, d.IssueTx, issuedStale)
			}
			continue
		}
		if age < s.cfg.IssueRebroadcast || rebroadcast[d.IssueTx] {
			continue
		}
		rebroadcast[d.IssueTx] = true
		var is Issue
		if !s.st.get(issueKey(d.IssueTx), &is) || is.Hex == "" || is.Confirmed || now.Sub(time.Unix(is.Rebroad, 0)) < s.cfg.IssueRebroadcast {
			continue
		}
		if !missing(d.IssueTx) {
			continue
		}
		is.Rebroad = now.Unix()
		s.st.PutIssue(&is)
		if _, err := s.cfg.Node.SendRawTransaction(is.Hex); err != nil {
			s.cfg.Logf("bridge: issuance %s is neither mined nor in the mempool; broadcasting it again failed: %v", d.IssueTx, err)
			continue
		}
		delete(gone, d.IssueTx)
		s.cfg.Logf("bridge: issuance %s was neither mined nor in the mempool; broadcast it again", d.IssueTx)
	}
}

// buildIssue assembles the unsigned issuance transaction for batch. It
// returns the deposits it pays: a deposit for which this guardian signed an
// issuance before can only be paid by spending one of that issuance's coins,
// and waits while none of them can be spent.
func (s *Service) buildIssue(batch, pending []Inbound, tipHeight uint32) (*consensus.Tx, []consensus.TxOut, []Inbound, error) {
	h, _, _ := s.st.Cursor("l1")
	indexTip := uint32(h)
	for {
		if len(batch) == 0 {
			return nil, nil, nil, errNothingToIssue
		}
		anchors, blocked := s.anchorCoins(batch, indexTip, tipHeight)
		if len(blocked) == 0 && len(anchors) > maxIssueInputs {
			batch = batch[:len(batch)-1] // too many earlier issuances to conflict with at once: pay fewer
			continue
		}
		if len(blocked) > 0 {
			var kept []Inbound
			for _, d := range batch {
				if blocked[InboundRef{Chain: d.Chain, ID: d.ID}] {
					s.holdDeposit(d, "waits until a custody coin of an issuance signed earlier for it can be spent")
					continue
				}
				kept = append(kept, d)
			}
			batch = kept
			continue
		}
		inBatch := map[InboundRef]bool{}
		for _, d := range batch {
			inBatch[InboundRef{Chain: d.Chain, ID: d.ID}] = true
		}
		tx, spent, err := s.assembleIssue(batch, anchors, s.reservedCoins(pending, inBatch, indexTip), tipHeight)
		return tx, spent, batch, err
	}
}

// anchorCoins picks, for every live issuance signed earlier for a deposit
// of batch, one of its custody coins that is still unspent, so the new
// issuance conflicts with it. blocked lists the deposits for which that is
// not possible now.
func (s *Service) anchorCoins(batch []Inbound, indexTip, tipHeight uint32) ([]UTXO, map[InboundRef]bool) {
	var chosen []UTXO
	have := map[string]bool{}
	done := map[string]bool{} // earlier issuance → it can be conflicted with
	blocked := map[InboundRef]bool{}
	for _, d := range batch {
		ref := InboundRef{Chain: d.Chain, ID: d.ID}
		for _, old := range s.st.SignedIssuesFor(ref) {
			ok, seen := done[old.TxID]
			if !seen {
				ok = s.anchorFor(&old, have, &chosen, indexTip, tipHeight)
				done[old.TxID] = ok
			}
			if !ok {
				blocked[ref] = true
			}
		}
	}
	return chosen, blocked
}

func (s *Service) anchorFor(old *SignedIssue, have map[string]bool, chosen *[]UTXO, indexTip, tipHeight uint32) bool {
	if s.issueDead(old, indexTip) {
		return true
	}
	for _, op := range old.Inputs {
		if have[op] {
			return true
		}
	}
	ops := append([]string{}, old.Inputs...)
	sort.Strings(ops)
	for _, op := range ops {
		u, st, err := s.custodyCoin(op, tipHeight)
		if err != nil || st != coinOK {
			continue
		}
		have[op] = true
		*chosen = append(*chosen, u)
		return true
	}
	return false
}

// reservedCoins are the custody coins of live issuances signed earlier for
// pending deposits outside this batch: spending one as a mere fee coin
// would make those deposits wait.
func (s *Service) reservedCoins(pending []Inbound, inBatch map[InboundRef]bool, indexTip uint32) map[string]bool {
	res := map[string]bool{}
	for _, d := range pending {
		ref := InboundRef{Chain: d.Chain, ID: d.ID}
		if inBatch[ref] {
			continue
		}
		for _, old := range s.st.SignedIssuesFor(ref) {
			if s.issueDead(&old, indexTip) {
				continue
			}
			for _, op := range old.Inputs {
				res[op] = true
			}
		}
	}
	return res
}

// assembleIssue builds the transaction: one uARG output per deposit, the
// anchors and then the largest custody coins for the fee (each confirmed
// with the node first; a coin the node no longer has is dropped), ARG
// change back to custody, the deposit references, and a single issuance of
// exactly the total.
func (s *Service) assembleIssue(batch []Inbound, anchors []UTXO, reserved map[string]bool, tipHeight uint32) (*consensus.Tx, []consensus.TxOut, error) {
	tx := &consensus.Tx{Version: consensus.TxVersionAssets}
	var total consensus.Amount
	refs := make([]InboundRef, len(batch))
	for i, d := range batch {
		dest, err := wallet.DecodeAddress(s.cfg.Params.HRP, d.To)
		if err != nil {
			return nil, nil, fmt.Errorf("deposit %s#%d: %w", d.ChainName, d.ID, err)
		}
		tx.Outputs = append(tx.Outputs, consensus.TxOut{Amount: d.Amount, Type: dest.Type, Payload: dest.Payload, Asset: s.asset})
		if total, err = consensus.AddAssetAmounts(total, d.Amount); err != nil {
			return nil, nil, err
		}
		refs[i] = InboundRef{Chain: d.Chain, ID: d.ID}
	}
	memo, err := EncodeIssueMemo(refs)
	if err != nil {
		return nil, nil, err
	}
	memoOut := consensus.TxOut{Type: consensus.OutputTypeNullData, Payload: memo}
	tx.Issuances = []consensus.Issuance{{Asset: s.asset, Issuer: s.issuer, Symbol: s.cfg.AssetSymbol, Amount: total}}
	var spent []consensus.TxOut
	var in consensus.Amount
	used := map[string]bool{}
	add := func(u UTXO) {
		h, err := consensus.HashFromHex(u.TxID)
		if err != nil {
			return
		}
		tx.Inputs = append(tx.Inputs, consensus.TxIn{PrevOut: consensus.OutPoint{TxID: h, Index: u.Index}})
		spent = append(spent, s.cfg.Custody.Output(u.Amount))
		in += u.Amount
		used[u.Outpoint()] = true
	}
	placeholders := func() {
		tx.Witnesses = make([][]byte, len(tx.Inputs)+1)
		for i := range tx.Witnesses {
			tx.Witnesses[i] = make([]byte, consensus.SchnorrSigLen)
		}
	}
	feeFor := func() consensus.Amount {
		placeholders()
		withChange := *tx
		withChange.Outputs = append(append([]consensus.TxOut{}, tx.Outputs...), s.cfg.Custody.Output(0), memoOut)
		return consensus.Amount(withChange.SerializedSize()+16)*s.cfg.FeeRate + assetSurcharge(&withChange) // small margin over the relay minimum
	}
	for _, u := range anchors {
		add(u)
	}
	fee := feeFor()
	if in < fee+wallet.DustLimit {
		for _, u := range s.st.UTXOs() {
			if len(tx.Inputs) >= maxIssueInputs {
				break
			}
			op := u.Outpoint()
			if used[op] || reserved[op] || u.Height == 0 || (u.Coinbase && u.Height+consensus.CoinbaseMaturity > tipHeight+1) {
				continue
			}
			live, st, err := s.custodyCoin(op, tipHeight)
			if err != nil || st == coinImmature {
				continue
			}
			if st == coinMissing {
				s.st.DelUTXO(u.TxID, u.Index)
				s.cfg.Logf("bridge: custody coin %s is not in the node's chain any more; dropped", op)
				continue
			}
			add(live)
			if fee = feeFor(); in >= fee+wallet.DustLimit {
				break
			}
		}
	}
	if len(tx.Inputs) == 0 {
		return nil, nil, errors.New("custody holds no confirmed ARG to pay the fee")
	}
	if in < fee {
		return nil, nil, fmt.Errorf("custody holds %s ARG but the fee is %s", wallet.FormatAmount(in), wallet.FormatAmount(fee))
	}
	if change := in - fee; change >= wallet.DustLimit {
		tx.Outputs = append(tx.Outputs, s.cfg.Custody.Output(change))
	}
	tx.Outputs = append(tx.Outputs, memoOut)
	placeholders()
	return tx, spent, nil
}

// custody coin lookups

type coinState int

const (
	coinMissing  coinState = iota // not in the node's chain (spent, or never was)
	coinImmature                  // a coinbase not yet spendable
	coinOK
)

// custodyCoin asks the node for a custody coin. The node's UTXO set is the
// authority: a coin is usable only if it is there, pays the custody key in
// ARG and is mature.
func (s *Service) custodyCoin(outpoint string, tipHeight uint32) (UTXO, coinState, error) {
	txid, idx, ok := parseOutpoint(outpoint)
	if !ok {
		return UTXO{}, coinMissing, nil
	}
	raw, err := s.cfg.Node.Call("gettxout", txid, idx)
	if err != nil {
		return UTXO{}, coinMissing, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return UTXO{}, coinMissing, nil
	}
	var c struct {
		Amount   consensus.Amount `json:"amount"`
		Type     uint8            `json:"type"`
		Payload  string           `json:"payload"`
		Height   uint32           `json:"height"`
		Coinbase bool             `json:"coinbase"`
		Asset    string           `json:"asset"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return UTXO{}, coinMissing, err
	}
	payload, err := hex.DecodeString(c.Payload)
	if err != nil {
		return UTXO{}, coinMissing, nil
	}
	o := consensus.TxOut{Amount: c.Amount, Type: c.Type, Payload: payload}
	if c.Asset != "" || !s.cfg.Custody.PaysCustody(&o) {
		return UTXO{}, coinMissing, nil
	}
	u := UTXO{TxID: strings.ToLower(txid), Index: idx, Amount: c.Amount, Height: c.Height, Coinbase: c.Coinbase}
	if c.Coinbase && c.Height+consensus.CoinbaseMaturity > tipHeight+1 {
		return u, coinImmature, nil
	}
	return u, coinOK, nil
}

// issueDead reports whether an issuance signed earlier can never confirm:
// one of its custody coins was spent on chain by another transaction that
// is at least issueDeadDepth (and l1_confirmations) deep.
func (s *Service) issueDead(old *SignedIssue, indexTip uint32) bool {
	depth := s.cfg.L1Confirmations
	if depth < issueDeadDepth {
		depth = issueDeadDepth
	}
	for _, op := range old.Inputs {
		if sp, ok := s.st.CustodySpend(op); ok && sp.TxID != old.TxID && sp.Height+depth <= indexTip+1 {
			return true
		}
	}
	return false
}

// checkSigningMemory refuses an issuance that does not conflict with every
// issuance this guardian signed before for any of its deposits.
func (s *Service) checkSigningMemory(f *issueFacts) error {
	h, _, _ := s.st.Cursor("l1")
	ins := map[string]bool{}
	for _, op := range f.inputs {
		ins[op] = true
	}
	for _, ref := range f.refs {
		for _, old := range s.st.SignedIssuesFor(ref) {
			if old.TxID == f.txid {
				continue
			}
			conflicts := false
			for _, op := range old.Inputs {
				if ins[op] {
					conflicts = true
					break
				}
			}
			if conflicts || s.issueDead(&old, uint32(h)) {
				continue
			}
			return fmt.Errorf("deposit %s: this guardian already signed issuance %s for it, which spends %s; another issuance for it must spend one of those custody coins", ref, old.TxID, strings.Join(old.Inputs, ", "))
		}
	}
	return nil
}

// ---- checking an issuance ----

// verifyIssue is every guardian's independent check of an issuance before it
// signs (see the comment at the top of this file).
func (s *Service) verifyIssue(ctx context.Context, req SignRequest) (*consensus.Tx, []consensus.TxOut, *issueFacts, error) {
	raw, err := hex.DecodeString(req.TxHex)
	if err != nil {
		return nil, nil, nil, err
	}
	tx, err := consensus.DecodeTx(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := consensus.CheckTxStateless(tx); err != nil {
		return nil, nil, nil, err
	}
	if len(tx.Inputs) > maxIssueInputs {
		return nil, nil, nil, fmt.Errorf("an issuance spends at most %d custody coins, not %d", maxIssueInputs, len(tx.Inputs))
	}
	if len(req.Spent) != len(tx.Inputs) {
		return nil, nil, nil, errors.New("spent outputs do not match inputs")
	}
	if len(req.Inbound) == 0 || len(req.Inbound) > maxIssueBatch {
		return nil, nil, nil, fmt.Errorf("an issuance pays 1 to %d deposits", maxIssueBatch)
	}
	if p, ok := s.st.GetPause(pauseL1); ok {
		return nil, nil, nil, fmt.Errorf("issuance is paused on this guardian: %s", p.Reason)
	}
	info, err := s.cfg.Node.ChainInfo()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("node: %w", err)
	}
	facts := &issueFacts{txid: tx.TxID().String(), refs: req.Inbound}
	spent := make([]consensus.TxOut, len(tx.Inputs))
	var in consensus.Amount
	for i, sh := range req.Spent {
		o, err := decodeTxOut(sh)
		if err != nil {
			return nil, nil, nil, err
		}
		if !s.cfg.Custody.PaysCustody(&o) || o.Asset != consensus.NativeAsset {
			return nil, nil, nil, errors.New("input is not a custody ARG coin")
		}
		op := outpointString(tx.Inputs[i].PrevOut.TxID.String(), tx.Inputs[i].PrevOut.Index)
		coin, st, err := s.custodyCoin(op, info.Blocks)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("custody coin %s: %w", op, err)
		}
		if st != coinOK {
			return nil, nil, nil, fmt.Errorf("custody coin %s is not spendable on this guardian's node", op)
		}
		if coin.Amount != o.Amount {
			return nil, nil, nil, fmt.Errorf("custody coin %s holds %d, not %d", op, coin.Amount, o.Amount)
		}
		spent[i] = o
		in += o.Amount
		facts.inputs = append(facts.inputs, op)
	}
	if len(tx.Issuances) != 1 {
		return nil, nil, nil, errors.New("exactly one issuance expected")
	}
	is := tx.Issuances[0]
	if is.Asset != s.asset || is.Issuer != s.issuer || is.Symbol != s.cfg.AssetSymbol {
		return nil, nil, nil, errors.New("issuance is not this bridge's asset")
	}
	memo, err := EncodeIssueMemo(req.Inbound)
	if err != nil {
		return nil, nil, nil, err
	}
	usedIDs := map[InboundRef]bool{}
	var deps []*Inbound
	var issued, out consensus.Amount
	di := 0
	change, memos := 0, 0
	for i := range tx.Outputs {
		o := &tx.Outputs[i]
		switch {
		case o.Type == consensus.OutputTypeNullData:
			if memos++; memos > 1 || !bytes.Equal(o.Payload, memo) {
				return nil, nil, nil, errors.New("the deposit references do not name the deposits paid")
			}
			continue
		case o.Asset == consensus.NativeAsset:
			if !s.cfg.Custody.PaysCustody(o) {
				return nil, nil, nil, errors.New("ARG output does not return to custody")
			}
			if change++; change > 1 {
				return nil, nil, nil, errors.New("more than one change output")
			}
			out += o.Amount
			continue
		case o.Asset != s.asset:
			return nil, nil, nil, errors.New("output in a foreign asset")
		}
		if di >= len(req.Inbound) {
			return nil, nil, nil, errors.New("more asset outputs than deposits")
		}
		ref := req.Inbound[di]
		di++
		if usedIDs[ref] {
			return nil, nil, nil, errors.New("deposit listed twice")
		}
		usedIDs[ref] = true
		d, ok := s.st.GetInbound(ref.Chain, ref.ID)
		if ok && d.Status == InboundReorged {
			return nil, nil, nil, fmt.Errorf("deposit %s is not in block %d of %s any more (reorganised): it is never issued", ref, d.Block, d.ChainName)
		}
		if !ok || d.Status != InboundPending {
			return nil, nil, nil, fmt.Errorf("deposit %s is not pending here", ref)
		}
		dc, ok := s.byID[d.Chain]
		if !ok {
			return nil, nil, nil, fmt.Errorf("deposit %s is on a chain this guardian does not serve", ref)
		}
		if why := s.issuancePaused(dc); why != "" {
			return nil, nil, nil, fmt.Errorf("deposit %s: %s", ref, why)
		}
		if why := s.depositBlocker(dc, d); why != "" {
			return nil, nil, nil, fmt.Errorf("deposit %s: %s", ref, why)
		}
		dest, err := wallet.DecodeAddress(s.cfg.Params.HRP, d.To)
		if err != nil || dest.Type != o.Type || !bytes.Equal(dest.Payload, o.Payload) {
			return nil, nil, nil, fmt.Errorf("deposit %s pays the wrong address", ref)
		}
		if o.Amount != d.Amount {
			return nil, nil, nil, fmt.Errorf("deposit %s pays %d, expected %d", ref, o.Amount, d.Amount)
		}
		if issued, err = consensus.AddAssetAmounts(issued, o.Amount); err != nil {
			return nil, nil, nil, err
		}
		deps = append(deps, d)
	}
	if memos != 1 {
		return nil, nil, nil, errors.New("the issuance carries no deposit references")
	}
	if di != len(req.Inbound) {
		return nil, nil, nil, errors.New("fewer asset outputs than deposits")
	}
	if is.Amount != issued {
		return nil, nil, nil, fmt.Errorf("issuance creates %d but outputs pay %d", is.Amount, issued)
	}
	facts.amount = is.Amount
	// Finality against heads read now from every provider, and every
	// deposit's receipt read now: nothing the leader says about a deposit is
	// taken on trust.
	finals := map[uint64]uint64{}
	for _, d := range deps {
		c := s.byID[d.Chain]
		final, ok := finals[c.ID]
		if !ok {
			if final, err = s.actionableFinal(ctx, c); err != nil {
				return nil, nil, nil, fmt.Errorf("%s: %w", c.Name, err)
			}
			finals[c.ID] = final
		}
		if d.Block > final {
			mode := ""
			if c.Finality == FinalityFinalized {
				mode = " and below the finalized block"
			}
			return nil, nil, nil, fmt.Errorf("deposit %d#%d in block %d is not final on %s here: the final block is %d (%d confirmations%s)", d.Chain, d.ID, d.Block, c.Name, final, c.Confirmations, mode)
		}
		err := s.checkDeposit(ctx, c, d, final)
		var gone depositGoneErr
		var moved depositMovedErr
		switch {
		case errors.As(err, &gone):
			return nil, nil, nil, errors.New(gone.msg)
		case errors.As(err, &moved):
			return nil, nil, nil, fmt.Errorf("deposit %d#%d is not in block %d of %s any more: its transaction is in block %d now", d.Chain, d.ID, d.Block, c.Name, moved.ev.Block)
		case err != nil:
			return nil, nil, nil, fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	if s.cfg.MaxIssue > 0 && is.Amount > s.cfg.MaxIssue {
		return nil, nil, nil, fmt.Errorf("the issuance of %s exceeds this guardian's per-issuance limit of %s", wallet.FormatAmount(is.Amount), wallet.FormatAmount(s.cfg.MaxIssue))
	}
	if s.cfg.MaxIssueDay > 0 {
		// Each deposit counts once: those of this issuance are in is.Amount.
		mine := map[InboundRef]bool{}
		for _, ref := range facts.refs {
			mine[ref] = true
		}
		used := sumSigned(s.st.SignedSince(time.Now().Add(-issueDay).Unix()), mine)
		if used+is.Amount > s.cfg.MaxIssueDay {
			return nil, nil, nil, fmt.Errorf("the issuance of %s would exceed this guardian's daily limit of %s (%s signed in the last 24 hours)", wallet.FormatAmount(is.Amount), wallet.FormatAmount(s.cfg.MaxIssueDay), wallet.FormatAmount(used))
		}
	}
	if err := s.checkSigningMemory(facts); err != nil {
		return nil, nil, nil, err
	}
	fee := (consensus.Amount(tx.SerializedSize())*s.cfg.FeeRate + assetSurcharge(tx)) * 2 // allow the leader a different size estimate
	if in-out > fee {
		return nil, nil, nil, fmt.Errorf("fee %d exceeds the allowed %d", in-out, fee)
	}
	return tx, spent, facts, nil
}

func encodeTxOut(o consensus.TxOut) string {
	var w consensus.Writer
	w.I64(int64(o.Amount))
	w.U8(o.Type)
	w.VarBytes(o.Payload)
	return hex.EncodeToString(w.Bytes())
}

func decodeTxOut(s string) (consensus.TxOut, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return consensus.TxOut{}, err
	}
	r := consensus.NewReader(b)
	a, err := r.I64()
	if err != nil {
		return consensus.TxOut{}, err
	}
	t, err := r.U8()
	if err != nil {
		return consensus.TxOut{}, err
	}
	p, err := r.VarBytes(consensus.MaxNullDataLen)
	if err != nil {
		return consensus.TxOut{}, err
	}
	return consensus.TxOut{Amount: consensus.Amount(a), Type: t, Payload: p}, r.ExpectEOF()
}

// messages lists everything the custody key must sign for tx: one message
// per input, then the issuance message.
func (s *Service) messages(tx *consensus.Tx, spent []consensus.TxOut) [][32]byte {
	cache := consensus.NewSigHashCache(s.cfg.Params.ChainTag, tx, spent)
	out := make([][32]byte, 0, len(tx.Inputs)+len(tx.Issuances))
	for i := range tx.Inputs {
		out = append(out, cache.Hash(uint32(i)))
	}
	for range tx.Issuances {
		out = append(out, cache.IssuanceHash())
	}
	return out
}

// ---- MuSig2 with every guardian ----

// signWithGuardians runs the two MuSig2 rounds with every guardian (the
// leader included) and fills tx.Witnesses.
func (s *Service) signWithGuardians(ctx context.Context, req SignRequest, tx *consensus.Tx, spent []consensus.TxOut) error {
	n := s.cfg.Custody.N()
	if 1+len(s.cfg.Peers) != n {
		return fmt.Errorf("%d guardians configured but custody needs %d", 1+len(s.cfg.Peers), n)
	}
	local, err := s.newSigning(ctx, req)
	if err != nil {
		return err
	}
	k := len(local.sessions)
	allNonces := make([][][]byte, k) // [message][guardian]
	for i := 0; i < k; i++ {
		allNonces[i] = append(allNonces[i], local.nonces[i])
	}
	for _, peer := range s.cfg.Peers {
		var resp struct {
			Nonces []string `json:"nonces"`
		}
		if err := s.callPeer(ctx, peer, "/guardian/nonces", req, &resp); err != nil {
			return fmt.Errorf("%s: %w", peer, err)
		}
		if len(resp.Nonces) != k {
			return fmt.Errorf("%s: wrong nonce count", peer)
		}
		for i, nh := range resp.Nonces {
			nb, err := hex.DecodeString(nh)
			if err != nil {
				return err
			}
			allNonces[i] = append(allNonces[i], nb)
		}
	}
	partials := make([][][]byte, k)
	own, err := s.partialSign(ctx, local, allNonces)
	if err != nil {
		return err
	}
	for i := 0; i < k; i++ {
		partials[i] = append(partials[i], own[i])
	}
	noncesHex := make([][]string, k)
	for i := range allNonces {
		for _, nb := range allNonces[i] {
			noncesHex[i] = append(noncesHex[i], hex.EncodeToString(nb))
		}
	}
	for _, peer := range s.cfg.Peers {
		var resp struct {
			Partials []string `json:"partials"`
		}
		if err := s.callPeer(ctx, peer, "/guardian/sign", map[string]any{"id": req.ID, "nonces": noncesHex}, &resp); err != nil {
			return fmt.Errorf("%s: %w", peer, err)
		}
		if len(resp.Partials) != k {
			return fmt.Errorf("%s: wrong partial count", peer)
		}
		for i, ph := range resp.Partials {
			pb, err := hex.DecodeString(ph)
			if err != nil {
				return err
			}
			partials[i] = append(partials[i], pb)
		}
	}
	for i := 0; i < k; i++ {
		for g := 1; g < len(partials[i]); g++ {
			if _, err := local.sessions[i].Combine(partials[i][g]); err != nil {
				return err
			}
		}
		sig, err := local.sessions[i].Final()
		if err != nil {
			return err
		}
		tx.Witnesses[i] = sig
	}
	return nil
}

// newSigning verifies a request and creates this guardian's sessions.
func (s *Service) newSigning(ctx context.Context, req SignRequest) (*pendingSigning, error) {
	tx, spent, _, err := s.verifyIssue(ctx, req)
	if err != nil {
		return nil, err
	}
	p := &pendingSigning{req: req, created: time.Now(), ready: true}
	for range s.messages(tx, spent) {
		sess, err := s.cfg.Custody.NewSession()
		if err != nil {
			return nil, err
		}
		p.sessions = append(p.sessions, sess)
		p.nonces = append(p.nonces, sess.PublicNonce())
	}
	return p, nil
}

// partialSign checks the request again, registers the other guardians'
// nonces, records the issuance in the signing memory (on disk) and only
// then returns this guardian's partial signatures. Sessions are single-use.
func (s *Service) partialSign(ctx context.Context, p *pendingSigning, nonces [][][]byte) ([][]byte, error) {
	s.signMu.Lock()
	defer s.signMu.Unlock()
	tx, spent, facts, err := s.verifyIssue(ctx, p.req)
	if err != nil {
		return nil, err
	}
	msgs := s.messages(tx, spent)
	if len(nonces) != len(msgs) || len(p.sessions) != len(msgs) {
		return nil, errors.New("nonce list does not match the messages")
	}
	n := s.cfg.Custody.N()
	for i := range msgs {
		if len(nonces[i]) != n {
			return nil, fmt.Errorf("message %d has %d nonces, want one per guardian (%d)", i, len(nonces[i]), n)
		}
		own := 0
		for _, nb := range nonces[i] {
			if bytes.Equal(nb, p.nonces[i]) {
				own++
			}
		}
		if own != 1 {
			return nil, fmt.Errorf("message %d: this guardian's nonce must appear exactly once", i)
		}
	}
	for i := range msgs {
		for _, nb := range nonces[i] {
			if bytes.Equal(nb, p.nonces[i]) {
				continue // our own
			}
			if _, err := p.sessions[i].RegisterNonce(nb); err != nil {
				return nil, err
			}
		}
	}
	if err := s.st.RecordSignedIssue(&SignedIssue{TxID: facts.txid, Inputs: facts.inputs, Inbound: facts.refs, Amount: facts.amount, Time: time.Now().Unix()}); err != nil {
		return nil, fmt.Errorf("recording the issuance before signing it: %w", err)
	}
	out := make([][]byte, len(msgs))
	for i := range msgs {
		partial, err := p.sessions[i].Sign(msgs[i])
		if err != nil {
			return nil, err
		}
		out[i] = partial
	}
	return out, nil
}

// followerNonces answers /guardian/nonces: a new signing session, at most
// maxSigningSessions at a time, each living signingSessionTTL.
func (s *Service) followerNonces(ctx context.Context, req SignRequest) ([]string, error) {
	if req.ID == "" || len(req.ID) > 64 {
		return nil, errors.New("bad signing request id")
	}
	now := time.Now()
	s.mu.Lock()
	for id, old := range s.sessions {
		if now.Sub(old.created) > signingSessionTTL {
			delete(s.sessions, id)
		}
	}
	if _, dup := s.sessions[req.ID]; dup {
		s.mu.Unlock()
		return nil, errors.New("signing request id already in use")
	}
	if len(s.sessions) >= maxSigningSessions {
		s.mu.Unlock()
		return nil, errors.New("too many signing sessions in progress; try again shortly")
	}
	slot := &pendingSigning{created: now}
	s.sessions[req.ID] = slot // holds the place while the request is checked
	s.mu.Unlock()
	p, err := s.newSigning(ctx, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.sessions[req.ID] == slot {
			delete(s.sessions, req.ID)
		}
		return nil, err
	}
	s.sessions[req.ID] = p
	out := make([]string, len(p.nonces))
	for i, n := range p.nonces {
		out[i] = hex.EncodeToString(n)
	}
	return out, nil
}

// followerSign answers /guardian/sign: once per session.
func (s *Service) followerSign(ctx context.Context, id string, nonces [][][]byte) ([][]byte, error) {
	s.mu.Lock()
	p, ok := s.sessions[id]
	if ok && p.ready {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok || !p.ready {
		return nil, errors.New("unknown signing request")
	}
	if time.Since(p.created) > signingSessionTTL {
		return nil, errors.New("signing request expired")
	}
	return s.partialSign(ctx, p, nonces)
}
