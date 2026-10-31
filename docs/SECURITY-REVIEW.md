# Security review package

This page records the security reviews of Argonmine so far, and is the
starting point for an external review, which is planned once funded. Every
High and Critical finding of the round of 2026-10-28 was fixed before the
mainnet launch on 28 October 2026; one Low finding of that round is still
open (see "Round of 2026-10-28" below).

## What to review, in priority order

| Priority | Component | Why |
|---|---|---|
| 1 | Consensus rules ([Specification](docs/SPEC.md)) | A bug here is a chain split or inflation. About 1,900 lines, pure functions. |
| 2 | Chain state and reorgs | Undo data, fork choice, rollback of invalid branches, tx index. |
| 3 | P2P | Unauthenticated input. DoS, sync stalls, ban bypass, resource exhaustion. |
| 4 | Mempool | Policy only, but RBF and eviction can be abused for free relay. |
| 5 | Wallet crypto and key storage | Key derivation, encryption at rest, signing. |
| 6 | RPC, app, explorer, including the public work server | Local or operator-facing; auth and input handling. The work server is public and unauthenticated: template building and one Argon2id check per submission must stay cheap. |

Out of scope for the first review: UI styling, docs, release scripts.

## Trust boundaries

- **Untrusted**: every P2P peer; every transaction and block; browser miners
  (work requests and submissions); anyone who can reach the explorer HTTP port.
- **Semi-trusted**: the node's RPC caller (token-authenticated, localhost by
  default); the mining app's browser page (per-launch token, localhost).
- **Trusted**: the local filesystem (chain DB, wallet file); the operator.

## Invariants the reviewer should try to break

1. No block connects that creates more coins than `Subsidy(height) + fees`,
   and no sum of amounts ever exceeds `MaxMoney` or wraps.
2. No input is spent twice: within a tx, within a block, across the chain.
3. No P2PK output is spent without a BIP-340 signature over the exact sighash
   of SPEC §6.2, which commits to every spent amount and script.
4. Two nodes given the same blocks in any order reach the same tip and the
   same UTXO set (fork choice by total work; reorg undo/redo is exact).
5. Decoders accept exactly one encoding per structure (canonical), and never
   panic or allocate unboundedly on untrusted bytes (fuzz tests cover this).
6. Difficulty (ASERT) is a total, deterministic function of the parent
   time and height with no overflow or sign error at any u64 timestamp.
7. A peer cannot make the node perform unbounded Argon2id work, grow memory
   without bound, or stall sync indefinitely without being disconnected.
8. The wallet file's seed is unrecoverable without the password (Argon2id
   64 MiB, 3 passes; XChaCha20-Poly1305).

## Tests

