# Argonmine consensus specification, version 1

Status: final. These are the rules of mainnet, which launched on 28 October
2026, 12:00 UTC with genesis block
`e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`. Every
rule here is consensus-critical unless marked *policy*. A change to a
consensus rule is a scheduled hard fork with a version bump, announced in
advance with an activation height.

Names: chain **Argonmine**, unit **ARG**, smallest unit "unit" (1 ARG = 100,000,000 units). Domain argonmine.com.

## 0. Honesty note on "no mistakes"

No non-trivial software can be proven free of bugs. What this project does instead:

1. Minimal consensus surface. No scripting language in v1. One signature scheme. One output type that moves value. Fewer rules means fewer places to be wrong.
2. Every rule written down here first, then implemented, then tested against the written rule.
3. Every known Bitcoin consensus bug or design regret (section 9) has a specific rule here that prevents it, and a test that exercises it.
4. Canonical, round-trip-exact serialization. Anything non-canonical is invalid.
5. Checked integer arithmetic everywhere money or time is handled.
6. A public testnet first (2026-10-26 to 2026-10-28), then review, then mainnet. The review before launch was done in house: four reviews, each with a second pass over its fixes, in place of an external review, which is planned once funded ([Security review](https://docs.argonmine.com/security-review)). The mainnet parameters were frozen at launch.

## 1. Chain parameters

| Parameter | Value | Note |
|---|---|---|
| Smallest unit | 1 unit = 1e-8 ARG | same as satoshi |
| MAX_MONEY | 21,000,000 × 1e8 = 2,100,000,000,000,000 units | fits in int64 with large margin |
| Initial block subsidy | 50 ARG | |
| Halving interval | 210,000 blocks | |
| Subsidy after 64 halvings | 0 | explicit, not shift-UB |
| Target block interval | 600 s | |
| Difficulty algorithm | ASERT (absolute exponential) | section 5 |
| ASERT half-life | 86,400 s (1 day) | faster response than BCH's 2 days, because app-miner hashrate is volatile |
| Proof of work | Argon2id over the 120-byte header | section 4 |
| Argon2id params | time=1, memory=8 MiB (8192 KiB), lanes=1, keyLen=32, salt=`POW_SALT` | consensus constants |
| Coinbase maturity | 100 blocks | |
| Max block serialized size | 1,000,000 bytes (incl. witnesses) | one simple limit, no weight |
| Max tx serialized size | 100,000 bytes (consensus) | |
| Max inputs per tx / outputs per tx | 10,000 / 10,000 | bounded, anti-DoS |
| Max future block time | 7,200 s ahead of the node's local clock (no network time adjustment; operators run NTP) | |
| Median time past window | 11 blocks | |
| Timestamp width | 64-bit unsigned seconds | no year-2106 problem |
| Nonce width | 64-bit | no extra-nonce in coinbase needed |
| Genesis coinbase | NOT added to UTXO set | explicit, unlike Bitcoin's accident |
| Premine | none | |
| Address format | bech32m, HRP `arg` (mainnet) `targ` (testnet) `rarg` (regtest) | |

## 2. Primitives

### 2.1 Hashes
- `H(tag, data)` = SHA-256( SHA-256(tag) || SHA-256(tag) || data ). This is the BIP-340 tagged hash. Every hash in the protocol has its own tag so that no two hash domains can collide.
- Tags: `Argonmine/block`, `Argonmine/txid`, `Argonmine/wtxid`, `Argonmine/merkle/leaf`, `Argonmine/merkle/node`, `Argonmine/sighash`, `Argonmine/prevouts`, `Argonmine/amounts`, `Argonmine/scripts`, `Argonmine/outputs`.
- Block hash (block identifier) = `H("Argonmine/block", header_bytes)`. This is NOT the proof-of-work hash.
- PoW hash = `Argon2id(password=header_bytes, salt=POW_SALT, t=1, m=8192 KiB, p=1, keyLen=32)`, interpreted as a 256-bit little-endian integer and compared to the target.

### 2.2 Integers and encoding
- Fixed-width integers are little-endian.
- `varint`: unsigned LEB128 (base-128, little-endian groups, continuation bit), maximum 9 bytes (≤ 2^63 − 1). Encoding MUST be minimal: a trailing group of `0x00` or any encoding longer than necessary is invalid. (Replaces Bitcoin's CompactSize, which has non-canonical forms.)
- Amounts are `int64` in units. Valid range for every single output and for every sum is `0 ≤ x ≤ MAX_MONEY`. Every addition is checked for overflow before being compared. Any out-of-range value anywhere makes the transaction invalid.
- Byte strings: `varint length || bytes`.

### 2.3 Merkle tree
- Leaf hash: `H("Argonmine/merkle/leaf", txid)` (or wtxid for the witness tree).
- Node hash: `H("Argonmine/merkle/node", left || right)`.
- If a level has an odd number of nodes, the last node is **promoted unchanged** to the next level (RFC 6962 style). Nothing is ever duplicated. Hence no two distinct transaction lists have the same root (CVE-2012-2459 impossible), and leaves can never be confused with internal nodes (64-byte confusion impossible).
- Empty tree is invalid (every block has ≥ 1 transaction).

### 2.4 Keys and signatures
- Curve secp256k1. Public keys are 32-byte x-only (BIP-340). Signatures are 64-byte BIP-340 Schnorr. Verification is exactly BIP-340; no other encodings exist.
- Batch verification is allowed in implementations but must give identical accept/reject results.

## 3. Data structures

### 3.1 Block header (120 bytes, fixed)
```
version        u32   consensus version, MUST be 1
prev_hash      32B   block hash of parent
tx_root        32B   merkle root over txids
witness_root   32B   merkle root over wtxids
time           u64   unix seconds
bits           u32   compact target (section 5.1)
nonce          u64
```

### 3.2 Transaction
```
version        u32          MUST be 1 (ARG only) or 2 (assets, section 3.5)
lock_height    u32          tx valid only in a block with height >= lock_height
inputs         varint n, then n × { prev_txid 32B, prev_index u32 }
outputs        varint m, then m × { amount i64, type u8, payload bytes(varint-len) [, asset 32B if version 2] }
issuances      version 2 only: varint k (0 or 1), then k × { asset 32B, issuer 32B, symbol bytes(varint-len), amount i64 }
witnesses      (n + k) × bytes(varint-len)  one per input, then one per issuance, in order
```
- `txid`  = `H("Argonmine/txid", serialization WITHOUT witnesses)`.
- `wtxid` = `H("Argonmine/wtxid", full serialization)`.
- `n ≥ 1`, `m ≥ 1`. (A tx with no inputs or no outputs is invalid.)
- Output types:
  - `0x00 P2PK`: payload = 32-byte x-only pubkey. Spent by a 64-byte Schnorr signature in the witness (section 6.2).
  - `0x01 NULLDATA`: payload ≤ 80 bytes, `amount` MUST be 0. Unspendable, never enters the UTXO set.
  - any other type: **invalid**. Upgrades are coordinated version bumps (hard forks), not anyone-can-spend soft forks. This is a deliberate trade: no silent anyone-can-spend footgun.
- Payload length for type 0 MUST be exactly 32; for type 1 MUST be ≤ 80. Anything else invalid.
- Decoders reject any output payload longer than 80 bytes and any witness longer than 100 bytes while parsing (no valid output or witness can be longer), so such a transaction fails §6.1 step 1 rather than a later step. The verdict is the same on every node.

### 3.3 Coinbase transaction
- Exactly one input with `prev_txid = 0x00…00` and `prev_index = 0xFFFFFFFF`.
- `lock_height` MUST equal the block height. This makes every coinbase txid unique across the chain (BIP-34 purpose) without any script.
- Its single witness is the "miner tag": arbitrary bytes, length ≤ 100. (It is in wtxid, not txid.)
- Sum of outputs ≤ subsidy(height) + total fees of the block (checked arithmetic).
- Coinbase outputs are spendable only at height ≥ creation height + 100.
- Only the first transaction of a block may be a coinbase; any other tx with a null prevout is invalid.

#### 3.3.1 Treasury
From the network's treasury height `TREASURY_HEIGHT` on, the coinbase MUST pay the treasury at least `Treasury(h)` in ARG, summed over its P2PK outputs whose payload is the treasury key `TREASURY_KEY` (any number of such outputs, anywhere in the coinbase):
```
Treasury(h) = 0                                  if TREASURY_HEIGHT = 0 or h < TREASURY_HEIGHT
            = Subsidy(h) × 100 / 10000          otherwise (1% of the subsidy, rounded down)
```
Fees are not part of it: they stay with the miner. The sum-of-outputs limit above still applies to the coinbase as a whole. A node from before this rule accepts every block that follows it (a coinbase may have several outputs), so the rule was added as a soft fork at mainnet height 300. By convention the treasury output is the coinbase's last output; consensus does not require an order.

### 3.4 Block
```
header         120 B
txs            varint count, then each transaction (full serialization)
```
- Total serialized size ≤ 1,000,000 bytes.
- `tx_root`      MUST equal merkle(txids),  `witness_root` MUST equal merkle(wtxids).
- No duplicate txid within a block. No two inputs anywhere in the block spend the same outpoint (CVE-2018-17144 class).

### 3.5 Assets (version-2 transactions)
A version-2 transaction can hold and move units of assets other than ARG,
and can create units of an asset with an issuance. ARG is never issued:
it comes from the coinbase only. Every existing block is a version-1
object and is unchanged by this section.

- Every output of a version-2 transaction carries a 32-byte `asset`. The
  zero hash is ARG. A version-1 output is ARG by definition (no field).
- `asset = H("Argonmine/asset", issuer_xonly_pubkey 32B || varbytes(symbol))`
  where `symbol` is 1-8 characters of `A-Z` and `0-9`. The id therefore binds
  the issuer key and the ticker; two issuers can use the same ticker and get
  two different assets.
- An issuance creates `amount` units of `asset` in this transaction. The
  transaction's last witness MUST be a 64-byte BIP-340 signature by `issuer`
  over `sighash(tx, 0xFFFFFFFF)`. At most one issuance per transaction.
- Amount bounds for non-ARG outputs and issuances: `1 ≤ amount ≤ 2^62`
  (MAX_ASSET_AMOUNT); per-asset sums are checked against the same bound.
  NULLDATA outputs are ARG only (amount 0).
- Conservation is per asset: for each non-ARG asset,
  `Σ outputs ≤ Σ spent inputs + issued`. The difference, if any, is burned.
  ARG keeps rule 6.2.3; asset units never count toward ARG and never pay
  fees. Fees are ARG only, so miners are paid in the coin they mine and
  tokens cannot spam the chain for free.
- A version-1 transaction MUST NOT spend an output whose asset is not ARG
  (it could not express the asset and would destroy it silently).
- The coinbase MUST NOT carry an issuance or a non-ARG output.
- Version-2 sighash: two further hashes are committed after `outputs`:
  `H("Argonmine/assets", all spent asset ids in order)` and
  `H("Argonmine/issuance", varint k || issuances serialized)`, so a signer
  cannot be misled about which asset it spends or creates.
- Asset units never enter the ARG supply figures; wallets and explorers
  account for them per asset id.

## 4. Proof of work
- A header is valid PoW iff `PoWHash(header) ≤ target(bits)` and `target(bits) ≤ POW_LIMIT`.
- `POW_LIMIT` (easiest allowed target) = compact `0x1E400000` = 2^238, i.e. an expected 2^18 = 262,144 Argon2id evaluations per block. This floor is deliberately not trivial: a header on a side chain costs an attacker real work, and a lone fast machine cannot mint the first blocks after launch in seconds, while a small honest network of ~1 kH/s still finds blocks in minutes. Testnet (`0x1F7FFFFF`, 512 hashes) and regtest (`0x207FFFFF`, ~2 hashes) have easier limits.
- `POW_SALT` = the ASCII bytes `Argonmine/pow/v1` (Argon2id requires a salt; it is a constant because the header carries all per-block entropy).
- Argon2id is memory-hard (8 MiB working set per hash attempt), so phones and laptops are competitive and ASICs gain little. Verification costs one Argon2id evaluation (~5–15 ms); headers-first sync verifies PoW before anything else and bans peers who send headers that fail PoW.

## 5. Difficulty: ASERT

### 5.1 Compact target
Same encoding as Bitcoin `nBits` (1-byte exponent, 3-byte mantissa) but strictly canonical:
- mantissa high bit (sign bit in Bitcoin) MUST be 0 → reject if set.
- mantissa MUST be minimal: `Compact(TargetFromCompact(bits)) == bits` must hold, else invalid.
- target `0` is invalid. Target > POW_LIMIT is invalid.

### 5.2 Next-block target
Let the anchor be the genesis block: `A_height = 0`, `A_target = TargetFromCompact(genesis.bits)`, and define the anchor's virtual parent time `A_ptime = genesis.time − 600` (the reference algorithm anchors on the parent time of the anchor block; genesis has no parent, so it is defined to have been exactly on schedule).
For a block at height `h` with parent `P` (height `h−1`):
```
time_delta   = P.time − A_ptime                     (parent timestamp minus anchor parent time; computed without overflow)
height_delta = (h − 1) − A_height = h − 1
num          = time_delta − 600 × (height_delta + 1)      saturated to ±(2^47 − 1)
exponent     = (num × 65536) / 86400                      // 16 fractional bits, division truncates toward zero (as in C++)
```
At height 1 with `P = genesis`, `num = 0` and the target equals `A_target` exactly.
Implementations may additionally saturate `time_delta` itself to ±(2^47 − 1) before the subtraction (the reference implementation does); every input where the two orders differ already clamps the result to `POW_LIMIT` or 1, so the outcome is identical.
Then `target = A_target × 2^(exponent / 2^16)`, computed exactly as in the aserti3-2d reference:
```
shifts = exponent >> 16            (arithmetic shift, i.e. floor)
frac   = exponent & 0xFFFF
factor = 65536 + ((195766423245049 × frac + 971821376 × frac² + 5127 × frac³ + 2^47) >> 48)
target = A_target × factor
shifts -= 16
if shifts < 0: target >>= −shifts else: target <<= shifts
if target == 0: target = 1
if target > POW_LIMIT: target = POW_LIMIT
bits = Compact(target)
```
- Because the schedule is absolute (anchored at genesis) there is no cumulative drift and no per-period off-by-one (Bitcoin's 2015-vs-2016 timewarp root cause does not exist).
- Regtest: difficulty is constant (`bits = POW_LIMIT_REGTEST`) to make tests deterministic.

### 5.3 Timestamp rules
- `time > MedianTimePast(previous 11 blocks)` (strictly greater).
- `time ≤ local adjusted time + 7,200 s` (nodes reject but do not ban; the block may become valid later).
- Together with ASERT, the attacker's ability to move difficulty by lying about time is bounded to the 2-hour window and decays with half-life.

## 6. Transaction validation

### 6.1 Stateless (no UTXO needed)
1. Canonical deserialization succeeds and re-serialization is byte-identical.
2. `version ∈ {1, 2}`; issuances and non-ARG outputs only in version 2.
3. `1 ≤ inputs ≤ 10,000`, `1 ≤ outputs ≤ 10,000`, size ≤ 100,000 bytes.
4. No two inputs reference the same outpoint.
5. Every ARG output: `0 ≤ amount ≤ MAX_MONEY`; type/payload rules of 3.2; `Σ ARG amounts ≤ MAX_MONEY` (checked add). Every non-ARG output and issuance: rules of 3.5, per-asset sums ≤ MAX_ASSET_AMOUNT (checked add). At most one issuance, and it must match `H(issuer || symbol)`.
6. Exactly one witness per input, plus one per issuance.
7. If not coinbase: no input has the null prevout. If coinbase: rules of 3.3.

### 6.2 Stateful (needs UTXO set, block height `h`)
1. `lock_height ≤ h`.
2. Every input's outpoint exists in the UTXO set (unspent), is not a NULLDATA output (they are never in the set), and if it is a coinbase output then `h ≥ its_height + 100`.
3. `Σ ARG input amounts ≤ MAX_MONEY` (checked), `fee = Σ ARG in − Σ ARG out ≥ 0`.
4. For each input `i` spending a P2PK output with pubkey `pk`: witness `i` MUST be exactly 64 bytes and a valid BIP-340 signature by `pk` over `sighash(tx, i)`.
5. Version 2 only: for every non-ARG asset, `Σ out ≤ Σ in + issued` (3.5); a version-1 transaction spending a non-ARG output is invalid.
6. Version 2 with an issuance: witness `n` MUST be a valid signature by the issuer over `sighash(tx, 0xFFFFFFFF)`.

Sighash (all-inputs, all-outputs; there is only one sighash type):
```
sighash(tx, i) = H("Argonmine/sighash",
    CHAIN_TAG                      32B  = genesis block hash  (replay protection across networks/forks)
 || version u32 || lock_height u32
 || H("Argonmine/prevouts",  all (prev_txid||prev_index) in order)
 || H("Argonmine/amounts",   all spent amounts i64 in order)
 || H("Argonmine/scripts",   all spent (type u8 || payload as a byte string: varint(len) || bytes) in order)
 || H("Argonmine/outputs",   all outputs serialized in order)
 || [version 2 only: H("Argonmine/assets", spent asset ids) || H("Argonmine/issuance", issuances)]
 || i  u32 )
```
Committing to all spent amounts and spent scripts means an offline signer cannot be tricked about fees (BIP-143 problem) and cannot be made to sign for a different output type.

## 7. Block validation (connect to chain tip)
1. Header: `version == 1`, `prev_hash` is the tip, PoW valid, `bits == ExpectedBits(parent)`, timestamp rules (5.3).
2. Size limit. First tx is coinbase, no other tx is coinbase.
3. Merkle roots match. No duplicate txids. No duplicate outpoints spent anywhere in the block.
4. Every tx passes 6.1; every non-coinbase tx passes 6.2 against a UTXO view that is updated **sequentially** (a tx may spend an output created earlier in the same block, never later).
5. Coinbase outputs sum ≤ `Subsidy(h) + Σ fees` (checked), and from the treasury height the coinbase pays the treasury at least `Treasury(h)` (3.3.1).
6. Apply: remove spent outputs, add new P2PK outputs (never NULLDATA; never genesis outputs). Write undo data so the block can be disconnected on reorg.

Fork choice: the valid chain with the greatest total work (Σ 2^256 / (target+1)) wins. Ties: first seen.

## 8. Subsidy
```
Subsidy(h): halvings = h / 210000
            if halvings >= 64: return 0
            return 50 × 1e8 >> halvings
```
Total emission is strictly below MAX_MONEY (equal to Bitcoin's 20,999,999.9769 ARG).

## 9. Bitcoin pitfalls and the rule here that prevents each

| Bitcoin issue | Year | Rule here |
|---|---|---|
| Value overflow, 184 billion BTC created (CVE-2010-5139) | 2010 | §2.2 checked arithmetic + per-output and per-sum MAX_MONEY, §6.1.5, §6.2.3 |
| OP_RETURN "always true" script bug, OP_CHECKMULTISIG off-by-one, SIGHASH_SINGLE bug, FindAndDelete quirks | 2010+ | No script language. §3.2 |
| Merkle duplicate-leaf collision (CVE-2012-2459) | 2012 | §2.3 promote-odd, tagged leaf/node hashes |
| Token layers bolted on later (coloured coins, Omni) that nodes do not validate, so a wallet bug can create or destroy tokens | 2013+ | §3.5: assets are consensus objects; per-asset conservation and issuer signatures are enforced by every node, ARG and tokens never mix, version-1 software cannot touch asset coins |
| 64-byte tx leaf/node confusion | 2017 (disclosed) | §2.3 domain separation |
| Transaction malleability (signature not in txid) | 2014 | §3.2 txid excludes witnesses; witness committed in header via `witness_root` |
| Non-strict DER / OpenSSL parsing fork risk (BIP-66) | 2015 | §2.4 fixed 64-byte Schnorr only |
| Duplicate coinbase txids (BIP-30/34) | 2012 | §3.3 coinbase `lock_height == height` |
| Duplicate-input inflation (CVE-2018-17144) | 2018 | §6.1.4 within tx, §3.4 across block, always checked |
| Timewarp: retarget over 2015 blocks and unconstrained period boundary | known since 2011 | §5 ASERT, absolute schedule, no periods |
| Time-based vs height-based locktime confusion, `nLockTime` 500M split | design | §3.2 `lock_height` only |
| 32-bit timestamp (year 2106) | design | §3.1 u64 time |
| 32-bit nonce forcing extra-nonce in coinbase | design | §3.1 u64 nonce |
| Genesis coinbase accidentally unspendable | 2009 | §1 made explicit |
| BDB lock exhaustion chain split (0.7 vs 0.8) | 2013 | §3.4 single explicit byte limit; storage layer never influences validity |
| Shift of subsidy by ≥ 64 is C undefined behaviour | design | §8 explicit 0 |
| Non-canonical CompactSize encodings | design | §2.2 minimal LEB128 only |
| Signer cannot see amounts (hardware wallet fee attack, fixed by BIP-143) | 2016 | §6.2 sighash commits to all spent amounts and scripts |
| Replay of signatures across forks / testnets | design | §6.2 `CHAIN_TAG` |
| Bech32 length-extension weakness | 2019 | §1 bech32m |
| Sign bit in compact target allows negative targets | design | §5.1 canonical compact, sign bit rejected |
| Unbounded orphan/mempool growth, inv flooding | ops | *policy*, node layer |

## 10. Policy (non-consensus, node defaults)
- Min relay fee 1 unit/byte; dust limit 546 units for ARG P2PK outputs; max standard tx 100,000 bytes; full RBF by fee; mempool cap 300 MB.
- Asset outputs have no dust limit (asset units have no ARG value). Instead every asset output beyond the first two in a transaction adds 546 units (`-assetoutputfee`) to the minimum relay fee, so an asset UTXO costs about what an ARG one does. A payment with its change, or one issuance output, pays only for its bytes.
- Chains of unconfirmed transactions: a transaction with all its unconfirmed ancestors, and every ancestor with all its unconfirmed descendants, at most 25 transactions and 101,000 bytes (Bitcoin's defaults). Making room in a full pool never evicts the newcomer's own ancestors. Block templates order transactions parents first, best fee rate first among those whose parents are placed.
- Rejections are remembered for 2 minutes and forgotten on every new tip. A failure that does not depend on the witness (dust, inputs, lock height, maturity, conflicts) is remembered by txid; one that does (signature, fee, size) only by wtxid, so a copy with a garbage witness cannot block the genuine transaction.
- Relay and connection limits are node policy too: [Run a node, "Connection limits"](../docs/NODE.md#connection-limits).

## 11. Networks
| | mainnet | testnet (retired) | regtest |
|---|---|---|---|
| HRP | `arg` | `targ` | `rarg` |
| P2P port | 9333 | 19333 | 29333 |
| RPC port | 9332 | 19332 | 29332 |
| magic | `41 52 47 4D` ("ARGM") | `54 41 52 47` ("TARG") | `52 41 52 47` ("RARG") |
| POW_LIMIT | compact 0x1E400000 (2^18 hashes) | compact 0x1F7FFFFF (512 hashes) | compact 0x207FFFFF, constant |
| TREASURY_HEIGHT | 300 | 0 (none) | 0 (none) |
| TREASURY_KEY | `7ad1a30d482a3fdb23efd2de4da69de2e9dc50d204d8cd82495183e6b3ffdf6c` (address `arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2`) | none | none |

## 12. Where this chain is better than Bitcoin (by design, v1)

| Area | Bitcoin | This chain |
|---|---|---|
| Who can mine | ASIC farms only; a phone earns nothing | Memory-hard Argon2id: phones and laptops are competitive, so the mining app is real mining |
| Difficulty response | Every 2016 blocks (~2 weeks); hashrate swings cause long stalls or block floods | ASERT, every block, 1-day half-life; absolute schedule with no drift |
| Timewarp attack | Possible (unfixed, mitigated by soft-fork proposals) | Impossible by construction |
| Tx malleability | Fixed late via segwit workaround | Native: witness never in txid, committed in header |
| Signatures | ECDSA with DER encoding quirks; Schnorr only via taproot | BIP-340 Schnorr only, fixed 64 bytes |
| Fee-visibility for hardware wallets | Required BIP-143/341 patches | Sighash commits to every spent amount and script from day one |
| Replay across forks | None | Chain tag in every signature |
| Merkle tree | Duplicate-leaf and 64-byte confusion flaws | Tagged, promote-odd tree; no known weakness |
| Timestamp / nonce | 32-bit, breaks in 2106; extra-nonce hack in coinbase | 64-bit both |
| Encoding | Non-canonical CompactSize accepted in places | Single canonical encoding; anything else is invalid |
| Script engine | ~5 historical consensus bugs, large attack surface | None in v1; value transfer is the only operation |
| Supply bugs | 2010 overflow created 184 billion BTC; 2018 inflation bug | Checked arithmetic and MAX_MONEY on every amount and every sum |
| Upgrade path | Anyone-can-spend soft forks (subtle security trade-offs) | Explicit versioned hard forks; unknown types are invalid |
| Address checksum | bech32 (length-extension weakness) | bech32m |

Candidate upgrades that need a product decision (not in v1): shorter block time for app feedback, larger block size, privacy (confidential amounts / ring signatures), a safe smart-contract layer (predicates, not a VM), light-client proofs, encrypted P2P.

## 13. Settled at launch, and known gaps

Settled and frozen at the mainnet launch:
- Names: chain Argonmine, unit ARG, HRP `arg` (§1, §11).
- Argon2id memory: 8 MiB per attempt (§1, §4), chosen for phone friendliness against verification cost.
- ASERT half-life: 1 day (§1, §5), chosen because app-miner hashrate is volatile.
- Genesis: mined for the launch moment, 28 October 2026, 12:00 UTC, hash `e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`. The difficulty schedule is anchored at its timestamp (§5.2).

Known gaps (not bugs, but not done), as listed in the [Security review](https://docs.argonmine.com/security-review):
- No network-adjusted time: a node trusts its local clock for the 2-hour future bound (§5.3). Operators must run NTP.
- Peer-to-peer traffic is unencrypted and unauthenticated, like Bitcoin's v1 transport.
- The mempool does not re-add transactions from blocks that were reorganised away (*policy*).
