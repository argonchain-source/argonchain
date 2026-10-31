# Mining

Mining is how new ARG is created and how transactions get confirmed. This
page explains what mining pays, when you can spend it, how long a block
takes, and how invite links work. The last sections cover the proof of work
and the work protocol in detail, for developers.

## What is mining?

About every 10 minutes the network adds a new block of transactions. To add
one, a computer has to find a number that gives the block a hash below a
target. There is no shortcut: miners try number after number until one
works. Each try is called an attempt, or a hash, and the number of
attempts per second is your **hashrate**.

The first miner to find a valid block is paid the **block reward** plus the
fees of the transactions in the block. From block 300, 1% of the block
reward goes to the project treasury and the miner keeps the rest. The
reward is paid to that miner's own address inside the block itself.
Nobody collects it first and passes it on.

Argonmine uses Argon2id for its attempts. Each attempt needs 8 MiB of
memory. Memory speed is something phones, laptops and desktop computers
already have, so specialised mining machines are expected to gain much less
than they do on coins such as Bitcoin.

## How do I mine in the browser?

1. Open [app.argonmine.com](https://app.argonmine.com).
2. Create a wallet, or log in with your 24 words.
3. Press **Start mining** on the Mine page.

Your browser asks the explorer for a block to work on, makes attempts in
the background, and sends back what it finds. The explorer passes blocks
to the network. The Mine page offers two ways to be paid:

- **Pool** (the default). The block pays the pool, and the pool pays every
  miner in proportion to the work they did, a little at a time. See [Pool
  mining](#pool-mining).
- **Solo.** The block pays your address directly, the whole reward at
  once, when you find one. The explorer never holds your coins. Before
  mining a template, the app checks that it really pays your address.

Solo suits a server or a desktop with many cores; the pool suits a phone
or a laptop, which would otherwise wait months for a block.

Keep these points in mind:

- **Keep the tab open.** Closing the tab stops mining. Reloading pauses it
  for a moment, and it then resumes by itself.
- **One tab mines on one processor core.** Mining keeps that core busy, so
  the device runs warm. Plug in a laptop or phone when you can.
- **Phones slow down background tabs.** Keep the app in the foreground. In
  Settings, **Keep the screen on** stops the phone from sleeping while it
  mines.
- **Start when the app opens** in Settings starts mining on every visit.
- **Several devices can mine to the same address.** Each one fetches its own
  work, and every block it finds pays the same wallet.

The [Getting started](docs/GETTING-STARTED.md) guide explains every figure
on the Mine page. [Ways to mine](docs/WAYS-TO-MINE.md) takes a phone, a
computer and a rented server step by step.

## What do I earn?

A block pays its miner the **block subsidy** for its height plus every
**transaction fee** in the block. From block 300, 1% of the subsidy goes to
the project treasury, not to the miner. Fees are paid in ARG, also for
transfers of uARG and wARG, so you are always paid in ARG.

| Period | Block heights | Subsidy per block | To the treasury (from block 300) | To the miner, plus fees | Roughly |
|---|---|---|---|---|---|
| 1 | 0 to 209,999 | 50 ARG | 0.5 ARG | 49.5 ARG (50 ARG for blocks 1 to 299) | years 0 to 4 |
| 2 | 210,000 to 419,999 | 25 ARG | 0.25 ARG | 24.75 ARG | years 4 to 8 |
| 3 | 420,000 to 629,999 | 12.5 ARG | 0.125 ARG | 12.375 ARG | years 8 to 12 |
| 4 | 630,000 to 839,999 | 6.25 ARG | 0.0625 ARG | 6.1875 ARG | years 12 to 16 |
| 5 | 840,000 to 1,049,999 | 3.125 ARG | 0.03125 ARG | 3.09375 ARG | years 16 to 20 |

The subsidy keeps halving every 210,000 blocks and reaches zero after 64
halvings. All rewards together, the treasury's share included, add up to
20,999,999.9769 ARG.

**From block 300, 1% of each block subsidy goes to the project treasury.**
This is a consensus rule: a block that leaves the treasury out is invalid.
Transaction fees are not touched; they stay with the miner, as in Bitcoin.
Blocks 1 to 299 paid the miner in full. The treasury address is public:
[`arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2`](https://explorer.argonmine.com/address/arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2).
Its key is held offline by the project. The only other share of your
blocks is a referral share that you agree to yourself (see
[Invite links](#invite-links-and-referral-shares) below). All fees in the
system are listed in [Fees](docs/FEES.md).

## When can I spend a reward?

A mining reward can be spent **100 blocks** after the block that paid it.
At 10 minutes per block that is about 17 hours. Until then the app shows
the reward as **Maturing**, with the moment each one unlocks.

The lock exists because two miners sometimes find a block at the same
height. The network keeps one and drops the other, and the dropped block's
reward disappears. Waiting 100 blocks makes sure a reward is final before
anyone can spend it.

## Mining at launch

Mainnet started on 28 October 2026, 12:00 UTC at the easiest difficulty the rules
allow: 2^18 (262,144) attempts per block on average. The difficulty cannot
go lower. Blocks arrive every 10 minutes only once the whole network makes
about 440 attempts per second. While it makes fewer, blocks come more
slowly. What that means in the first days:

- **A block takes 262,144 attempts on average.** A browser tab making 10
  attempts per second needs about 7 hours of attempts for that, and a node
  making 200 per second about 22 minutes. Everyone mining competes for the
  same blocks, so your share is what counts (next section).
- **Rewards take longer to unlock.** The lock is 100 blocks, not 17 hours.
  With slower blocks, it takes longer.
- **Swap and the bridge start about a day after launch.** Each needs mined
  ARG that has unlocked before it can work.

As more people mine, the difficulty rises and blocks settle at about one
every 10 minutes.

## Pool mining

In the pool, miners combine their work and share every block the pool
finds. A phone that is one ten-thousandth of the network earns one
ten-thousandth of each pool block, every time, instead of one whole block
every few months.

How it works:

1. Your miner asks the pool for work: the same block template a solo miner
   gets, except that its reward pays the pool's address. The app checks
   that before mining, exactly as it checks a solo template, against the
   pool address the explorer published.
2. Along with the block target, the job carries a much easier **share
   target**, tuned per miner so that it is met about every 15 seconds. A
   hash that meets it is a **share**: proof of the attempts it took, which
   the pool checks one by one. The pool keeps a window of the most recent
   shares worth two blocks of work at the current difficulty; this is the
   pay-per-last-N-shares scheme (PPLNS) most pools use.
3. A hash that also meets the block target is a block. It pays the pool,
   and the window at that moment is the block's **round**. The window is
   not emptied by a block, so a share is paid by every block found while
   it is in the window, and a miner who stops keeps earning from the
   blocks that follow for a while.
4. When that block has 100 confirmations, its reward less the pool fee is
   split over the round's shares, by weight, and credited to each miner's
   balance. A balance of 0.1 ARG or more is paid out automatically, many
   miners in one transaction, usually within a minute of maturing.

Finding a block is random. The pool page shows its expected wait for the
next block, a block's work divided by the pool's hashrate, and how far
the current round is past it. A small pool waits hours between blocks;
the fix for that is more miners in it, not a different payout rule.

What you should know:

- **The fee is 1%** of each block the pool finds. The 1% of the subsidy to
  the treasury applies first, as in every block.
- **The pool holds your coins briefly.** Between a block maturing and the
  payout, your part sits in the pool wallet. The pool never holds more
  than what is on its way to miners: everything matured is paid at the
  next check, every 30 seconds.
- **Everything is published.** The [pool page](https://explorer.argonmine.com/pool)
  shows the hashrate, every block, every payout and your own balance.
  The complete ledger of shares, balances and payouts is at
  `/api/pool/ledger`, and every payout is a transaction on the chain from
  the pool address `arg1q6f8efzllg7fmyuva7k2qhju8t6kaeuk737ypuanhz8faakq32tysz67vuc`.
- **Orphans pay nothing.** A pool block that ends up off the main chain
  pays nothing; the shares are still in the window for the next block.
- **Referral shares apply to solo mining only.** A pool block has no
  output for a referrer.
- **The pool is run by the project**, on the explorer. Solo mining does not
  depend on it in any way, and anyone can run a pool: the code is in the
  [source repository](https://github.com/argonchain-source/argonchain).

Switch between Pool and Solo on the Mine page or in Settings at any time;
mining restarts in the other mode. Node miners (`argonmined -mine`) always
mine solo. To mine in the pool from a server without a browser, the
release archives carry `argonmine-miner`, the browser miner as a program:

```
argonmine-miner -address <your address> -threads 4
```

It needs no node and no chain data; add `-solo` to mine whole blocks to
your address through the explorer instead.

## How long until I find a block?

This section is about solo mining. In the pool there is no wait for a
block of your own: you are paid for every pool block, in proportion to
your work. The Mine page shows **Estimated per day** instead.

Your chance of finding the next block is your share of the network's
hashrate. When the difficulty has settled, the average wait is:

```
average wait = 10 minutes × (network hashrate, including you) ÷ (your hashrate)
```

| Your hashrate | Network hashrate, including you | Your share | Average wait |
|---|---|---|---|
| 10 H/s | 500 H/s | 2% | 8.3 hours |
| 10 H/s | 5,000 H/s | 0.2% | 3.5 days |
| 25 H/s | 500 H/s | 5% | 3.3 hours |
| 25 H/s | 5,000 H/s | 0.5% | 1.4 days |
| 200 H/s | 5,000 H/s | 4% | 4.2 hours |
| 200 H/s | 50,000 H/s | 0.4% | 1.7 days |

The Mine page shows your hashrate, the network's, your share and the
**Expected time to a block**, calculated from live figures.

Finding a block is random. The average is not a schedule. Within one
average wait, your chance of having found at least one block is about 63%;
within three average waits it is about 95%. Two blocks in one hour and then
nothing for several days is normal.

### What hashrate should I expect?

As a rough guide, measured with the node's own mining code on a recent
16-core desktop processor: about 25 attempts per second on one core, and
about 200 on 15 cores. Your results depend on your processor and memory.

- **A browser tab** mines on as many cores as you choose under Settings,
  Mining, **Processor cores** (4 on a phone and half the cores on a computer
  by default). WebAssembly, which the browser runs, is slower than the
  node's native code, so each core makes fewer attempts than a native one.
- **A phone** makes fewer attempts than a laptop, and slows down further
  when it gets warm or its screen turns off. Treat phone mining as a way to
  take part, not as an income. On a busy network a phone may wait weeks for
  a block.
- **A node** mines with several cores. See
  [Mining with a node](#mining-with-a-node).

The explorer's hashrate chart shows the whole network over time.

### The miner count on the website

The website shows how many miners are at work and in which countries. A
miner is a payout address. When an address asks the mining server for work,
the server notes the time and the country of the connection. It keeps the
country and the time, never the IP address, and stores addresses only as
hashes. Miners on their own node
never ask for work; they count from the blocks they find, without a
country. A VPN shows the VPN's country.

The countries come from the regional internet registries' address data,
published by the Number Resource Organization (https://www.nro.net) under
Creative Commons Attribution 4.0, as compiled by ip-location-db.

## Invite links and referral shares

Every wallet has an invite link: the app's address with your own address
added, `https://app.argonmine.com/?ref=<your address>`. You find it on the
Dashboard under **Invite friends**, with Share and Copy buttons.

When someone opens your link, the app shows a banner naming you and the
share. Nothing happens unless they press **Accept**. **Ignore** drops the
offer.

After they accept:

- **5% of each block they find goes to you.** The share applies to what
  the block pays after the treasury's 1% of the subsidy (from block 300):
  the rest of the subsidy plus the transaction fees. The other 95% goes to
  them. At today's subsidy, a block without fees pays 0.5 ARG to the
  treasury, 2.475 ARG to you and 47.025 ARG to the miner.
- **It is paid inside the block.** The block's reward transaction has an
  output to the miner and one to you, and from block 300 one to the
  treasury. No new coins are created for referrals, and nobody holds the
  money in between.
- **It comes out of the miner's reward**, not out of anyone else's.
- **The same 100-block lock applies** to your share as to any mining
  reward.
- **The miner can remove you at any time** on their Dashboard. The change
  applies from the next block they work on. Accepting a different invite
  replaces you.

The share is set by the operator of the work server (the explorer, flag
`-referral-share`, 5 by default, at most 50, 0 turns referrals off). It
applies only to browser miners who get their work from the explorer. A
node that mines on its own builds its own blocks and pays no referral
share.

The Dashboard shows your referral earnings, and the explorer marks
referral rewards in an address's history.

## Mining with a node

If you run a node (see [Run a node](docs/NODE.md); install a release or
[build it from source](docs/NODE.md#build-from-source)), it can mine with
as many processor cores as you give it. It builds blocks from its own copy of
the chain and needs no explorer. Use the address from the app's
**Receive** screen, and the rewards appear in the app:

```
argonmined -mine arg1… -threads 4
```

`-threads` defaults to the number of cores minus one. Each thread uses
about 8 MiB of memory while it mines. Setup, monitoring and every flag are
in [Run a node](docs/NODE.md#mining-with-a-node).

A node that mines must run release 1.1.0 before block 300. Blocks from an
older node leave the treasury out and are rejected (see
[Run a node, Upgrades](docs/NODE.md#upgrades)).

## How the proof of work works

This section and the next are for developers.

A block is valid when the Argon2id hash of its 120-byte header, read as a
256-bit little-endian number, is at most the target encoded in the header's
`bits` field. The Argon2id parameters are consensus rules:

| Parameter | Value |
|---|---|
| Function | Argon2id |
| Memory | 8 MiB (8192 KiB) |
| Passes | 1 |
| Lanes | 1 |
| Output | 32 bytes |
| Salt | the ASCII bytes `Argonmine/pow/v1` |

The block's identifier is a separate tagged SHA-256 hash of the header. The
Argon2id hash is only compared with the target. The header's `nonce` and
`time` fields are both 64 bits, so a miner never runs out of nonces on one
template and there is no year-2106 limit.

Checking a block's proof of work costs one Argon2id attempt. Nodes check
every header before they download the block behind it.

### Difficulty

The target is recalculated for every block with ASERT (absolute scheduled
exponentially rising targets). Block *h* is due 600 × *h* seconds after the
genesis block. For each new block the node measures how far the chain is
ahead of or behind that schedule and sets:

```
target = anchor_target × 2^((time behind schedule) / 86400)
```

The anchor is the genesis block, and 86,400 seconds (one day) is the
half-life. A chain one day behind schedule gets a target twice as easy; one
day ahead, twice as hard. Because the reference point is fixed at genesis,
rounding errors do not add up and false timestamps gain an attacker
nothing lasting. The exact integer arithmetic is in
[Specification §5](docs/SPEC.md).

The target can never be easier than the network's floor:

| Network | Easiest target (`bits`) | Average attempts per block at the floor |
|---|---|---|
| mainnet | `0x1E400000` | 262,144 (2^18) |
| regtest | `0x207FFFFF`, fixed | about 2 |

On mainnet the floor means that blocks arrive every 10 minutes only once
the whole network makes about 440 attempts per second (262,144 ÷ 600).
Below that, blocks come more slowly. This stops one machine from filling
the first blocks after launch in seconds. (The retired public testnet had
a floor of `0x1F7FFFFF`, 512 attempts.)

## The work protocol

The browser app mines through the explorer, which acts as a **work
server**. It builds block templates from its node and passes solved blocks
back to it. For solo work it holds no coins and has no wallet. The protocol
is two HTTP calls, and the browser miner is its reference client. The pool
speaks the same protocol under the `/pool` prefix (see [Pool work](#pool-work)).

### Get work

```
GET /api/work?address=<your address>[&ref=<inviter's address>]
```

```json
{
  "job_id": "<32 hex characters>",
  "header_prefix": "<112 bytes, hex>",
  "target": "<64 hex characters, a big-endian number>",
  "height": 12345,
  "network": "mainnet",
  "algorithm": "argon2id",
  "coinbase": "<the block's reward transaction, hex>",
  "merkle_branch": ["<hex>", "…"],
  "witness_branch": ["<hex>", "…"],
  "referrer": "<inviter's address>",
  "referral_pct": 5
}
```

`header_prefix` is the header without its final 8-byte nonce. The reward
transaction (`coinbase`) pays `address` the block value, or, with `ref`,
the value minus the referral share and the share to `ref`. From block 300
it also pays the treasury 1% of the subsidy, as its last output, and the
block value shared by `address` and `ref` is what remains after that.
`referrer` and `referral_pct` appear only when a share applies. Hashing
`coinbase` up `merkle_branch` and `witness_branch` gives the two roots in
the header, so a miner can check that the job pays it before mining. The
browser miner refuses a job that fails this check.

### Mine

Append a little-endian 64-bit nonce to the prefix, hash the 120 bytes with
Argon2id and the parameters above, read the 32-byte result as a
little-endian number and compare it with `target`. Repeat with the next
nonce.

### Submit

```
POST /api/work/submit
Content-Type: application/json

{"job_id": "…", "nonce": 123456789, "address": "<your address>"}
```

```json
{"accepted": true, "stale": false, "hash": "<block hash>", "reason": ""}
```

The server checks the proof once, hands the block to its node and answers.
`stale` means another block has already been found at that height: fetch
new work. A refused block comes with a `reason`.

### Pool work

```
GET  /pool/api/work?address=<your address>
POST /pool/api/work/submit   {"job_id": "…", "nonce": 123, "address": "<your address>"}
```

A pool job is the same object as a solo job, with two differences: its
`coinbase` pays the pool's address (from block 300 followed by the
treasury's output, nothing else), and `target` is your **share target**
while `block_target` carries the real one. Mine against `target` and
submit every hash that meets it. The answer gains one field:

```json
{"accepted": false, "share": true, "stale": false}
```

`share` means the hash was credited as a share, worth the job's share
difficulty (2^256 divided by `target`). Keep working on the same job after
a share. `accepted` is set as well when the hash also met `block_target`
and the node took the block; `stale` means the job is finished, fetch new
work. The share target is retuned per address towards one share every
15 seconds, between 500 attempts and the block difficulty, and a job
takes at most 24 submissions. Pool status is at `/api/pool/stats`, one
miner's at `/api/pool/miner/<address>`, and the full ledger at
`/api/pool/ledger`.

### Timing and limits

| Item | Value |
|---|---|
| Template reuse | The server reuses its node's template for 10 seconds. |
| Job lifetime | A job is forgotten 2 minutes after it was handed out. |
| Refresh | The reference client fetches new work every 20 seconds, and at once after a solution. |
| Jobs per address | The server keeps at most 8 open jobs per payout address; the oldest go first. |
| Submissions per job | At most 3 solutions are checked per solo job, 24 per pool job. |
| Browser origins | Browsers may submit only from the web app origins the operator allows (`-allow-origin`). Programs that send no `Origin` header are not affected. |

### Writing your own miner

1. Fetch a job from a work server, or call `getblocktemplate` on your own
   node and build the reward transaction yourself. From block 300 it must
   pay the treasury 1% of the subsidy
   ([Specification §3.3.1](docs/SPEC.md)), or the block is invalid.
2. Build the 120-byte header: the 112-byte prefix plus your 64-bit
   little-endian nonce.
3. Hash it with Argon2id: 8192 KiB of memory, one pass, one lane, 32-byte
   output, salt `Argonmine/pow/v1`.
4. Read the 32 bytes as a little-endian number. If it is at most the
   target, submit the nonce. Otherwise try the next nonce.
5. Fetch a new job when the server says your job is stale, and at least
   every 20 seconds.

To run your own work server, run an explorer next to a node. See
[Run a node](docs/NODE.md#the-explorer-and-work-server).
