package main

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The node operator's release archives (docs/NODE.md, "Download") live on a
// separate file service on the private network (-downloads-upstream), so the
// site's own image stays small; /downloads/<file> is forwarded to it. Only
// GET and HEAD of a plain file name pass. The answer carries none of the file
// service's own headers except the ones a download needs, and is always
// served as an attachment that nothing may run.

var downloadName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func downloadsProxy(upstream string) (http.Handler, error) {
	u, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.URL.Path = u.Path + "/" + strings.TrimPrefix(pr.In.URL.Path, "/downloads/")
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = ""
			pr.Out.Host = u.Host
			for _, h := range []string{"Cookie", "Authorization", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				pr.Out.Header.Del(h)
			}
		},
		Transport: noTrace{&http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConns:          8,
			IdleConnTimeout:       90 * time.Second,
		}},
		ModifyResponse: func(resp *http.Response) error {
			keep := http.Header{}
			for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
				if v := resp.Header.Values(k); len(v) > 0 {
					keep[k] = v
				}
			}
			name := strings.TrimPrefix(resp.Request.URL.Path, u.Path+"/")
			if strings.HasSuffix(name, ".zip") {
				keep.Set("Content-Type", "application/zip")
			} else {
				keep.Set("Content-Type", "text/plain; charset=utf-8")
			}
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
				keep.Set("Content-Disposition", `attachment; filename="`+name+`"`)
				keep.Set("Cache-Control", "public, max-age=300")
			} else {
				keep.Set("Cache-Control", "no-store")
			}
			keep.Set("X-Content-Type-Options", "nosniff")
			keep.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
			resp.Header = keep
			resp.Trailer = nil
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "downloads are unavailable right now", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !downloadName.MatchString(strings.TrimPrefix(r.URL.Path, "/downloads/")) || strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}
