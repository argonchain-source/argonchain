# Vault (EVM side of the bridge)

`Vault.sol` holds the stablecoins that back uARG on Argonmine and pays them
out again for uARG burns with threshold guardian signatures. Read
[docs/BRIDGE.md](../docs/BRIDGE.md) first.

One vault is deployed per EVM chain (Ethereum, Base, BNB Chain, Arbitrum,
Polygon; testnets first). Each vault holds several allowlisted tokens, USDC
and USDT, and every token unit is one dollar, so uARG stays one dollar
whatever it was deposited as. A redemption names the token and the chain it
is paid out in; the vault on that chain pays from its reserve of that token.
The bridge takes a fee on each deposit and each redemption, which stays in
the vault as surplus above the backing (see "Fees, obligations, surplus").

## Contracts

| File | What it is |
|---|---|
| `Vault.sol` | the multi-token vault: allowlist, deposits, threshold releases, per-token release limits, fees and surplus, owner timelock, guardian pause, guardian veto with owner override, two-step ownership |
| `DepositRouter.sol` | deposit addresses: `DepositRouter` (one per chain, no owner) and the `Forwarder` it deploys with CREATE2 behind each address (see "Deposit addresses") |
| `MockUSDC.sol` | six-decimal ERC-20 anyone can mint, standard boolean returns |
| `MockUSDT.sol` | six-decimal ERC-20 anyone can mint, **non-standard like mainnet Tether**: `transfer`, `transferFrom` and `approve` return nothing |
| `MockUSD18.sol` | eighteen-decimal ERC-20 anyone can mint, standard returns; stands in for the BNB Chain stablecoins, which have 18 decimals |

The mocks are fine for local chains and public testnets, where the bridge
page offers their `mint` as a faucet (daemon flag `-token-mint`); they have no
value and must never be deployed to mainnet or stand behind anything real.

### USDT

Mainnet USDT on Ethereum does not return a boolean from `transfer`,
`transferFrom` or `approve`, so a caller compiled against the standard
`IERC20` reverts when it tries to decode the missing return value. The vault
therefore talks to tokens with a low-level `call` and accepts either no return
data or the boolean `true` (SafeERC20 semantics, without the library); any
other outcome reverts with `TransferFailed`. `MockUSDT` reproduces the missing
return values so the tests and the testnets exercise that path.

Mainnet USDT also refuses `approve(spender, n)` while the current allowance is
non-zero (it must be reset to 0 first). The vault is not affected, but the
bridge page should approve exactly the deposit amount, which `deposit`
consumes in full.

Decimals differ by chain: USDC and USDT have 6 decimals on Ethereum, Base,
Arbitrum and Polygon but 18 on BNB Chain. Minimums, amounts and the release
limits are in token units and set per token; the daemon reads `decimals()`
per token.

## Admin model

The owner can never move the backing directly: releases need `threshold`
guardian signatures, and `withdrawSurplus` is bounded by the surplus. What
the owner *can* do is change who the guardians are, which tokens are
accepted, the fees, the release limits and whether the vault is paused, and
a guardian set of the owner's choosing could release everything. So every one
of those changes is public and delayed:

| Action | Who | When |
|---|---|---|
| `pause()` | the owner **or any guardian** | at once |
| lift the pause: `proposeUnpause()`, then `executeUnpause()` | owner | after `delay`; proposable only while paused; a later `pause()` cancels a pending unpause |
| guardians and threshold: `proposeGuardians(address[] guardians, uint256 threshold)`, then `executeGuardians(...)` | owner | after `delay` |
| allow or disallow a token, its minimum deposit: `proposeToken(address token, bool allowed, uint256 minDeposit)`, then `executeToken(...)` | owner | after `delay`; at once with `setToken` before `finalizeSetup` |
| fees: `proposeFees(uint16 depositBps, uint16 releaseBps)`, then `executeFees(...)` | owner | after `delay`; at once with `setFees` before `finalizeSetup` |
| release limits: `proposeLimits(address token, uint256 maxRelease, uint256 periodLimit)`, then `executeLimits(...)` | owner | after `delay`; at once with `setLimits` before `finalizeSetup` |
| the delay itself: `proposeDelay(uint256 seconds)`, then `executeDelay(...)` | owner | after the current `delay`, raising and lowering alike |
| drop a pending proposal: `cancel(bytes32 id)` | owner | at once |
| veto a pending proposal: `veto(bytes32 id, bytes[] sigs)`, `threshold` guardian signatures over `vetoDigest(id)` | anyone, with the guardians' signatures | at once; not an owner override |
| propose again a change the guardians vetoed (an **owner override**) | owner | after `OVERRIDE_DELAY` = 60 days instead of `delay`; cannot be vetoed |
| `finalizeSetup()` | owner | once, for good |
| `withdrawSurplus(token, to, amount)` | owner | at once: it cannot reach the backing |
| `transferOwnership(to)`, then `acceptOwnership()` | owner, then the new owner | at once: the new owner faces the same timelock |

