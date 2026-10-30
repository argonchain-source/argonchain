package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"net/netip"
	"sort"
	"strings"
	"sync"
)

// The country of a miner, for the miners-per-country count on the website
// (miners.go). geo/ip-country.csv.gz is built from the regional internet
// registries' delegation data (CC BY 4.0 by the NRO, see geo/README.md):
// first,last,country per line. It is read once, in the background, into
// sorted ranges; a lookup is a binary search.

//go:embed geo/ip-country.csv.gz
var ipCountry []byte

type geoRange struct {
	first, last netip.Addr
	cc          uint16 // index into geoDB.codes
}

type geoDB struct {
	once   sync.Once
	ready  chan struct{}
	v4, v6 []geoRange
	codes  []string
}

func newGeoDB() *geoDB { return &geoDB{ready: make(chan struct{})} }

// load parses the embedded database; it is safe to call more than once.
func (g *geoDB) load(logf func(string, ...any)) {
	g.once.Do(func() {
		defer close(g.ready)
		zr, err := gzip.NewReader(bytes.NewReader(ipCountry))
		if err != nil {
			logf("geo: %v", err)
			return
		}
		index := map[string]uint16{}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			parts := strings.Split(sc.Text(), ",")
			if len(parts) != 3 || len(parts[2]) != 2 || parts[2] == "ZZ" {
				continue
			}
			first, err1 := netip.ParseAddr(parts[0])
			last, err2 := netip.ParseAddr(parts[1])
			if err1 != nil || err2 != nil || first.Is4() != last.Is4() {
				continue
			}
			cc := strings.ToUpper(parts[2])
			i, ok := index[cc]
			if !ok {
				i = uint16(len(g.codes))
				index[cc] = i
				g.codes = append(g.codes, cc)
			}
			r := geoRange{first, last, i}
			if first.Is4() {
				g.v4 = appendRange(g.v4, r)
			} else {
				g.v6 = appendRange(g.v6, r)
			}
		}
		if err := sc.Err(); err != nil {
			logf("geo: %v", err)
		}
		for _, rs := range [][]geoRange{g.v4, g.v6} {
			sort.Slice(rs, func(i, j int) bool { return rs[i].first.Less(rs[j].first) })
		}
		logf("geo: %d IPv4 and %d IPv6 ranges in %d countries", len(g.v4), len(g.v6), len(g.codes))
	})
}

// appendRange merges r into the previous range when they are adjacent and
// name the same country (the file is sorted), keeping the table small.
func appendRange(rs []geoRange, r geoRange) []geoRange {
	if n := len(rs); n > 0 && rs[n-1].cc == r.cc && rs[n-1].last.Next() == r.first {
		rs[n-1].last = r.last
		return rs
	}
	return append(rs, r)
}

// country returns the ISO 3166-1 alpha-2 code of ip, or "" when it is not
// known yet, private, malformed or not in the database.
func (g *geoDB) country(ip string) string {
	select {
	case <-g.ready:
	default:
		return ""
	}
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ""
	}
	a = a.Unmap()
	if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return ""
	}
	rs := g.v6
	if a.Is4() {
		rs = g.v4
	}
	i := sort.Search(len(rs), func(i int) bool { return a.Less(rs[i].first) }) - 1
	if i < 0 || rs[i].last.Less(a) {
		return ""
	}
	return g.codes[rs[i].cc]
}
