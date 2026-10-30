package bridge

import _ "embed"

// bridgePage is the bridge's single page (deposit and withdraw). Handler fills
// its {{PLACEHOLDERS}} on every request.
//
//go:embed page.html
var bridgePage string

// ownerPage (GET /owner) lets the wallet a vault was offered to accept it
// (acceptOwnership) on every chain. It reads owner and pendingOwner through
// each chain's public RPC; the vault itself refuses any other sender.
//
//go:embed owner.html
var ownerPage string
