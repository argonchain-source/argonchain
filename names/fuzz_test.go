package names

import "testing"

// Name memos arrive inside transactions relayed by peers, so Parse must
// never panic on arbitrary bytes.
func FuzzParseMemo(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("name"))
	f.Add([]byte{0x00, 0x01, 0x02})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}
