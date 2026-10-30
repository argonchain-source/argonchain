package bridge

import "argonmine/swapaddr"

// Swap addresses (docs/BRIDGE.md, "Swap addresses") are encoded and parsed
// by package swapaddr, which the browser wallet shares; these are the names
// the bridge has always used for them.

// SwapDest is the payout a swap address names.
type SwapDest = swapaddr.Dest

// SwapPayload returns the P2PK payload of the swap address for d.
func SwapPayload(d SwapDest) ([32]byte, error) { return swapaddr.Payload(d) }

// SwapAddress returns the swap address for d on the network with this HRP.
func SwapAddress(hrp string, d SwapDest) (string, error) { return swapaddr.Address(hrp, d) }

// ParseSwapPayload reports the payout a P2PK payload names, if it is the
// canonical payload of a swap address.
func ParseSwapPayload(p []byte) (SwapDest, bool) { return swapaddr.ParsePayload(p) }

// ParseSwapAddress reports the payout an address names, if it is a swap
// address of the network with this HRP.
func ParseSwapAddress(hrp, addr string) (SwapDest, bool) { return swapaddr.Parse(hrp, addr) }

// IsSwapAddress reports whether addr is a swap address of the network.
func IsSwapAddress(hrp, addr string) bool { return swapaddr.Is(hrp, addr) }
