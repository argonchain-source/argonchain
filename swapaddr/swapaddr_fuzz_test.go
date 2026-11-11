package swapaddr

import "testing"

// Swap payloads and addresses are parsed from data supplied by other
// chain users, so neither parser may panic.
func FuzzParsePayload(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte("arg1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParsePayload(data)
		_, _ = Parse("arg", string(data))
	})
}