**The timelock.** `delay` is a constructor argument of at least `MIN_DELAY` =
1 day and at most `MAX_DELAY` = 30 days (`BadDelay` otherwise; the upper bound
keeps an absurd value from bricking the timelock). Each `propose*` checks its
arguments, stores `proposalEta[id] = now + delay`, emits `Proposed(id, data,
eta)` and returns `id`. The matching `execute*`, called by the owner with the
*same arguments*, works from `eta` until `eta + GRACE_PERIOD` (14 days),
checks the arguments again against the state at that moment, deletes the
proposal and emits `Executed(id)` followed by the change's own event
(`GuardiansSet`, `TokenSet`, `FeesSet`, `LimitsSet`, `DelaySet` or
`Paused(false)`). The errors are `NotProposed` (never proposed, cancelled,
already executed, or other arguments), `TooEarly`, `ProposalExpired` (an
expired proposal may be proposed again) and `AlreadyProposed` (the same
change is already pending). A pending proposal keeps its eta when the delay
changes. Only the owner proposes, executes and cancels; a new owner inherits
the pending proposals and decides on them.

**Proposal ids.** `id = keccak256(data)`, where `data` is the calldata of the
`execute*` call that applies the change (selector and ABI-encoded arguments).
Different kinds of change therefore never share an id, and the `data` in
`Proposed` decodes with the vault's ABI. Off-chain:

```
cast keccak $(cast calldata "executeFees(uint16,uint16)" 100 100)
```

**Watching.** Whoever watches `Proposed` events gets `delay` of notice before
any change takes effect: users can redeem, and any guardian can `pause()` at
once to stop releases while the owners sort it out. Lifting a pause always
takes a fresh `proposeUnpause` and a full delay, and every further `pause()`
cancels a pending unpause, so a compromised owner key cannot quickly undo a
guardian's pause. The other side of that: a single rogue guardian can keep
the vault paused until the owner rotates it out (one delay) and then unpauses
(another delay).

**The guardian veto and the owner override.** A threshold of guardians can
drop a pending proposal with `veto(id, sigs)` (anyone submits it), against a
stolen owner key. The signatures are over `vetoDigest(id)`, the EIP-191 hash
of `abi.encode("Argonmine/vault/veto", chainid, vault, id, proposalEta(id),
proposalNonce(id))`: it names the proposal instance pending now
(`proposalNonce(id)` counts the proposals of that id), so signatures made
for one proposal, public once used, never veto a later proposal of the same
change. A vetoed id is marked (`vetoed(id)`), and the owner's next proposal
of it is an **owner override** (`overrideOf(id)`): its eta is `now +
OVERRIDE_DELAY` (`2 × MAX_DELAY`, 60 days) and `veto` reverts on it with
`NotVetoable`. Executing or cancelling it clears the mark, and an expired one
proposed again is an ordinary proposal; a `pause()` still drops a pending
unpause, an override too. Ordinary proposals keep `delay` and stay
vetoable. So guardians can delay the owner but never freeze the vault: a
compromised threshold of guardians that vetoes every rotation and every
unpause is rotated out by an override after 60 days, then the vault is
unpaused (`test/ReviewFVeto.t.sol`). A guardian's veto is a check on a
stolen owner key for 60 days at most; the owner multisig remains the last
word.

**Setup.** A new vault starts in setup mode (`finalized()` is false): the
owner may also call `setToken`, `setFees` and `setLimits`, which apply at
once, to configure the deployment. `finalizeSetup()` (event
`SetupFinalized`) ends setup mode for good; afterwards those three revert with
`AlreadyFinalized` and every change goes through the timelock. The guardian
set has no setup shortcut (the constructor sets it), and neither does lifting
a pause. In setup mode the owner can disrupt the bridge (disallow a token,
raise fees within the bounds, set tight limits) but still cannot take the
backing; the daemon and the page should nonetheless treat a vault whose
`finalized()` is false as not ready for real funds.

