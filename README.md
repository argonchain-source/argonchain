# Argonmine

A proof-of-work layer-1 blockchain in Go, built on Bitcoin's rules and
lessons. Every coin is mined, and there is no premine. From block 300, 1%
of each block reward goes to the project treasury; the miner keeps the rest
and all fees. Mining uses memory-hard Argon2id, so mining in a browser on
a laptop or phone is real mining, not a simulation.

The product is the browser app at **app.argonmine.com**: create a wallet
(or log in with your 24 words), mine, and send, receive and swap the
chain's three coins. Nothing to install.

* Rules: [docs/SPEC.md](docs/SPEC.md), the consensus specification, with a
  table of known Bitcoin pitfalls and the rule here that prevents each one.
* Plan: [docs/ROADMAP.md](docs/ROADMAP.md).
* Every fee in one place: [docs/FEES.md](docs/FEES.md).

The chain is **Argonmine**; its coin is **ARG**. Mainnet launched on
28 October 2026, 12:00 UTC and is the only public network. Its addresses start with
`arg1`. The public testnet is retired.

## The three coins

| Coin | What it is |
|---|---|
| ARG | The mined coin. 21 million at most, 50 per block at first, halving like Bitcoin. |
| wARG | Wrapped ARG, one for one, issued by the swap service and backed by the ARG it holds. |
| uARG | A dollar token issued by the bridge, backed by USDC and USDT in vaults on EVM chains and redeemable one for one through the bridge, less the 1% withdrawal fee. |

## Public services

| Address | Binary | What it is |
|---|---|---|
| argonmine.com | `argonmine-web` | The landing page. |
| app.argonmine.com | `argonmine-web` | The browser app: wallet, mining, send, receive, swap, dashboard, settings. |
| docs.argonmine.com | `argonmine-web` | This documentation, rendered. |
| explorer.argonmine.com | `argonmine-explorer` | Blocks, transactions, addresses, mempool, charts, JSON API, and the work server for browser miners. |
| bridge.argonmine.com | `argonmine-bridge` | Deposit USDC or USDT for uARG; withdraw uARG to USDC or USDT. |

Hosting is described in [deploy/README.md](deploy/README.md) and
[deploy/railway/README.md](deploy/railway/README.md).

## Layout

| Package | What it is |
|---|---|
| `consensus/` | The rules: encoding, hashing, merkle trees, proof of work, ASERT difficulty, transaction and block validation, the asset layer. Pure functions, exhaustively tested. |
| `chain/` | Block index and chain state on LevelDB: connect, disconnect, reorganise, undo data, persistence. |
| `mempool/` | Unconfirmed transactions: validation, fees, replace-by-fee, eviction, block template selection. |
| `p2p/` | Wire protocol, peers, headers-first sync, block and transaction relay, address gossip, banning. |
| `miner/` | Block templates and the multi-threaded Argon2id search. |
| `rpc/` | JSON-RPC 2.0 over HTTP for wallets, miners and the explorer. |
| `wallet/` | Addresses, keys, signing, coin selection and the node-backed wallet. |
| `litewallet/` | The browser wallet's signer: one key from the 24 words, sealed under a password (Argon2id + XChaCha20-Poly1305), payments and swap signing checked before anything is signed. |
| `swapaddr/` | Swap addresses: an address that names a payout on an EVM chain, so uARG paid to it is withdrawn there. |
| `swap/` | The native swap service between ARG, wARG and uARG: one co-signed transaction per swap. |
| `bridge/`, `contracts/` | The federated bridge: guardians, the vault and deposit-router contracts, deposits, withdrawals and their tracking. |
| `workclient/` | The miner core and work protocol used by the browser miner. |
| `ui/` | The shared design system: stylesheet, icons, formatting, charts, brand constants. |
| `cmd/argonmined` | The node. |
| `cmd/argonmine-web` | The landing page, the browser app and the docs site. |
| `cmd/argonmine-webminer` | The miner and wallet signer compiled to WebAssembly for the browser app. |
| `cmd/argonmine-explorer` | The block explorer and work server. |
| `cmd/argonmine-bridge` | A bridge guardian (leader or follower). |
| `cmd/argonmine-swap` | The swap service. |
| `cmd/argonmine-wallet` | Command-line wallet for node operators. |
| `cmd/argonmine-app` | Node, wallet and miner in one program with a local web interface, for operators who mine on their own node. |
| `cmd/genesis` | Reproduces the genesis blocks. |
| `scripts/`, `deploy/` | Release builds, installers, and the hosting kit (Docker, compose, systemd, Railway). |

