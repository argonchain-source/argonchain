# Names: yourname.arg

A name is a short word that stands for an address: `alice.arg` instead of
`arg1q…`. Anyone can register one from the app. Names live on the chain,
in ordinary transactions, and are paid for in ARG to the treasury. No
node rule changes: nodes carry the transactions, the explorer reads them.

## What a name does

- **Send to it.** The app accepts `alice.arg` wherever it accepts an
  address and shows the address it resolves to before you confirm.
- **Shows on the explorer.** An address that owns a name is shown by its
  name on the explorer, in the app's dashboard, and in pool payouts.
- **Is yours for a year or more**, then renewed, or let go.
- **Can be transferred** to another address by its owner.

## Rules

A name is a label of 3 to 32 characters: lowercase letters `a`–`z`, digits
`0`–`9` and hyphens, not at either end. The suffix `.arg` is implied and
never written on the chain.

| Length | Price per year |
|---|---|
| 3 characters | 250 ARG |
| 4 characters | 50 ARG |
| 5 characters or more | 10 ARG |

Prices are paid to the treasury. A year is 52,560 blocks (10 minutes a
block). A registration or renewal is for 1 to 5 years at once.

When a name expires, its owner has a grace period of 4,320 blocks (30
days) in which only they can renew it. After that anyone can register it.
An expired name no longer resolves.

Reserved labels (`argonmine`, `treasury`, `pool`, `bridge`, `swap`,
`explorer`, `docs`, `app`, `www`, `admin`, `support`, `team`, `official`)
can be registered only by the treasury itself.

On a network without a treasury (testnet, regtest) names are free: the
transaction carries the memo and pays nothing but the network fee.

## On the chain

A name operation is a transaction with one NULLDATA output (at most 80
bytes) whose text is

```
NMS1 <op> <label> [years]
```

| Operation | Memo | Who | What it does |
|---|---|---|---|
| register | `NMS1 register alice 2` | anyone, when the label is free | the signer owns `alice` for 2 years |
| renew | `NMS1 renew alice 1` | the owner, while active or in grace | adds 1 year to the expiry |
| transfer | `NMS1 transfer alice` | the owner | the address of output 0 becomes the owner |
| primary | `NMS1 primary alice` | the owner | `alice` is the name shown for the signer's address |

The **signer** is the address that owns the coin spent by input 0. For
register and renew, the transaction's outputs must pay the treasury at
least the price for the label and years, in ARG, or the operation is
ignored. Years outside 1 to 5, a label that breaks the rules, an operation
by anyone but the owner, or a register of a name that is taken, are all
ignored: the transaction confirms as a normal payment and does nothing to
names. Only the first `NMS1` memo in a transaction counts.

The first valid operation on the chain wins, in block order and then
transaction order. A renewal extends from the current expiry when the name
is still active, and from the renewing block when it is in grace. A
transfer keeps the expiry and clears the old owner's primary flag on the
name. An address with no primary name set shows its oldest active name.

A reorganisation undoes operations with the blocks they were in, like
everything else the explorer indexes.

## For developers

The explorer serves names at these endpoints (all GET, JSON, CORS open):

| Endpoint | Answer |
|---|---|
| `/api/names/info` | rules and prices: `suffix`, `min_len`, `max_len`, `blocks_per_year`, `grace_blocks`, `years_max`, `prices` (units per year by length: `"3"`, `"4"`, `"5"` meaning 5 or more), `treasury` (address, `""` when names are free), `reserved`, `count` (active names), `height` |
| `/api/name/{label}` | one name: `name`, `status` (`available`, `registered`, `grace`, `pending`, `reserved`, `invalid`), `reason` for invalid, `owner`, `primary` (bool), `registered_height`, `registered_time`, `expires_height`, `expires_time` (estimate), `txid`, `price` (units per year for this label), `history` (newest first: `op`, `height`, `time`, `txid`, `from`, `to`, `years`) |
| `/api/resolve/{label}` | `name`, `address`; 404 when the name is not active |
| `/api/names?owner={address}` | `address`, `primary` (label or `""`), `names` (each: `name`, `expires_height`, `expires_time`, `primary`, `grace`) |
| `/api/names/recent?limit=50` | `count` (active names), `names` (newest registrations: `name`, `owner`, `height`, `time`, `txid`, `expires_height`) |

`pending` means a transaction in the mempool claims a free name; it is not
yet on the chain and may still lose to another.

The parser and the rules are the Go package `names` of the source tree,
shared by the explorer and the browser wallet, so a name the app accepts
is one the explorer will index.
