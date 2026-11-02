package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// A private copy of the explorer (a staging deployment the team tests
// before a release goes public) sits behind one password: -basic-auth
// user:password asks every request for it, pages and API alike. The app of
// the same copy reaches this API through its own site, which adds the
// password itself (argonmine-web -explorer-proxy).

// basicAuth asks every request for the credentials ("user:password"), in
// constant time; with none set it is a no-op.
func basicAuth(cred string, next http.Handler) http.Handler {
	user, pass, found := strings.Cut(cred, ":")
	if !found || user == "" || pass == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, has := r.BasicAuth()
		if !has || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Argonmine staging", charset="UTF-8"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "This is a private copy of the Argonmine explorer. Ask the team for the password.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
