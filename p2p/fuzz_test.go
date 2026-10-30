package p2p

import (
	"bytes"
	"testing"

	"argonmine/consensus"
)

// A frame that parses must re-encode to exactly the same bytes, and the
// parser must never panic on arbitrary input.
func FuzzReadMessage(f *testing.F) {
	magic := consensus.RegTest.Magic
	seeds := []Message{
		&MsgVersion{Version: 1, Services: 1, Timestamp: 1, Nonce: 2, UserAgent: "/x/", StartHeight: 3, Genesis: consensus.RegTest.GenesisHash},
		&MsgVerack{}, &MsgPing{Nonce: 9}, &MsgGetAddr{},
		&MsgGetHeaders{Locator: []consensus.Hash{consensus.RegTest.GenesisHash}},
		&MsgHeaders{Headers: []consensus.Header{consensus.RegTest.Genesis.Header}},
		&MsgInv{Items: []InvVect{{InvBlock, consensus.Hash{1}}}},
		&MsgBlock{Block: consensus.RegTest.Genesis},
		&MsgAddr{Addrs: []NetAddress{{Time: 1, Services: 1, Port: 9333}}},
		&MsgReject{Cmd: "tx", Reason: "no", Hash: consensus.Hash{2}},
	}
	for _, m := range seeds {
		b, _ := EncodeMessage(magic, m)
		f.Add(b)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		msg, err := ReadMessage(r, magic)
		if err != nil {
			return
		}
		// ReadMessage consumes exactly one frame; bytes after it belong to
		// the next frame on the stream.
		consumed := data[:len(data)-r.Len()]
		out, err := EncodeMessage(magic, msg)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(out, consumed) {
			t.Fatalf("non-canonical frame accepted for %s:\n in %x\nout %x", msg.Command(), consumed, out)
		}
	})
}
