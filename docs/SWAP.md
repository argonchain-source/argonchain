# Native swaps: ARG, wARG and uARG

The swap service trades the three coins of the Argonmine chain with
wallets: ARG ↔ uARG, wARG ↔ uARG and ARG ↔ wARG. It is a **maker**: it holds
coins, quotes a price and is the other side of every trade. The service is
`argonmine-swap`; the app at
[app.argonmine.com](https://app.argonmine.com) calls it from its **Swap**
screen, through `/swap/api/` on the app's own address. The fees are also
listed in [Fees](docs/FEES.md).

The service runs on mainnet. It starts trading once it holds mined ARG
that has unlocked, about a day after launch: mined ARG can be spent only
100 blocks after the block that paid it. Until then, swaps are refused.

## How a swap works

Every swap is **one transaction that both sides sign**. Neither side sends
first, so neither side can keep what the other sent.

1. The wallet picks coins of what it pays and asks for a quote
   (`POST /api/quote`): the pair, the amount, the coins, its receive and
   change addresses.
2. The maker checks the coins on the chain (unspent, confirmed, mature, of
   the right coin), prices the swap and builds the whole transaction: the
   wallet's coins and its own as inputs; what the wallet gets, the wallet's
   change, what the maker gets and the maker's change as outputs; the
   network fee paid from the maker's ARG. It returns the transaction
   unsigned, with every spent output, and holds its own coins for the
   quote's lifetime (30 seconds).
3. The wallet checks the transaction against **its own records**:
   - the inputs it is asked to sign are exactly the coins it offered,
     described exactly as it recorded them;
   - it gives exactly the amount it asked to swap (for ARG, at most a dust
     change more: below 546 units a change cannot be paid back);
   - it receives at least the quoted amount, in the coin it asked for,
     identified by asset id;
   - nothing else of its leaves, and the transaction is valid in the next
     block, never later.
   Only then, and with the wallet password, does it sign its inputs.
4. The maker checks those signatures, signs its own inputs (and the wARG
   issuance, when it issues), broadcasts, and returns the transaction id.
   The wallet checks it is the id of the transaction it signed.

Every signature covers every input, amount and output of the transaction
(SPEC 6.2), so a wallet's signatures are worth nothing for any other
transaction. The maker signs last, after the quote's deadline has been
checked, so a wallet cannot hold a quote open and use it only if the price
moves its way. A maker holding the wallet's signatures without broadcasting
gains nothing: the transaction pays the wallet what it agreed to, and the
wallet can cancel it by spending the same coins.

## Prices and fees

| Pair | Amount out |
|---|---|
| ARG or wARG → uARG | in × price × (1 − spread) |
| uARG → ARG or wARG | in ÷ price × (1 − spread) |
| ARG ↔ wARG | in × (1 − wrap fee) |

The price (uARG per ARG), the spread and the wrap fee are set by the
operator (`-price`, `-spread-bps`, `-wrap-fee-bps`). On mainnet the price
is 0.10 uARG per ARG, with a spread of 1% each way and no wrap fee. The
spread goes to the project treasury: the maker books each swap's 1% and
pays it to the treasury once the swap has 6 confirmations and a coin's
earned fees reach 1 ARG or 1 uARG (`-treasury`, default the chain's
treasury address; `-fee-sweep-min`). The price is a setting, not a market. The network fee is paid by the maker, so
a wallet can swap without holding ARG.

## wARG

wARG is ARG held by the maker, one for one. The maker issues wARG for ARG it
receives (the issuance is part of the swap transaction, signed by the
maker's issuer key) and burns the wARG it receives back (it pays the wARG
nowhere, so it is destroyed). Its asset id is
`AssetID(issuer key, "WARG")`; wallets and the explorer recompute it from
the issuer key the maker publishes, and any other asset called WARG shows
as unverified.

The maker keeps a ledger of the wARG it issued and burned, transaction by
transaction (entries whose transaction never confirms are dropped after
half an hour), and **never pays out ARG that backs wARG in circulation**: a
quote that would leave its spendable ARG below the wARG supply is refused.
Unwrapping is always possible. `GET /api/info` shows the wARG supply and the
maker's ARG; both can be checked on the explorer.

## Trust

The maker is one operator. Wallets do not trust it with their coins during
a swap (above), but wARG holders trust it to keep the ARG behind their
wARG, and every user trusts its price. The operator holds the swap
reserve. Moving most of it to cold storage, with a small balance kept for
trading, is planned; the reserve can later move to the bridge guardians'
federation.

## API

| Endpoint | |
|---|---|
| `GET /api/info` | `network`, `maker` (its address), `issuer` (x-only key, hex), `uarg`, `warg`, `price`, `spread_bps`, `wrap_fee_bps`, `pairs` (`from`, `to`, `rate` after fees, `min`, `max`, `available`: of `to` it can pay now), `quote_ttl`, `warg_supply`, `arg_reserve`, `height`. CORS `*`. |
| `POST /api/quote` | `{from, to, amount, inputs:[{txid,index}], receive, change}` → `{id, expires, from, to, amount_in, amount_out, rate, tx (hex, witnesses empty), spent:[{amount,type,payload,asset}], user_inputs}`. 400 for coins that are not the wallet's unspent confirmed mature coins of `from`, addresses of another network, swap addresses or the maker's own; 409 when the maker cannot pay; 429 over the rate limit or with 3 open quotes from one client. |
| `POST /api/swap` | `{quote_id, sigs:[{index, sig}]}` → `{txid}`. 404 for an unknown or used quote (one attempt per quote), 410 when expired, 400 when a signature does not verify or the network refuses the transaction. |

Coins are `ARG`, `UARG`, `WARG`; amounts are in units (10⁻⁸).

## Running it

```
argonmine-swap -network mainnet -wallet swap.wallet init     # prints the backup phrase and the address
argonmine-swap -network mainnet -wallet swap.wallet \
  -node-rpc http://127.0.0.1:9332 -node-token … \
  -bridge-url https://bridge… -price 0.10 -listen 127.0.0.1:8410
```

Pass `-network mainnet` every time: without it, `argonmine-swap` starts on
the retired testnet.

The password comes from `SWAP_PASSWORD` or `-password-file`. Fund the
maker's address with ARG (for payouts, the fee and the wARG reserve) and
uARG (for payouts). The maker spends only confirmed coins, so it keeps its
ARG and its uARG in several coins (`-split-arg 100`, `-split-uarg 10` by
default; 0 turns one off): swaps in the same block then pay from
different coins instead of waiting for each other's change to confirm.
Wallets likewise offer only confirmed coins in a quote request.
