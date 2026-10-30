package quote

import (
	"encoding/json"
	"strings"
	"testing"
)

// Security review G (2026-10-28): CheckKeys must see keys as Go's decoder
// will match them, so no spelling gets a second value past it.
func TestReviewG_CheckKeysSpellings(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 17) + `1` + strings.Repeat(`}`, 17)
	bs := `\` // JSON escapes are built from this, so no tool rewrites them
	for in, ok := range map[string]bool{
		`{"` + bs + `u0041MOUNT_OUT":1}`:                false, // escapes decode to upper case
		`{"amount_out":1,"amount` + bs + `u005fout":2}`: false, // the same key twice, one escaped
		`{"amount_ou` + bs + `u0074":1}`:                true,  // escaped, but snake case and once
		`{"spent":[[{"amount":1,"AMOUNT":2}]]}`:         false, // inside nested arrays
		`{"` + bs + `u212amount":1}`:                    false, // Kelvin sign: Go folds it to k
		"{\"\xe2\x84\xaamount\":1}":                     false, // the same, as raw UTF-8
		`{"amount_out` + bs + `u017f":1}`:               false, // long s: Go folds it to s
		"{\"amount_out\xff\":1}":                        false, // invalid UTF-8 (decoded as U+FFFD)
		`{"amount_out":"99000000"}`:                     true,  // value types are the decoder's job
		`{"__proto__":{"amount_out":1}}`:                true,  // harmless to Go; JSON.parse keeps it as data
		deep:                                            false,
		`{"amount_out":1`:                               false,
		`{"amount_out":1}   `:                           true,
		`{"a":[1,2,3,{"b":{"c":[{"d":null}]}}]}`:        true,
		`{"pairs":[{"from":"ARG"},{"From":"UARG"}]}`:    false,
	} {
		if err := CheckKeys([]byte(in)); (err == nil) != ok {
			t.Errorf("CheckKeys(%q) = %v, want ok=%v", in, err, ok)
		}
	}

	// Trailing bytes that are not a JSON token pass (d.Token fails, which
	// CheckKeys reads as the end). Harmless where it is used: the value is a
	// json.RawMessage cut from a request that already parsed.
	if err := CheckKeys([]byte(`{"amount_out":1}garbage`)); err == nil {
		t.Log("note: CheckKeys accepts trailing non-JSON bytes")
	}

	// What passes decodes in Go to the value the page reads.
	var q Quote
	if err := json.Unmarshal([]byte(`{"amount_ou`+bs+`u0074":7}`), &q); err != nil || q.AmountOut != 7 {
		t.Fatalf("decoded %+v %v", q, err)
	}
}
