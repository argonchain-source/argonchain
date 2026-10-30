# Argonmine documentation

Argonmine is a new digital currency with its own blockchain. Its coin is
called ARG. New ARG is created only by mining, and you can mine it in a web
browser on an ordinary laptop or phone. Nobody received coins in advance:
there is no premine. From block 300, 1% of each block reward goes to the
project treasury; the miner keeps the rest and all fees.

**Mainnet** launched on 28 October 2026, 12:00 UTC. It is the only public network,
and its coins have real value: a payment cannot be undone, and nobody can
restore lost 24 words. Mainnet addresses start with `arg1`. The public
testnet that ran before launch is retired. The [Roadmap](docs/ROADMAP.md)
lists what comes next.

## What is Argonmine for?

Bitcoin and many other proof-of-work coins are mined almost entirely with
specialised machines. Argonmine is designed so that ordinary hardware can
take part:

- **Fair start.** Every coin that will ever exist is created by mining,
  block by block. The first block (the genesis block) pays nobody. From
  block 300, 1% of each block's subsidy goes to the project treasury and
  the rest to the miner.
- **Mining on everyday devices.** Each mining attempt needs 8 MiB of
  memory. That makes memory, not raw chip speed, the limit, so phones,
  laptops and desktop computers stay useful.
- **Simple, fixed rules.** Supply, rewards and timing follow Bitcoin's
  well-known schedule. The rules are small enough to read in full in the
  [Consensus specification](docs/SPEC.md).
- **Fewer places for bugs.** There is no scripting language and no smart
  contracts. A coin is locked to one public key and spent with one
  signature.

## The three coins

The Argonmine chain carries three coins. You can hold and send all three
from the same wallet.