**The owner must be a multisig.** The timelock buys time; it does not stop a
determined holder of the owner key. **Before the vault holds real funds,
transfer ownership to a multisig (a Safe, for instance) whose signers are
independent people holding their own keys**, none of them the hot key that
deploys contracts or pays release gas, with a threshold such that no single
signer or machine can act alone. Releases need no owner at all (anyone may
submit guardian-signed releases), so the daemon never needs the owner key.

## Release limits

Each token has two limits in its own units, on the gross amount, 0 meaning no
limit, so a six-decimal USDC limit and an eighteen-decimal BNB Chain USDT
limit are set separately:

- `maxRelease(token)`: the most a single `release` may pay;
- `periodLimit(token)`: the most all releases of that token may pay in one
  window of `PERIOD` = 1 day. Windows are fixed: one opens at the first
  release after the previous window ended (`periodStart(token)`) and lasts
  `PERIOD` seconds, and `periodReleased(token)` is what was released in it.
  As with any fixed window, up to twice the limit can go out around a window
  boundary.

A release past either limit reverts with `LimitExceeded` and changes nothing:
the burn stays payable, in a later window or once the limit is raised.
`periodAvailable(token)` is what can still be released in the current window
(`type(uint256).max` when the token has no period limit), so the daemon can
hold a payout back instead of sending a transaction that would revert.
Releases are counted in the window whether or not a limit is set, so a limit
set in the middle of a window sees what already went out in it. Limits are
set with `setLimits(token, maxRelease, periodLimit)` during setup and with
`proposeLimits`/`executeLimits` afterwards.

## Fees, obligations, surplus

The owner sets two fees in basis points (100 = one percent; each at most
`MAX_FEE_BPS` = 1000 = ten percent, else `BadFee`): with `setFees(depositBps,
releaseBps)` during setup and with `proposeFees`/`executeFees` afterwards.
Both default to 0 and the deploy script sets them to 100 each. **The release
fee may rise by at most `MAX_RELEASE_FEE_STEP_BPS` = 100 bps per change**
(`FeeStepTooLarge`), checked when proposing and again when executing against
the fee then in force; lowering it is not limited. A user who has already
burned uARG pays the release fee in force when the release lands, so a rise
is announced at least `delay` ahead and is at most one percentage point per
change. The deposit fee is known before depositing and has no step limit.
Fee math is integer division that floors, so rounding is always in the
user's favour: a fee on fewer than 100 units at 1 percent is zero.

**Deposits.** `deposit(token, amount, to)` pulls the gross `amount`. The
minimum deposit is checked on the gross. `fee = amount * depositFeeBps /
10000`, `net = amount - fee`. The guardians pay out `net` as uARG, the vault
adds `net` to `obligations[token]`, and the `Deposited` event carries `net` as
its `amount` and `fee` separately. Example at 1 percent: deposit 100 USDC,
receive 99 uARG; the vault holds 100, owes 99, surplus 1.

**Releases.** `release(token, to, amount, burnId, sigs)` takes the gross
`amount`, the value of the burn in token units, which is what the guardians
sign (`releaseDigest` is unchanged) and what the release limits count. `fee =
amount * releaseFeeBps / 10000`; `to` receives `amount - fee`;
`obligations[token]` drops by the full `amount` (floored at zero, since the
uARG it backed is gone). The `Released` event carries the gross `amount` and
the `fee`. Example at 1 percent on the vault above: burn 50 uARG, receive
49.5 USDC; the vault holds 50.5, owes 49, surplus 1.5.

**Obligations and surplus.** `obligations(token)` is the amount of `token`
that backs the uARG issued for it: the sum of net deposits minus the gross
releases. `surplus(token)` is `balanceOf(vault) - obligations(token)`, floored
at zero: the collected fees, plus anything sent to the vault outside
`deposit`. `reserve(token)` is the whole balance, obligations plus surplus.

**Withdrawing surplus.** `withdrawSurplus(token, to, amount)` (owner, not
timelocked) pays out at most `surplus(token)`, else `NoSurplus`; the backing
can never be withdrawn. It does not check the allowlist, so the surplus of a
token that has been disallowed (or of a token that was never allowed and was
sent to the vault by mistake) can still be swept. Fees change only for later
deposits and releases; the obligations already recorded are unaffected.

## Deposit addresses

A deposit through `deposit` needs a connected wallet that approves the vault
and then calls it. `DepositRouter.sol` removes that step: every Argonmine
address has its own **deposit address** on each chain, and a plain token
transfer to it, from any wallet or exchange, becomes a vault deposit credited
to that Argonmine address. The vault is unchanged; the router is a separate
contract deployed next to it, one per chain, bound to that chain's vault.

