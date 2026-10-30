package consensus

import (
	"bytes"
	"testing"
)

// Property shared by every decoder: if decoding succeeds, re-encoding gives
// back exactly the input bytes (canonical encoding), and decoding that again
// yields an identical structure. Any panic is a bug.

func FuzzDecodeTx(f *testing.F) {
	k := []byte("0123456789abcdef0123456789abcdef")
	tx := &Tx{Version: 1, LockHeight: 5,
		Inputs:    []TxIn{{PrevOut: OutPoint{TxID: TaggedHash("a", nil), Index: 1}}},
		Outputs:   []TxOut{{Amount: 1000, Type: OutputTypeP2PK, Payload: k}, {Type: OutputTypeNullData, Payload: []byte("hi")}},
		Witnesses: [][]byte{make([]byte, 64)},
	}
	f.Add(tx.Serialize())
	f.Add(RegTest.Genesis.Txs[0].Serialize())
	f.Add([]byte{})
	f.Add([]byte{1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		tx, err := DecodeTx(data)
		if err != nil {
			return
		}
		out := tx.Serialize()
		if !bytes.Equal(out, data) {
			t.Fatalf("non-canonical encoding accepted:\n in %x\nout %x", data, out)
		}
		again, err := DecodeTx(out)
		if err != nil {
			t.Fatalf("re-decode failed: %v", err)
		}
		if again.TxID() != tx.TxID() || again.WTxID() != tx.WTxID() {
			t.Fatal("ids differ after round trip")
		}
		// Validation must never panic on any decodable input.
		_ = CheckTxStateless(tx)
		_ = CheckCoinbase(tx, 1)
	})
}

func FuzzDecodeBlock(f *testing.F) {
	f.Add(RegTest.Genesis.Serialize())
	f.Add(MainNet.Genesis.Serialize())
	f.Add([]byte{})
	f.Add(make([]byte, HeaderSize))
	f.Fuzz(func(t *testing.T, data []byte) {
		blk, err := DecodeBlock(data)
		if err != nil {
			return
		}
		out := blk.Serialize()
		if !bytes.Equal(out, data) {
			t.Fatalf("non-canonical block accepted")
		}
		if _, err := DecodeBlock(out); err != nil {
			t.Fatalf("re-decode failed: %v", err)
		}
		// Connecting against an empty set must not panic (it will fail).
		ov := NewOverlay(MemView{})
		_ = ConnectBlock(RegTest, blk, 1, ov)
		_, _, _ = blk.ComputeRoots()
	})
}

func FuzzDecodeHeader(f *testing.F) {
	f.Add(RegTest.Genesis.Header.Serialize())
	f.Add(make([]byte, HeaderSize))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := DecodeHeader(data)
		if err != nil {
			return
		}
		if !bytes.Equal(h.Serialize(), data) {
			t.Fatal("header round trip")
		}
		_ = CheckHeaderStateless(RegTest, h) // may fail; must not panic
	})
}

func FuzzVarint(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0x7f})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(data)
		v, err := r.Varint()
		if err != nil {
			return
		}
		var w Writer
		w.Varint(v)
		consumed := len(data) - r.Remaining()
		if !bytes.Equal(w.Bytes(), data[:consumed]) {
			t.Fatalf("non-minimal varint accepted: %x -> %d -> %x", data[:consumed], v, w.Bytes())
		}
	})
}

func FuzzCompact(f *testing.F) {
	f.Add(uint32(0x1F0FFFFF))
	f.Add(uint32(0x1d00ffff))
	f.Add(uint32(0))
	f.Fuzz(func(t *testing.T, bits uint32) {
		tgt, err := CompactToTarget(bits)
		if err != nil {
			return
		}
		if tgt.Sign() <= 0 || tgt.BitLen() > 256 {
			t.Fatalf("%08x decoded to out-of-range target", bits)
		}
		if TargetToCompact(tgt) != bits {
			t.Fatalf("%08x not canonical but accepted", bits)
		}
	})
}

func FuzzASERT(f *testing.F) {
	f.Add(uint64(1_792_886_400), uint32(0))
	f.Add(uint64(0), uint32(1<<32-1))
	f.Add(uint64(1<<63-1), uint32(12345))
	f.Fuzz(func(t *testing.T, parentTime uint64, parentHeight uint32) {
		if parentTime > MaxBlockTime {
			parentTime = MaxBlockTime
		}
		tgt := NextTarget(MainNet, parentTime, parentHeight)
		if tgt.Sign() <= 0 || tgt.Cmp(MainNet.PowLimit) > 0 {
			t.Fatalf("target out of range: %x", tgt)
		}
		bits := TargetToCompact(tgt)
		if _, err := CompactToTarget(bits); err != nil {
			t.Fatalf("NextBits produced non-canonical bits %08x: %v", bits, err)
		}
	})
}
