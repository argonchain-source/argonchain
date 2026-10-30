package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// The website's miner count (GET /api/miners): how many miners are at work
// and in which countries. A miner is a payout address. Every address that
// asks for work is noted with the time and the country of the request
// (geoip.go); miners on their own node never ask for work, so the addresses
// paid by the latest blocks count too (without a country). Addresses are
// kept only as salted hashes and IP addresses not at all; the book is saved
// to <datadir>/miners.json so a restart does not reset the counts.

const (
	minersOnline      = 15 * time.Minute // asked for work this recently: mining now
	minersFoundOnline = time.Hour        // found a block this recently: mining now (node miners)
	minersDay         = 24 * time.Hour
	minersMax         = 200000 // addresses remembered at most; the stalest go first
	minersViewTTL     = 30 * time.Second
)

type minerSeen struct {
	T int64  `json:"t"`           // unix seconds of the latest request
	C string `json:"c,omitempty"` // country of the latest located request
}

type minerBook struct {
	mu    sync.Mutex
	path  string
	geo   *geoDB
	seen  map[string]minerSeen
	dirty bool

	viewMu sync.Mutex
	viewAt time.Time
	view   minersView
}

type countryCount struct {
	CC     string `json:"cc"`
	Miners int    `json:"miners"` // last 24 hours
	Online int    `json:"online"` // mining now
}

type minersView struct {
	Online    int            `json:"online"`    // mining now
	Day       int            `json:"day"`       // mined in the last 24 hours
	Countries []countryCount `json:"countries"` // last 24 hours, most miners first
	Unlocated int            `json:"unlocated"` // last 24 hours, country unknown (own node, private network)
	Updated   int64          `json:"updated"`
	Source    string         `json:"geo_source"`
}

func minerKey(addr string) string {
	h := sha256.Sum256([]byte("argonmine/miner:" + addr))
	return hex.EncodeToString(h[:12])
}

func newMinerBook(path string, geo *geoDB) *minerBook {
	b := &minerBook{path: path, geo: geo, seen: map[string]minerSeen{}}
	if raw, err := os.ReadFile(path); err == nil {
		var m map[string]minerSeen
		if json.Unmarshal(raw, &m) == nil {
			cut := time.Now().Add(-minersDay).Unix()
			for k, v := range m {
				if v.T >= cut && len(k) == 24 && (v.C == "" || len(v.C) == 2) {
					b.seen[k] = v
				}
			}
		}
	}
	return b
}

// note records that addr asked for work from ip.
func (b *minerBook) note(addr, ip string, now time.Time) {
	if addr == "" {
		return
	}
	cc := ""
	if b.geo != nil {
		cc = b.geo.country(ip)
	}
	k := minerKey(addr)
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.seen[k]
	e.T = now.Unix()
	if cc != "" {
		e.C = cc
	}
	b.seen[k] = e
	b.dirty = true
	if len(b.seen) > minersMax {
		b.pruneLocked(now, true)
	}
}

// pruneLocked drops entries older than a day, and with full also the
// stalest tenth when the book is over its size.
func (b *minerBook) pruneLocked(now time.Time, full bool) {
	cut := now.Add(-minersDay).Unix()
	for k, v := range b.seen {
		if v.T < cut {
			delete(b.seen, k)
		}
	}
	if full && len(b.seen) > minersMax {
		ts := make([]int64, 0, len(b.seen))
		for _, v := range b.seen {
			ts = append(ts, v.T)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		limit := ts[len(ts)/10]
		for k, v := range b.seen {
			if v.T <= limit {
				delete(b.seen, k)
			}
		}
	}
}

// saveLoop prunes and saves the book once a minute.
func (b *minerBook) saveLoop(logf func(string, ...any)) {
	for range time.Tick(time.Minute) {
		b.save(logf)
	}
}

func (b *minerBook) save(logf func(string, ...any)) {
	b.mu.Lock()
	b.pruneLocked(time.Now(), false)
	if !b.dirty || b.path == "" {
		b.mu.Unlock()
		return
	}
	raw, err := json.Marshal(b.seen)
	b.dirty = false
	b.mu.Unlock()
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		err = os.Rename(tmp, b.path)
		if err != nil {
			logf("miners: %v", err)
		}
	}
}

// foundBlock is a block's payout address and time (unix seconds).
type foundBlock struct {
	Miner string
	Time  int64
}

// compute builds the view at now from the book and the latest blocks.
func (b *minerBook) compute(now time.Time, found []foundBlock) minersView {
	online, day, country := map[string]bool{}, map[string]bool{}, map[string]string{}
	onCut, dayCut := now.Add(-minersOnline).Unix(), now.Add(-minersDay).Unix()
	b.mu.Lock()
	for k, v := range b.seen {
		if v.T >= dayCut {
			day[k] = true
			if v.C != "" {
				country[k] = v.C
			}
		}
		if v.T >= onCut {
			online[k] = true
		}
	}
	b.mu.Unlock()
	foundCut := now.Add(-minersFoundOnline).Unix()
	for _, f := range found {
		if f.Miner == "" || f.Time < dayCut {
			continue
		}
		k := minerKey(f.Miner)
		day[k] = true
		if f.Time >= foundCut {
			online[k] = true
		}
	}
	per, perOn := map[string]int{}, map[string]int{}
	for k := range day {
		if cc := country[k]; cc != "" {
			per[cc]++
			if online[k] {
				perOn[cc]++
			}
		}
	}
	v := minersView{Online: len(online), Day: len(day), Countries: []countryCount{}, Updated: now.Unix(), Source: "regional internet registry data (CC BY 4.0, NRO)"}
	located := 0
	for cc, n := range per {
		v.Countries = append(v.Countries, countryCount{cc, n, perOn[cc]})
		located += n
	}
	sort.Slice(v.Countries, func(i, j int) bool {
		a, c := v.Countries[i], v.Countries[j]
		return a.Miners > c.Miners || (a.Miners == c.Miners && a.CC < c.CC)
	})
	v.Unlocated = v.Day - located
	return v
}

// apiMiners answers GET /api/miners for the website.
func (s *server) apiMiners(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=30")
	if s.miners == nil {
		json.NewEncoder(w).Encode(minersView{Countries: []countryCount{}})
		return
	}
	b := s.miners
	b.viewMu.Lock()
	defer b.viewMu.Unlock()
	if time.Since(b.viewAt) > minersViewTTL {
		var found []foundBlock
		if s.ix != nil {
			if h, _, _ := s.ix.status(); h >= 0 {
				for _, blk := range s.ix.latestBlocks(h, 300) {
					found = append(found, foundBlock{blk.Miner, int64(blk.Time)})
				}
			}
		}
		b.view, b.viewAt = b.compute(time.Now(), found), time.Now()
	}
	json.NewEncoder(w).Encode(b.view)
}
