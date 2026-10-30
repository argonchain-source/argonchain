package ui

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// QRSVG renders text as a crisp SVG QR code. Only bech32 characters are
// accepted and the length is capped, so the endpoint that serves it cannot
// be used to encode arbitrary content.
func QRSVG(text string) (string, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if len(text) == 0 || len(text) > 128 || strings.Trim(text, "qpzry9x8gf2tvdw0s3jn54khce6mua7l1") != "" {
		return "", errors.New("qr: not an address")
	}
	return qrSVG(text)
}

// evmAddress is an EVM address: 0x and 40 hex digits in any case.
var evmAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// EVMQRSVG renders an EVM address (a deposit address on another chain) as
// an SVG QR code. The case is kept, so an EIP-55 checksum survives the scan.
func EVMQRSVG(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if !evmAddress.MatchString(addr) {
		return "", errors.New("qr: not an EVM address")
	}
	return qrSVG(addr)
}

func qrSVG(text string) (string, error) {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bits := q.Bitmap()
	n := len(bits)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, n, n)
	for y, row := range bits {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}

// ServeQR answers /qr.svg?d=<address> for the apps and the explorer: an
// address of this chain, or an EVM address (0x…) for the Swap screens'
// deposit addresses.
func ServeQR(w http.ResponseWriter, data string) {
	svg, err := QRSVG(data)
	if evmAddress.MatchString(strings.TrimSpace(data)) {
		svg, err = EVMQRSVG(data)
	}
	if err != nil {
		http.Error(w, "bad data", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write([]byte(svg))
}