**The address.** `depositAddress(to)` is the CREATE2 address of a
`Forwarder` deployed by the router with salt `keccak256(bytes(to))`:

```
depositAddress(to) = address(uint160(uint256(keccak256(abi.encodePacked(
    bytes1(0xff), router, keccak256(bytes(to)), forwarderCodeHash())))))
```

`forwarderCodeHash()` is `keccak256(type(Forwarder).creationCode)` (the
forwarder has no constructor arguments). The address exists before anything
is deployed there, so the daemon can show it at once and compute it off-chain
from the router address and the code hash; reading `forwarderCodeHash()` from
the deployed router once is the safe way to get the hash, because it depends
on the exact compiled source. The salt is the exact bytes of `to`, so always
derive it from the canonical form of the address (lowercase bech32m): an
uppercase spelling is another deposit address. Different routers (one per
chain, at different addresses) give different deposit addresses for the same
`to`. `depositAddress` reverts with `BadDestination` for a `to` the vault
would refuse (empty or over 128 bytes).

**Sweeping.** `sweep(token, to)` deploys the forwarder on the first sweep of
that address, has it send its whole balance of `token` to the router
(`Forwarder.pull`, callable only by the router), approves the vault for
exactly that amount and calls `vault.deposit(token, amount, to)`. The vault
applies its allowlist, minimum (on the gross amount), pause and deposit fee as
for any deposit, and emits `Deposited` with `from` = the router; the router
then emits `Swept(token, forwarder, amount, depositId, to)` with the gross
amount and the vault's deposit id. Anyone may call `sweep` (in practice the
bridge leader, who pays the gas): it can only move what sits at the address
derived from `to` into the vault credited to that same `to`, so a third-party
sweep is harmless. There is no owner, no pause and no other entry point.

**When a sweep fails** it reverts as a whole, the forwarder's deployment
included, and the funds stay at the deposit address:

- `NothingToSweep`: the address holds none of `token`;
- the vault's `TokenNotAllowed`, `AmountTooSmall` or `IsPaused` (bubbled
  unchanged): a token off the allowlist waits until it is allowed; a balance
  below the minimum waits until more arrives, and the next sweep deposits the
  sum;
- `TransferFailed` (router or forwarder): the token call failed or returned
  `false`.

**What cannot be recovered.** Only allowlisted ERC-20 tokens can leave a
deposit address. The chain's native coin sent there is lost (the forwarder
has no `receive`), as is any token the vault never allows. Tokens sent to the
router itself stay there for good.

**Gas.** The first sweep of an address deploys its forwarder (760 bytes of
code); on anvil it used about 316,000 gas, a later sweep of the same address
about 100,000. USDT's approve rule is honoured: the router resets a non-zero
allowance to 0 before approving (the vault consumes each approval in full, so
the allowance is normally 0 already).

## Build, test, deploy (Foundry)

```
curl -L https://foundry.paradigm.xyz | bash && foundryup
cd contracts
forge install foundry-rs/forge-std       # the test library, into lib/ (not checked in)
forge fmt && forge test -vv              # runs test/Vault.t.sol and test/DepositRouter.t.sol
```

**Constructor.** `constructor(address[] guardians, uint256 threshold,
uint256 delay, uint256 firstDepositId)`: the guardians' EVM addresses
(distinct, non-zero), the release threshold (1 to the number of
guardians), the timelock delay in seconds (86400 to 2592000, that is 1 to
30 days) and the id of the vault's first deposit (1 to 2^62; reverts
`BadFirstDepositId` otherwise). The deployer becomes the owner and the
vault starts unpaused, in setup mode, with no token allowed, no fees and
no limits.

**`firstDepositId`.** The guardians name a deposit by its chain id and its
deposit id alone (in their state and in the deposit references every
issuance carries on Argonmine), so the ids of every vault ever used on a
chain must be distinct. The first vault on a chain uses 1. A vault that
**replaces** another on the same chain must start above every deposit id
the guardians have on record for the chain: the largest
`chains[].max_deposit_id` in `/api/status` of the guardians, plus one
(rounding up generously costs nothing but a byte or two of memo per
deposit). The guardians read `firstDepositId()` and refuse to scan or
issue anything of a replacement vault that does not (docs/BRIDGE.md,
"Replacing a vault").

**Setup sequence.** Configure, then finalize, then hand over:

1. Deploy the vault (and its `DepositRouter`).
2. Configure it at once while in setup mode: `setToken(token, true,
   minDeposit)` per token (the minimum in token units: 1000000 is one
   six-decimal USDC; use 10^18 on BNB Chain), `setFees(depositBps,
   releaseBps)` (the release fee in steps of at most 100 bps from 0, so
   `100` in one call but `200` takes two), and `setLimits(token, maxRelease,
   periodLimit)` per token with the caps you are comfortable with while the
   bridge is new.
