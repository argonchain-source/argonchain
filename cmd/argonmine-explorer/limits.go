package main

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ---- client address ----

// clientIPFrom returns a function that names the client of a request for the
// per-IP limits. With header empty it is the TCP peer (RemoteAddr). Behind a
// proxy, header names the one the proxy sets: its right-most entry is used
// (for X-Forwarded-For the hop the proxy itself appended; a client can only
// add entries to the left of it), and a missing or malformed value falls
// back to the peer. IPv6 clients are grouped by /64, the block one
// subscriber usually holds.
func clientIPFrom(header string) func(*http.Request) string {
	header = http.CanonicalHeaderKey(strings.TrimSpace(header))
	var warn sync.Once
	return func(r *http.Request) string {
		if header == "" && r.Header.Get("X-Forwarded-For") != "" {
			warn.Do(func() {
				log.Printf("limits: requests arrive through a proxy (X-Forwarded-For) but -client-ip-header is not set: every client behind it shares one rate limit")
			})
		}
		if header != "" {
			if vals := r.Header.Values(header); len(vals) > 0 {
				all := strings.Split(strings.Join(vals, ","), ",")
				if ip, ok := parseIP(all[len(all)-1]); ok {
					return ipKey(ip)
				}
			}
		}
		if ip, ok := parseIP(r.RemoteAddr); ok {
			return ipKey(ip)
		}
		return r.RemoteAddr
	}
}

// parseIP accepts "1.2.3.4", "1.2.3.4:5", "::1", "[::1]:5".
func parseIP(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if ip, err := netip.ParseAddr(s); err == nil {
		return ip.Unmap(), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			return ip.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func ipKey(ip netip.Addr) string {
	if ip.Is6() {
		if p, err := ip.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip.String()
}

// ---- token buckets per client ----

type rateLimiter struct {
	rate  float64 // tokens per second
	burst float64

	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// limiterMaxClients bounds the memory of one limiter (about 100 bytes per
// client). Full buckets are forgotten first; only a flood from more
// addresses than this resets everyone.
const limiterMaxClients = 50_000

func newRateLimiter(perSecond, burst float64) *rateLimiter {
	return &rateLimiter{rate: perSecond, burst: burst, m: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= limiterMaxClients {
			l.sweepLocked(now)
			if len(l.m) >= limiterMaxClients {
				l.m = map[string]*bucket{}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked forgets clients whose bucket has refilled: forgetting them
// changes nothing.
func (l *rateLimiter) sweepLocked(now time.Time) {
	for k, x := range l.m {
		if now.Sub(x.last).Seconds()*l.rate+x.tokens >= l.burst {
			delete(l.m, k)
		}
	}
}

func (l *rateLimiter) sweep() {
	l.mu.Lock()
	l.sweepLocked(time.Now())
	l.mu.Unlock()
}

// Limits per client address. Pages and the read API are generous for
// people; the heavy pages (a whole block, a transaction with its prevouts,
// the mempool, search) cost the node more and get less. A browser miner
// fetches a job every 20 seconds and submits only solved blocks.
var (
	limitPages = [2]float64{10, 100} // per second, burst
	limitHeavy = [2]float64{3, 30}
	// A miner asks for work every 20 seconds, so 20 a second serves 400
	// miners behind one address: a carrier's shared mobile address holds
	// that many. Serving a job is cheap (the template is cached); checking
	// a submission is not, and submissions are rare (a found block).
	limitWork   = [2]float64{20, 200}
	limitSubmit = [2]float64{2, 20}
	// Pool shares arrive about every 15 seconds per miner, so 20 a second
	// serves 300 miners behind one address; each costs one Argon2id hash.
	limitShare = [2]float64{20, 200}
	limitTx    = [2]float64{1, 10}
)

type limiters struct {
	pages, heavy, work, submit, share, tx *rateLimiter
}

func newLimiters() *limiters {
	return &limiters{
		pages:  newRateLimiter(limitPages[0], limitPages[1]),
		heavy:  newRateLimiter(limitHeavy[0], limitHeavy[1]),
		work:   newRateLimiter(limitWork[0], limitWork[1]),
		submit: newRateLimiter(limitSubmit[0], limitSubmit[1]),
		share:  newRateLimiter(limitShare[0], limitShare[1]),
		tx:     newRateLimiter(limitTx[0], limitTx[1]),
	}
}

func (ls *limiters) sweepEvery(d time.Duration) {
	for range time.Tick(d) {
		ls.pages.sweep()
		ls.heavy.sweep()
		ls.work.sweep()
		ls.submit.sweep()
		ls.share.sweep()
		ls.tx.sweep()
	}
}

// limit wraps a handler with a per-client token bucket. Refused requests get
// 429 with Retry-After; JSON routes get a JSON body the miner can read.
func (s *server) limit(l *rateLimiter, jsonBody bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if l != nil && !l.allow(s.clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "2")
			if jsonBody {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"accepted":false,"stale":false,"reason":"` + workReasonLimit + `"}` + "\n"))
				return
			}
			http.Error(w, "Too many requests. Wait a moment and try again.", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	}
}

// ---- browser origins allowed to submit work ----

// originSet is the -allow-origin list: scheme://host[:port] values compared
// exactly (case-insensitively), or "*" for any origin.
type originSet struct {
	any bool
	m   map[string]bool
}

func parseOrigins(list string) originSet {
	o := originSet{m: map[string]bool{}}
	for _, v := range strings.Split(list, ",") {
		v = strings.TrimRight(strings.ToLower(strings.TrimSpace(v)), "/")
		switch {
		case v == "":
		case v == "*":
			o.any = true
		default:
			o.m[v] = true
		}
	}
	return o
}

func (o originSet) allowed(origin string) bool {
	return o.any || o.m[strings.ToLower(strings.TrimSpace(origin))]
}

func (o originSet) String() string {
	if o.any {
		return "*"
	}
	var out []string
	for k := range o.m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}
