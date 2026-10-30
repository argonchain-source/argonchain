package main

import (
	"net/http"
	"testing"

	"argonmine/bridge"
)

// TestHTTPServerTimeouts: every timeout is set, and a guardian request
// (the leader waits up to bridge.PeerTimeout for it) can be answered.
func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes <= 0 {
		t.Fatalf("a timeout is missing: %+v", srv)
	}
	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Fatal("the header timeout is longer than the read timeout")
	}
	if srv.WriteTimeout <= bridge.PeerTimeout {
		t.Fatalf("write timeout %v: a guardian request may take %v", srv.WriteTimeout, bridge.PeerTimeout)
	}
}
