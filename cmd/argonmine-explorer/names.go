package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/names"
	"argonmine/wallet"
)

// Names on the explorer (docs/NAMES.md): the JSON API the app reads, the
// /names and /name/{label} pages, and the name shown for an address.

const (
	namesRecentDefault = 50
	namesRecentMax     = 200
	nameHistoryMax     = 500
)

// Statuses the API adds to the ones of package names.
const (
	statusPending  = "pending"
	statusReserved = "reserved"
	statusInvalid  = "invalid"
)

// nameInfo is one name as the API and the pages see it.
type nameInfo struct {
	Label   string
	Status  string
	Reason  string // for an invalid label
	Rec     *names.Record
	Height  int64 // the index height the status was judged at
	Price   consensus.Amount
	History []nameEvent
}

// Display is the name as people write it.
func (n nameInfo) Display() string { return names.Display(n.Label) }

// lookupName judges label at the index height. label is what the person
// typed: it is normalised first.
func (s *server) lookupName(label string, withHistory bool) nameInfo {
	label = names.Normalize(label)
	n := nameInfo{Label: label, Height: s.ix.indexedHeight(), History: []nameEvent{}}
	if err := names.Check(label); err != nil {
		n.Status, n.Reason = statusInvalid, err.Error()
		return n
	}
	n.Price = names.PricePerYear(s.params, label)
	if r, ok := s.ix.nameRecord(label); ok {
		n.Rec = &r
	}
	h := uint32(0)
	if n.Height >= 0 {
		h = uint32(n.Height)
	}
	n.Status = names.Status(n.Rec, h)
	if n.Status == names.StatusAvailable {
		switch {
		case names.IsReserved(label):
			n.Status = statusReserved
		case s.pendingRegister(label):
			n.Status = statusPending
		}
	}
	if withHistory {
		n.History = s.ix.nameHistory(label, nameHistoryMax)
	}
	return n
}

// current reports whether the record is the name's present ownership
// (registered or in grace) rather than a dropped one.
func (n nameInfo) current() bool {
	return n.Rec != nil && (n.Status == names.StatusRegistered || n.Status == names.StatusGrace)
}

// pendingRegister reports whether a mempool transaction carries a register
// of label that would succeed in the next block. The mempool view is
// rebuilt at most every few seconds (cache.go).
func (s *server) pendingRegister(label string) bool {
	if s.client == nil {
		return false
	}
	h := s.ix.indexedHeight()
	if h < 0 {
		return false
	}
	for _, rw := range s.mempoolView().All {
		if rw.Name == nil || rw.Name.Kind != names.OpRegister || rw.Name.Label != label {
			continue
		}
		tx := names.Tx{TxID: rw.TxID}
		if len(rw.Spends) > 0 {
			tx.Signer = rw.Spends[0].Addr
		}
		treasury := names.TreasuryAddress(s.params)
		for _, o := range rw.Pays {
			if o.Index == 0 {
				tx.Out0 = o.Addr
			}
			if treasury != "" && o.Asset == "" && strings.EqualFold(o.Addr, treasury) {
				tx.Paid = satAdd(tx.Paid, o.Amount)
			}
		}
		if _, ok := names.Apply(s.params, nil, *rw.Name, tx, uint32(h)+1); ok {
			return true
		}
	}
	return false
}

// ---- JSON API ----

func writeNamesJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// apiNamesInfo serves GET /api/names/info: the rules and prices.
func (s *server) apiNamesInfo(w http.ResponseWriter, r *http.Request) {
	h := s.ix.indexedHeight()
	var count int64
	if h >= 0 {
		count = s.ix.activeNames(uint32(h))
	}
	writeNamesJSON(w, http.StatusOK, map[string]any{
		"suffix": names.Suffix, "min_len": names.MinLen, "max_len": names.MaxLen,
		"blocks_per_year": names.BlocksPerYear, "grace_blocks": names.GraceBlocks, "years_max": names.YearsMax,
		"prices": names.Prices(s.params), "treasury": names.TreasuryAddress(s.params), "reserved": names.Reserved,
		"count": count, "height": h,
	})
}

// apiName serves GET /api/name/{label}.
func (s *server) apiName(w http.ResponseWriter, r *http.Request) {
	n := s.lookupName(strings.TrimPrefix(r.URL.Path, "/api/name/"), true)
	out := map[string]any{
		"name": n.Label, "status": n.Status, "price": n.Price, "history": n.History,
		"owner": "", "primary": false, "registered_height": 0, "registered_time": 0, "expires_height": 0, "expires_time": 0, "txid": "",
	}
	if n.Status == statusInvalid {
		out["reason"] = n.Reason
	}
	if n.current() {
		out["owner"], out["primary"], out["txid"] = n.Rec.Owner, n.Rec.Primary, n.Rec.RegTxID
		out["registered_height"], out["registered_time"] = n.Rec.Registered, s.ix.timeAt(n.Rec.Registered)
		out["expires_height"], out["expires_time"] = n.Rec.Expires, s.ix.timeAt(n.Rec.Expires)
	}
	writeNamesJSON(w, http.StatusOK, out)
}