3. Check the configuration (`tokens()`, `isAllowed`, `minDeposit`, the fees,
   `maxRelease`, `periodLimit`, `delay`, `guardianCount()`, `threshold()`),
   then call `finalizeSetup()`. From here on every change takes `delay`.
4. Transfer ownership to the multisig: `transferOwnership(multisig)` from
   the deployer, then `acceptOwnership()` from the multisig; check `owner()`.
   Do this before the vault holds real funds.

By hand:

```
forge create Vault.sol:Vault --rpc-url $RPC --private-key $DEPLOYER_KEY --broadcast \
  --constructor-args "[0xGuardian1,0xGuardian2,0xGuardian3]" 2 86400 1   # firstDepositId: 1 for the first vault on the chain
cast send $VAULT "setToken(address,bool,uint256)" $USDC true 1000000 --rpc-url $RPC --private-key $DEPLOYER_KEY
cast send $VAULT "setToken(address,bool,uint256)" $USDT true 1000000 --rpc-url $RPC --private-key $DEPLOYER_KEY
cast send $VAULT "setFees(uint16,uint16)" 100 100 --rpc-url $RPC --private-key $DEPLOYER_KEY
cast send $VAULT "setLimits(address,uint256,uint256)" $USDC 10000000000 50000000000 --rpc-url $RPC --private-key $DEPLOYER_KEY
cast send $VAULT "setLimits(address,uint256,uint256)" $USDT 10000000000 50000000000 --rpc-url $RPC --private-key $DEPLOYER_KEY
forge create DepositRouter.sol:DepositRouter --rpc-url $RPC --private-key $DEPLOYER_KEY --broadcast \
  --constructor-args $VAULT
cast send $VAULT "finalizeSetup()" --rpc-url $RPC --private-key $DEPLOYER_KEY
cast send $VAULT "transferOwnership(address)" $MULTISIG --rpc-url $RPC --private-key $DEPLOYER_KEY
# then acceptOwnership() from the multisig
forge verify-contract <address> Vault --chain <id> --etherscan-api-key $KEY
```

(The limits above are 10,000 and 50,000 six-decimal dollars; scale them by
10^12 for an eighteen-decimal token.) Afterwards, for example to raise the
release fee by one step: `proposeFees(100, 200)` from the multisig, wait
`delay`, then `executeFees(100, 200)` with the same arguments.

The deploy script `deploy/railway/bridge-chain.sh` deploys testnets and the
local anvil and records the addresses and fees for the Railway services:

```
deploy/railway/bridge-chain.sh check  sepolia     # chain, deployer, balance, fees, env file
deploy/railway/bridge-chain.sh deploy sepolia     # mocks + vault + setFees + router -> deploy/railway/chains/sepolia.env
deploy/railway/bridge-chain.sh router sepolia     # only the router, for a vault deployed before it; appends ROUTER, ROUTER_BLOCK
FEE_DEPOSIT_BPS=50 FEE_RELEASE_BPS=0 deploy/railway/bridge-chain.sh deploy anvil   # other fees
```

The script passes all four constructor arguments: the timelock delay from
`DELAY_SECONDS` and the first deposit id from `FIRST_DEPOSIT_ID` (default 1;
for a replacement vault, above every `max_deposit_id` the guardians
report), and checks `firstDepositId()` afterwards. A `FEE_RELEASE_BPS`
above 100 takes several `setFees` steps.

Chains: `sepolia`, `base-sepolia`, `bsc-testnet` (18-decimal USDT mock),
`arbitrum-sepolia`, `polygon-amoy`, `anvil`. The deployer is guardian1's EVM
key from `$KEYS/guardian1.json` and becomes the vault owner. The fees come
from `FEE_DEPOSIT_BPS` and `FEE_RELEASE_BPS` (default 100 each) and are
written to the env file. `deploy` also deploys the `DepositRouter` right
after the vault and writes `ROUTER` and `ROUTER_BLOCK` (its deployment block)
with the rest; `router <chain>` does only that for a chain whose env file
already exists, refusing if `ROUTER` is already there, and checks that the
new router's `vault()` reads back.

## ABI used by the daemon and the page

The daemon encodes calls by hand (`bridge/evm.go`) and the bridge page builds
the depositor's two transactions in the browser. Any change here must be
mirrored there.