## Build and test

Requires Go 1.26 or later. No cgo, so it builds anywhere Go does.

```
go build ./...
go test ./...
go build -o argonmined ./cmd/argonmined
```

The browser miner and wallet: `GOOS=js GOARCH=wasm go build -o cmd/argonmine-web/site/static/miner.wasm ./cmd/argonmine-webminer`.
The contracts: `forge test` in `contracts/` (Foundry).

## Mining in the browser

The browser app mines solo: it asks the explorer for a block template that
pays the miner's own address (`GET /api/work?address=…`), searches nonces
in WebAssembly, and submits a solved block (`POST /api/work/submit`). The
explorer passes the block to its node; the reward goes to the miner, less
the treasury's 1% of the subsidy from block 300 (see
[docs/FEES.md](docs/FEES.md)). There is no pool and nobody holds funds in
between. Before mining a job the app checks that its coinbase pays the
miner's address and that the header commits to it, so the server cannot
make a miner work for someone else.

## Referrals

An invite link is the app's address with `?ref=<your address>`. Someone who
opens it and presses Accept gives a fixed share of every block they find
(5% today, `argonmine-explorer -referral-share`) to the inviter, paid inside
the block: the coinbase has two outputs, miner and inviter. From block 300
the share is of what remains after the treasury's 1% of the subsidy, and
the coinbase has a third output, to the treasury. Nothing extra is
created. The share accepted is kept with the inviter, and a job paying
the inviter more is refused. The invited miner can remove the inviter on
the Dashboard.

## Running a node

See [docs/NODE.md](docs/NODE.md). In short:

```
argonmined                                        # join mainnet (the default)
argonmined -network regtest -listen none          # a private chain for development
```

A node started without `-network` runs mainnet and finds peers through
the built-in seed `altaria.proxy.rlwy.net:12347`.

## Network parameters

| | mainnet | regtest | testnet (retired) |
|---|---|---|---|
| Address prefix | `arg1…` | `rarg1…` | `targ1…` |
| P2P / RPC port | 9333 / 9332 | 29333 / 29332 | 19333 / 19332 |
| Easiest block (PoW floor) | 2^18 hashes (`bits` `0x1E400000`), 10-minute target, ASERT | about 2 hashes, constant | 512 hashes |
| Genesis | `e378ed19…cac8f6` | `b3682483…97959d` | `81fa0f3b…7cff32` |

The mainnet genesis block, `e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`, is timestamped at the launch moment,
28 October 2026, 12:00 UTC, because the difficulty schedule is anchored at genesis
(SPEC §13, [docs/LAUNCH.md](docs/LAUNCH.md)). Newly mined ARG can be
spent 100 blocks after the block that paid it, about 17 hours at
10-minute blocks. The code still knows the testnet's parameters, but no
public nodes run it any more.

## Status

Mainnet launched on 28 October 2026, 12:00 UTC. The chain, the app, the explorer, the
bridge and the swap service all run on it. The bridge starts issuing uARG,
and the swap service starts trading, once each holds mined ARG that has
unlocked, about a day after launch. What comes next is in
[docs/ROADMAP.md](docs/ROADMAP.md).

There is no external audit. The code is reviewed in house instead, as
described in [SECURITY.md](SECURITY.md), with every finding proved by a
test and listed in [docs/SECURITY-REVIEW.md](docs/SECURITY-REVIEW.md). The
bridge holds real funds without an external audit and without caps on
issuance per deposit or per day. Bridge only what you can afford to
have at risk.

Health of the public services: `https://explorer.argonmine.com/api/health`
answers 200 while blocks keep coming and 503 with the reasons when they do
not.