The source code is public at [github.com/argonchain-source/argonchain](https://github.com/argonchain-source/argonchain)
with its test suite: unit tests for every
rule, end-to-end tests that start real nodes and bridge guardians on a
private regtest chain, contract tests for the vault, and a fuzz target for
every decoder.

## Internal review log

- 2026-10-30 (mainnet, found in production): **browser blocks paid 0.5 ARG
  short since block 300.** The node's `getblocktemplate` reported
  `coinbasevalue` as its own template's first output, which from the
  treasury height is the subsidy less the treasury's share; the explorer's
  work server, which takes the share out itself, took it out a second
  time, so a browser-found block paid the miner 49.0 ARG and the treasury
  0.5 and left 0.5 unminted. Blocks found by nodes were right. About 660
  browser blocks were affected, roughly 330 ARG that will never exist; the
  supply cap is unchanged and no coins went to anyone. Fixed on both
  sides: the node reports the whole coinbase (test
  `TestTemplateCoinbaseValueWithTreasury`), `miner.AssembleFromTemplate`
  pays the treasury itself, and the explorer refuses a template whose
  value is below the subsidy, so an old node cannot cause it again. Found
  while reading coinbases on the explorer by hand; the end-to-end tests
  ran the explorer against a regtest node without a treasury, which is
  why they passed. A regtest treasury is now part of the test.
- 2026-10-30: the explorer labelled the treasury's output of a block without
  an inviter as a referral, and did not recognise the inviter's output of
  a block with one, because its referral detector predated the treasury.
  Address pages and the app's Dashboard showed referral income under
  blocks mined and the treasury as having referrals. Fixed with a test
  for each coinbase shape; the index rebuilds on the next start.

- 2026-10-25: self-review of consensus against SPEC §9 (Bitcoin pitfalls)
  with a dedicated test per pitfall; nine fuzz targets, 20 s each, no crashes.
- 2026-10-25: independent read of consensus, chain, mempool and P2P by a
  second reviewer (findings and fixes recorded below as they are closed).
- 2026-10-26: second pass over P2P and mempool for denial of service, with
  proof-of-concept tests; findings 19 to 27 below, each with a regression
  test that shows the attack failing.
- 2026-10-28: four independent in-house reviews in place of an external
  audit (no funds were raised): chain rules and networking; the bridge's
  custody, payouts and contracts; the bridge's withdrawal flow and public
  API; the wallets, app, explorer and swap service. Every finding was proved
  by a test before it counted. Fixes were then reviewed a second time by
  three more independent reviewers. See "Round of 2026-10-28" below.

### Findings

| # | Severity | Component | Status | Summary |
|---|---|---|---|---|
| 1 | High | p2p | Fixed | A peer claiming a huge height with empty header replies kept the node "syncing" forever and suppressed other peers' block announcements (eclipse). Now: block invs are fetched regardless of sync state, progress only counts real headers/blocks, an unsubstantiated height claim marks the peer unreliable, sync ends when the peer's headers run out (not on its claimed height), per-IP inbound cap of 3. Test: `TestUnsubstantiatedHeightClaimDoesNotEclipse`. |
| 2 | High | chain | Fixed | Every PoW-valid side-chain block was stored in full forever. Now lighter side chains are kept header-only (cap 100k); bodies are fetched only if the branch becomes heaviest (`MissingDataError`), and invalid blocks' bodies are dropped. Mainnet PoW floor raised from 2^12 to 2^18 hashes. |
| 3 | Medium | chain | Fixed | A disk error during connect was treated as a consensus failure (block marked invalid, honest peers banned). Now `StorageError` is a distinct type: never marks invalid, never bans; a failing coin read aborts the process instead of answering "missing". |
| 4 | Medium | p2p | Fixed | `getdata` could request 10,000 full blocks, each decoded and re-encoded, with the per-peer queue bounded only in messages. Now ≤32 blocks per request, raw stored bytes are sent, the queue is bounded to 32 MiB, and the loop stops when the peer is gone. |
| 5 | Medium | mempool | Fixed | Signatures were verified before the fee check (free CPU for zero-fee spam). Now fee and RBF rules run first; rejected txids are remembered for 2 minutes (missing-parent rejections excepted). |
| 6 | Medium | p2p/chain | Fixed | Serving `getheaders` walked parent pointers per header (quadratic). The chain now keeps an O(1) height index for the main chain. |
| 7 | Medium | p2p | Fixed | A block rejected as too-far-in-future was re-requested in a tight loop. Rejected hashes are parked for 60 s; requests time out and are re-asked once. |
| 8 | Medium | p2p | Fixed | No per-IP connection limit. Now 3 inbound per IP when the address identifies the peer (`TestPerIPInboundLimit`; see 19). |
| 9 | Medium | p2p | Fixed | Address gossip accepted private/loopback addresses with no per-peer cap, enabling outbound eclipse. Now only routable addresses from gossip, ≤50 stored per message, and previously-connected ("tried") addresses are preferred when dialing. |
| 10 | Low/Med | consensus | Fixed | Launch fairness: with a 4096-hash floor the first blocks after launch could be minted in seconds. Floor raised to 2^18; launch runbook requires genesis time = launch moment. |
| 11 | Low | p2p | Fixed | Argon2id header verification ran under the sync mutex. PoW is now checked outside the lock. |
| 12 | Low | mempool | Fixed | RBF accepted equal fee rates and eviction ran before conflicts were removed. Now the replacement must beat every replaced fee rate, replaces ≤100 txs, and conflicts are removed first; eviction is amortised. |
| 13 | Low | p2p | Fixed | Orphan cache accepted blocks without PoW and never expired. Now PoW-checked, 10-minute expiry, oldest evicted. |
| 14 | Low | p2p | Fixed | Unknown message types banned the peer (would ban newer nodes). Now skipped. |
| 15 | Info | spec | Fixed | SPEC now states POW_SALT, the saturation order in ASERT, the byte-string encoding in the scripts sighash, decoder length caps, and that no network time adjustment exists. The node asserts at start that the anchor target is not easier than the PoW limit. |
| 16 | Medium | wallet CLI, app | Fixed | Fee rate was parsed as a coin amount: "1" meant 1e8 units per byte, making ordinary payments fail as "insufficient funds" (found in the live bridge run). Both now parse an integer of units per byte, capped at 1,000,000. |
| 17 | Low | bridge | Fixed | The guardian sign-mint HTTP handler passed a nil context to the EVM client, so followers could never sign against a real RPC (the in-memory fake tolerated it). Fixed; the fake now rejects nil contexts so the end-to-end test covers it. |
| 18 | Info | contracts | Fixed | A Foundry test applied `vm.expectRevert` to an inline view call instead of `mint`. Test corrected; 7 of 7 pass under solc 0.8.26. |
| 19 | High | p2p | Fixed | Behind a TCP proxy every inbound peer has the proxy's address: one bad message banned it for 24 h and three connections filled the per-IP cap. Now `-behind-proxy` (and, by default, loopback/private/CGNAT sources) never bans or caps by address; misbehaviour disconnects. A full inbound table evicts the least useful inbound peer (best ping, recent block senders and the sync peer protected) instead of refusing; accepts are limited to 10/s, burst 20. `TestBehindProxyNeverBansAndEvicts`, `TestAcceptTokenBucket`. |
| 20 | High | p2p | Fixed | A peer claiming a huge height and never answering kept the node `Syncing()`, and transaction announcements were ignored while syncing. Now announcements are skipped only while the tip is over 2 h old and the sync peer has delivered new headers; a sync peer must answer `getheaders` within 15 s; failed claims are remembered per endpoint/host for an hour (per connection when the address is shared); outbound peers are preferred for sync. `TestFakeHeightPeerDoesNotBlockTxRelay`. |
| 21 | Medium | p2p | Fixed | `getdata` answers were queued from the reader goroutine with a blocking send, so two nodes with large mempools deadlocked until the write deadline. Now a per-peer responder serves requests in order with a bounded queue and a 4 MB lead; the requester asks ≤500 transactions per `getdata`, ≤1,000 in flight per peer, one peer per transaction, the next announcer after 60 s. `TestMutualLargeMempoolsExchange` (2,000 each way, about 2 s). |
| 22 | Medium | p2p | Fixed | The connect-time mempool announcement listed up to 10,000 txids in map order (children before parents were lost), could be fetched repeatedly, and the `mempool` gap was per connection. Now parents first, best fee rate first, in invs of ≤1,000; a transaction is served only after being announced to that peer and once per announcement; `mempool` gap per address (per connection plus a global rate for shared addresses); dump bytes budgeted per address; a 100-transaction orphan pool. `TestChainedTxsAllArrive`, `TestTxRelayRules`. |
| 23 | Medium | mempool | Fixed | The reject cache was keyed by txid, which does not commit to the witness: relaying a copy with a garbage signature blocked the genuine transaction for 2 minutes. Now witness-dependent failures are keyed by wtxid, the cache is cleared on every new tip, and a bad copy makes the node ask another announcer. `TestGarbageWitnessDoesNotBlockGenuineTx`. |
| 24 | Medium | mempool | Fixed | No limit on chains of unconfirmed transactions, and ordering was quadratic under the pool lock (0.75 s per template for a 2,000-long chain). Now 25 transactions / 101,000 bytes for ancestors and descendants, Kahn's algorithm with a fee-rate heap, eviction never removes the newcomer's ancestors, and a confirmed parent no longer takes its pool children with it. `TestChainLimitAndFastSelect`, `TestTopologicalScales`, `TestEvictionKeepsAncestors`, `TestChildSurvivesParentConfirmation`. |
| 25 | Low | p2p | Fixed | A `-connect` peer given by host name was redialled every 5 s (compared by resolved address only). The dial string is kept and both forms are compared. `TestConnectByHostnameOnce`. |
| 26 | Low | p2p | Fixed | The map of addresses found to be this node was never pruned, and one static nonce identified the node on every connection. Now pruned and capped at 32, with a fresh nonce per connection. `TestSelfMapBoundedAndFreshNonces`, `TestSelfConnectionDropped`. |
| 27 | Low | mempool | Fixed | Asset outputs are exempt from the dust limit, making asset UTXO spam about 8 times cheaper than ARG. Every asset output beyond the first two now adds 546 units to the minimum relay fee (policy only). `TestAssetOutputSurcharge`. |

Not changed (accepted): the mempool does not re-add transactions from disconnected blocks; nodes rely on the local clock (NTP required). Load-time fork choice re-run was added.

Bridge-specific scope for the external audit: the vault contract (signature ordering, burn-id replay protection, pause and governance, release cap); the guardians' MuSig2 session handling (single use, nonce registration); each guardian's own checks of an issuance and of a release, which are the guardians' only defence against a malicious leader; settlement of deposits from the chain, so a deposit cannot be issued twice; the EVM client (ABI encoding, EIP-155 signing); and the consensus asset layer (issuance signatures and per-asset conservation).

### Round of 2026-10-28

No reviewer found a way to create coins, double-spend, issue uARG twice or
pay a withdrawal twice. What they did find, and where it stands:

| Area | Finding | Severity | Status |
|---|---|---|---|
| Chain | A block body that does not match its header (altered in transit) marked the real block invalid for good, cutting the node off the honest chain | Critical | Fixed: the body is checked against the header's merkle roots before anything is stored or marked; only the sender is banned |
| P2P | A new node more than about 2,000 blocks behind could not finish syncing (header checks outran the stall watchdog) | High | Fixed: headers checked in parallel with progress per header; verified by syncing the whole testnet from zero in 48 s |
| P2P | Bodies of lighter competing branches were downloaded and kept forever | Medium | Fixed: header-only leaves are evicted least work first when the cap is full, so a header forked near the tip always finds room; the first fix's regression (an honest reorg failing at a full cap) is fixed with it |
| P2P | Parallel header checks multiplied the cost of junk header batches by the number of cores | Medium | Fixed: the first header of a batch is checked alone, and the parallel check starts only after a valid one, so a junk batch costs one evaluation before the ban |
| Mempool | Every new block re-checked the whole pool under its lock | Medium | Fixed: only the block's transactions, conflicts and their descendants are removed |
| Bridge | One cheap request could cost the guardian seconds of CPU and 150 MB (withdrawal views over many payments) | High | Fixed: bounded, cached views; per-address and per-client limits |
| Bridge | Free withdrawals could fill the global cap; the mempool scan was unbounded; rate limits could be shared behind the proxy | Medium | Fixed |
| Bridge | Replacing a vault broke deposit numbering: new deposits frozen or wrongly marked paid | High | Fixed (vault `firstDepositId`, identity check); the second pass found that rolling back to an earlier vault could lock a chain, fixed: a new vault is only a candidate until its identity check passes, so a refused or mistyped vault never replaces the one in use |
| Bridge | No minimum payout: dust payments could empty the gas key | High | Fixed: `min_release` per token, enforced by every guardian and checked by the browser app and the operator app before sending |
| Bridge | One faulty guardian could block every payout with a wrong signature | Medium | Fixed: wrong signers and non-canonical signatures are both dropped before the release is sent |
| Bridge | Splitting a payout into pieces skipped the confirmation tiers | Medium | Fixed: the payout depth follows the volume paid in a window, so pieces of a split payout wait as long as the whole |
| Contract | A guardian pause did not stop a compromised owner key | Medium | Fixed with a guardian veto; the second pass found the veto could freeze the vault and be replayed, fixed: the owner can override a veto after 60 days, and each veto signs a digest bound to the proposal's time and nonce |
| App | The browser miner's job check could be bypassed by a JSON key-case trick | High | Fixed: the miner gets exactly the fields that were checked |
| App | A swap could show one amount and get a signature for another (key-case trick) | High | Fixed: the review shows the wallet's own reading; only the reviewed transaction is signed |
| App | The swap proxy served the swap service's answers on the app's origin as given | High | Fixed: JSON only, its headers dropped, sandboxed |
| Swap | A race let two swaps leave wARG under-backed | Medium | Fixed: reserves stay held until the spend is recorded |
| Various | Lows: price floor, referral share, auto-lock, proxy 1xx and trailers, ledger on transport errors, page hardening, server timeouts | Low | Fixed |
| Bridge | A guardian request can be replayed to the other follower | Low | Open: griefing only (a failed round that is retried) |

### 2026-10-30: the mining pool

The pool (`cmd/argonmine-explorer/pool.go`) was added after launch. What
a reviewer should try to break, and what already holds:

- A share is one Argon2id evaluation on the server, like a solo
  submission: the same per-CPU slots, a per-client rate limit of 20 a
  second and at most 24 submissions per job bound the work a miner can
  cause. Duplicate nonces are refused.
- Pool jobs are checked by the browser exactly as solo jobs are, against
  the pool address the page fetched; a solo job offered as pool work, a
  pool job offered as solo work, a tampered block target or a share target
  harder than the block are all refused (tests in
  `browser_jobcheck_test.go`).
- The reward split uses integer arithmetic over the round's weights; the
  sum never exceeds the reward less the fee, and the remainder stays with
  the pool as fee. Orphaned blocks pay nothing and their shares are carried
  into the next round, so a reorganisation cannot credit coins that do not
  exist.
- Balances are deducted and the ledger written before a payout is
  broadcast; a crash in between can lose a record, never pay twice.
- Custody: the pool wallet holds matured rewards for at most one tick
  (30 seconds) before paying balances above the minimum. Its key lives on
  the explorer host as an environment variable, like the swap service's.
- The ledger is public (`/api/pool/ledger`) and every payout is on the
  chain from the pool address, so anyone can audit it.

## Known gaps (not bugs, but not done)

- No network-adjusted time; the node trusts the local clock for the 2-hour
  future bound. Operators must run NTP.
- P2P traffic is unencrypted and unauthenticated (like Bitcoin's v1 transport).
- Mempool does not re-add transactions from blocks that were reorganised away.
- Mainnet launched with one seed node (`altaria.proxy.rlwy.net:12347`); more
  seeds on other providers, and a DNS seed, come after launch.

## Deliverables requested from the reviewer

Report with severity-ranked findings, reproduction steps or test cases for
each, and explicit statements of what was covered and judged correct. We
publish the report alongside the fixes.