// apiResolve serves GET /api/resolve/{label}: the address an active name
// stands for.
func (s *server) apiResolve(w http.ResponseWriter, r *http.Request) {
	n := s.lookupName(strings.TrimPrefix(r.URL.Path, "/api/resolve/"), false)
	if n.Status != names.StatusRegistered {
		writeNamesJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	writeNamesJSON(w, http.StatusOK, map[string]any{"name": n.Label, "address": n.Rec.Owner})
}

// ownedName is one name of an address.
type ownedName struct {
	Label       string           `json:"name"`
	Expires     uint32           `json:"expires_height"`
	ExpiresTime uint64           `json:"expires_time"`
	Primary     bool             `json:"primary"`
	Grace       bool             `json:"grace"`
	Registered  uint32           `json:"-"`
	Display     string           `json:"-"`
	ExpiresIn   string           `json:"-"`
	Price       consensus.Amount `json:"-"`
}

// ownedNames lists the names addr holds, oldest first, with the index
// height they were judged at.
func (s *server) ownedNames(addr string) []ownedName {
	h := s.ix.indexedHeight()
	out := []ownedName{}
	for _, r := range s.ix.ownerNames(addr) {
		o := ownedName{Label: r.Label, Expires: r.Expires, ExpiresTime: s.ix.timeAt(r.Expires), Primary: r.Primary, Registered: r.Registered,
			Grace: h >= 0 && names.Status(&r, uint32(h)) == names.StatusGrace, Display: names.Display(r.Label), Price: names.PricePerYear(s.params, r.Label)}
		o.ExpiresIn = blocksAhead(int64(r.Expires) - h)
		out = append(out, o)
	}
	return out
}

// apiNamesByOwner serves GET /api/names?owner={address}.
func (s *server) apiNamesByOwner(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/names" {
		writeNamesJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	addr := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("owner")))
	if addr == "" {
		writeNamesJSON(w, http.StatusBadRequest, map[string]any{"error": "owner is required"})
		return
	}
	if _, err := wallet.DecodeAddress(s.params.HRP, addr); err != nil {
		writeNamesJSON(w, http.StatusNotFound, map[string]any{"error": "not a valid address for this network"})
		return
	}
	writeNamesJSON(w, http.StatusOK, map[string]any{"address": addr, "primary": s.ix.displayName(addr), "names": s.ownedNames(addr)})
}

// apiNamesRecent serves GET /api/names/recent?limit=50.
func (s *server) apiNamesRecent(w http.ResponseWriter, r *http.Request) {
	limit := namesRecentDefault
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > namesRecentMax {
		limit = namesRecentMax
	}
	h := s.ix.indexedHeight()
	var count int64
	if h >= 0 {
		count = s.ix.activeNames(uint32(h))
	}
	writeNamesJSON(w, http.StatusOK, map[string]any{"count": count, "names": s.ix.recentNames(limit)})
}

// ---- pages ----

// priceRow is one line of the price table.
type priceRow struct {
	Length string
	Price  consensus.Amount
}

// nameRow is one registration on the /names page.
type nameRow struct {
	nameReg
	Display   string
	ExpiresIn string
}

type namesPageData struct {
	Count        int64
	Day          int // registered in the last 24 hours
	Free         bool
	Treasury     string
	Prices       []priceRow
	From         consensus.Amount // the lowest price per year
	Recent       []nameRow
	Suffix       string
	HRP          string
	ReservedList []string
}

func (s *server) namesPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/names" {
		s.notFound(w, r.URL.Path)
		return
	}
	h := s.ix.indexedHeight()
	d := namesPageData{Free: !names.HasTreasury(s.params), Treasury: names.TreasuryAddress(s.params), Suffix: names.Suffix, HRP: s.params.HRP, ReservedList: names.Reserved, Recent: []nameRow{}}
	if h >= 0 {
		d.Count = s.ix.activeNames(uint32(h))
		d.Day = s.ix.namesSince(uint64(time.Now().Add(-24 * time.Hour).Unix()))
	}
	prices := names.Prices(s.params)
	lengths := make([]string, 0, len(prices))
	for l := range prices {
		lengths = append(lengths, l)
	}
	sort.Strings(lengths)
	for _, l := range lengths {
		row := priceRow{Length: l + " characters", Price: prices[l]}
		if l == lengths[len(lengths)-1] {
			row.Length = l + " characters or more"
		}
		if d.From == 0 || row.Price < d.From {
			d.From = row.Price
		}
		d.Prices = append(d.Prices, row)
	}
	for _, reg := range s.ix.recentNames(namesRecentDefault) {
		d.Recent = append(d.Recent, nameRow{nameReg: reg, Display: names.Display(reg.Label), ExpiresIn: blocksAhead(int64(reg.Expires) - h)})
	}
	s.render(w, "names", "Names", d)
}

