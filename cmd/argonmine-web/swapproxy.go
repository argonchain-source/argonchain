package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// The swap service (docs/SWAP.md) is reached at this site's own address:
// /swap/api/info, /swap/api/quote and /swap/api/swap are forwarded to it on
// the private network (-swap-upstream), so wallets talk to one address and
// the service needs no public one of its own. The client's address goes
// along as the right-most X-Forwarded-For entry, which the service's rate
// limits read.
//
// The answers come back on this site's own origin, so they are made inert
// whatever the service sends: JSON only (anything else becomes a 502), at
// most swapMaxBody bytes, none of the service's own headers (no cookies,
// no service-worker scope, no clearing of site data), and a policy that
// lets nothing in them run.

const swapMaxBody = 2 << 20

var swapPaths = map[string]bool{"/swap/api/info": true, "/swap/api/quote": true, "/swap/api/swap": true}

func swapProxy(upstream string) (http.Handler, error) {
	u, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/swap")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = u.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("X-Forwarded-For", requestIP(pr.In))
		},
		// No client trace reaches the transport, so the proxy never relays
		// the service's 1xx answers (and their headers) on its own.
		Transport: noTrace{&http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
		}},
		ModifyResponse: func(resp *http.Response) error {
			mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			keep := http.Header{}
			for _, k := range []string{"Cache-Control", "Retry-After"} {
				if v := resp.Header.Get(k); v != "" {
					keep.Set(k, v)
				}
			}
			keep.Set("Content-Type", "application/json")
			keep.Set("X-Content-Type-Options", "nosniff")
			keep.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
			keep.Set("Cross-Origin-Resource-Policy", "same-origin")
			keep.Set("X-Frame-Options", "DENY")
			keep.Set("Referrer-Policy", "no-referrer")
			resp.Header = keep
			var body []byte
			if mt == "application/json" {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, swapMaxBody+1))
			}
			resp.Body.Close()
			resp.Trailer = nil // after the body: reading it to the end fills in the service's trailers
			if mt != "application/json" || len(body) > swapMaxBody || !json.Valid(body) {
				resp.StatusCode, resp.Status = http.StatusBadGateway, "502 Bad Gateway"
				body = []byte(`{"error":"the swap service sent an answer that is not JSON"}`)
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":"the swap service cannot be reached right now"}`))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !swapPaths[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		rp.ServeHTTP(w, r)
	}), nil
}

// requestIP is the client's address: the right-most X-Forwarded-For entry
// when a proxy in front added one (Railway's edge does), else the peer.
func requestIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		parts := strings.Split(v, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// noTrace runs the upstream request with a context that carries no values,
// so no httptrace.ClientTrace (the proxy's 1xx relay) reaches the transport.
type noTrace struct{ rt http.RoundTripper }

func (t noTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.rt.RoundTrip(r.WithContext(valueless{r.Context()}))
}

// valueless keeps a context's deadline and cancellation and drops its values.
type valueless struct{ context.Context }

func (valueless) Value(any) any { return nil }
