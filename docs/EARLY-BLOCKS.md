# Early blocks

For 2,000 blocks the chain mints ARG for anyone who asks: 20 ARG for
1 uARG, up to 500 ARG a block, 1,000,000 in all. Then ARG comes only
from mining.

| Item | Mainnet |
|---|---|
| Open | block 2,925 to block 4,924, 2,000 blocks |
| Rate | 20 ARG for 1 uARG, for everyone |
| Most per request | 25 uARG, which is 500 ARG |
| Most per block | 500 ARG, over all requests in the block |
| Most in all | 1,000,000 ARG |
| Paid to | the treasury, `arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2`, in uARG |
| Paid out | from the coinbase of the block that includes the request, spendable 100 blocks later |

## How it works

Send uARG to the treasury address above with the memo `EARLY1`, from
the address that should receive the ARG, or use the app at
[app.argonmine.com/early](https://app.argonmine.com/early). The next
block that takes the request pays 20 ARG per uARG from its coinbase,
spendable 100 blocks later. A request is at most 25 uARG and a block
mints at most 500 ARG; the uARG goes to the treasury.

## Why it is built this way

- Nothing is set aside. ARG is minted only when asked for, block by
  block, from the coinbase, like a mining reward.
- One rate for everyone, written in the rules.
- The uARG is paid on the chain, to an address anyone can watch.
- It ends. Outside blocks 2,925 to 4,924 the memo is refused, so no uARG
  leaves your wallet for coins no block can make.

## Node runners: upgrade before block 2,925

Early blocks change the consensus rules. An older node rejects a block
whose coinbase pays more than the subsidy and the fees, so it stops
following the chain at block 2,925. Every node must run release 1.2.0
before then ([Run a node, Upgrades](docs/NODE.md#upgrades)). Pool and
browser miners need nothing. The rule is [SPEC §3.3.2](docs/SPEC.md).

## For developers

The explorer serves these endpoints (GET, JSON, CORS open); the page
`/early` shows the same.

| Endpoint | Answer |
|---|---|
| `/api/early` | `window` (whether this network has early blocks), `start`, `end`, `height`, `status` (`none`, `upcoming`, `open`, `closed`), `perBlock`, `rate`, `maxPaid`, `asset`, `address`, `minted`, `requests`, `cap`, `remaining`, `pending` (still in the mempool), `maturity`, `recent` (buyer, paid, minted, height, txid, time); amounts in units, with a `…Coin` twin in ARG or uARG |
| `/api/early/{address}` | the same, with the address's own requests, confirmed and pending: paid uARG, ARG, block, maturity |

A node's `getblocktemplate` lists the buyers the next coinbase must pay
under `mint` ([Run a node, RPC](docs/NODE.md#rpc)). Testnet and regtest
have no early blocks unless node and explorer get the same
`-mint start:end:perBlockARG:rateARG:asset:key`.

A node refuses a request in these cases, with the answer the code
prints; the app refuses each before it is sent.

| Case | Node's answer |
|---|---|
| Before block 2,925, after block 4,924, or on a network without early blocks | `mint request outside the mint window` |
| No uARG to the treasury in the transaction | `mint request pays nothing to the mint key` |
| More than 25 uARG (500 ARG) in one request | `mint request exceeds the per-block mint` |
| The first input does not spend an ordinary key output | `mint request's first input is not a key` |
| A block whose requests exceed 500 ARG | `mint requests exceed the per-block mint` (the block is invalid) |
| A block whose coinbase pays a buyer less than its due | `coinbase pays a buyer less than its mint` (the block is invalid) |