type namePageData struct {
	nameInfo
	OwnerName      string // the owner's display name, if any
	RegisteredTime uint64
	ExpiresTime    uint64
	ExpiresIn      string
	YearsLeft      string
	Free           bool
	Reserved       bool
}

// StatusWord is the status as the page's badge says it.
func (d namePageData) StatusWord() string {
	switch d.Status {
	case names.StatusRegistered:
		return "Registered"
	case names.StatusGrace:
		return "In grace"
	case statusPending:
		return "Pending"
	case statusReserved:
		return "Reserved"
	case statusInvalid:
		return "Invalid"
	}
	return "Available"
}

// BadgeClass is the badge style of the status.
func (d namePageData) BadgeClass() string {
	switch d.Status {
	case names.StatusRegistered:
		return "badge-ok"
	case names.StatusGrace:
		return "badge-warn"
	case statusPending:
		return "badge-accent"
	case statusInvalid:
		return "badge-danger"
	}
	return ""
}

// Current is whether the page shows an ownership (registered or in grace).
func (d namePageData) Current() bool { return d.nameInfo.current() }

func (s *server) namePage(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/name/")
	if raw == "" {
		// The look-up form on /names: /name/?q=alice.arg.
		if q := names.Normalize(r.URL.Query().Get("q")); q != "" {
			http.Redirect(w, r, "/name/"+q, http.StatusFound)
			return
		}
		http.Redirect(w, r, "/names", http.StatusFound)
		return
	}
	d := s.nameData(raw)
	if strings.TrimSpace(raw) != d.Label {
		http.Redirect(w, r, "/name/"+d.Label, http.StatusFound)
		return
	}
	s.render(w, "name", d.Display(), d)
}

// nameData builds the /name/{label} page.
func (s *server) nameData(raw string) namePageData {
	n := s.lookupName(raw, true)
	d := namePageData{nameInfo: n, Free: !names.HasTreasury(s.params), Reserved: names.IsReserved(n.Label)}
	if n.current() {
		d.OwnerName = s.ix.displayName(n.Rec.Owner)
		d.RegisteredTime = s.ix.timeAt(n.Rec.Registered)
		d.ExpiresTime = s.ix.timeAt(n.Rec.Expires)
		left := int64(n.Rec.Expires) - n.Height
		d.ExpiresIn = blocksAhead(left)
		d.YearsLeft = strconv.FormatFloat(float64(left)/names.BlocksPerYear, 'f', 2, 64)
	}
	return d
}

// nameOpView is a name operation as the transaction page shows it.
type nameOpView struct {
	Op        string
	Label     string
	Display   string
	Years     int
	Confirmed bool
	Applied   bool // the index recorded it (the operation was valid)
}

// Before and After are the words around the name: "register alice.arg for
// 2 years"; Text is the whole line.
func (v nameOpView) Before() string {
	if v.Op == names.OpPrimary {
		return "make"
	}
	return v.Op
}

func (v nameOpView) After() string {
	switch v.Op {
	case names.OpRegister, names.OpRenew:
		if v.Years == 1 {
			return "for 1 year"
		}
		return fmt.Sprintf("for %d years", v.Years)
	case names.OpTransfer:
		return "to output 0"
	case names.OpPrimary:
		return "the primary name of the signer"
	}
	return ""
}

func (v nameOpView) Text() string {
	return strings.TrimSpace(v.Before() + " " + v.Display + " " + v.After())
}

// nameOpOf finds the name operation of a transaction page's data, and
// whether the index applied it.
func (s *server) nameOpOf(tx map[string]any, typed *txJSON) *nameOpView {
	op, ok := names.FindMemo(nullDataPayloads(typed))
	if !ok {
		return nil
	}
	v := &nameOpView{Op: op.Kind, Label: op.Label, Display: names.Display(op.Label), Years: op.Years}
	if bh, _ := tx["blockhash"].(string); bh != "" {
		v.Confirmed = true
		if hf, ok := tx["height"].(float64); ok {
			_, v.Applied = s.ix.nameEventOf(op.Label, uint32(hf), typed.TxID)
		}
	}
	return v
}

// blocksAhead says how far n blocks is at ten minutes a block: "2.0
// years", "45 days", "3 h"; "expired" when n is not positive.
func blocksAhead(n int64) string {
	if n <= 0 {
		return "expired"
	}
	days := float64(n) * float64(consensus.TargetSpacing) / 86400
	switch {
	case days >= 365:
		return strconv.FormatFloat(days/365, 'f', 1, 64) + " years"
	case days >= 2:
		return strconv.Itoa(int(days)) + " days"
	default:
		return strconv.Itoa(int(days*24)) + " h"
	}
}
