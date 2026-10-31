# Roadmap

This page shows what has been built, what launched and what comes next. It
was last updated on 2026-10-28.

**Where things stand.** Mainnet launched on 28 October 2026, 12:00 UTC, with genesis
block `e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`. The chain, the browser app, the explorer,
the bridge on Ethereum, Base, Arbitrum One and BNB Chain, and the swap
service all run on mainnet. The public testnet, which ran from 2026-10-26,
is retired. The work left is mostly security, operations and polish, not
new features.

**How to read this page.** Each milestone has a priority and a status.
There are no calendar dates for future work. "Next" means the work in
progress now.

| Priority | Meaning |
|---|---|
| **P1** | Next. The most important open risks and gaps after launch. |
| **P2** | Later. |

| Status | Meaning |
|---|---|
| Done | Built, tested and, where it applies, running on mainnet. |
| In progress | Being worked on now. |
| Partly done | Some of the items are done. |
| Planned | Planned, nothing built yet. |

## Order of work

1. **Next:** run every flow on mainnet once the bridge and the swap service
   have started ([A1](#a1-every-flow-proven-on-mainnet)), then the open
   risks: the guardians on separate machines and operators
   ([A3](#a3-keys-custody-and-the-treasury)), seeds on more providers
   ([A4](#a4-network-and-hosting)) and signed releases
   ([A6](#a6-releases)).
2. **When funded:** an external audit ([A2](#a2-security)).
3. **Later:** the rest of [After launch](#after-launch).

## Launch: 2026-10-28

| Item | Status |
|---|---|
| Mainnet genesis block mined for the launch moment, 28 October 2026, 12:00 UTC: `e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`. Easiest block 2^18 Argon2id attempts (`bits` `0x1E400000`), 10-minute target, ASERT | Done |
| Mainnet services: node and seed (`altaria.proxy.rlwy.net:12347`, the built-in default seed), explorer, website and app, docs, bridge guardians, swap service | Done |
| Bridge vaults on Ethereum, Base, Arbitrum One and BNB Chain with the hardened contract: a 7-day timelock on every change the owner makes, guardian pause and veto, 1% fees, no release limits; each owned for now by one wallet of the project | Done |
| The bridge issuing uARG | Starts once its fee wallet holds unlocked ARG, about a day after launch. Deposits made before then wait in the vault and are issued then |
| The swap service trading ARG, wARG and uARG at 1 ARG = 0.10 uARG, 1% spread each way | Starts once it holds unlocked ARG, about a day after launch |
| Four in-house security reviews (chain rules and networking; bridge custody and contracts; bridge withdrawals and public API; wallets, app, explorer and swap service), each followed by a second pass over its fixes | Done; every finding and its status is in [Security review](docs/SECURITY-REVIEW.md), "Round of 2026-10-28" |
| A health check for uptime monitors: `https://explorer.argonmine.com/api/health` | Done |
| Release archives 1.0.0 for node operators on six platforms, with `SHA256SUMS` ([Run a node, Download](docs/NODE.md#download)) | Done, not yet signed |
| Docs updated for mainnet: addresses, chains, tokens, fees, links | Done |
| The public testnet retired. Testnet coins and test bridge balances do not carry over | Done |

From launch on, every change to the consensus rules is scheduled at a
block height and announced in advance ([SPEC §3.2](docs/SPEC.md)). The
treasury rule from block 300 only turns away blocks that leave the
treasury out, so nodes on release 1.0.0 keep following the chain; a node
that mines must run release 1.1.0 before block 300
([Run a node, Upgrades](docs/NODE.md#upgrades)).

## After launch

### A1: Every flow proven on mainnet

**Priority P1 · Planned.** Possible once the bridge and the swap
service have started.

| Item | Status |
|---|---|
| Every flow run with small real amounts: mine, send each coin, swap every pair, deposit and withdraw on every chain and token | Planned |
| A deposit with a real EVM wallet extension, and one through a deposit address, on every chain | Planned |
| Fee collection and the fee records checked against the chain | Planned |
| Close watch in the first days, with fixes shipped the same day | In progress |

### A2: Security

**Priority P1 · Partly done.** No funds were raised for an external
audit, so the security work so far was done in house. The bridge holds
real money without an external audit and without limits.

| Item | Status |
|---|---|
| An external audit of the vault contract, the deposit router and the guardian daemon | Planned, when funded |
| Every item still open from the round of 2026-10-28 closed ([Security review](docs/SECURITY-REVIEW.md)) | In progress |
| Property tests and fuzzing of the parts that move money: transaction validation, the asset layer, the vault contract and the guardians' payout checks | Planned |
| Failure drills: a guardian down, a reorganisation, a stuck payout, a full disk | Partly done: automated tests cover reorganisations and stuck payouts |
| A public record of every fix | Partly done: [Security review](docs/SECURITY-REVIEW.md) |

### A3: Keys, custody and the treasury

**Priority P1 · Partly done.**

| Item | Status |
|---|---|
| 1% of each block subsidy to the project treasury from mainnet block 300, by consensus rule; fees stay with the miner. Treasury address `arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2`, key held offline by the project ([Fees](docs/FEES.md#the-treasury)) | Done (2026-10-28) |
| Each guardian key on its own machine, under its own operator. Today the project runs all three itself. The keys can move as they are; replacing a guardian key changes the custody key and the uARG asset id ([Bridge](docs/BRIDGE.md)) | Planned |
| Independent people as guardians and treasury signers | Planned |
| A treasury whose keys are on no server: a 2-of-3 multisig (Safe) on each EVM chain, with written custody. Vault ownership moved to it; bridge surplus withdrawn only to it | Planned |
| Most of the swap service's reserve in cold storage, with a small balance kept for trading | Planned |
| A Treasury page on the explorer listing every fee received and every payment out ([Fees](docs/FEES.md)) | Planned |

### A4: Network and hosting

**Priority P1 · Partly done.** One seed node runs today.

| Item | Status |
|---|---|
| Seed nodes on at least two more hosting providers, in the built-in list and in DNS (`seed.argonmine.com`) | Planned |
| Always-on miners: each seed mines with a few threads, so blocks continue when any one computer is off | Planned |

### A5: Operations

**Priority P1 · In progress.** Operational hardening (monitoring, backups,
more providers) continues after launch. A health check for uptime monitors
is public: `https://explorer.argonmine.com/api/health` fails when the
explorer loses its node, falls behind, or sees no block for an hour.

### A6: Releases

**Priority P1 · Partly done.**

| Item | Status |
|---|---|
| Release archives 1.0.0 for six platforms, with `SHA256SUMS` ([Run a node, Download](docs/NODE.md#download)) | Done (2026-10-28) |
| Code-signed programs and a signed checksum file | Planned |
| Source code published at [github.com/argonchain-source/argonchain](https://github.com/argonchain-source/argonchain) | Done (2026-10-30) |
| A mining pool for browser miners, run on the explorer: shares, per-round payouts, a public ledger, 1% fee; solo mining unchanged ([Mining, Pool mining](docs/MINING.md#pool-mining)) | Done (2026-10-30) |

### A7: Legal

**Priority P1 · Planned.**

| Item | Status |
|---|---|
| Legal review | Planned |

### A8: Bridge design and experience

**Priority P2 · Partly done.**

| Item | Status |
|---|---|
| Withdraw: address, QR code, memo and four live steps, with a message for every kind of wrong payment | Done (2026-10-27) |
| "No liquidity" per chain and token | Done (2026-10-28) |
| A redesign of bridge.argonmine.com: layout, hierarchy, type and spacing | Planned |
| Deposit with the same statuses, progress, errors and confirmations as Withdraw | Partly done |
| Every screen checked on phone and desktop, in light and dark | Partly done |
| The old hand-off code endpoints removed | Planned |

### A9: The app

**Priority P2 · Partly done.**

| Item | Status |
|---|---|
| Dashboard (holdings, mining totals, activity, invite link) and redesigned Settings | Done (2026-10-28) |
| Home screen with a centred balance and round action buttons | Planned |
| Tap targets of at least 44 px on phones | Planned |
| Activity beyond the newest 500 entries | Planned |
| A page for each coin, opened from its row | Planned |
| Plainer words on Mine and Settings | Planned |
| A full test as a new user, on a phone and a laptop, start to finish | Partly done |

### A11: Names and buying ARG

**Priority P1 · In progress (2026-10-31).**

| Item | Status |
|---|---|
| Names: `alice.arg` for an address, registered from the app, paid in ARG to the treasury ([Names](docs/NAMES.md)) | In progress |
| Send to a name; names on the explorer and the dashboard | In progress |
| Buy ARG: pay USDC or USDT on four chains, receive ARG; the swap sells ARG one way | In progress |

### A10: Documentation

**Priority P2 · In progress.**

| Item | Status |
|---|---|
| Plain-language docs: overview, getting started, mining, the three coins, bridge, swaps, fees, security | In progress |
| A complete node operator guide ([Run a node](docs/NODE.md)) | In progress |

### Later

**Priority P2.** In rough order of importance.

| Item | Why |
|---|---|
| Threshold signing (FROST) for uARG issuance, so it no longer needs every guardian | Keeps the bridge running when one guardian is offline |
| WalletConnect on the bridge page | Pair a phone wallet with a desktop browser by QR code |
| More chains for the bridge: Polygon, Bitcoin later | More ways to bring dollars in |
| Node: compact block relay, encrypted peer connections, waiting transactions put back after a reorganisation | Less bandwidth, more privacy, fewer lost transactions |
| Miner: reuse Argon2id memory between attempts | A higher hashrate per core |

Ideas that may come later, as decisions rather than plans: a shorter block
interval, larger blocks, privacy features, a safe contract layer and light
clients ([SPEC §12](docs/SPEC.md)).

## Done before launch

### By 2026-10-25: the chain and the node

- Consensus specification ([SPEC](docs/SPEC.md)) and its implementation:
  canonical encoding, tagged hashes, checked money arithmetic, Argon2id
  proof of work with a mainnet floor of 2^18 attempts per block, ASERT
  difficulty, Schnorr signatures, transaction and block validation.
- A test for every rule and for every Bitcoin pitfall listed in SPEC §9;
  fuzz tests for every decoder.
- Genesis blocks for mainnet, testnet and regtest. The mainnet genesis was
  mined again for the launch moment.
- The full node `argonmined`: chain state on LevelDB, reorganisations with
  undo data, headers-first sync, the peer-to-peer network, the mempool with
  replace-by-fee, and JSON-RPC.
- First internal security review of the consensus, chain, mempool and
  networking code: findings 1 to 15, all fixed.

### By 2026-10-26: wallets, mining, explorer and the public testnet

- Command-line wallet: 24-word keys, encrypted wallet file.
- Mining: the node's built-in miner, the solo work protocol, and the browser
  miner (WebAssembly in a background worker). Every block pays the miner
  directly.
- Pool mining removed by decision (2026-10-26).
- Invite links: a referral share paid inside the block.
- Block explorer with an address index, charts, a JSON API and the work
  server for browser miners.
- Public testnet (2026-10-26): seed node, explorer, website and browser
  app. Retired at the mainnet launch.
- Seed-node kit (Docker, compose, systemd), release scripts for six
  platforms and per-user installers.
- Launch runbook ([LAUNCH](docs/LAUNCH.md)).

### 2026-10-26: assets and the bridge

- Asset layer in consensus: version-2 transactions carry coins other than
  ARG, issued only by their issuer's key ([SPEC §3.5](docs/SPEC.md)).
- Inbound bridge: a vault contract per EVM chain holding USDC and USDT, and
  guardians that issue uARG (all guardians must sign) and pay withdrawals
  (2 of 3 must sign). First run on the test networks of Ethereum, Base,
  BNB Chain and Arbitrum.
- Bridge fee: 1% on deposits and 1% on withdrawals, kept in the vault as
  surplus above the backing.
- uARG in the wallets and the explorer, recognised by its asset id, never
  by its name.
- Wallet backup check: a new wallet asks for four of its 24 words back.
- More internal review findings, including a second pass over the
  networking and mempool code (findings 19 to 27): all fixed, 27 findings
  in total ([Security review](docs/SECURITY-REVIEW.md)).

### By 2026-10-27: bridge hardening, browser wallet and swaps

- Bridge hardening, first deployed on the testnet: only the leader
  guardian may coordinate signing, and every guardian checks every deposit
  itself; issuance caps per issuance and per day (in the code, off on
  mainnet by decision); deposits acted on only once their block is final;
  payout depth that grows with the amount; a separate low-balance key that
  pays gas; stuck payouts resent without paying twice; a hardened public
  API.
- Browser miners check that each job pays them before they mine it.
- The hardened vault contract (a timelock of at least one day on changes,
  pause by any guardian, optional release limits per token) written and
  tested.
- Deposit addresses: deposit without a browser wallet, from any wallet or
  exchange.
- Swap addresses: withdraw with an ordinary payment that names the payout
  in the address itself.
- Browser wallet, deployed 2026-10-27: create a wallet or log in with 24
  words, a password that locks it in the browser, Send ARG, uARG and wARG,
  Swap between the three on this chain.
- Swap service and wARG, first on the testnet (2026-10-27)
  ([Swaps](docs/SWAP.md)).
- The explorer lists each address's coins and broadcasts signed payments.
- The argonmine.com domain, with the app, explorer, bridge and docs on
  their own names.
- Withdraw rebuilt on the bridge page: paste an address, enter an amount, pay
  the address shown with its memo, and follow four live steps.

### 2026-10-28

- Tokens whose vault has no money are marked "no liquidity" for withdrawals
  and reopen by themselves once funded.
- Four in-house security reviews, each followed by a second pass over its
  fixes ([Security review](docs/SECURITY-REVIEW.md)).
- Decisions: in-house security work instead of an external audit for now;
  no bridge limits; seeds on three providers; and, in the evening, 1% of
  each block subsidy to the treasury from block 300, which replaced the
  earlier decision of no fee on mining rewards (see
  [Decisions](#decisions)).
- Mainnet launched; the public testnet retired.

## Decisions

| Date | Decision |
|---|---|
| 2026-10-26 | Pool mining removed. Browser miners mine whole blocks on their own. |
| 2026-10-26 | The bridge brings dollars in as uARG. ARG never leaves the chain. The earlier bridge that exported ARG was retired. |
| 2026-10-26 | wARG is wrapped ARG, one for one. |
| 2026-10-26 | Bridge fee of 1% on deposits and 1% on withdrawals, kept in the vault. |
| 2026-10-27 | Swaps between ARG, wARG and uARG happen on the Argonmine chain only. USDC and USDT move through the bridge. |
| 2026-10-28 | No fee on mining rewards. Replaced the same evening by the treasury rule below. |
| 2026-10-28 | No external audit for now, since no funds were raised. In-house reviews instead. (The small limits first planned for the launch were dropped; see below.) |
| 2026-10-28 | Treasury: a 2-of-3 multisig on each EVM chain, keys on separate devices, none on a server. Planned after launch; until then each vault's owner is one wallet of the project. |
| 2026-10-28 | Seed nodes are to run on at least three providers, each also mining. |
| 2026-10-28 | The testnet is retired and everything runs on mainnet. Testnet coins and test bridge balances do not carry over. |
| 2026-10-28 | No limits per deposit, per withdrawal or per day on the bridge. Users are told plainly that it holds real funds without an external audit and without limits. |
| 2026-10-28 | The vault owner on every chain is, for now, one wallet of the project, not a key on a server. Every change the owner makes to a vault waits 7 days; 2 of the 3 guardians can veto it, and the owner can override a veto after 60 days. Moving ownership to the multisig is planned. |
| 2026-10-28 (evening) | From mainnet block 300, every block pays 1% of the block subsidy to the project treasury, by consensus rule. Transaction fees stay with the miner. No premine still holds: the treasury's coins are mined too. The swap spread and the bridge fees also go to the treasury. Node runners who do not mine earn nothing from the protocol; the treasury may fund public seed nodes later. |
