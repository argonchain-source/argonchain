// Package p2p implements the peer-to-peer protocol: framing, handshake,
// headers-first synchronisation, block and transaction relay, address
// gossip and misbehaviour banning.
package p2p

import (
	"errors"
	"fmt"
	"io"

	"argonmine/consensus"
)

const (
	// ProtocolVersion is bumped for incompatible wire changes.
	ProtocolVersion uint32 = 1

	headerLen      = 4 + 12 + 4 + 4 // magic, command, length, checksum
	MaxPayload     = 2 << 20
	MaxHeadersMsg  = 2000
	MaxInvMsg      = 10_000
	MaxLocator     = 64
	MaxAddrMsg     = 1000
	MaxUserAgent   = 256
	commandLen     = 12
	checksumTag    = "Argonmine/p2p"
	ServiceNetwork = uint64(1) // full node
)

var (
	ErrBadMagic     = errors.New("p2p: bad magic")
	ErrBadChecksum  = errors.New("p2p: bad checksum")
	ErrPayloadSize  = errors.New("p2p: payload too large")
	ErrBadCommand   = errors.New("p2p: unknown command")
	ErrMsgTooMany   = errors.New("p2p: too many items in message")
	ErrMsgMalformed = errors.New("p2p: malformed message")
)

// Message is any wire message.
type Message interface {
	Command() string
	encode(w *consensus.Writer)
}

// Commands.
const (
	CmdVersion    = "version"
	CmdVerack     = "verack"
	CmdPing       = "ping"
	CmdPong       = "pong"
	CmdGetHeaders = "getheaders"
	CmdHeaders    = "headers"
	CmdInv        = "inv"
	CmdGetData    = "getdata"
	CmdNotFound   = "notfound"
	CmdBlock      = "block"
	CmdTx         = "tx"
	CmdGetAddr    = "getaddr"
	CmdAddr       = "addr"
	CmdReject     = "reject"
	// CmdMempool asks a peer to announce its pending transactions (sent once
	// a node has caught up; older nodes ignore unknown commands).
	CmdMempool = "mempool"
)

// InvType identifies inventory items.
type InvType uint8

const (
	InvTx    InvType = 1
	InvBlock InvType = 2
)

// InvVect is one inventory item.
type InvVect struct {
	Type InvType
	Hash consensus.Hash
}

// NetAddress is a gossiped peer address.
type NetAddress struct {
	Time     uint64
	Services uint64
	IP       [16]byte // IPv4 mapped to ::ffff:a.b.c.d
	Port     uint16
}

type MsgVersion struct {
	Version     uint32
	Services    uint64
	Timestamp   int64
	Nonce       uint64 // detects connections to self
	UserAgent   string
	StartHeight uint32
	Genesis     consensus.Hash // must equal ours; the real network identifier
}
type MsgVerack struct{}
type MsgPing struct{ Nonce uint64 }
type MsgPong struct{ Nonce uint64 }
type MsgGetHeaders struct {
	Locator []consensus.Hash
	Stop    consensus.Hash
}
type MsgHeaders struct{ Headers []consensus.Header }
type MsgInv struct{ Items []InvVect }
type MsgGetData struct{ Items []InvVect }
type MsgNotFound struct{ Items []InvVect }

// MsgBlock carries a block. Raw, when set, is the stored serialization and
// is sent as-is (no decode/encode round trip when relaying).
type MsgBlock struct {
	Block *consensus.Block
	Raw   []byte
}
type MsgTx struct{ Tx *consensus.Tx }
type MsgGetAddr struct{}

// MsgMempool asks the peer for an inv of its mempool.
type MsgMempool struct{}
type MsgAddr struct{ Addrs []NetAddress }
type MsgReject struct {
	Cmd    string
	Reason string
	Hash   consensus.Hash
}

func (*MsgVersion) Command() string    { return CmdVersion }
func (*MsgVerack) Command() string     { return CmdVerack }
func (*MsgPing) Command() string       { return CmdPing }
func (*MsgPong) Command() string       { return CmdPong }
func (*MsgGetHeaders) Command() string { return CmdGetHeaders }
func (*MsgHeaders) Command() string    { return CmdHeaders }
func (*MsgInv) Command() string        { return CmdInv }
func (*MsgGetData) Command() string    { return CmdGetData }
func (*MsgNotFound) Command() string   { return CmdNotFound }
func (*MsgBlock) Command() string      { return CmdBlock }
func (*MsgTx) Command() string         { return CmdTx }
func (*MsgGetAddr) Command() string    { return CmdGetAddr }
func (*MsgMempool) Command() string    { return CmdMempool }
func (*MsgAddr) Command() string       { return CmdAddr }
func (*MsgReject) Command() string     { return CmdReject }

