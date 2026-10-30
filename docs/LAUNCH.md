# Mainnet launch runbook

## Status

**Mainnet launched on 28 October 2026, 12:00 UTC, with genesis hash
`e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`.** The
public testnet was retired the same day. This page is the record of the
launch, and the procedure for a coordinated restart with a new genesis
should one ever be needed.

The launch followed the procedure below, except for these points:

- **Review (step 1).** There was no external review. Four in-house reviews,
  each followed by a second pass over its fixes, took its place. Every High
  and Critical finding was fixed before launch; one Low finding is still
  open ([Security review](docs/SECURITY-REVIEW.md)). An external review is
  planned once funded.
- **Seed nodes (steps 4 and 7).** The launch ran with one seed node,
  `altaria.proxy.rlwy.net:12347`, built into `argonmined` as the default,
  instead of three, and without a DNS seed. More seeds and the DNS seed
  come after launch ([Seed nodes](deploy/README.md)).
- **Release (step 8).** The 1.0.0 archives for six platforms are published
  with a `SHA256SUMS` file ([Run a node, Download](docs/NODE.md#download)).
  Neither the programs nor the checksum file are signed yet; signing is
  planned ([Roadmap](docs/ROADMAP.md)).

## Procedure

The difficulty schedule (ASERT) is anchored at the genesis block's timestamp.
A genesis in the past keeps difficulty at the minimum until the chain catches
up; a genesis in the future makes mining impossible until then. So the
mainnet genesis time is set **once, at launch**, to the announced launch
moment. Everything below is in order; do not skip steps.

### T minus 2 weeks

1. Freeze consensus: the open items of the
   [Specification](docs/SPEC.md) (§13) resolved, external review findings closed
   ([Security review](docs/SECURITY-REVIEW.md)), the whole test suite and
   the fuzzers green on Linux, macOS and Windows.
2. Decide the launch moment `T` (unix seconds, UTC). Announce it publicly with
   the genesis message that will be embedded.
3. Final names: chain name, unit name, address prefix (HRP), ports, user
   agent. Changing the HRP after this point invalidates every printed
   address.
4. Stand up at least three seed nodes ([Seed nodes](deploy/README.md)) on
   testnet first; confirm they peer with each other.

### T minus 1 day

5. Mine the genesis block for `T` with the `genesis` tool:
   ```
   genesis -time <T> -message "<the announced message>" mainnet
   ```
   It prints the nonce and hash. Put `T` and the nonce into the mainnet
   parameters, leaving the testnet and regtest genesis times as they are
   (changing them would make a different testnet). Publish the hash.
6. The genesis test must pass: it checks the proof of work, the chain tag
   and the round trip of every genesis block.
7. Set the DNS seeds and the built-in seed list, and create the DNS
   records.
8. Build the release 1.0.0 for every platform. Publish the archives **and**
   `SHA256SUMS`; sign the checksum file with a key announced in advance.
9. Deploy the release to the seed nodes with `-network mainnet`. They will sit
   at height 0 and refuse to mine until `T` (block timestamps must exceed the
   genesis time).

### T

10. Start mining on at least one node you control so the first blocks arrive
    on schedule; stop when public hashrate takes over. Never premine: the
    genesis coinbase is unspendable by rule and the first real block is at
    height 1, after `T`.
11. Watch `getblockchaininfo` on every seed: heights equal, same best hash.
12. Start the explorer against a seed node; it also serves work to browser miners.

### T plus 1 hour and later

13. Check ASERT behaviour: `difficulty` should move smoothly toward the
    600-second target as hashrate arrives. If blocks are coming faster than one
    per minute for over an hour, something is wrong with the anchor; stop and
    investigate before more coins are issued.
14. Keep the signing key and seed node credentials offline. Rotate the RPC tokens.
15. Tag the release; from now on every consensus change is a scheduled
    hard fork with a version bump ([Specification](docs/SPEC.md), §3.2).

### Abort criteria

Abort before `T` if any of these holds: tests fail, a seed node is
unreachable, a review finding of High or Critical severity is open, or the
genesis hash in the release programs does not match the one you mined.
After `T`, a consensus bug means a coordinated restart with a new genesis;
announce it, do not patch silently.
