// Command downloads serves the node release archives (the zips and
// SHA256SUMS in ./files) to the website on the private network. It serves
// plain files only: no directory listings, GET and HEAD only.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var name = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func main() {
	dir := "files"
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		f := strings.TrimPrefix(r.URL.Path, "/")
		if !name.MatchString(f) {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(dir, f)
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, p)
	})
	srv := &http.Server{Addr: ":" + port, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	log.Printf("downloads on :%s", port)
	log.Fatal(srv.ListenAndServe())
}