func (m *MsgVersion) encode(w *consensus.Writer) {
	w.U32(m.Version)
	w.U64(m.Services)
	w.I64(m.Timestamp)
	w.U64(m.Nonce)
	w.VarBytes([]byte(m.UserAgent))
	w.U32(m.StartHeight)
	w.Hash(m.Genesis)
}
func (*MsgVerack) encode(*consensus.Writer)   {}
func (m *MsgPing) encode(w *consensus.Writer) { w.U64(m.Nonce) }
func (m *MsgPong) encode(w *consensus.Writer) { w.U64(m.Nonce) }
func (m *MsgGetHeaders) encode(w *consensus.Writer) {
	w.Varint(uint64(len(m.Locator)))
	for _, h := range m.Locator {
		w.Hash(h)
	}
	w.Hash(m.Stop)
}
func (m *MsgHeaders) encode(w *consensus.Writer) {
	w.Varint(uint64(len(m.Headers)))
	for i := range m.Headers {
		w.Raw(m.Headers[i].Serialize())
	}
}
func encodeInv(w *consensus.Writer, items []InvVect) {
	w.Varint(uint64(len(items)))
	for _, it := range items {
		w.U8(uint8(it.Type))
		w.Hash(it.Hash)
	}
}
func (m *MsgInv) encode(w *consensus.Writer)      { encodeInv(w, m.Items) }
func (m *MsgGetData) encode(w *consensus.Writer)  { encodeInv(w, m.Items) }
func (m *MsgNotFound) encode(w *consensus.Writer) { encodeInv(w, m.Items) }
func (m *MsgBlock) encode(w *consensus.Writer) {
	if m.Raw != nil {
		w.Raw(m.Raw)
		return
	}
	w.Raw(m.Block.Serialize())
}
func (m *MsgTx) encode(w *consensus.Writer)  { w.Raw(m.Tx.Serialize()) }
func (*MsgGetAddr) encode(*consensus.Writer) {}
func (*MsgMempool) encode(*consensus.Writer) {}
func (m *MsgAddr) encode(w *consensus.Writer) {
	w.Varint(uint64(len(m.Addrs)))
	for _, a := range m.Addrs {
		w.U64(a.Time)
		w.U64(a.Services)
		w.Raw(a.IP[:])
		w.U8(uint8(a.Port >> 8))
		w.U8(uint8(a.Port))
	}
}
func (m *MsgReject) encode(w *consensus.Writer) {
	w.VarBytes([]byte(m.Cmd))
	w.VarBytes([]byte(m.Reason))
	w.Hash(m.Hash)
}

