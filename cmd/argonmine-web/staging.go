package main

import (
	"bytes"
	"crypto/subtle"
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

// A private copy of the site (a staging deployment the team tests before a
// release goes public) sits behind one password: -basic-auth user:password
// asks every request for it. The app on such a copy reaches its explorer
// through this site (-explorer-proxy, served at /x/), so the browser sends
// the password it already has instead of a second one for the explorer's
// own address; the proxy adds the explorer's password itself.

const explorerProxyPrefix = "/x"

// splitCredentials reads "user:password"; ok is false when either is empty.
func splitCredentials(s string) (user, pass string, ok bool) {
	user, pass, found := strings.Cut(s, ":")
	return user, pass, found && user != "" && pass != ""
}

// basicAuth asks every request for the credentials, in constant time.
func basicAuth(cred string, next http.Handler) http.Handler {
	user, pass, ok := splitCredentials(cred)
	if !ok {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, has := r.BasicAuth()
		if !has || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Argonmine staging", charset="UTF-8"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "This is a private copy of Argonmine. Ask the team for the password.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// explorerProxy forwards /x/<path> to the explorer at upstream, with the
// explorer's own credentials (cred, "user:password", may be empty). Only the
// explorer's JSON API and its mining endpoints are forwarded; pages are
// not, they live at the explorer's own address.
func explorerProxy(upstream, cred string) (http.Handler, error) {
	u, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, err
	}
	user, pass, withAuth := splitCredentials(cred)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, explorerProxyPrefix)
			pr.Out.URL.RawPath = ""
			pr.Out.Host = u.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			if withAuth {
				pr.Out.SetBasicAuth(user, pass)
			}
			pr.Out.Header.Set("X-Forwarded-For", requestIP(pr.In))
		},
		Transport: noTrace{&http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
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
			keep.Set("Referrer-Policy", "no-referrer")
			resp.Header = keep
			var body []byte
			if mt == "application/json" {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, swapMaxBody+1))
			}
			resp.Body.Close()
			resp.Trailer = nil
			if mt != "application/json" || len(body) > swapMaxBody || !json.Valid(body) {
				resp.StatusCode, resp.Status = http.StatusBadGateway, "502 Bad Gateway"
				body = []byte(`{"error":"the explorer sent an answer that is not JSON"}`)
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":"the explorer cannot be reached right now"}`))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, explorerProxyPrefix)
		if !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/pool/api/") {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		rp.ServeHTTP(w, r)
	}), nil
}