| Item | Signature | Used by |
|---|---|---|
| constructor | `constructor(address[] guardians, uint256 threshold, uint256 delay, uint256 firstDepositId)`; owner = deployer; `delay` 1 to 30 days; `firstDepositId` 1 to 2^62 | the deploy script |
| deposit ids | `firstDepositId() returns (uint256)` (immutable), `depositCount() returns (uint256)`: the last id given out (`firstDepositId - 1` before the first deposit), `MAX_FIRST_DEPOSIT_ID()` = 2^62 | the daemon (a replacement vault's ids), the deploy script |
| deposit | `deposit(address token, uint256 amount, string to) returns (uint256 id)`; `amount` is gross | the page, after `approve(vault, amount)` on the token |
| Deposited | `Deposited(uint256 indexed id, address indexed token, address indexed from, uint256 amount, uint256 fee, string to)`; `amount` is the **net** credited as uARG, `fee` the part kept | the daemon's scanner |
| releaseDigest | `releaseDigest(address token, address to, uint256 amount, bytes32 burnId) returns (bytes32)` = `keccak256("\x19Ethereum Signed Message:\n32" ‖ keccak256(abi.encode(chainid, vault, token, to, amount, burnId)))`; `amount` is the gross burn value | what guardians sign |
| release | `release(address token, address to, uint256 amount, bytes32 burnId, bytes[] sigs)`; `amount` is gross, `to` receives `amount - fee`; reverts `LimitExceeded` past `maxRelease(token)` or `periodLimit(token)` | the leader |
| Released | `Released(bytes32 indexed burnId, address indexed token, address indexed to, uint256 amount, uint256 fee)`; `amount` is the **gross** burn value, `fee` the part kept | the daemon |
| released | `released(bytes32) returns (bool)` | the daemon, before and after a payout |
| tokens | `tokens() returns (address[])`: the allowed tokens in insertion order | the daemon and the page |
| isAllowed | `isAllowed(address) returns (bool)` | the daemon, before paying a redemption |
| minDeposit | `minDeposit(address) returns (uint256)` in token units, checked on the gross deposit | the page |
| TokenSet | `TokenSet(address indexed token, bool allowed, uint256 minDeposit)` | the daemon, to refresh its token list |
| tokens (admin) | `setToken(address token, bool allowed, uint256 minDeposit)` (owner, setup only); `proposeToken`/`executeToken` with the same arguments (owner, timelocked) | the deploy script, governance |
| fees | `depositFeeBps() returns (uint16)`, `releaseFeeBps() returns (uint16)`, `MAX_FEE_BPS() returns (uint16)` = 1000, `MAX_RELEASE_FEE_STEP_BPS() returns (uint16)` = 100; `setFees(uint16 depositBps, uint16 releaseBps)` (owner, setup only), `proposeFees`/`executeFees` with the same arguments (owner, timelocked); event `FeesSet(uint16 depositBps, uint16 releaseBps)` | the deploy script, the page (to show the net), governance |
| limits | `maxRelease(address) returns (uint256)`, `periodLimit(address) returns (uint256)`, `periodStart(address) returns (uint256)`, `periodReleased(address) returns (uint256)`, `periodAvailable(address) returns (uint256)`, `PERIOD() returns (uint256)` = 86400; `setLimits(address token, uint256 maxRelease, uint256 periodLimit)` (owner, setup only), `proposeLimits`/`executeLimits` with the same arguments (owner, timelocked); event `LimitsSet(address indexed token, uint256 maxRelease, uint256 periodLimit)` | the daemon (to hold payouts back), governance |
| obligations | `obligations(address token) returns (uint256)`: the units of `token` backing the uARG issued for it | the status API |
| surplus | `surplus(address token) returns (uint256)` = `max(token.balanceOf(vault) - obligations(token), 0)` | the status API, governance |
| withdrawSurplus | `withdrawSurplus(address token, address to, uint256 amount)` (owner, immediate), `amount <= surplus(token)`, allowlist not checked; event `SurplusWithdrawn(address indexed token, address indexed to, uint256 amount)` | governance |
| reserve | `reserve(address token) returns (uint256)` = `token.balanceOf(vault)` = obligations + surplus | the page and the status API |
| guardians | `guardians(uint256) returns (address)`, `guardianCount()`, `threshold()`, `isGuardian(address)`; `proposeGuardians(address[],uint256)`/`executeGuardians(address[],uint256)` (owner, timelocked); event `GuardiansSet(address[] guardians, uint256 threshold)` | the daemon's sanity checks, governance |
| pause | `paused() returns (bool)`; `pause()` (owner or any guardian, immediate); `proposeUnpause()`/`executeUnpause()` (owner, timelocked); event `Paused(bool paused)` | guardians, governance |
| timelock | `delay() returns (uint256)`, `MIN_DELAY()` = 86400, `MAX_DELAY()` = 2592000, `GRACE_PERIOD()` = 1209600, `OVERRIDE_DELAY()` = 5184000, `proposalEta(bytes32 id) returns (uint256)` (0 = none), `proposalNonce(bytes32 id)`, `vetoed(bytes32 id)`, `overrideOf(bytes32 id)`; `proposeDelay(uint256)`/`executeDelay(uint256)`, `cancel(bytes32 id)` (owner); `vetoDigest(bytes32 id) returns (bytes32)`, `veto(bytes32 id, bytes[] sigs)` (anyone, with `threshold` guardian signatures; errors `NotProposed`, `NotVetoable`, `BadSignatures`); events `Proposed(bytes32 indexed id, bytes data, uint256 eta)`, `Executed(bytes32 indexed id)`, `Cancelled(bytes32 indexed id)` (also on a veto), `DelaySet(uint256 delay)` | governance, monitoring |
| setup | `finalized() returns (bool)`, `finalizeSetup()` (owner, once); event `SetupFinalized()` | the deploy script, the daemon and the page (readiness) |
| ownership | `owner()`, `pendingOwner()`, `transferOwnership(address)`, `acceptOwnership()`; events `OwnershipTransferStarted(address indexed to)`, `OwnershipTransferred(address indexed to)` | governance |

Deposit ids start at `firstDepositId` and are one counter for the whole
vault, whatever the token. `to` is the bech32m Argonmine address, 1 to 128 bytes; the guardians
validate it. Guardian signatures are 65 bytes `r ‖ s ‖ v` with `v` 27 or 28
and low `s`, passed in strictly increasing signer-address order; the digest
commits to the token, so a signature over a USDC payout cannot release USDT.
`burnId` is the burn transaction's id on Argonmine, so a burn can be paid at
most once, in one token, on one chain (the digest also commits to the chain
id and the vault address). A release of a token that has been disallowed is
refused until the token is allowed again.

Function selectors of the admin calls:

| Function | Selector |
|---|---|
| `setToken(address,bool,uint256)` | `0x48c40d17` |
| `setFees(uint16,uint16)` | `0x9ef833d4` |
| `setLimits(address,uint256,uint256)` | `0xa08d5654` |
| `finalizeSetup()` | `0x36f107c1` |
| `finalized()` | `0xb3f05b97` |
| `pause()` | `0x8456cb59` |
| `proposeGuardians(address[],uint256)` / `executeGuardians(address[],uint256)` | `0x5bee5bac` / `0xf8e39e7b` |
| `proposeToken(address,bool,uint256)` / `executeToken(address,bool,uint256)` | `0x3075602b` / `0x76bbde88` |
| `proposeFees(uint16,uint16)` / `executeFees(uint16,uint16)` | `0x7122098d` / `0x91734abb` |
| `proposeLimits(address,uint256,uint256)` / `executeLimits(address,uint256,uint256)` | `0x17ca7a13` / `0xf1788193` |
| `proposeUnpause()` / `executeUnpause()` | `0xfae8a3d6` / `0x9bdfce98` |
| `proposeDelay(uint256)` / `executeDelay(uint256)` | `0x74dedf1c` / `0x9ab37dd0` |
| `cancel(bytes32)` | `0xc4d252f5` |
| `proposalEta(bytes32)` | `0x177e5a9f` |
| `delay()` | `0x6a42b8f8` |
| `maxRelease(address)` / `periodLimit(address)` | `0xecbef372` / `0xdf08c23b` |
| `periodStart(address)` / `periodReleased(address)` / `periodAvailable(address)` | `0x37425fa0` / `0xf063389a` / `0x130eec3d` |
| `transferOwnership(address)` / `acceptOwnership()` | `0xf2fde38b` / `0x79ba5097` |

Event topic0 hashes (`cast sig-event`), for the daemon's log filters:

| Event | topic0 |
|---|---|
| `Deposited(uint256,address,address,uint256,uint256,string)` | `0x4cda720afbfd50d696163bec1fdbf96c4bd80408e81d56942ed4341b63bb4f0b` |
| `Released(bytes32,address,address,uint256,uint256)` | `0x703d1f677dc7a240b8578621df4ac5a0d8323e943fcc50ce0ffd9dcc8f6974cc` |
| `FeesSet(uint16,uint16)` | `0x947504f5e2a48952ffa34f7cceb8582c6705286ad16842e7258c4207a40ce518` |
| `SurplusWithdrawn(address,address,uint256)` | `0x35325b58b52583a05e1085a13763f4b3400154b793dedbac7371ee26a8940043` |
| `TokenSet(address,bool,uint256)` | `0x96e8f5708e37de67c3a8a777620384984d6b87718b3c6378b70ba99bd56094c8` |
| `GuardiansSet(address[],uint256)` | `0x61c3ef76fc3a3e366dde2fc8881a1e9504f76f52692a251bd4b11d79d2fa69c4` |
| `LimitsSet(address,uint256,uint256)` | `0x95180f961a87161f1ee31a2e7f3e8f67999f6e8142fa725269143a2c9cf03012` |
| `Paused(bool)` | `0x0e2fb031ee032dc02d8011dc50b816eb450cf856abd8261680dac74f72165bd2` |
| `Proposed(bytes32,bytes,uint256)` | `0xbebd4e757b91b0f4e9671796f940352b8ce72b4878d18387feb19859ae5c9e25` |
| `Executed(bytes32)` | `0xa74c8847d513feba22a0f0cb38d53081abf97562cdb293926ba243689e7c41ca` |
| `Cancelled(bytes32)` | `0xbaa1eb22f2a492ba1a5fea61b8df4d27c6c8b5f3971e63bb58fa14ff72eedb70` |
| `DelaySet(uint256)` | `0x63e09f16584208fba1fc7ff64c62b00f07bec177c0d97ca6689891b1e77a35c7` |
| `SetupFinalized()` | `0xde59f2818f40d4bea5a9ae67aeb7fa6a9ae51c7618ff74510ddd52ffff8d21bc` |

Custom errors: `NotOwner` (`0x30cd7471`), `NotOwnerOrGuardian`
(`0x2c49223e`), `IsPaused` (`0x1309a563`), `NotPaused` (`0x6cd60201`),
`BadGuardians` (`0xccb720aa`), `BadToken` (`0x7401d931`), `TokenNotAllowed`
(`0xa29c4986`), `AlreadyReleased` (`0x63b4904e`), `BadSignatures`
(`0x42c8b573`), `BadDestination` (`0x83b1982b`), `AmountTooSmall`
(`0xc2f5625a`), `LimitExceeded` (`0x3261c792`), `TransferFailed`
(`0x90b8ec18`), `BadFee` (`0x917f1a53`), `FeeStepTooLarge` (`0xa4b757fa`),
`NoSurplus` (`0xc0ef17d3`), `BadDelay` (`0x0137f4a0`), `AlreadyFinalized`
(`0x475a2535`), `AlreadyProposed` (`0x79429186`), `NotProposed`
(`0xf2a87d5e`), `TooEarly` (`0x085de625`), `ProposalExpired` (`0x28a72379`).

### DepositRouter

| Item | Signature | Selector / topic0 |
|---|---|---|
| constructor | `constructor(address vault)`; reverts `BadVault` unless `vault` is a contract | |
| vault | `vault() returns (address)` | `0xfbfa77cf` |
| depositAddress | `depositAddress(string to) view returns (address)`; reverts `BadDestination` unless 1 <= `bytes(to).length` <= 128 | `0xe2e944b8` |
| sweep | `sweep(address token, string to) returns (uint256 id)`; `id` is the vault's deposit id | `0x7e456405` |
| forwarderCodeHash | `forwarderCodeHash() pure returns (bytes32)` = `keccak256(type(Forwarder).creationCode)` | `0x96beb9d7` |
| Swept | `Swept(address indexed token, address indexed forwarder, uint256 amount, uint256 depositId, string to)`; `amount` is **gross** (the vault's `Deposited` carries net and fee) | `0x22b568cad982e95de408058a83fdb34a9128f86950928c6a47b0e73d057e1b33` |
| Forwarder.pull | `pull(address token) returns (uint256 amount)`; only the router; sends the whole balance to the router | `0x52d11238` |
| Forwarder.router | `router() returns (address)` | `0xf887ea40` |

Router errors: `BadVault` (`0x5d1c3036`), `BadDestination` (`0x83b1982b`),
`NothingToSweep` (`0x351261fc`), `TransferFailed` (`0x90b8ec18`). Forwarder
errors: `NotRouter` (`0x91655201`), `TransferFailed`. A sweep the vault
refuses reverts with the vault's own error data (`TokenNotAllowed`,
`AmountTooSmall`, `IsPaused`).