func decodeInv(r *consensus.Reader) ([]InvVect, error) {
	n, err := r.VarintMax(MaxInvMsg)
	if err != nil {
		return nil, err
	}
	items := make([]InvVect, n)
	for i := range items {
		t, err := r.U8()
		if err != nil {
			return nil, err
		}
		items[i].Type = InvType(t)
		if items[i].Hash, err = r.Hash(); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func decodeMessage(cmd string, payload []byte) (Message, error) {
	r := consensus.NewReader(payload)
	var msg Message
	var err error
	switch cmd {
	case CmdVersion:
		m := &MsgVersion{}
		if m.Version, err = r.U32(); err != nil {
			return nil, err
		}
		if m.Services, err = r.U64(); err != nil {
			return nil, err
		}
		if m.Timestamp, err = r.I64(); err != nil {
			return nil, err
		}
		if m.Nonce, err = r.U64(); err != nil {
			return nil, err
		}
		ua, err := r.VarBytes(MaxUserAgent)
		if err != nil {
			return nil, err
		}
		m.UserAgent = string(ua)
		if m.StartHeight, err = r.U32(); err != nil {
			return nil, err
		}
		if m.Genesis, err = r.Hash(); err != nil {
			return nil, err
		}
		msg = m
	case CmdVerack:
		msg = &MsgVerack{}
	case CmdPing:
		m := &MsgPing{}
		if m.Nonce, err = r.U64(); err != nil {
			return nil, err
		}
		msg = m
	case CmdPong:
		m := &MsgPong{}
		if m.Nonce, err = r.U64(); err != nil {
			return nil, err
		}
		msg = m
	case CmdGetHeaders:
		m := &MsgGetHeaders{}
		n, err := r.VarintMax(MaxLocator)
		if err != nil {
			return nil, err
		}
		m.Locator = make([]consensus.Hash, n)
		for i := range m.Locator {
			if m.Locator[i], err = r.Hash(); err != nil {
				return nil, err
			}
		}
		if m.Stop, err = r.Hash(); err != nil {
			return nil, err
		}
		msg = m
	case CmdHeaders:
		m := &MsgHeaders{}
		n, err := r.VarintMax(MaxHeadersMsg)
		if err != nil {
			return nil, err
		}
		m.Headers = make([]consensus.Header, n)
		for i := range m.Headers {
			raw, err := r.Raw(consensus.HeaderSize)
			if err != nil {
				return nil, err
			}
			h, err := consensus.DecodeHeader(raw)
			if err != nil {
				return nil, err
			}
			m.Headers[i] = *h
		}
		msg = m
	case CmdInv:
		items, err := decodeInv(r)
		if err != nil {
			return nil, err
		}
		msg = &MsgInv{Items: items}
	case CmdGetData:
		items, err := decodeInv(r)
		if err != nil {
			return nil, err
		}
		msg = &MsgGetData{Items: items}
	case CmdNotFound:
		items, err := decodeInv(r)
		if err != nil {
			return nil, err
		}
		msg = &MsgNotFound{Items: items}
	case CmdBlock:
		blk, err := consensus.DecodeBlock(payload)
		if err != nil {
			return nil, err
		}
		return &MsgBlock{Block: blk}, nil
	case CmdTx:
		tx, err := consensus.DecodeTx(payload)
		if err != nil {
			return nil, err
		}
		return &MsgTx{Tx: tx}, nil
	case CmdGetAddr:
		msg = &MsgGetAddr{}
	case CmdMempool:
		msg = &MsgMempool{}
	case CmdAddr:
		m := &MsgAddr{}
		n, err := r.VarintMax(MaxAddrMsg)
		if err != nil {
			return nil, err
		}
		m.Addrs = make([]NetAddress, n)
		for i := range m.Addrs {
			a := &m.Addrs[i]
			if a.Time, err = r.U64(); err != nil {
				return nil, err
			}
			if a.Services, err = r.U64(); err != nil {
				return nil, err
			}
			ip, err := r.Raw(16)
			if err != nil {
				return nil, err
			}
			copy(a.IP[:], ip)
			hi, err := r.U8()
			if err != nil {
				return nil, err
			}
			lo, err := r.U8()
			if err != nil {
				return nil, err
			}
			a.Port = uint16(hi)<<8 | uint16(lo)
		}
		msg = m
	case CmdReject:
		m := &MsgReject{}
		c, err := r.VarBytes(commandLen)
		if err != nil {
			return nil, err
		}
		m.Cmd = string(c)
		reason, err := r.VarBytes(256)
		if err != nil {
			return nil, err
		}
		m.Reason = string(reason)
		if m.Hash, err = r.Hash(); err != nil {
			return nil, err
		}
		msg = m
	default:
		return nil, ErrBadCommand
	}
	if err := r.ExpectEOF(); err != nil {
		return nil, err
	}
	return msg, nil
}

func checksum(payload []byte) [4]byte {
	h := consensus.TaggedHash(checksumTag, payload)
	var c [4]byte
	copy(c[:], h[:4])
	return c
}

// EncodeMessage frames m for the wire.
func EncodeMessage(magic [4]byte, m Message) ([]byte, error) {
	var body consensus.Writer
	m.encode(&body)
	payload := body.Bytes()
	if len(payload) > MaxPayload {
		return nil, ErrPayloadSize
	}
	cmd := m.Command()
	if len(cmd) > commandLen {
		return nil, ErrBadCommand
	}
	var w consensus.Writer
	w.Raw(magic[:])
	var c [commandLen]byte
	copy(c[:], cmd)
	w.Raw(c[:])
	w.U32(uint32(len(payload)))
	ck := checksum(payload)
	w.Raw(ck[:])
	w.Raw(payload)
	return w.Bytes(), nil
}

// WriteMessage frames and writes m.
func WriteMessage(w io.Writer, magic [4]byte, m Message) error {
	b, err := EncodeMessage(magic, m)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadMessage reads one framed message, verifying magic, size and checksum.
func ReadMessage(r io.Reader, magic [4]byte) (Message, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if [4]byte{hdr[0], hdr[1], hdr[2], hdr[3]} != magic {
		return nil, ErrBadMagic
	}
	cmdBytes := hdr[4 : 4+commandLen]
	end := len(cmdBytes)
	for end > 0 && cmdBytes[end-1] == 0 {
		end--
	}
	cmd := string(cmdBytes[:end])
	for _, b := range cmdBytes[end:] {
		if b != 0 {
			return nil, ErrBadCommand
		}
	}
	length := uint32(hdr[16]) | uint32(hdr[17])<<8 | uint32(hdr[18])<<16 | uint32(hdr[19])<<24
	if length > MaxPayload {
		return nil, ErrPayloadSize
	}
	var want [4]byte
	copy(want[:], hdr[20:24])
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	if checksum(payload) != want {
		return nil, ErrBadChecksum
	}
	m, err := decodeMessage(cmd, payload)
	if errors.Is(err, ErrBadCommand) {
		// Unknown command: the frame was well-formed and fully consumed, so
		// the caller can skip it (forward compatibility) instead of banning.
		return nil, ErrBadCommand
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrMsgMalformed, cmd, err)
	}
	return m, nil
}