| Coin | What it is | Where it comes from |
|---|---|---|
| **ARG** | The native coin. Its supply is capped at 21 million. | Created only by mining. |
| **wARG** | Wrapped ARG. One wARG is backed by one ARG. | Issued by the swap service when you swap ARG for it, and destroyed when you swap it back. See [Swaps](docs/SWAP.md). |
| **uARG** | A dollar token, backed by USDC and USDT in the bridge vaults. It is redeemable one for one for USDC or USDT through the bridge, less the 1% withdrawal fee, subject to the risks in [Bridge, Risks](docs/BRIDGE.md#risks). | Issued by the bridge when you deposit USDC or USDT on another chain. See [Bridge](docs/BRIDGE.md). |

ARG never leaves the Argonmine chain. Dollars move in and out through the
bridge as uARG.

## What you can do

| Where | What you do there |
|---|---|
| [app.argonmine.com](https://app.argonmine.com) | The main product. Create a wallet or log in with your 24 words, mine in the browser, receive, send ARG, uARG and wARG, swap between the three, and follow your holdings, mining totals and invite link on the Dashboard. Works on phones and computers. Nothing to install. |
| [bridge.argonmine.com](https://bridge.argonmine.com) | Move dollars in and out. **Deposit** USDC or USDT from an EVM chain and receive uARG. **Withdraw** uARG and receive USDC or USDT at an EVM address. |
| [explorer.argonmine.com](https://explorer.argonmine.com) | Look up any block, transaction or address, and see network charts. It also hands out mining work to browser miners. |
| [docs.argonmine.com](https://docs.argonmine.com) | This documentation. |

To start, read [Getting started](docs/GETTING-STARTED.md). It walks
through the app screen by screen.

## Key numbers

| Item | Value |
|---|---|
| Coin | ARG. 1 ARG = 100,000,000 units. |
| Supply | Capped at 21,000,000 ARG. The rewards add up to 20,999,999.9769 ARG. |
| Block reward | 50 ARG at the start, halving every 210,000 blocks (about every four years). From block 300, 1% of it goes to the project treasury and 99% to the miner. |
| Time between blocks | 10 minutes on average. |
| Reward lock | A mining reward can be spent 100 blocks after the block that paid it, about 17 hours at 10-minute blocks. |
| Easiest block | 2^18 (262,144) Argon2id attempts on average. Mainnet started at this floor. |
| Proof of work | Argon2id with 8 MiB of memory per attempt. |
| Difficulty | Adjusted every block (ASERT, one-day half-life). |
| Block size | At most 1,000,000 bytes. |
| Signatures | Schnorr (BIP-340) on secp256k1. |
| Addresses | `arg1…` on mainnet. Developers also see `rarg1…` on a private test chain (regtest). |
| Fees | Transaction fees are paid in ARG and go to the miner of the block. From block 300, 1% of each block subsidy goes to the project treasury. Every fee is listed in [Fees](docs/FEES.md). |

## How does it work?

**Blocks and mining.** Transactions are collected into blocks, about one
every 10 minutes. To add a block, a miner must find a number (a nonce) that
makes the block's Argon2id hash fall below a target. That takes many
attempts, and each attempt needs 8 MiB of memory. Whoever finds a valid
block first receives the block reward plus the fees of the transactions in
it, less 1% of the block reward for the project treasury from block 300.
In the app, your browser makes these attempts itself and the reward goes
straight to your address. There is no pool and nobody holds your reward for
you. See [Mining](docs/MINING.md).

**Difficulty.** The network aims for one block every 10 minutes. After each
block it compares the chain with that schedule and makes the target easier
or harder. The adjustment has a one-day half-life: if the total mining
power doubles, block times move back toward 10 minutes over the following
day or two. See [Specification §5](docs/SPEC.md).

**Supply.** The reward starts at 50 ARG and halves every 210,000 blocks,
like Bitcoin. After 64 halvings it reaches zero. The total never exceeds
21 million ARG.

**Transactions.** A coin is an unspent output locked to a public key. To
spend it, your wallet signs a transaction with the matching private key.
The signature covers every amount, every key, the coins involved and the
chain itself, so a transaction cannot be changed after signing or replayed
on another network. Your private key comes from your 24 words and never
leaves your wallet.

**Other coins on the chain.** Besides ARG, a transaction can carry coins
issued by a known key, such as uARG (issued by the bridge) and wARG (issued
by the swap service). The chain makes sure that nobody else can create them
and that none are created or lost in a transfer. Fees are always paid in
ARG. See [Assets](docs/TOKENS.md).

**Nodes.** A node is a program, `argonmined`, that keeps a full copy of the
chain, checks every block and transaction, and passes them on to other
nodes. Anyone can run one, from a release archive or built from the
[source](https://github.com/argonchain-source/argonchain). A node earns nothing by itself;
mining is what pays. The treasury may fund public seed nodes later. See
[Run a node](docs/NODE.md).

## Where does the project stand?

| Part | Status |
|---|---|
| Mainnet | Launched on 28 October 2026, 12:00 UTC. Genesis block `e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`. |
| Chain, node, wallets, browser app, explorer | Live on mainnet. |
| Bridge (uARG) | Live on mainnet on Ethereum, Base, Arbitrum One and BNB Chain, with real USDC and USDT ([Bridge](docs/BRIDGE.md)). It starts issuing uARG once its fee wallet holds mined ARG that has unlocked, about a day after launch. Deposits made before then wait in the vault and are issued then. |
| Swap service (wARG and swaps) | Live on mainnet. It starts trading once it holds unlocked ARG, about a day after launch. |
| Security | There is no external audit. Four in-house reviews, each followed by a second pass over its fixes, were done on 2026-10-28; earlier internal reviews of the chain, node and networking code recorded 27 findings, all fixed ([Security review](docs/SECURITY-REVIEW.md)). The bridge holds real funds without an external audit and without limits per deposit, per withdrawal or per day: bridge only what you can afford to have at risk. |
| Testnet | Retired on 2026-10-28. Testnet coins and test bridge balances do not carry over to mainnet. |

## Where to go next

- [Getting started](docs/GETTING-STARTED.md): create a wallet, start mining and read every figure the app shows.
- [Mining](docs/MINING.md): what you earn, when you can spend it, how long a block takes, and invite links.
- [Fees](docs/FEES.md): every fee, who receives it and how to check it.
- [Bridge](docs/BRIDGE.md) and [Swaps](docs/SWAP.md): moving dollars in and out, and trading ARG, wARG and uARG.
- [FAQ](docs/FAQ.md): short answers to common questions.
- [Roadmap](docs/ROADMAP.md): what launched and what comes next.
- [Run a node](docs/NODE.md): for operators who want to verify the chain themselves or run a seed.
- [Consensus specification](docs/SPEC.md): every rule, in full.
- [Source code](https://github.com/argonchain-source/argonchain): the node, wallets, explorer, bridge, swap service and app, under the MIT licence.
