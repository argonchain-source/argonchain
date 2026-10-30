# The bridge: dollars in as uARG, and back out

The bridge at [bridge.argonmine.com](https://bridge.argonmine.com) moves
US dollars between EVM chains and Argonmine. You **deposit** USDC or USDT
into the bridge's vault on an EVM chain and receive **uARG** on Argonmine,
one uARG per dollar, less a 1% fee. You **withdraw** by paying uARG to a
withdrawal address and receive USDC or USDT on the chain you choose, less
the same 1% fee. ARG itself never crosses: it is mined on Argonmine and
stays there.

The bridge runs on mainnet on four chains: Ethereum, Base, Arbitrum One and
BNB Chain, with real USDC and USDT (see [Chains and tokens](#chains-and-tokens)).
It holds real money. Read [Risks](#risks) before you deposit.

The first section of this page explains how to use the bridge. The rest
describes the protocol, the security model and the operations in detail,
for reviewers and operators.

## How do I use the bridge?

### At launch

The bridge starts issuing uARG once its fee wallet holds mined ARG that
has unlocked, about a day after launch. Every issuance pays a small
Argonmine network fee in ARG, and mined ARG can be spent only 100 blocks
after it is mined. A deposit made before then waits in the vault and is
issued then. Withdrawals need uARG, so they start once the first uARG is
issued.

### Chains and tokens

| Chain | Token | Token contract | Decimals | Smallest deposit | Smallest withdrawal |
|---|---|---|---|---|---|
| Ethereum | USDC | `0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48` | 6 | 10 USDC | 10 USDC |
| Ethereum | USDT | `0xdAC17F958D2ee523a2206206994597C13D831ec7` | 6 | 10 USDT | 10 USDT |
| Base | USDC | `0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913` | 6 | 2 USDC | 1 USDC |
| Arbitrum One | USDC | `0xaf88d065e77c8cC2239327C5EDb3A432268e5831` | 6 | 2 USDC | 1 USDC |
| Arbitrum One | USDT (Tether's USDT0) | `0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9` | 6 | 2 USDT | 1 USDT |
| BNB Chain | USDC (issued by Binance) | `0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d` | 18 | 2 USDC | 1 USDC |
| BNB Chain | USDT | `0x55d398326f99059fF775485246999027B3197955` | 18 | 2 USDT | 1 USDT |

There is no USDT from Tether on Base, so the bridge takes only USDC there.
The minimum is higher on Ethereum because every payout there costs real
gas.

Send only the tokens in this table, at these contracts. Another token,
even one with the same name, is not credited and cannot be recovered.

| Chain | Vault |
|---|---|
| Ethereum | [`0xd5558B5C1CB37D70ADB238b1E8a21451603D6213`](https://etherscan.io/address/0xd5558B5C1CB37D70ADB238b1E8a21451603D6213) |
| Base | [`0xd5558B5C1CB37D70ADB238b1E8a21451603D6213`](https://basescan.org/address/0xd5558B5C1CB37D70ADB238b1E8a21451603D6213) |
| Arbitrum One | [`0xd5558B5C1CB37D70ADB238b1E8a21451603D6213`](https://arbiscan.io/address/0xd5558B5C1CB37D70ADB238b1E8a21451603D6213) |
| BNB Chain | [`0xd5558B5C1CB37D70ADB238b1E8a21451603D6213`](https://bscscan.com/address/0xd5558B5C1CB37D70ADB238b1E8a21451603D6213) |

The vault has the same address on all four chains, and so does its
deposit router, `0x95Ae54AfD724629b5c88a13c542D56dCDc1f5294`. Both were
deployed on 28 October 2026 with a 7-day timelock on every owner action.
A wallet of the project accepted ownership on all four chains the same day; the page
`https://bridge.argonmine.com/owner` shows each vault's owner and lets the
wallet it was offered to accept it, chain by chain (`acceptOwnership()`;
the vault refuses any other sender).

Every vault holds the dollars that back uARG. Anyone can read its balance
on that chain's block explorer. `https://bridge.argonmine.com/api/status`
lists every vault and its reserves.

### Deposit: USDC or USDT in, uARG out

1. Open the bridge and choose **Deposit**.
2. Pick the chain and the token, and paste your Argonmine address (from
   the app's **Receive** screen).
3. Pay in one of two ways:
   - **With an EVM wallet** such as MetaMask: enter the amount, press
     **Connect wallet**, then approve and deposit. The page shows what you
     will receive after the fee and steps you through both transactions.
   - **Without one:** press **No wallet? Get a deposit address**, then
     **Show deposit address**. The page shows an address of your own on
     that chain. Send the token there from any wallet or exchange, **on
     that chain only**. Anything else sent to that address, or the right
     token on another chain, is lost.
4. The page follows the deposit. The bridge acts once the deposit's block
   is final on its chain: about 13 minutes on Ethereum, 15 to 30 minutes on
   Base and Arbitrum, a few blocks on BNB Chain. The uARG is then issued and
   arrives with the next Argonmine block, about 10 minutes on average. A
   deposit address takes a little longer, because the bridge first moves
   the money from that address into the vault.

The smallest deposit is 10 USDC or USDT on Ethereum and 2 on the other
chains, measured before the fee. The page shows it too.

### Withdraw: uARG out, USDC or USDT in

1. Open the bridge and choose **Withdraw**.
2. Paste the EVM address that should receive the money, enter the amount of
   uARG, and pick the chain and token. A token whose vault has no money
   right now is marked **no liquidity**. It becomes available again by
   itself once the vault is funded.
3. The page gives you a **withdrawal address**, its QR code and a **memo**
   such as `K7M2-X9RT`.
4. Send exactly that amount of uARG to the address, with the memo, from any
   Argonmine wallet. In the app: **Send**, paste the address, and paste the
   memo into the **Memo** field that appears.
5. The page tracks the payment through four steps: **Waiting for
   deposit**, **Deposit detected**, **Processing withdrawal** and
   **Completed**, with a link to the payout on the EVM chain. You can close
   the page; opening the same link again shows where it is.

How long it takes depends on the amount. A withdrawal of up to 1,000 uARG
waits for 6 Argonmine blocks, about an hour; up to 10,000 waits 30 blocks,
about 5 hours; up to 100,000 waits 120 blocks, about 20 hours. A larger
payout waits until the guardians approve it by hand, so the page does not
take more than 100,000 uARG in one withdrawal. It also refuses an amount
the vault cannot pay right now. The smallest withdrawal is 10 USDC or USDT
on Ethereum and 1 on the other chains.

If you send less than the amount, what arrived is paid out and you can send
the rest with the same memo. If you send more, all of it is paid out. A
payment without the memo still reaches the same EVM address. **Send only
uARG** to a withdrawal address: ARG, wARG or anything else sent there can
never be recovered, which is why the Argonmine wallets refuse to send it.

### What does it cost?

1% of each deposit and 1% of each withdrawal, kept in the vault until the
project moves it to its treasury, which it does regularly. Depositing
from an EVM wallet also costs that chain's gas; the bridge pays the gas of
every payout. See [Fees](docs/FEES.md) for the details and how to check
them.

uARG is an asset on the Argonmine chain in the sense of
[Specification §3.5](docs/SPEC.md): a 32-byte asset id derived from the
guardians' joint key and the symbol `UARG`. Only that key can issue it, the
chain conserves it in every transaction, and fees are always paid in ARG.
uARG has 8 decimals. Each unit is redeemable one for one for USDC or USDT
through the bridge, less the 1% withdrawal fee, subject to the risks in
[Risks](#risks) below. Which stablecoin on which chain backs a given unit
is not tracked, only that every unit is backed by a dollar of USDC or USDT
in some vault. The three coins are described in [Assets](docs/TOKENS.md).

### Risks

Read this before you deposit.

- **No external audit.** No outside firm has reviewed the vault contract,
  the deposit router or the guardians. Four in-house reviews, each followed
  by a second pass over its fixes, were done on 2026-10-28 (see
  [Security review](docs/SECURITY-REVIEW.md), "Round of 2026-10-28").
- **No limits.** There is no limit per deposit, per withdrawal or per day.
  The project chose this. A bug the reviews missed could cost what
  the vaults hold, not only a capped amount. Only the wait grows with the
  amount (see "Payout depth"), and a single withdrawal above 100,000 uARG
  waits for the guardians to approve it, because a payout that large is
  worth reorganising a young chain for.
- **The guardians are run by the project today.** All three guardians are
  run by the project itself, not by independent operators on separate
  machines. Issuing uARG needs all three; each payout needs 2
  of the 3. Moving them to separate machines and operators is planned after
  launch.
- **The vault owner is one wallet.** Each vault's owner is currently one
  wallet of the project, not a key on a server. The vault contract's own
  documentation says its owner must be a multisig of independent parties
  before the vault holds real funds; on mainnet it is not one yet, and
  moving ownership to a multisig is planned. Every change the owner makes (guardians,
  tokens, fees, limits, lifting a pause) waits 7 days in the vault's
  timelock, visible on chain. During that delay 2 of the 3 guardians can
  veto it; the owner can then propose it again as an override, which waits
  60 days. Two things are not delayed: withdrawing the fee surplus (never
  the backing), and handing the vault to a new owner, who then faces the
  same delay.
- **Stablecoins and chains.** uARG can be redeemed for a dollar only while
  USDC and USDT hold their dollar value, and while the chains the vaults
  sit on work as they should.

Bridge only what you can afford to have at risk.

## Trust model, stated plainly

This is a **federated** bridge. A fixed set of guardians, each running this
daemon on its own host with its own keys, watches Argonmine and every EVM
chain.

- **Issuing uARG** needs every guardian. The issuer key is a MuSig2
  (BIP-327) aggregate of all guardian keys, so an issuance is a single
  Schnorr signature that no subset of guardians can produce. The chain
  rejects any uARG that was not issued by that key.
- **Paying out a stablecoin** needs a threshold of guardians (2 of 3 in
  the reference setup). Each vault checks the signatures, which are bound
  to its own chain id, address, token, recipient, amount and burn id, and
  refuses to pay the same burn twice.
- **Nothing is automatic for an invalid deposit or burn.** A deposit whose
  destination is not a valid Argonmine address, that is below the token's
  minimum, or in a token the vault does not accept, is recorded as
  "unclaimable" and needs a human. A burn whose memo names a chain or token
  the bridge does not serve is "unreleasable" and needs a human.
- **One guardian coordinates, and only it.** The leader asks the others for
  signatures; every such request is signed with the leader's own guardian
  key, and a follower takes requests from that key and nothing else. The
  shared secret is a second layer, not the authority. A coordinator that
  lies gets nothing signed that a follower cannot check on its own, and a
  compromised leader still cannot issue a deposit twice, issue one before
  its confirmations, or issue more than the guardians' limits ("Security
  model" below).

What a user trusts: that the guardians are honest and online, that the
vaults hold the dollars that back every uARG, that USDC and USDT are each
worth a dollar, that no EVM chain reverts a block it has called finalized
(or, on a chain without that notion, reorganises deeper than its
confirmation depth), that the RPC providers of a chain are not all lying
together, and that Argonmine does not reorganise deeper than the payout
depth of a burn. Anyone can verify the backing: every vault's balance per token is
public on its chain, the bridge status sums them in uARG units, and the
explorer shows the uARG issued on Argonmine.

What a user does not trust: a web page, a database or a payout queue. The
bridge page is a convenience that builds the two EVM transactions; the
deposit is between the user and the vault contract, and the issuance is
between the guardians and the chain.

## Protocol

### Deposit (EVM to Argonmine)

1. The user approves the vault to take `amount` of a token and calls
   `deposit(token, amount, "arg1…")` on the vault of that chain. The vault
   checks that the token is allowlisted and the amount at least the
   minimum, takes the tokens, keeps its deposit fee and emits
   `Deposited(id, token, from, amount, fee, to)`, where `amount` is the
   **net** credited to the user and `fee` what the vault kept. Deposit ids
   start at 1 in every vault, so a deposit is identified by (chain id,
   deposit id).
2. Every guardian scans every vault's events. A deposit becomes
   **pending** with the uARG units its net amount pays, converted from the
   token's decimals (see "Amounts" below); the fee is recorded with it, and
   so are its block, the block's hash and the log's index. Deposits above
   the chain's final block are recorded as soon as they are seen, so a
   wallet can follow them, but nothing is ever done with them before their
   block is final ("Finality and providers" below).
3. Once a deposit's block is final the leader builds one
   **issuance transaction** paying final pending deposits from every
   chain, oldest first, within the issuance limits and the per-deposit
   caps: custody ARG coins for
   the fee (at most 20), one uARG output per deposit, at most one ARG
   change output back to custody, a NULLDATA output with the **deposit
   references** (`BRI1`, below), and a single issuance of exactly the
   total. Each guardian checks it on its own before it takes part in the
   MuSig2 signing ("What a guardian checks before it signs" below) and
   records what it signed before its partial signature leaves it.
4. The leader broadcasts the transaction. When it is mined, every guardian
   marks the deposits it names **confirmed**, from the deposit references
   in the transaction itself (never by address and amount), so no deposit
   can be paid twice even if the leader changes or a guardian's state is
   rebuilt.

### Deposit addresses (no wallet needed)

A deposit through `deposit` needs a browser wallet that approves the vault
and calls it. A **deposit address** removes that step: every Argonmine
address has its own EVM address on each chain that has a
`DepositRouter` contract, and a plain USDC or USDT
transfer to it, from any wallet or exchange, becomes a vault deposit
credited to that Argonmine address.

1. The page asks the leader for the address (`GET
   /api/deposit-address`). The leader computes it off-chain as the CREATE2
   address of the router's `Forwarder`:
   `keccak256(0xff ‖ router ‖ keccak256(bytes(to)) ‖ forwarderCodeHash)[12:]`,
   where `to` is the Argonmine address in its canonical lowercase form (the
   salt is its exact bytes) and `forwarderCodeHash()` is read once per chain
   from the router. Before it hands out any address on a chain it checks,
   once, that the router's `vault()` is that chain's vault and that the
   router's own `depositAddress(to)` view gives the same address; if either
   check fails, deposit addresses stay off on that chain (the reason is in
   `/api/status` as `router_error`). It then remembers the address as a
   **watch**.
2. The user sends USDC or USDT to the address, on that chain only. The
   leader looks for `Transfer` events of the chain's tokens into its
   watched addresses (`eth_getLogs` per token, 100 addresses per query,
   2000 blocks per range, from the router's deployment block), every scan
   round.
3. Once a transfer has the chain's `confirmations`, the leader reads the
   address's balance. At or above the token's `min_deposit` it calls
   `router.sweep(token, to)`: the router deploys the forwarder on its first
   sweep, pulls the whole balance and calls `vault.deposit(token, amount,
   to)`. Below the minimum nothing happens: the funds wait at the address,
   and the next transfer that lifts the sum over the minimum is swept with
   it. A sweep the vault refuses because of its own minimum
   (`AmountTooSmall`) is treated the same way, not as an error.
4. The vault takes its fee and emits `Deposited` with the router as
   `from`; from here it is an ordinary deposit, issued as uARG by every
   guardian's checks and the MuSig2 issuance like any other.

There is never more than one sweep in flight per address and token: a sweep
that is sent is followed to its receipt before another is considered. A
sweep that fails to go out (the node is down, no gas) is retried with a
backoff of 30 seconds doubling to 10 minutes; one that reverts on chain, or
is not mined within an hour, is marked failed and the address is read again
after the backoff. Sweeping is open to everyone and harmless: it can only
move what sits at the address derived from `to` into the vault credited to
that same `to`, so a third party sweeping first changes nothing.

**Who pays.** The leader's gas key pays the gas of every sweep, as it
does for releases ("Release transactions") (about 270,000 gas for an address's first sweep, which
deploys its forwarder, and about 100,000 after that). The user pays only
the vault's deposit fee. On a chain where gas is expensive, set the
tokens' `min_deposit` so that a minimum deposit's fee is worth the gas. A
gas key without gas shows the problem in the chain's `error`, and the sweep
waits with the reason in its `error`.

**What is lost if sent wrong.** Only the chain's allowlisted tokens can ever
leave a deposit address. Another token, the chain's native coin, or the
right token sent on another chain (where that address belongs to no one)
cannot be recovered. The page says so next to the address.

**Followers** do none of this: they neither watch nor sweep and answer the
deposit-address endpoints with 503. Watches live in the leader's state
(`w/` records) with what
arrived and the sweeps made, so a restart loses nothing and never sends a
second sweep for one that was in flight. A watch that never received
anything is dropped after 30 days without a request for it; one that did
stays, since its owner may send to the same address again without visiting
the page. The leader watches at most 100,000 addresses; past that,
`/api/deposit-address` answers 503 with that reason. Registrations are
limited per client to a burst of 20 and one every 3 seconds after that.

### Redemption (Argonmine to EVM)

1. The user sends a version-2 transaction that spends uARG coins and pays
   less uARG forward than it spends (the difference is burned), with a data
   output carrying a memo that names the payout:

   | Memo | Bytes | Meaning |
   |---|---|---|
   | `BRG1` ‖ recipient (20) | 24 | pay the bridge's **default** token: the first chain's first token in its configuration |
   | `BRG2` ‖ chain id (u64, big-endian) ‖ token (20) ‖ recipient (20) | 52 | pay that token on that chain |

   This is the bridge's original way out. The bridge page and the app no
   longer use it: they withdraw through swap addresses (below). The
   command-line wallet still builds such a burn with `argonmine-wallet
   redeem`, with a chain and token choice, and shows the burn's
   transaction id, which is its redemption id. `GET /api/memo` returns the
   memo for a chosen chain, token and address, for burns built by hand.
2. Every guardian recognises the burn and records a **redemption**: the
   chain, the token, the recipient, and the burned amount converted to that
   token's units. A memo naming a chain or token that is not configured,
   or a payout below the token's minimum ("Minimum withdrawal" below),
   makes the redemption **unreleasable** with the reason.
3. After the burn's payout depth, which grows with what its transaction
   pays out ("Payout depth" below), the leader checks that the vault can pay it now (not
   paused, within its per-release and 24-hour limits, enough of the token
   beyond the releases already in flight) and asks the other guardians for
   release signatures. Each guardian fetches the burn from its own node,
   decodes the memo against its **own** configuration, recomputes the
   amount and recipient, checks the token's minimum, the depth its **own**
   tiers ask for what the transaction pays out and what the bridge pays
   out of the blocks up to it, that releases are not paused on it, that
   the chain's configured vault is the one in use, and that the burn is at
   or above the height the chain's current vault pays from ("Vault epochs"
   below), and signs `releaseDigest(token, to, amount, burnId)` as that
   chain's vault defines it: the EIP-191 digest of
   `keccak256(abi.encode(chainid, vault, token, to, amount, burnId))`,
   where `burnId` is the burn's transaction id. A guardian signs a burn
   for one release only: it records the digest before the signature
   leaves it, returns the same signature if asked again for the same
   release, and refuses any other release of that burn.
4. The leader counts an answer toward the threshold only when it is a
   signature over this digest by one of the vault's guardians, in the one
   encoding the vault accepts (`Vault._recover`: 65 bytes `r ‖ s ‖ v`, `v`
   27 or 28, or 0 or 1 taken as 27 or 28, and `s` at most half the curve
   order). Anything else (another digest or key, the "compressed key"
   recovery codes 31 to 34, a high `s`, all of which recover to the signer
   off chain) would make the release revert: it is ignored and logged, and
   the leader asks the next guardian. A guardian's own signatures are
   always in that encoding.
   With a threshold of signatures the leader calls `release(token, to,
   amount, burnId, sigs)` on that chain's vault with the **gross** amount
   of the burn, from its **gas key** ("Release transactions" below): the
   vault lets anyone call `release` with valid guardian signatures, so the
   sender is a dedicated low-balance key, never a guardian key or the
   vault's owner. The vault keeps its release fee, pays `amount − fee`,
   records the burn id and emits `Released(burnId, token, to, amount,
   fee)`. The redemption is **confirmed** when the vault's
   `released(burnId)` says it was paid; the transaction that did it is
   found among those sent for it by its receipt, and the fee and the net
   paid are taken from its event.

### Swap addresses (no memo needed)

A **swap address** is an Argonmine address that names a payout: uARG sent
to it from any wallet, in an ordinary transfer with no memo, is paid out as
the named token on the named chain to the named EVM address. It is what the
bridge page's **Withdraw** tab gives (with a memo that only tracks the
payment, see Withdrawals below), and what an exchange or another wallet can
use without knowing anything about the bridge.

Its 32-byte P2PK payload is not anyone's key; it spells the destination:

| Bytes | Content |
|---|---|
| 0–2 | `83 ba 1x`: every swap address reads `<hrp>1qswap…`; the low 4 bits of byte 2 are the layout version, 0 |
| 3 | the smallest value that makes the payload a valid x coordinate, so wallets that check keys accept it, and each payout has exactly one address |
| 4–7 | EVM chain id, big-endian |
| 8–11 | token symbol, ASCII upper case, zero-padded (`USDC`, `USDT`) |
| 12–31 | the EVM recipient |

Nobody has the private key of such a point: finding one is the discrete
logarithm problem, and a key made at random matches the 248 fixed bits with
no real chance. What is sent to a swap address can therefore never move
again: uARG sent there is burned as surely as by a burn transaction, and
**ARG or any other asset sent there is lost**. The Argonmine wallets (the
app, the command-line wallet and the older desktop program) refuse to send
anything but the bridge's own uARG (by asset id) to one, and refuse a swap
address naming a chain or token the bridge does not serve.

Every guardian's index treats each output of the bridge's uARG to a swap
address as a redemption of that output's amount, with the id `txid:vout`
(one transaction can pay several). The payout is read from the address
alone, so there is nothing to register and no server can redirect it; the
rest is a redemption like any other (steps 2 to 4 below, the payout depth,
the vault epochs and the signing memory), except that its `burnId` in the
vault is `H("Argonmine/bridge/swap-payment", txid ‖ vout as 4 bytes
big-endian)` (BIP-340 tagged hash), which no transaction id can equal. A
swap address whose chain or token the bridge does not serve makes the
redemption unreleasable with the reason, like a bad memo. The token is
named by symbol, not by contract, so an address stays valid when a testnet
token is redeployed; a chain that lists two tokens with one symbol cannot
be named by a swap address.

`GET /api/swap-address` returns the address for a chain, token and
recipient; anyone can compute the same one with
`argonmine-wallet swap-address <chain-id> <token> <0x-address>`. The
explorer labels swap addresses with the payout they name.

### Withdrawals (the bridge page)

The bridge page's **Withdraw** tab follows one payout from start to end:

1. The user pastes the EVM address to be paid, enters the uARG amount and
   picks the chain and token. `POST /api/withdrawals` checks it as a payout
   would be checked (a served chain and token, at least 1 uARG and at least
   the token's minimum withdrawal, at most the largest payout tier, at most
   what the vault can pay now, nothing paused)
   and answers with the payout's swap address and a **memo**: eight
   characters, unique among the bridge's withdrawals, shown as `K7M2-X9RT`.
2. The page shows the address, its QR code and the memo, each with a copy
   button. The user sends the uARG there from any Argonmine wallet with the
   memo (the browser app's Send has a Memo field for a withdrawal address;
   the command-line wallet takes `-memo`). The memo rides in a NULLDATA
   output as text.
3. The leader reads the node's mempool every few seconds, so a payment
   shows the moment it is broadcast, and every guardian records the memo
   with the payment when it is mined.
4. The page polls `GET /api/withdrawal/{memo}` and shows four steps:
   **Waiting for deposit**, **Deposit detected** (seen, then confirming to
   the payout depth), **Processing withdrawal** (the guardians sign and
   send the release, or say why it waits: vault liquidity, a pause, the
   vault's limits), **Completed** (the token delivered, with the EVM
   transaction). `?w=<memo>` in the page's address reopens it on any device.

Only the bridge page itself creates withdrawals: `POST /api/withdrawals`
takes `application/json` only, which a page of another origin can send
only after a CORS preflight, and the preflight is refused (403); none of
its answers carries `Access-Control-Allow-Origin`. So a hostile site
cannot make withdrawals from its visitors' browsers. Reading a withdrawal
(`GET /api/withdrawal/{memo}`) stays open to every origin.

The memo moves no money: the swap address alone decides the payout, for
every guardian, so nothing about a withdrawal is signed or shared between
guardians and a missing or mistyped memo loses nothing. Each payment is
paid out as it arrives:

| What arrives | What happens | What the page says |
|---|---|---|
| The amount, with the memo | paid out | the four steps |
| Less than the amount | what arrived is paid out | how much is missing, and to send the rest with the same memo |
| More than the amount | all of it is paid out | the extra, and the total payout |
| No memo, or one that is no withdrawal to this address | paid out; matched to the newest withdrawal to the address made before the payment was first seen (in the leader's mempool, else its block's time) | that it was matched by address |
| Less than 1 uARG | held, unreleasable, when below the token's minimum withdrawal (by default it is) | counted, not listed |
| Below the token's minimum withdrawal | held, unreleasable: never sent | a reference for the guardians |
| ARG or another asset | can never be paid out or moved | that it is lost (counted, not listed) |
| A payment that cannot be paid automatically | held, unreleasable | a reference for the guardians |

A payment that leaves the mempool stays on the page until its block is
indexed (ten minutes at most), and a payment mined again after a
reorganisation is shown again even when its payout was already made.

Withdrawals are kept by the guardian that serves the page (state keys
`q/…`). They cost nothing to make, so everything about them is bounded.
A withdrawal counts as paid only once a uARG payment of at least 1 uARG
belongs to it; one never paid is forgotten after 48 hours (it is free to
make again), and any after 180 days. At most 200 000 are open (never paid)
across the bridge and 20 per swap address: past either, the oldest open
one is forgotten to make room (past the address's limit, a recent one for
the same amount is handed out again instead), and a paid one never is. One
client keeps at most 30 open within 48 hours. `GET /api/withdrawal/{memo}`
lists the newest 50 payments of at least 1 uARG, with the totals of all of
them and counts of the smaller and lost ones; it is worked out once per
swap address and served from memory until something about that address
changes (at most 5 seconds, or a new block). The leader follows a mempool
of up to 50 000 transactions (above that, payments show once mined),
outputs of at least 1 uARG, 50 per address and 10 000 in all. The
payments themselves are redemptions like any other and stay on record.

### Amounts

uARG units are dollars with 8 decimals. For a token with `D` decimals:

- **Deposit:** `units = amount × 10^8 / 10^D`. For `D > 8` (an 18-decimal
  USDT on BNB Chain) the division rounds down and the remainder, less than
  one uARG unit, stays in the vault. For `D < 8` it is an exact multiply.
- **Payout:** `token amount = units × 10^D / 10^8`. For `D < 8` (USDC and
  most USDT, 6 decimals) the division rounds down: a 6-decimal token cannot
  pay fractions of a cent, so the dust below `10^(8−D)` units is burned and
  not paid. A burn that rounds to zero is unreleasable ("burn too small to
  pay out"), and so is any payout below the token's minimum withdrawal
  (below). For `D ≥ 8` the payout is exact.

### Minimum withdrawal

Every token has a **minimum withdrawal**, `tokens[].min_release` in the
chains file: the smallest payout, gross in token units, that a release is
sent for (default: the token's `min_deposit`, or one token when that is
0). A redemption below it is recorded as **unreleasable** with the reason
("below the minimum withdrawal of 10 USDC on Ethereum"), on every
guardian from its own configuration; no guardian signs a release below its
minimum and the leader never sends one. Without it, one cheap transaction
with many dust outputs to a swap address (each output is a redemption,
and asset outputs are exempt from the dust limit) would make the leader
send one release transaction per output and empty the gas key. Set it
well above what a release costs in gas on the chain: about 100 000 gas,
so tens of dollars at busy times on Ethereum, cents on the rollups. The
status shows it per token (`min_release`), and the bridge page and `POST
/api/withdrawals` ask for at least the larger of 1 uARG and what pays it.
The wallets refuse less than what pays it, converting the uARG amount
exactly as the guardians do (`units × 10^D / 10^8`, rounded down): the
browser app's Send to a withdrawal address and the desktop app's Swap say
"The smallest withdrawal of USDC on Ethereum is 10.00 uARG" and
send nothing. Set the same value on every guardian.

All of this is integer arithmetic on big integers; nothing is ever a float.
Token amounts are carried as decimal strings in the state and the API,
because 18-decimal amounts exceed 64 bits.

### Fees

The bridge charges **1 percent on deposits and 1 percent on redemptions**.
Both fees are taken by the vault contract, in the token, and stay in the
vault as its **surplus**; the daemon never moves a fee and could not. The
project withdraws the surplus to its treasury regularly
([Fees](docs/FEES.md)).

- **Where it is taken.** On a deposit the vault takes the whole `amount`
  the user approved, keeps `fee = amount × depositFeeBps / 10000` (rounded
  down to a token unit) and credits `amount − fee`; the guardians issue
  exactly that net, converted to uARG. On a redemption the guardians sign
  and call `release` with the gross value of the burn (`units × 10^D /
  10^8`, rounded down); the vault keeps `fee = gross × releaseFeeBps /
  10000` (rounded down) and transfers `gross − fee`. A fee on a tiny
  amount rounds to zero, so the user never pays more than the stated
  share, and a burn that was releasable at zero fee is releasable at any
  fee. The deposit minimum (`min_deposit`, and the vault's own) is
  measured on what the user deposited, before the fee.
- **Backing is never touched.** Every vault keeps `obligations(token)`, the
  token units that back issued uARG: a deposit adds its net, a release
  subtracts the gross of the burn. The fee of every deposit and every
  release lands in the difference `surplus(token) = balance −
  obligations(token)`. Only the surplus can ever be withdrawn, only by
  the vault's owner, and only up to that amount (`withdrawSurplus(token,
  to, amount)` reverts above it); the obligations leave the vault through
  `release` and nothing else. So uARG in circulation stays
  fully backed whatever is withdrawn, and `reserve ≥ obligations` holds
  on every chain.
- **The rates live in the contract.** `depositFeeBps()` and
  `releaseFeeBps()` are set by the owner (`setFees`, at most 10 percent
  each) and are read by the daemon from every vault on every scan, with
  the reserves; the chains file carries no fee, and a vault that charges
  nothing simply reports 0.
  The guardians only read the fees: they never set, collect or forward
  them.
- **What the status shows.** Per chain, `deposit_fee_bps` and
  `release_fee_bps`; per token, `obligations` and `surplus` in token
  units beside `reserve`; at the top, `backing_units` and `surplus_units`,
  the dollar sums of the obligations and of the surplus over every chain
  and token (`reserve_units` is their sum, up to rounding of 18-decimal
  tokens). A deposit carries `token_fee`, the units the vault kept, beside
  `token_amount`, the net it credited. A redemption carries `token_amount`
  (the gross, what `release` is called with and what the guardians sign),
  `token_fee` and `token_paid` (`token_amount − token_fee`), computed from
  the chain's release fee when the burn is classified and recomputed from
  the latest read of it when the release is sent, then replaced by the
  `Released` event's figures once the receipt is in.

### Reorganisations

Every scanner keeps a cursor. On Argonmine every block the daemon indexes
is recorded with its hash, and every transaction that changes the state (a
custody coin received or spent, a deposit settled, a burn recorded) is
applied in one atomic write together with an undo record: the previous
value of every key it wrote. Each round the daemon asks the node for the
hash at its cursor. Only a hash that differs from the recorded one counts
as a reorganisation; an error from the node stops the round and nothing is
undone. On a real reorganisation it walks back to the last block both
agree on and undoes exactly what the blocks above it did, newest first: a
deposit settled there is pending again (issued, on the leader), a custody
coin received there is gone, one spent there is back, a burn recorded
there is forgotten unless its release was already sent. Then it indexes
the new branch, where the issuance is usually mined again and settles the
same deposits again. Undo records are kept for 1000 blocks; a deeper
reorganisation stops the daemon with an error. A block indexed only in
part (the daemon stopped in its middle) is undone and indexed again.
Before it spends a custody coin, the leader asks its node (`gettxout`)
whether the coin is still there and drops it if not. A reorganisation
that undoes more than `-l1-reorg-pause` blocks (default 3) **pauses every
issuance and release** on that guardian until its operator resumes them
("Pauses" below): a burn's payout depth is chosen on the assumption that
Argonmine does not reorganise that deep, so the guardians stop and a human
looks.

On the EVM chains the protection is finality, below.

### Finality and providers

A deposit is acted on only once its block is **final**. Each chain has a
`finality` mode:

- `finalized` (the default on Ethereum, Sepolia, Holesky, Hoodi, Base,
  Base Sepolia, OP, Arbitrum One and Sepolia, BNB Chain and its testnet,
  Polygon and Amoy): the final block is the lower of the block the node
  calls finalized (`eth_getBlockByNumber("finalized")`) and `latest −
  confirmations`. On Ethereum that is about 13 minutes; on Arbitrum and
  Base it follows the L1 and takes 15 to 30 minutes; on BNB Chain fast
  finality gives a few blocks, so the confirmations decide.
- `depth` (every other chain, a local anvil included): `latest −
  confirmations`.

The scan's cursor never passes the final block. Every block it stops at
(each scanned range's last block) is recorded with its hash, and so is
every deposit's block hash and log index. Each round the hash at the
cursor is read again. When it changed, the chain reorganised below a block
the guardian took for final (a provider fault, a reorganisation deeper than
`confirmations` on a `depth` chain, or a broken finality): the scan goes
back to the newest recorded block that is still in the chain (or to
`start_block` if none is) and logs an ALERT, and every deposit above that
point is looked up again **by its transaction hash**:

- the transaction is in another block with the same `Deposited` event
  (same id, token, depositor, amount, fee and destination): the record is
  **re-located** there and the deposit goes on as before, no human needed;
- the transaction is gone, failed, or no longer holds that event, and the
  deposit was never issued (nor signed by this guardian): it is marked
  **`reorged`** and is never issued. If its id is later taken by another
  deposit on the new branch, that deposit replaces the record and the old
  one is kept aside (`z/` in the state);
- the deposit was already issued (or this guardian signed an issuance for
  it): an **alert**. Issuance on that chain is paused on the guardian
  ("Pauses") until its operator has looked and resumes it; the deposit
  keeps its status with the alert in its `error`.

The same look-up runs for any recorded deposit whose block the final scan
reaches without finding its event there, which also covers the deposits
recorded above the final block (they are previewed from the first
provider each round so wallets can follow them) when they do not survive.

Right before it signs an issuance, every guardian, the leader for its own
share included, reads each deposit's **receipt** again: status 1, mined in
exactly the block on record (same hash, same log index), at or below the
final block read from every provider just now, and holding a `Deposited`
log of the vault's address with exactly the recorded id, token, depositor,
amount, fee and destination. A deposit is also issued only once the
guardian's own final scan has passed its block.

**Several RPC providers.** A chain can list `"rpcs"` beside `"rpc"`. The
heads, the block hashes, the deposit logs of every final range and the
receipts are read from **every** provider, and a deposit is acted on only
when they all agree on its block hash and its log. When they disagree, the
chain is paused on that guardian (nothing on it is issued or released, and
`chains[].paused` in the status says why, with an ALERT in the log) until
they agree again. The first provider is the one transactions are sent
through. Use providers run by different companies.

## Security model

### Who may coordinate

Only the leader ever asks the other guardians for anything:
`POST /guardian/nonces` and `POST /guardian/sign` (the two MuSig2 rounds
of an issuance) and `POST /guardian/sign-release`. Every request carries
four headers:

| Header | Content |
|---|---|
| `X-Guardian-Time` | Unix seconds when the request was made |
| `X-Guardian-Nonce` | 16 random bytes, hex |
| `X-Guardian-Auth` | BIP-340 Schnorr signature by the leader's L1 guardian key over `TaggedHash("ArgonmineBridge/guardian-request/v1", P)` |
| `X-Guardian-Signature` | HMAC-SHA256 with the shared secret over `"ArgonmineBridge/guardian-request/v1" ‖ P` |

where `P = varbytes(network) ‖ varbytes(custody key) ‖ varbytes(path) ‖
time (i64) ‖ varbytes(nonce) ‖ SHA-256(body)` in the consensus encoding.
A follower is configured with the leader's public key (`-leader-pubkey`,
one of `-guardians`; it does not start without it) and refuses, with 401,
a request signed by any other key, without the secret, more than 60
seconds away from its own clock, or whose nonce it has accepted before
(the nonces of accepted requests are remembered until their time is too
old anyway; only a request already proven to come from the leader is
remembered, so nobody else can fill that memory). The leader takes no
guardian requests at all (403), so its public address never serves
`/guardian/*`. The secret must have at least 32 characters; the daemon
reads it from `ARGONMINE_BRIDGE_SECRET` first, then `-secret-file`, and
only then `-secret`, which shows in the process list. A follower keeps at
most 4 signing sessions in flight, each for at most 2 minutes, an issuance
spends at most 20 coins and pays at most 50 deposits, and a request body
is at most 1 MB.

### What a guardian checks before it signs an issuance

Every guardian, the leader for its own share included, checks the
transaction against its own node, its own EVM clients and its own state:

- every input is a custody ARG coin its node has (`gettxout`): unspent,
  mature, of the amount the leader claims; at most 20 of them;
- every uARG output pays, in order, a deposit it saw itself, still
  pending, to that deposit's address and for its amount;
- each deposit's block is final on every one of the guardian's providers
  read just now, and passed by its own final scan; its receipt says the
  transaction succeeded in exactly that block with exactly the recorded
  event of the vault ("Finality and providers");
- issuance is not paused on the guardian, for everything or for that
  chain, and its providers agree ("Pauses");
- each deposit's token answered `decimals()` with the configured value
  ("Token decimals"), and its gross is within the token's `max_deposit`;
- the chain's vault passed its deposit id check, and the deposit is one of
  its own (its id at or above the vault's first deposit id: "Replacing a
  vault");
- the only other outputs are at most one ARG change output to custody and
  exactly one NULLDATA output, equal to the deposit references of the
  deposits paid;
- the single issuance is the bridge's asset, under the custody key, of
  exactly the sum of the outputs, and the fee is bounded;
- the issuance is within the limits: at most `-max-issue` uARG, and with
  what this guardian signed in the last 24 hours at most `-max-issue-day`;
- the signing memory allows it (next section).

It checks again when the second round comes, and records the issuance
(the coins it spends, the deposits it pays, its amount and time) on disk,
flushed, before it computes and returns its partial signature.

### The signing memory

A guardian remembers, per deposit, every issuance transaction it partially
signed, with the custody coins it spends. It signs another issuance for
that deposit only if, for each of those earlier ones, the new transaction
is the same transaction, or spends at least one of the same coins (so the
two conflict and at most one can ever be mined), or the earlier one can
never be mined any more because one of its coins was spent on Argonmine by
another transaction at least 100 blocks deep (and at least
`l1_confirmations`): a reorganisation shallower than that could bring the
earlier one back. Whatever the
coordinator does, every issuance that could still confirm for a deposit
conflicts with every other, so a deposit is issued at most once. The
leader builds its transactions to satisfy this: for a deposit it signed
before, it spends one of that issuance's coins that is still unspent, and
the deposit waits while there is none; and it does not spend such coins
for other deposits. An issuance broadcast by the leader that is neither
mined nor in its node's mempool after 30 minutes returns its deposits to
pending, to be issued again under the same rule. The memory survives
every format change; deleting the state directory deletes it.

### Deposit references

Every issuance carries, in one NULLDATA output,

```
"BRI1" ‖ (chain id varint ‖ deposit id varint) per deposit, in output order
```

with the minimal LEB128 varints of the consensus encoding, at most 80
bytes (the NULLDATA limit); the leader ends a batch when the next
reference would not fit (15 Sepolia deposits, about 19 on chains with
smaller ids). A guardian settles deposits from these references, in a
transaction that creates uARG (which only the custody key can do), and
from nothing else: a reference to a deposit it has not seen yet is kept,
with the issuance's height, and applied when it sees the deposit. An
issuance mined before references existed is settled only by the leader,
by its own record of the transaction id.

A reference names a deposit by chain and id alone, so the ids of every
vault on a chain must be distinct ("Replacing a vault"). As a last line
of defence, a reference from an issuance mined below the height the
chain's current vault pays from (its epoch or `l1_start_height`) to an id
that vault numbers its own deposits with is never applied: that issuance
paid a deposit of an earlier vault, and the current vault's deposit with
the same id cannot be told from it. The guardian pauses issuance on the
chain (`evm/<chain id>`, "Pauses") with an ALERT, and the deposit waits.

### Limits

`-max-issue` (default 10000 uARG) caps one issuance and `-max-issue-day`
(default 100000 uARG) what one guardian signs over any 24 hours; 0
disables a limit. The mainnet guardians run with both set to 0: there is
no cap on issuance. The project chose this on 2026-10-28. Every guardian enforces its own values, so set the same
on all of them. The daily limit counts each deposit once, however many
issuances were signed for it (a round tried again, an issuance built anew
after one was dropped), with its amount on record. A deposit above `-max-issue` waits with the reason in its
`error` and is counted in `held` in the status; one that would pass the
daily limit waits until the window frees up. Raising a limit needs every
guardian.

### Per-deposit cap

A token may have a `max_deposit` (token units, measured on the gross the
user deposited, like `min_deposit`). A deposit above it is recorded as
pending **with the reason in its `error`** and counted in `held`; it is
never issued while it is above the cap of the guardian asked, and every
follower refuses it. Its funds are in the vault (they count in
`obligations`, since the vault took the deposit). Two ways out:

- **Issue it**: raise `max_deposit` on every guardian (a new chains file
  and a restart); it is issued in the next batch.
- **Refund it**: the guardians sign by hand a release of the deposit's net
  to the depositor's address (`from` in `/api/deposit/<chain>/<id>`) with
  `burnId = keccak256("refund" ‖ chain id ‖ deposit id)` and send it from
  any key (the vault pays once per burn id, and the release lowers the
  obligations by the amount, so the backing stays exact). Then, **on every
  guardian**, `argonmine-bridge mark-unclaimable <chain id>#<id> refunded`
  marks it unclaimable so it can never be issued afterwards, whatever the
  cap becomes ("Operator commands").

### Token decimals

At start every guardian reads `decimals()` of every configured token (on
the first scan of each chain, and again on every start: the chains file is
read only at start, so a change of the file always goes with a restart).
A token whose `decimals()` differs from the file, or that has none, is
**refused**: its deposits are recorded with the reason and never issued,
its burns are not paid, followers refuse both, and the status shows the
reason in `chains[].tokens[].error` with an ALERT in the log. Until the
call has succeeded once (an RPC that is down at start) the token is not
acted on either. When the file is fixed and the guardians restarted,
pending deposits recorded with the old decimals are recomputed from their
token amount on chain.

### Payout depth

How many Argonmine blocks a burn waits before it is paid scales with what
its **transaction** pays out of the bridge: `-l1-confirmation-tiers`
(default `1000:6,10000:30,100000:120`) is a list of `amount:blocks`, the
amount in uARG (dollars), increasing. A transaction paying out at most
1000 uARG waits 6 blocks, up to 10000 waits 30, up to 100000 waits 120;
`-l1-confirmations` is the floor of every tier (a payout never waits
fewer). The amount is the transaction's whole bridge payout: every output
of the bridge's uARG to a swap address plus what it burns (its uARG in
less its uARG out), the redemption's `tx_payout`. A reorganisation undoes
all of a transaction's payouts together, so splitting a payout into
several outputs, each under a tier, gets each of them the depth of the
total, not of its part. Every guardian computes the total from the
transaction as its own node has it.

A reorganisation undoes whole blocks, too, so payouts split across
**transactions** add up as well: the depth is the tier of the larger of
the transaction's payout and the **window volume**, what the bridge pays
out of the `W` blocks up to the payout's height (heights `h − W + 1` to
`h`, the payout's own block included): the sum of the own amount of every
redemption on record there, whatever its status. `W` is the deepest depth
a payout can wait, the last tier's (at least `-l1-confirmations`; 120
blocks with the default tiers). Each guardian adds it up from its own
redemption records (the leader before it releases, a follower before it
signs, once its own Argonmine scan has reached the payout's height), from
an index of the amounts by height kept in memory, so it costs one lookup
per block of the window whatever the number of redemptions. So many
transactions each under the first tier, within a few blocks, wait longer
as they add up: with tiers `50:3,500:6`, twelve payments of 45 uARG in
four blocks wait 3, then 6, and the last ones, which take the window above
500 uARG, are held like a payout above the last tier
(`TestPayoutDepthWindow`). A payout's window holds only blocks at or
below its own height, so later payouts never change its depth, and one
already released is never touched. The cost is the guardians' attention:
while the bridge pays out more than the last tier within `W` blocks, the
payouts that take it there wait for `approve-release`.

A payout whose transaction, or whose window, pays out **above the last
tier** is held, with the reason in its `error`, for a
manual release: once the operators have checked it, `argonmine-bridge
approve-release <burn txid>` (`<txid>:<output>` for each payment to a
swap address) **on every guardian that signs** lets it be paid at the
last tier's depth. Every guardian applies its own tiers before it signs,
so set the same values on all of them; a follower asked too early answers
"burn has n of m confirmations". The depth a payout needs is in its
`confirmations` field and the tiers in the status.

### Release transactions

The leader sends every release, and every sweep of a deposit address, from
its **gas key** (`-gas-key`): an EVM key of its own with a little native gas
on every chain, distinct from every guardian's signing key and from the
vault's owner. The daemon refuses to start a client that would send from
the guardian's own key, and on each chain it checks once that the gas key
is neither one of the vault's guardians nor its owner (`isGuardian`,
`owner()`); if it is, nothing is sent there and the chain's `error` says
why. The gas key's balance on every chain is in `chains[].gas_balance`
(wei), with `gas_low` and a warning in the log once an hour below the
chain's `min_gas_balance` (default 0.01 of the native coin, 1 POL on
Polygon). Losing the gas key loses that gas and nothing else: it signs no
release, it only carries the guardians' signatures to the vault.

Each release is tracked by its nonce, its fees and every hash broadcast for
it (`send` in the redemption). It is an EIP-1559 transaction (tip from
`eth_maxPriorityFeePerGas`, fee cap twice the base fee plus the tip) except
on BNB Chain, which gets a legacy transaction at the network's gas price
plus 10 percent. Each round the leader:

- reads the vault's `released(burnId)`: when true, the burn is paid, by
  whichever of the transactions sent for it (found by its receipt), and
  the redemption is confirmed. This is also what makes double payment
  impossible: the vault pays a burn id once, so a replacement and its
  original, or two transactions with different nonces, cannot both pay;
- goes back to pending when one of them reverted;
- when the nonce was taken by a mined transaction that is none of the
  release's (it was replaced, or dropped and the nonce reused) and the burn
  is not paid, sends the same call again with a new nonce;
- when none is mined after `-release-timeout` (default 5 minutes), sends it
  again **with the same nonce** at higher fees: tip and fee cap at least
  12.5 percent up (legacy: gas price at least 10 percent up), and at least
  what the network asks now; it is also how a transaction dropped from the
  mempool is rebroadcast;
- never pays more than the chain's `max_gas_price_gwei` (defaults: 300 on
  Ethereum and Sepolia, 30 on BNB Chain, 20 on Base, OP and Arbitrum, 3000
  on Polygon, 500 elsewhere): when the next raise would pass it, the
  transaction stays as it is and the redemption shows "not mined at the
  chain's gas price cap" until the network is cheaper again.

Before sending, a release is simulated on the latest block (`eth_call`):
one the vault would refuse because the burn is already paid confirms the
redemption; one refused with `LimitExceeded` or `IsPaused` waits.

### The vault's limits and pause

Before it asks for signatures the leader reads, for the token, the vault's
`paused()`, `maxRelease(token)` and `periodAvailable(token)` (what the
24-hour window still allows), and its balance less the payouts of the
releases already in flight. A burn the vault cannot pay now **waits** with
the reason in its `error` and is tried again every round, never failed:
above the window's room it is paid when the window has room again; above
`maxRelease` it waits for the owner to raise the limit (through the
timelock); while the vault is paused it waits for the pause to be lifted;
short of the token it waits for liquidity. A `LimitExceeded` at the moment
of sending is handled the same way. The status shows, per chain and token,
`available_now`: the most one redemption in that token can be paid right
now (gross token units: the balance less the releases in flight, within
`maxRelease` and the window; "0" while the vault is paused), with
`max_release`, `period_available` and the chain's `vault_paused`, so the
apps can warn before a user burns more than can be paid at once. A vault
deployed before these views existed reads as unlimited and not paused.

### Guardian veto

Every change the vault's owner makes after setup waits for the timelock
delay (`propose*`, then `execute*`). If the owner's key is stolen, its
changes are visible as `Proposed` events for the whole delay, and the
guardians can drop any pending one with `veto(id, sigs)`: `threshold`
guardian signatures over `vetoDigest(id)`, ordered by signer address,
submitted by anyone. One guardian alone cannot veto, so a single faulty
guardian cannot block the owner's changes, and a pause (which any one
guardian can trigger) does not cancel them.

`vetoDigest(id)` is the EIP-191 hash of `"Argonmine/vault/veto"`, the chain
id, the vault, the proposal id, and the proposal's `proposalEta(id)` and
`proposalNonce(id)` (how many times that id was proposed), read from the
vault at the moment: it names the one proposal pending now. A proposal's id
is the hash of its calldata, so the same change proposed again has the same
id; signatures made (and published in the veto's calldata) for one proposal
never veto a later one of the same change.

A veto delays the owner; it cannot stop the owner for good. The proposal
that follows a veto of the same id is an **owner override**
(`overrideOf(id)`, set while `vetoed(id)` was): it waits `OVERRIDE_DELAY`,
twice `MAX_DELAY` (60 days), instead of `delay`, and `veto` refuses it
(`NotVetoable`). Every other proposal keeps `delay` and stays vetoable,
an override that expired or was cancelled included when proposed again.
Without it a compromised threshold of guardians could veto every guardian
rotation and every unpause, and keep the backing frozen forever; now the
owner's way out is to propose the rotation again and execute it after
`OVERRIDE_DELAY` (`ReviewFVetoTest`). A pause still drops a pending
unpause, an override too, so guardians who keep pausing are rotated out
first, which a pause does not touch. This rests on the owner being a
multisig of independent parties. Today each mainnet vault's owner is one
wallet of the project; moving it to a multisig is planned
([Risks](#risks)). Added after the in-house
reviews of 2026-10-28; vaults deployed before them lack `veto` (the first)
or the override and the instance-bound digest (the second).

### Pauses

A **pause** stops a guardian from acting until its operator resumes it. It
is kept in the guardian's state (`h/`), survives restarts, is listed in
`/api/status` under `paused` and logged at start:

| Scope | Set when | Stops |
|---|---|---|
| `l1` | Argonmine reorganised deeper than `-l1-reorg-pause` blocks | every issuance and every release on that guardian; a follower refuses to sign either |
| `evm/<chain id>` | a deposit already issued (or signed) on that chain left it in a reorganisation, or its id now belongs to another transaction; or an issuance mined before the chain's current vault references an id that vault uses ("Deposit references") | issuance of that chain's deposits; a follower refuses to sign them |

A chain whose RPC providers disagree is not acted on either, but that
clears itself once they agree again and is not a stored pause; it shows in
`chains[].paused`. Every guardian pauses on its own, from its own view; the
leader cannot get signatures from a paused follower, so resuming means
resuming every guardian that paused.

### Stuck items

Anything waiting longer than `-stuck-after` (default 1 hour) is listed in
`/api/status` under `stuck` (oldest first, at most 100) with a reason
stripped of URLs and secrets, and logged at warn level once an hour each:
a pending deposit (`deposit`: waiting for finality and the next batch, or
held with a reason), an issuance broadcast and not mined (`issuance`), a
pending burn (`redemption`: its depth, or why the vault cannot pay it) and
a release transaction not mined (`release`, with its nonce and fee
raises). An issuance that is neither mined nor in the leader's node's
mempool after `-issue-rebroadcast` (default 10 minutes) is broadcast again
(the same signed transaction, at most once per that interval); after 30
minutes its deposits are issued anew under the signing memory, as before.

### Operator commands

An operator acts on a guardian with the same binary, on the same host:

```
argonmine-bridge -network <net> -datadir <dir> -key <guardian key file> -listen <the daemon's -listen> resume [all|l1|evm/<chain id>]
argonmine-bridge ... approve-release <burn txid>
argonmine-bridge ... mark-unclaimable <chain id>#<deposit id> [reason]
argonmine-bridge ... reset-vault <chain id>
```

`reset-vault` drops the chain's candidate vault epoch ("Vault epochs"),
pending or refused, with its reason: the next round builds it again from
the Argonmine tip and the deposit ids on record then, and checks it again.
The epoch in use is never touched. A stopped guardian keeps no candidate
(the next start builds it afresh), so offline the command only says so.

The flags come before the command. The command is signed with the
guardian's **own** L1 key (Schnorr over `TaggedHash("ArgonmineBridge/admin-request/v1",
network ‖ key ‖ path ‖ time ‖ nonce ‖ SHA-256(body))`) and sent to `POST
/admin/<action>` of the daemon on this host; the daemon takes it only from
a loopback address, not through a proxy (no `X-Forwarded-For`,
`Forwarded`, `X-Real-Ip`), within 60 seconds of its clock, once per nonce,
and signed by its own key, and answers with what it did and the pauses
left. When no daemon answers there (it is stopped), the command opens the
state directory (`<datadir>/<network>/bridge-state`) and applies itself
there.

**Runbook: a pause.**

1. Read `/api/status` (`paused`, `chains[].paused`, `stuck`) and the
   guardian's log (lines with `ALERT`).
2. `l1`: compare the Argonmine chain of each guardian's node with the
   explorer and the seed nodes. A reorganisation deeper than expected may be
   an attack on the payout depth: before resuming, check every
   redemption `released` or `confirmed` since the fork height against the
   chain as it is now (`/api/redemptions`; a burn that left the chain and
   was paid is a loss to report). Raise the tiers if blocks are being
   reorganised that deep.
3. `evm/<id>`: the alert names the deposit and its issuance. The deposit's
   dollars are not in the vault on the chain as it is now: the uARG is
   unbacked by that amount. Decide (with the other guardians) how to cover
   it, then resume. Deposits that only moved need nothing: they were
   re-located automatically.
4. Providers disagreeing: find out which one is wrong (compare
   `eth_getBlockByNumber` of the height in the message on each); fix or
   replace it in the chains file. It clears by itself once they agree.
5. Resume **every** guardian that paused (each with its own key):
   `argonmine-bridge ... resume` (or `resume l1`, `resume evm/1`).

### Vault epochs

A burn names a chain, not a vault. Each chain in the configuration has an
`l1_start_height`, the first Argonmine height whose burns its current
vault pays; burns below it are never paid on that chain ("unreleasable",
needs a human). Each guardian also records, per chain, the **vault in
use**, its epoch: when the configuration names a different vault, that
vault starts a new epoch at the guardian's Argonmine height (when it first
saw the change) plus one and pays no older burn, whatever
`l1_start_height` says. The effective start is the larger of the two.
With the set of burns each guardian signed (above), replacing a vault
cannot pay an old burn again, even with the step below forgotten, as long
as the guardians keep their state; with a rebuilt state, `l1_start_height`
is what protects it.

A vault the configuration names is a **candidate** until it passes the
checks below: its epoch is built in memory and recorded only when it
passes, together with the restart of the chain's scan, in one write. Until
then the vault in use on record stays as it was (its start, its first id,
its scan), and nothing of the candidate is scanned, issued or paid: its
deposits wait, and so do the burns naming the chain (the status says why,
in the chain's `error` and `paused` and the redemption's `error`). So a
refused, mistyped or not yet deployed vault never takes the place of the
one in use: going back to that vault in the chains file needs nothing,
and its epoch, and the burns made meanwhile, are paid from it as before
(`TestReviewF_VaultRollbackRefused`). A guardian restarted before its
candidate passes builds it again from the tip of that moment;
`reset-vault` does the same without a restart. Each recorded epoch keeps
the chain's earlier ones in its `history`. Going back to an earlier vault
takes its epoch back as it was only when every vault recorded after it
never passed its check; going back past a vault that was used is a
replacement like any other, and refused for its deposit numbering (the
reason says it was the chain's vault before).

The address must hold a contract (`eth_getCode`), which answers as a vault
(its fees and `firstDepositId()`): an address with no contract answers any
call with `0x`, which is never read as a vault's answer; the chain's
status says there is no contract there and nothing on the chain is
scanned (`TestReviewF_MistypedVault`).

The epoch also records the vault's **deposit numbering**. A deposit is
named by its chain id and deposit id alone, so every vault ever used on a
chain must number its deposits apart: a vault starts at its constructor's
`firstDepositId`. The first time a guardian scans a
vault it reads `firstDepositId()` from every provider (a vault deployed
before the view numbers from 1). When the vault **replaced** another, its
first id must be above every deposit id the guardian has on record for
the chain (its deposits from any vault, the issuances it signed, and the
deposit references of issuances seen on Argonmine, the scan of which
reaches the height the change was seen at first); otherwise it is refused,
nothing of the new vault is scanned, issued or paid, and the chain's
`error` and `paused` say so, with the first id it needs. When it passes,
it is the vault in use and the chain's EVM scan starts again from the new
`start_block` (the old vault's cursor says nothing about the new vault). Deposits of earlier vaults stay on record
as they were; one still pending is held for the guardians (it is not
issued from the new vault's chain view). The vault's first id and the
highest id on record are in the status (`first_deposit_id`,
`max_deposit_id`).

## Configuration

The daemon reads one JSON file, `-chains <file>`:

```json
{"chains":[
  {"name":"Ethereum Sepolia","chain_id":11155111,
   "rpc":"https://ethereum-sepolia-rpc.publicnode.com",
   "explorer":"https://sepolia.etherscan.io","native_symbol":"ETH",
   "rpcs":["https://sepolia.example-provider.io/v2/KEY"],
   "public_rpc":"https://ethereum-sepolia-rpc.publicnode.com",
   "vault":"0x…","start_block":11997800,"confirmations":12,"finality":"finalized",
   "max_gas_price_gwei":300,"min_gas_balance":"0.01",
   "router":"0x…","router_block":12002681,"l1_start_height":0,
   "tokens":[{"symbol":"USDC","address":"0x…","decimals":6,"mint":true,"max_deposit":"50000000000"},
             {"symbol":"USDT","address":"0x…","decimals":6,"mint":true,"min_deposit":"2000000"}]}
]}
```

| Field | Meaning |
|---|---|
| `name` | shown on the page and in the API |
| `chain_id` | the EVM chain id; the daemon refuses an RPC that reports another |
| `rpc` | JSON-RPC URL the daemon uses first; transactions are sent through it. May carry an API key: it is never shown |
| `rpcs` | optional: more providers. The heads, block hashes, deposit logs and receipts are read from all of them and a deposit is acted on only when they agree ("Finality and providers"). `rpc` may be left out when `rpcs` is given (the first is then the sending one); at most 4 in all |
| `public_rpc` | optional: the URL the page hands to browser wallets (`wallet_addEthereumChain`) and the only RPC `/api/status` shows. **No default**: without it the status shows no `rpc` for the chain (it never falls back to `rpc`, which may carry an API key) |
| `finality` | optional: `finalized` or `depth` (defaults by chain id, see "Finality and providers") |
| `max_gas_price_gwei` | optional: the most a release transaction pays per gas, fee raises included (number, decimals allowed; defaults by chain id, see "Release transactions") |
| `min_gas_balance` | optional: the gas key balance, in the native coin (`"0.05"`), below which the status sets `gas_low` (default 0.01; 1 on Polygon) |
| `explorer`, `native_symbol` | for the page and for adding the chain to a wallet (default symbol `ETH`) |
| `vault` | the Vault contract on that chain |
| `start_block` | the vault's deployment block; public RPCs do not serve older logs |
| `confirmations` | a deposit's block must be at least this many blocks below the head to be final (and below the finalized block with `finality: finalized`); default 20 |
| `tokens[].symbol`, `address`, `decimals` | the allowlisted stablecoins; decimals 0 to 18 |
| `tokens[].mint` | the token is a test mock with a public `mint(address,uint256)`: the page offers a "Get 100 test USDC" faucet. Never on mainnet |
| `tokens[].min_deposit` | smallest deposit paid out, in token units, measured before the vault's fee; default one token (`10^decimals`). The vault enforces its own minimum too. A deposit address is swept only at or above it |
| `tokens[].max_deposit` | optional: largest deposit issued without a human, token units, measured before the fee; a larger one is held ("Per-deposit cap"). Default none |
| `tokens[].min_release` | optional: the minimum withdrawal, the smallest payout (gross, token units) a release is sent for; a smaller one is unreleasable ("Minimum withdrawal"). Default `min_deposit` (one token when that is 0); a positive integer |
| `router` | optional: the chain's `DepositRouter`; turns on deposit addresses for the chain. Without it the chain has none |
| `router_block` | the router's deployment block, where the leader starts looking for transfers into deposit addresses (default `start_block`) |
| `l1_start_height` | the first Argonmine height whose burns this chain's vault pays (default 0); burns naming the chain below it are never paid. Set it to the current Argonmine height plus one before replacing a vault ("Replacing a vault") |

Validation: at least one chain with one token, unique chain ids, unique
token addresses per chain, valid addresses, a known `finality`, a positive
gas cap, `max_deposit` at or above `min_deposit`, a positive `min_release`, unknown fields rejected. The
fees are not in the file: the daemon reads them from each vault. The
**first chain's first token** is what a `BRG1` memo pays out, so order the
file accordingly.

## Components

| Piece | What it is |
|---|---|
| Vault contract | The EVM side, one per chain: holds the allowlisted stablecoins, takes the deposit fee and emits `Deposited`, pays `release` less the release fee with threshold guardian signatures bound to the chain, vault and token, remembers paid burn ids, tracks `obligations` per token so the fee surplus is separable from the backing. The owner can rotate guardians, allow and disallow tokens, pause, cap releases, set per-token minimums and withdraw the surplus; every such change waits for the timelock, except a pause, a surplus withdrawal and a handover of ownership ([Risks](#risks)). Its source is verified on Sourcify on all four chains. |
| DepositRouter contract | One per chain, bound to its vault, no owner: `depositAddress(to)`, the CREATE2 address of a `Forwarder` per Argonmine address, and `sweep(token, to)`, which moves what a forwarder holds into the vault as a deposit credited to `to`. |
| Test tokens | Six- and eighteen-decimal ERC-20s anyone can mint, for local chains and test networks, never mainnet. With `"mint": true` the page offers their `mint` as a faucet. |
| Guardian daemon, `argonmine-bridge` | One per guardian: the chains file and its checks; the guardians' MuSig2 key (the uARG issuer and the holder of the ARG fee float); a JSON-RPC client per EVM chain with its own ABI encoding and EIP-155 signing; the LevelDB state; scanning the EVM chains (finality, providers, reorganisations, receipts) and Argonmine (undo records, deposit references, burns, vault epochs); issuance and releases with every guardian's checks; deposit addresses, swap addresses and withdrawals; pauses, stuck items and the operator commands; the bridge page, the public JSON API and the guardian endpoints. Its commands: `keygen`, `gaskey`, `address`, `resume`, `approve-release`, `mark-unclaimable`, `reset-vault`, and run. |

## Operations

Each guardian, once:

```
argonmine-bridge keygen guardian1.json      # prints the L1 public key and the EVM address
```

and the leader, once more, its gas key:

```
argonmine-bridge gaskey gas.json            # prints the address to fund on every chain
```

Deploy a vault on every chain with the guardians' EVM addresses, the
threshold and the timelock delay, allow the tokens, set the fees and the
release limits, finalize the setup and hand the vault to its owner (a
multisig of independent parties is the goal; on mainnet today it is one
wallet of the project). The mainnet vaults were deployed this way with the
real USDC and USDT, a 7-day timelock, 1% fees and no release limits. Write
the chains file. Then on every guardian host:

```
ARGONMINE_BRIDGE_SECRET=<shared, 32+ characters> \
argonmine-bridge -network mainnet -key guardian2.json \
  -guardians <k1,k2,k3> -threshold 2 \
  -chains chains.json -leader-pubkey <k1> \
  -max-issue 0 -max-issue-day 0 \
  -l1-confirmation-tiers 1000:6,10000:30,100000:120 \
  -listen 127.0.0.1:8395
```

Behind a proxy, `-client-ip-header X-Forwarded-For` (or the header the
proxy sets) names the client for the page's per-client limits; the
right-most entry, the one the proxy appended, is used. Without it the TCP
peer is the client, unless the peer is a loopback, private or
carrier-grade NAT (100.64.0.0/10) address, when the right-most
`X-Forwarded-For` entry is. The page and the API carry a
Content-Security-Policy, `X-Frame-Options: DENY`, `nosniff` and
`Referrer-Policy: no-referrer`; the server bounds header, body, answer and
idle times (the answer time above the leader's 30-second wait on a
guardian).

Exactly one guardian runs with `-leader -gas-key gas.json -peers
https://g2…,https://g3…` (and no `-leader-pubkey`); every other one names
the leader's key with `-leader-pubkey` and signs for nothing else. The followers expose only
`/guardian/*` to the leader (firewall or mTLS) and their own node RPC to
themselves.

**Gas, per chain.** The leader's gas key sends every release
transaction, and every sweep of a deposit address on chains with a router,
so it needs a little native gas on **every** configured chain
(ETH on Ethereum, Base and Arbitrum; BNB on BNB Chain; POL on Polygon);
keep it small and top it up when `gas_low` says so. A
release that a node rejects for lack of gas is retried every round and the
reason appears in the chain's `error` in `/api/status` ("no gas: the gas
key 0x… needs BNB on BNB Chain to send release transactions"). The
guardian keys and the followers send nothing and need no gas. The custody address (`argonmine-bridge
address`) needs a small ARG balance for issuance fees; a few ARG lasts
years.

**One chain down does not stop the others.** Every round scans every
chain; a chain whose RPC fails is reported in its status entry and retried
next round, while deposits on the other chains are issued and burns to
them released. Burns naming the failed chain wait.

Flags that matter: `-chains` (the file above), `-leader-pubkey` (on a
follower: the leader's L1 public key, required), `-gas-key` (on the
leader, required: the file from `gaskey`, or any JSON with `evm_private`,
or a hex key), `-max-issue` and `-max-issue-day` (issuance limits in uARG,
defaults 10000 and 100000, 0 = none), `-secret-file` (or
`ARGONMINE_BRIDGE_SECRET`; `-secret` only as a last resort),
`-l1-confirmations` (the floor of every payout depth, default 30),
`-l1-confirmation-tiers` (default `1000:6,10000:30,100000:120`; empty: the
floor for every burn), `-l1-reorg-pause` (default 3, 0 = never),
`-release-timeout` (default 5 min), `-issue-rebroadcast` (default 10 min),
`-stuck-after` (default 1 h), `-issue-interval` (how often the leader
batches, default 2 min), `-scan-interval` (default 10 s), `-asset-symbol`
(`UARG`).

### Replacing a vault

1. Let the deposits already made into the old vault be issued:
   `pending` in `/api/deposits` should be empty for the chain. (So that no
   new one arrives meanwhile, a guardian or the owner can `pause()` the old
   vault first; a pause also holds its payouts, and lifting it takes the
   timelock.)
2. Read `chains[].max_deposit_id` of the chain in `/api/status` of **every**
   guardian and deploy the new vault with a `firstDepositId` above the
   largest of them (the constructor's `firstDepositId`; rounding up
   generously, to the next million say, costs nothing). A
   deposit is named by its chain and id alone, in the guardians' state and
   in the deposit references of every issuance on Argonmine: ids the old
   vault (or any earlier one) used must never come back.
3. Read the current Argonmine height (`l1_height` in `/api/status`) and set
   the chain's `l1_start_height` to that height plus one, next to the new
   `vault` and its `start_block`.
4. Give every guardian the new chains file and restart them, followers
   first. Keep their state directories.

Each guardian then reads the new vault's `firstDepositId()`, checks it is
above every id it has on record for the chain, and scans the new vault
from its `start_block` ("Vault epochs"). A vault that numbers too low is
refused: nothing of it is scanned, issued or paid, the chain's `error`
names the first id it needs, and the fix is to deploy another vault with
it (step 2) and change the chains file again, or to give the guardians
the old chains file back: a refused vault never replaced the old one, so
the old vault is simply in use again, with the burns made meanwhile paid
from it. Burns below `l1_start_height` are
never paid from the new vault; any still unpaid in the old one need a
human and show as unreleasable with the reason. A deposit into the old
vault still pending when the guardians switch is held with the reason
("a deposit into an earlier vault"): its dollars are in the old vault and
it needs the guardians. If step 3 is forgotten, a guardian that keeps its
state still refuses old burns: the new vault's epoch starts at the height
it first saw the change, and it never signs a burn for a second release.
Once the new vault has been used (it passed its check), going back to the
old one is a replacement like any other and is refused for its numbering:
deploy a new vault instead.

With a **rebuilt** state the guardians know no earlier deposit when they
first see the new vault, so steps 2 and 3 are what protect it: the new
vault's ids never meet the old ones, and an issuance mined below
`l1_start_height` whose deposit reference names an id the new vault uses
is never applied to the new vault's deposit (issuance on the chain pauses
with an ALERT instead, "Deposit references").

### State

The daemon keeps its bookkeeping in `<datadir>/<network>/bridge-state`
(LevelDB). The directory carries a **format version** (2 since the
multi-chain bridge, 3 since the fees, 4 since deposit references, the
per-block undo records, the signing memory, the signed burns and the vault
epochs). **Nothing that records money moved or promised is ever wiped**:
a format-3 directory is migrated in place (deposits, redemptions,
issuances, watches and cursors stay; only the custody coin set and the
Argonmine cursor go, and the next round rescans Argonmine from genesis to
rebuild them), and an older or unmarked one is moved under `a/<format>/`
and kept, with the signing memory, the signed burns and the epochs left
active; a redemption found there that was paid or sent stays so. A
directory of a newer format is refused. The second security pass
added only fields to the records (a deposit's `block_hash` and
`log_index`, a redemption's `confirmations` and `send`, an issuance's
signed `hex`) and new key prefixes (`h/` pauses, `v/` EVM block hashes,
`z/` replaced deposits, `y/` release approvals), so it changed no format:
a format-4 directory is used as it is. An EVM cursor from before kept no
block hash; the first round records the hash at the cursor. An empty directory is scanned
from Argonmine's genesis and every EVM chain's `start_block`; this takes a
few minutes and is logged. A rebuild is safe against double issuance: the
rounds scan the EVM chains before Argonmine, and a deposit reference seen
before its deposit is on record is kept and applied when the deposit is
seen. It is not safe against paying an old burn from a replaced vault:
that is what `l1_start_height` is for; and a replaced vault must number
its deposits above the old ones ("Replacing a vault"). A deposit
reference kept from before its height was recorded is applied as before.
The epoch's deposit numbering (`first_id`, `above_id`, `rescan`), its
`history` and a redemption's `tx_payout` are new fields in format 4; an
epoch recorded before them is taken as checked against nothing, with the
vault's first id read on the next scan, and as used (it is never given up
for an earlier vault). An epoch recorded before its check by the build of
2026-10-28 (`rescan` set, no `first_id`; that build was never deployed) is
checked as it is while the configuration still names its vault. A deposit's record carries its
chain id, chain name, token address and symbol, decimals, token amount
and fee (decimal strings) and uARG units; a redemption's record carries
the same for its payout, with the gross, the fee and the net paid.

On mainnet the guardians serve Ethereum, Base, Arbitrum One and BNB Chain
with the real USDC and USDT. Today the project runs all three guardians
([Risks](#risks)). The public testnet bridge, with mock tokens, is retired.

### Public API

Every `/api/*` response carries `Access-Control-Allow-Origin: *`, so the
app and other websites can read it from another origin, except those of
`POST /api/withdrawals`: only the bridge page itself creates withdrawals
(its CORS preflight is refused with 403, "Withdrawals (the bridge
page)"). The status
and the unfiltered lists are computed at most once every 3 seconds and
served from memory; the filtered lists come from indexes kept in memory,
at most 100 values per filter. No error reaches the API with a URL path,
a query string or anything that looks like a credential: URLs are cut to
their scheme and host.

| Endpoint | Returns |
|---|---|
| `GET /api/status` | `network`, `asset`, `asset_symbol`, `custody_address`, `custody_arg`, `guardians`, `threshold`, `l1_confirmations`, `l1_confirmation_tiers` (`up_to` in uARG units and `confirmations`), `l1_reorg_pause`, `l1_height`, `chains`, `reserve_units`, `backing_units`, `surplus_units`, `issued`, `redeemed`, `deposits`, `redemptions`, `pending`, `held` (pending deposits that wait with a reason in their `error`: a limit, a cap, a refused token), `reorged` (deposits whose event left their chain before they were issued), `issue_limit` and `issue_day_limit` (uARG units, 0 = none), `issued_day` (what this guardian signed issuances for in the last 24 hours, each deposit once), `paused` (`scope`, `reason`, `since`: see "Pauses"), `stuck` (`kind`, `id`, `chain_id`, `since`, `reason`: see "Stuck items"), `gas_address` (the leader's gas key), `leader`, `last_error` |
| `GET /api/deposits` | the 200 most recent deposits, newest first, with status: `pending`, `issued`, `confirmed`, `unclaimable`, `reorged`. Add `?to=addr1,addr2` to keep only deposits paid to those addresses (a wallet following its incoming transfers; progress while pending is `chains[].head - block + 1` of `chains[].confirmations`) |
| `GET /api/redemptions` | the 200 most recent redemptions (burns and swap address payments), newest first, with status: `pending`, `released`, `confirmed`, `unreleasable`. Add `?txids=a,b` to keep only those ids (`txid`, or `txid:vout` for a swap payment) (a wallet following its payouts; progress while pending is `l1_height - height + 1` of the redemption's `confirmations`) |
| `GET /api/redemption/{id}` | One redemption by its id (a burn's txid, or `txid:vout` for a payment to a swap address, which also carries `vout` and `address`), or 404 |
| `POST /api/withdrawals` | a new withdrawal (see Withdrawals), from the bridge page's own origin only (no `Access-Control-Allow-Origin`; `OPTIONS` is 403): body `{"chain_id": 11155111, "token": "USDC", "to": "0x…", "amount": "10"}`, sent as `Content-Type: application/json` (anything else is 415); answers as below, or 400 with the reason (unserved chain or token, below 1 uARG or the token's minimum withdrawal, above the largest payout tier or what the vault can pay now), 503 while paused, 429 when one client asks too often or has 30 withdrawals open |
| `GET /api/withdrawal/{memo}` | a withdrawal by its memo (any case, with or without the dash): `code`, `memo`, `address`, `chain_id`, `chain`, `token`, `to`, `amount` (uARG units asked), `expected_paid` (token units after the fee), `status` (`waiting`, `detected`, `processing`, `completed`, `failed`), `received`, `remaining`, `extra`, `token_paid` (delivered), `token_due` (what every payment pays once delivered), `l1_height`, `payments_total`, `dust_count` and `dust_amount` (uARG payments below 1 uARG), `lost_count` and `lost_arg` (payments of ARG or another asset; ARG units) and `payments`: the newest 50 of at least 1 uARG, oldest first, each with `stage`, `state` (`mempool`, `confirming`, `sending`, `paying`, `paid`, `failed`), `memo` (it carried this memo), `seen` (first seen), `confirmations` of `needed`, `token_paid`, `evm_tx`, `wait` (`liquidity`, `paused`, `limit`, `review`, `retry`) and `reason` (`small`: too small to pay out or below the token's minimum withdrawal; `manual`); 404 for an unknown memo |
| `GET /api/swap-address?chain=<id>&token=<symbol>&to=0x…` | the swap address that pays uARG out as that token on that chain to that address: `address`, `chain_id`, `chain`, `token` (symbol), `token_address`, `decimals`, `to` (EIP-55), `release_fee_bps`. Computed, never stored. 400 for a chain or token the bridge does not serve, a malformed or zero recipient, or one of the bridge's own contracts |
| `GET /api/deposit/{chain_id}/{id}` | one deposit; `GET /api/deposit/{0x tx hash}` finds it by its deposit transaction on any chain |
| `POST /api/handoff` | **Legacy, to be removed:** nothing in the current app creates these codes; only the older desktop program still reads them. Body `{"amount","chain_id","token","to"}` (or `{"kind":"send","amount","to"}`): a redemption prepared on a device without keys (the browser app). Returns a `code` of ten characters from `23456789ABCDEFGHJKLMNPQRSTUVWXYZ` (no 0/O or 1/I) in two groups, `K7M2Q-X9RT4`, valid 15 minutes, with the resolved chain and token. Nothing secret: amount, route and payout address only. Per client: a burst of 10 and one every 20 seconds (429), at most 10 pending (429); 5000 pending in all (503) |
| `GET /api/handoff/{code}` | **Legacy, to be removed.** The prepared redemption, for the desktop app to fill its Redeem form from, **once**: the code is gone after the first successful read. Any case, with or without the separator. 404 when unknown, used or expired; per client a burst of 20 and one every 2 seconds (429) |
| `GET /api/memo?to=0x…&chain=<id>&token=0x…` | For burns built by hand (no current page uses it): the `BRG2` memo (hex) for a payout of that token on that chain to that address, with the chain and token echoed; without `chain` and `token`, the `BRG1` memo for the default |
| `GET /api/deposit-address?chain=<id>&to=<address>` | the deposit address of that Argonmine address on that chain, which the leader starts watching (asking again only refreshes the watch): `chain_id`, `chain`, `to` (lowercased), `address` (EIP-55), `router`, `explorer` (the address on the chain's explorer, or empty), `confirmations`, `deposit_fee_bps`, `tokens` (`symbol`, `address`, `decimals`, `min_deposit`), `note`. 400 for an address of another network, an unknown chain or a chain without a router; 503 on a follower, when the router check failed or the chain cannot be reached, or at the watch limit; 429 over the rate limit |
| `GET /api/deposit-address/status?chain=<id>&to=<address>` | its progress: `address`, `watched`, `confirmations`, `head`, `balances` per token (`symbol`, `token`, `decimals`, `amount` at the address in token units: the last balance read plus the transfers seen since, `min_deposit`, `last_tx`), `sweeps` newest first (`token_symbol`, `token`, `decimals`, `amount` gross, `tx`, `status`: `pending` (not on chain yet; `error` says why it is retried), `sent`, `confirmed` or `failed`, `error`, `attempts`, `deposit_id` from the `Swept` event, `created`, `updated`), and `deposits`: the vault deposits made through the router for that address, as in `/api/deposits`, newest first |
| `GET /qr.svg?d=<text>` | a QR code (SVG) of a deposit address (`0x` and 40 hex digits) or of an Argonmine address, for the page |

`chains` is a list, in configuration order:

```json
{"name":"Ethereum Sepolia","chain_id":11155111,"vault":"0x…","rpc":"https://…","explorer":"https://sepolia.etherscan.io",
 "native_symbol":"ETH","confirmations":12,"head":7123456,"deposit_fee_bps":100,"release_fee_bps":100,"error":"",
 "router":"0x…","deposit_addresses":3,"finality":"finalized","final":7123390,"providers":2,
 "vault_paused":false,"gas_balance":"48000000000000000","first_deposit_id":1,"max_deposit_id":412,
 "tokens":[{"symbol":"USDC","address":"0x…","decimals":6,"mint":true,"min_deposit":"1000000","min_release":"1000000",
            "reserve":"1234567890","reserve_units":123456789000,"obligations":"1222222222","surplus":"12345668",
            "max_deposit":"50000000000","available_now":"1234567890","max_release":"10000000000","period_available":"99000000000"}]}
```

`rpc` is the chain's `public_rpc`, absent when none is configured (the
page then has no RPC to hand to a wallet for that chain). `head` is the
chain's head at the last scan and `final` the last final block this
guardian scanned (deposits at or below it can be issued), with `finality`
and the number of `providers`. `paused` (absent when empty) says why
deposits on the chain are not issued now. `vault_paused` is the vault's
own pause, and `vault_setup` (absent when false) says the vault is still
in setup mode (`finalized()` is false: its owner can change its tokens,
fees and limits at once, without the timelock; the guardians also log a
warning once an hour); `gas_balance` (wei) and `gas_low` are the leader's
gas key on that chain. `first_deposit_id` is the first deposit id of the
vault as this guardian checked it (0 until then) and `max_deposit_id` the
highest deposit id it has on record for the chain from any vault ("Vault
epochs"); `min_release` per token is the minimum withdrawal ("Minimum
withdrawal"). Per token, `available_now`, `max_release` and
`period_available` are described in "The vault's limits and pause",
`max_deposit` in "Per-deposit cap", and `error` (absent when empty) says
why the token is refused. `error` is the chain's last scan error, or the
leader's gas problem on that chain (for releases or sweeps, or a gas key
that is a guardian or the owner), or empty. `router` is the chain's
`DepositRouter` when deposit addresses are on there, else empty; when the
router failed its check, `router_error` says why. `deposit_addresses` is
how many deposit addresses the leader watches on the chain (0 on a
follower). `deposit_fee_bps` and `release_fee_bps` are the vault's
fees in basis points (100 is 1 percent). `reserve` is the vault's balance
of the token in token units (a decimal string), `reserve_units` the same
in uARG units; `obligations` is the part of the reserve that backs issued
uARG and `surplus` the rest, the fees collected (both token units, decimal
strings). The top-level `reserve_units`, `backing_units` and
`surplus_units` are the sums over every chain and token in uARG units:
the dollars in the vaults, the dollars backing uARG, the dollars earned.
All of it is read during each scan, so the status never waits on an RPC.

A deposit: `chain_id`, `chain`, `id`, `evm_tx`, `from`, `token`,
`token_symbol`, `decimals`, `token_amount` (token units credited after the
fee, decimal string), `token_fee` (token units the vault kept, decimal
string), `amount` (uARG units, from `token_amount`), `to`, `block`,
`block_hash`, `log_index`, `status`, `issue_tx`, `error`, `attempts`,
`created`, `updated`. A
redemption: `txid`, `height`, `amount` (uARG units burned), `tx_payout`
(what its whole transaction pays out of the bridge, the amount its payout
depth is chosen for), `chain_id`,
`chain`, `token`, `token_symbol`, `decimals`, `token_amount` (the gross in
token units, what `release` is called with), `token_fee` (what the vault
keeps), `token_paid` (`token_amount − token_fee`, what the recipient
gets), `to`, `status`, `evm_tx` (the latest release transaction sent, or the one
that paid), `confirmations` (the Argonmine depth it needs), `send` (the
release transaction as sent: `from`, `nonce`, `type`, `gas_price` or
`fee_cap` and `tip_cap` in wei, `gas`, `data`, `hashes` (every one
broadcast, oldest first), `sent`, `last`, `bumps`), `error`, `attempts`,
`created`, `updated`; a burn with a memo the bridge cannot route has `chain_id` 0,
an empty token and empty fee and paid fields.

## Verified run (2026-10-26)

The guardian daemon's test suite runs a regtest node, three guardians with real MuSig2
and ECDSA keys, and three in-memory chains with vaults that check
signatures, take the fees and keep the obligations like the contract:
chain A holding USDC and USDT with 6 decimals and charging nothing, chain
B holding an 18-decimal USDT and chain C a 6-decimal USDC, both at 1
percent each way. It verifies: nine deposits (good ones in four tokens,
one below the minimum, one of exactly the minimum whose net is below it,
one to an invalid address, one in a token the vault does not accept, one
with more precision than uARG carries) are classified and converted
correctly, with the net credited and the fee recorded (100 USDC on C
becomes 99 uARG with a fee of 1; 3.5 of the 18-decimal USDT becomes
3.465); deposit ids are per chain; nothing is issued before each chain's
confirmation depth and the chains are issued in separate batches when
they confirm at different times; one issuance pays several tokens across
chains with the right asset id and net amounts; followers settle the
deposits from the chain; the status reports every reserve, obligation and
surplus per chain and token, the fee rates, and the dollar sums; a `BRG1`
burn is released as USDC on chain A at no fee, a `BRG2` burn as the
18-decimal USDT on chain B (5 gross, 4.95 paid, 0.05 kept), a `BRG2` burn
of 50 uARG as USDC on chain C (`release` called with 50, 49.5 paid, 0.5
kept, read back from the `Released` event), and a `BRG2` burn naming an
unknown chain is unreleasable and refused by followers; releases are
idempotent; a chain whose RPC fails is reported in its status while a new
deposit on another chain is still issued, and the error clears when it
recovers; and the API shapes above. `TestDecodeDepositLog`,
`TestDecodeReleasedLog` and `TestFeeOf` pin the event layouts and the fee
rounding; `TestParseChains`, `TestUnitConversion` and
`TestStateFormatRebuild` cover the configuration rules (with
`l1_start_height`), the conversions and the state migrations (format 3
migrated in place, older formats archived with the signing memory and the
signed burns left active, a newer format refused).

The security tests (2026-10-27) run the proofs of concept of the
security review against the hardened daemon, each on its own regtest node
with three guardians, and each now fails to exploit.
`TestPoCDuplicateIssuanceWithSecretOnly`: with the shared secret alone
(the old HMAC over the body, or the new preimage signed by a key of the
attacker's) every follower answers 401 and the leader 403; with the
leader's own key a follower refuses a deposit without its confirmations,
both refuse a second issuance of a deposit already signed that spends
other custody coins and accept one that spends a coin of the first; the
user ends with the deposit's uARG exactly once.
`TestPoCRewindSwallowsNextDeposit`: a cursor hash that disagrees with the
node undoes nothing (the guardian's block record agrees), an unreachable
node is an error and not a reorganisation, and the next deposit with the
same address and amount is issued. `TestPoCEVMReorgedDepositStillIssued`:
a deposit whose event left its block waits with the reason, is counted in
`held`, and a follower asked directly refuses it.
`TestPoCRedeployRepaysOldBurn`: a vault replaced with every guardian's
state rebuilt and `l1_start_height` set pays neither of two old burns
(one already paid, one still pending) and the followers refuse both; the
same with the state kept and `l1_start_height` forgotten: the new epoch
refuses the pending burn.

`TestGuardianRequestAuth` refuses a replayed request, one signed by
another guardian's key, one 120 seconds old or ahead, one with the wrong
secret, one replayed on another path or with another body, one without
headers, and any request to the leader; a follower without the leader's
key, with a stranger's or its own as the leader's, or with a short secret
does not start. `TestBurnSignedOnce`: a follower asked twice for the same
release returns the same signature; after the vault is replaced and every
state is rebuilt from format 3 with the epochs and the redemption itself
deleted, the rescan finds the burn again and nobody signs it for the new
vault. `TestSettlementAcrossReorg` makes the regtest node really
reorganise (a two-block branch built on a second chain from the block
before the issuance): the issuance's memo names its deposit, every
guardian settled it from the memo, and after the reorganisation every
guardian has the deposit pending again (issued on the leader), the
custody coinbase and the change of the orphaned block gone and the coin
the issuance spent back; mined again, the issuance settles the deposit
again, and a new deposit of the same address and amount is issued.
`TestIssuanceLimits`: a deposit above `-max-issue` and one past
`-max-issue-day` wait with their reasons while the others are issued in
separate batches, the status reports `held`, the limits and `issued_day`,
and the followers refuse both when asked directly. `TestIssueMemo` pins
the `BRI1` bytes, the 80-byte limit (15 Sepolia references fit, 16 do
not) and what decoding rejects. `TestHandoffCodes`: ten-character codes in
two groups, read once in any spelling, reads and creations rate-limited
per client, at most 10 pending per client. `TestPublicAPIStatusCacheAndErrors`:
the status is served from its cache until it expires, filtered lists come
from the indexes, and no path, query string or credential of an RPC URL
reaches any endpoint. `TestIssuanceFeePolicy` keeps the issuance fee in
step with the relay policy for asset outputs. The whole suite runs in
about three and a half minutes.

The hardening tests (2026-10-27, second pass) cover finality,
reorganisations, providers, payout depth, pauses,
caps, decimals, release transactions and the gas key.
`TestEVMFinality`: with the finalized tag 50 blocks behind, a deposit 10
blocks deep is recorded (with its block hash) and not issued, a follower
asked directly refuses it as not final, and it is issued once finalized.
`TestEVMReorgRelocatesAndReorgs`: below the final block one deposit's
transaction leaves the chain and another's moves three blocks; on every
guardian the first is `reorged` and the second re-located, then issued
from its new block. `TestEVMReorgAfterIssueAlerts`: an issued deposit
leaves the chain: every guardian raises the alert and pauses issuance on
the chain, a new deposit waits and a follower refuses it, and both are
issued after every operator resumes. `TestProviderDisagreement`: a deposit
only one of two providers reports, then a block they see differently,
pause the chain (status and follower refusal) until they agree; then it
is issued. `TestPayoutDepthTiers`: with tiers `50:3,500:6` a 396 uARG burn
waits 6 blocks on the leader and on a follower, and a 990 uARG burn is
held and refused until approved on every guardian. `TestL1ReorgPause`: a
three-block Argonmine reorganisation with `-l1-reorg-pause 2` pauses every
guardian, a release waits and a follower refuses to sign, a resume signed
by a stranger, by another guardian, sent through a proxy or replayed is
refused, and after every operator resumes the burn is paid.
`TestTokenDecimalsMismatch`: USDT answering 8 decimals against 6 in the
file is refused (deposit held, follower refusal, status error) while USDC
works; with the file fixed and the guardians restarted the deposit is
recomputed and issued. `TestPerDepositCap`: a deposit above `max_deposit`
is held and refused, a smaller one issued, and the held one marked
unclaimable on every guardian. `TestReleaseRebroadcast`: a release not
mined after 5 minutes is sent again with the same nonce and fees 12.5
percent up; the first transaction lands late, the burn is paid once and
confirmed from it, and nothing more is sent; a release whose nonce went to
another transaction is sent with the next nonce; one whose raise would
pass `max_gas_price_gwei` waits and is listed as stuck.
`TestReleaseLimitsAndPause`: a burn over the vault's 24-hour room waits
(status `available_now`), a `LimitExceeded` at sending waits, a paused
vault holds payouts (`vault_paused`, `available_now` "0"). `TestGasKey`:
releases go from the gas key only, its balance and `gas_low` show, a
client sending from the guardian's own key is refused at start and a gas
key that is a vault guardian sends nothing. `TestPublicStatusRPC`: neither
`rpc` nor `rpcs` (with API keys) reach the status; `public_rpc` does, and
a chain without it has no `rpc` field. `TestIssuanceRebroadcast`: an
issuance missing from the node's mempool is broadcast again after 10
minutes and issued anew after 30. `TestAdminOffline` and
`TestHardeningConfig` pin the offline operator commands, the new
configuration rules, the tiers, the fee raises and the release call.

`TestAnvilHardenedVault` starts its own anvil (skipped without Foundry),
deploys a test USDC and the hardened vault contract
(three guardians, a one-day delay, fees, a 300 USDC window, setup
finalized) and runs three guardians over JSON-RPC with `finality:
finalized`: a deposit final on one branch is `reorged` after
`evm_revert` to another, and the next deposit takes over its id; four
deposits are issued; a release is sent from the gas key (no guardian key
ever sends) and pays 98.01 USDC; with automine off a release not mined in
time is replaced by one with the same nonce and fees at least 12.5 percent
up, the original is put back and mined, and the burn is confirmed from it
with one payment; a 198 USDC burn waits for the window and is paid after
a day; a burn on a paused vault waits until the pause is lifted through
the timelock.

`TestDepositAddresses` runs the same regtest node and three guardians
against a chain with a router and one without: the address is the CREATE2
formula over the router, `keccak256(to)` and the code hash, served in
EIP-55, the same for any spelling of `to` and registered once; a foreign
address, an unknown chain, a chain without a router and a follower are
refused; a new watch reads its balances once; 0.4 USDC is seen at once,
not touched before its confirmations, and then waits below the minimum
without being read again; a top-up to 1.2 is swept once, and while that
sweep is held unmined a further 0.5 arrives and the daemon is restarted,
yet no second sweep is sent; mined, it took all 1.7, the router's deposit
is issued and confirmed as 1.683 uARG and the followers agree; a USDT
balance below the vault's own minimum waits without error and goes with
the next transfer; a leader without gas shows it in the chain's error and
sweeps once it has gas; unused watches expire after 30 days and used ones
stay; the watch limit and the per-client rate limit answer 503 and 429;
and the chain without a router is never asked anything of a router.
`TestDepositAddressRouterChecks` turns the feature off for a router bound
to another vault or whose view disagrees with the formula.
`TestDepositAddressFormula` pins three addresses read from the deployed
routers on anvil and Sepolia with `cast`, and `TestRouterAnvil` (with
`BRIDGE_ANVIL_RPC`, `BRIDGE_ANVIL_ROUTER`, `BRIDGE_ANVIL_VAULT` and
optionally `BRIDGE_ANVIL_TOKEN` set) reads a live router through the
JSON-RPC client.

Against a local anvil (2026-10-26) a leader built from the current code, on its
own port and data directory, registered an address, saw 0.4 USDC wait
below the minimum, swept 1.2 USDC after a top-up (266,241 gas, the
forwarder's deployment included), read deposit #3 from the `Swept` event,
scanned the router's `Deposited` as pending, and kept the watch and the
sweep across a restart; the address matched `cast call depositAddress`
exactly.

The vault contract has its own Foundry test suite, run under solc 0.8.26.

The page has been checked headless in Edge at 390 px and 1440 px with a
fake status of three chains (one of them unreachable): no horizontal
overflow, selectors, faucets, memo and tracking working. The deposit
address flow was checked the same way at 360, 390 and 1440 px against the
anvil leader (entry button, wallet-modal button, form, address with QR,
below the minimum, forwarding, forwarded) and with mocked answers for the
issued and delayed states: no horizontal overflow in any of them.

## What this is not

Not trustless. Not audited by an outside firm. Not a market: uARG can be
withdrawn one to one for a dollar stablecoin, less the bridge fee, nothing
else. Volatile deposits (ETH, BNB, BTC)
are not accepted by the vaults; they may come later as wrapped assets or
by conversion to a stablecoin at entry.
The bridge does not price USDC against USDT: both are a dollar to it, and
a user who deposits one may withdraw the other.
