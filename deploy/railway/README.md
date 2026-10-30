# Argonmine on Railway

One Docker image (`deploy/railway/Dockerfile`) runs every public service. Each
Railway service sets `ARGONMINE_ROLE` to pick the program, and
`deploy/railway/start.sh` turns the variables below into command-line flags.

## Services

| Service | Role | Exposure | Storage |
|---|---|---|---|
| `node` | `argonmined` seed node | TCP proxy on port 19333 (P2P; the mainnet node runs with `-listen :19333` so the proxy needs no change). RPC on port 19332 is reachable only on the private network as `node.railway.internal`. | volume at `/data` |
| `explorer` | `argonmine-explorer` and the work server for browser miners | public domain, HTTP on `$PORT` | volume at `/data` (index) |
| `web` | `argonmine-web`: the browser app at `/` and the docs at `/docs` | public domain, HTTP on `$PORT` | none |

The deployment, created on 2026-10-26 for the testnet and moved to mainnet
on 2026-10-28 at the launch (the testnet is retired; the bridge services wait
for their mainnet vaults, see MAINNET.md 1.6):

| | |
|---|---|
| Project | `argonmine` (Railway project id `d3f2c6d3-0486-46ef-abf5-845a493605ce`) |
| Website, app, docs | https://argonmine.com, https://app.argonmine.com, https://docs.argonmine.com (see Domains) |
| Explorer | https://explorer.argonmine.com (also https://explorer-production-43ea.up.railway.app) |
| Bridge | https://bridge.argonmine.com (also https://bridge-production-0393.up.railway.app) |
| Swap service | private only, `http://swap.railway.internal:8410`; the app reaches it at `/swap/api/` |
| Seed node (P2P) | `altaria.proxy.rlwy.net:12347` (this is the entry in `defaultSeeds["mainnet"]` of `cmd/argonmined` and `cmd/argonmine-app`) |

## Domains

argonmine.com was bought on 2026-10-27; its DNS is at Hostinger. One `web`
service answers four names and tells them apart by host; the explorer and
the bridge have one name each. Railway issues each name's TLS certificate
once its record resolves; the `*.up.railway.app` names keep working (the
web one shows the landing page and sends app pages to the app).

| Name | Service | Port | Record at Hostinger | Serves |
|---|---|---|---|---|
| `argonmine.com` | `web` | 8370 | CNAME `@` → `s22r3eju.up.railway.app` | the landing page; app pages redirect to `app.`, `/docs/...` to `docs.` |
| `www.argonmine.com` | `web` | 8370 | CNAME `www` → `udr8qifm.up.railway.app` | redirects to `argonmine.com` |
| `app.argonmine.com` | `web` | 8370 | CNAME `app` → `vk46umwd.up.railway.app` | the app: Mine, Wallet, History, Settings |
| `docs.argonmine.com` | `web` | 8370 | CNAME `docs` → `1togl3x9.up.railway.app` | the docs at `/<page>`; `/docs/<page>` redirects there |
| `explorer.argonmine.com` | `explorer` | 8380 | CNAME `explorer` → `ni02zwgm.up.railway.app` | blocks, addresses, the wallet API and the browser miners' work server |
| `bridge.argonmine.com` | `bridge` | 8395 | CNAME `bridge` → `6dhyuvgh.up.railway.app` | the bridge page and its API |

Variables of `web` (set 2026-10-27): `SITE_URL=https://argonmine.com`,
`APP_URL=https://app.argonmine.com`, `DOCS_URL=https://docs.argonmine.com`,
`SITE_DOMAIN=argonmine.com`, `EXPLORER_URL=https://explorer.argonmine.com`,
`BRIDGE_URL=https://bridge.argonmine.com`. The explorer's `ARGONMINE_ARGS`
lists the pages whose browser miners may submit blocks in `-allow-origin`
(today `https://argonmine.com,https://www.argonmine.com,https://app.argonmine.com,https://web-production-acb53.up.railway.app`;
only `app.` mines now). A page on a name missing there has its blocks refused.

```
railway domain app.argonmine.com -s web -p 8370      # add a name
railway domain status app.argonmine.com -s web       # propagation and certificate
```

A wallet is kept by the browser per site name: one opened on another name is
opened on `app.argonmine.com` by logging in with the same 24 words.

The seed node's TCP proxy cannot carry a custom domain; a CNAME `seed` →
`altaria.proxy.rlwy.net` (port 12347) can name it for `defaultSeeds` later.

## Variables

Set on every service: `RAILWAY_DOCKERFILE_PATH=deploy/railway/Dockerfile` and
`ARGONMINE_ROLE`.

| Variable | Used by | Meaning |
|---|---|---|
| `ARGONMINE_NETWORK` | node, explorer, swap, bridge | `mainnet` (default) or `testnet` |
| `RPC_TOKEN` | node, explorer | shared bearer token for the node RPC; same value on both services |
| `RPC_PORT` | node | RPC listen port, default 19332 |
| `NODE_RPC` | explorer | `http://node.railway.internal:19332` |
| `PORT` | explorer, web | HTTP port Railway routes the public domain to |
| `EXPLORER_URL` | web | public explorer URL (the app fetches work, balances and coins from it and broadcasts payments through it) |
| `SITE_DOMAIN` | web | hostname shown on the site, `argonmine.com` |
| `SWAP_UPSTREAM` | web | the swap service on the private network, `http://swap.railway.internal:8410`, served at `/swap/api/` |
| `ARGONMINE_HRP` | web | address prefix, `arg` for mainnet (default), `targ` for testnet |
| `BRIDGE_URL`, `APP_URL`, `SITE_URL`, `DOCS_URL` | web | bridge link; the app's, the website's and the docs' own hosts (see Domains) |
| `ARGONMINE_SEEDS`, `ARGONMINE_DNSSEED` | node | optional extra seeds |
| `ARGONMINE_ARGS` | all | optional extra flags appended verbatim |

## Deploying

The project is linked to this directory with the Railway CLI (`railway status`).
Build and deploy from the working tree:

```
railway up -s node -d
railway up -s explorer -d
railway up -s web -d
```

`.railwayignore` keeps `build/`, `release/`, `_attic/` and the Foundry
artifacts out of the upload. The Dockerfile must not contain a `VOLUME`
instruction (Railway rejects it); volumes are attached to the service instead:

```
railway volume -s <service id> -e <environment id> add -m /data
railway domain -s explorer -p 8380      # a generated *.up.railway.app name
railway domain -s web -p 8370
railway tcp-proxy create --port 19333 -s node
```

The volume command needs ids, not names (`railway service list --json`,
`railway environment list --json`).

## Monitoring

Nothing pages anyone today unless a monitor is set up. A free uptime
service (UptimeRobot, Better Stack or similar: free plans check every few
minutes and send an email or a phone notification) needs only these
addresses:

| Check | Address | Down means |
|---|---|---|
| Chain and explorer | `https://explorer.argonmine.com/api/health` | 503 with the reasons: the node unreachable, the index behind the node, or no block for an hour (the chain has stopped: start a miner) |
| App | `https://app.argonmine.com/` | the website service is down |
| Bridge | `https://bridge.argonmine.com/api/status` | the leader guardian is down (look also at `last_error`, `paused` and `stuck` in the answer) |
| Landing page | `https://argonmine.com/` | the website service is down |

Set the explorer check to alert on any status other than 200. On the
testnet the chain stops whenever nobody is mining, so its alert is the
signal to start a miner.

## Bridge

The stablecoin bridge ([docs/BRIDGE.md](../../docs/BRIDGE.md)) runs as three
guardian services from the same image (`ARGONMINE_ROLE=bridge`), each with
its own key and volume. It accepts USDC and USDT on several EVM chains, one
vault per chain, and pays out on the chain and in the token the user
names. On the testnets the tokens are the mocks in `contracts/`
(`MockUSDC`, `MockUSDT`, `MockUSD18`), so the bridge page offers a faucet
per token.

| Service | Role | Exposure | Storage |
|---|---|---|---|
| `bridge` | guardian 1 and leader: scans every chain, batches issuances, collects release signatures and sends the releases from its gas key (`GAS_KEY`), serves the bridge page | public domain, HTTP on `$PORT` (8395) | volume at `/data` |
| `bridge-g2`, `bridge-g3` | guardians 2 and 3, followers | private network only (`bridge-g2.railway.internal:8395`, `bridge-g3.railway.internal:8395`) | volume at `/data` |

Created on 2026-10-26: `bridge` (`89fa6650-3fd9-43d5-9905-4d3da18d0e2b`),
`bridge-g2` (`f5e13782-3f80-4611-9e0b-3ec7ded5f364`), `bridge-g3`
(`e2c1aee3-3656-419e-9d2c-38248ea799ee`). The leader's domain is
https://bridge-production-0393.up.railway.app and the `web` service points
at it through `BRIDGE_URL`. The guardian keys came from `argonmine-bridge
keygen` and are kept off the repository; the guardians' joint custody
address on the testnet is
`targ1q9yf56tjyyyqh85ulcd7l4uh6238wedugj7zqwuthqxs86rqwe6ssech8ss`. The
leader's EVM address, which deploys every vault and pays every release, is
`0x084A0c27404CaA063D077680E7A041A572ceb0dC`.

The first deployment of 2026-10-26 was a single-token vault on Sepolia
(`0x913d757E2f3F5E2d5994e20860062Ad2c8C20019` holding MockUSDC
`0xD76B4777C17aBB9413714d53dE8b17153900C2E9`). It is superseded by the
multi-token vaults below and is no longer scanned.

### Contracts per chain

`deploy/railway/bridge-chain.sh deploy <chain>` deploys the mock tokens, the
hardened Vault and its DepositRouter to one chain (`sepolia`,
`base-sepolia`, `bsc-testnet`, `arbitrum-sepolia`, `polygon-amoy`, or
`anvil` for a local chain), then, while the vault is in its setup phase,
allows both tokens (`setToken`), sets the fees (`setFees`) and each
token's release limits (`setLimits`), calls `finalizeSetup()` (from then
on tokens, fees and limits change only through the vault's timelock), and
hands the vault to `OWNER` if it is set (`transferOwnership`; the new
owner then calls `acceptOwnership()`, which the script does itself on
anvil). It prints what it did and writes
`deploy/railway/chains/<chain>.env` with `NAME`, `CHAIN_ID`, `RPC`,
`PUBLIC_RPC` (the same keyless public endpoint), `EXPLORER`,
`NATIVE_SYMBOL`, `VAULT`, `USDC`, `USDC_DECIMALS`, `USDT`,
`USDT_DECIMALS`, `START_BLOCK`, `MINT`, `ROUTER`, `ROUTER_BLOCK`, and, as a
record, `FEE_DEPOSIT_BPS` and `FEE_RELEASE_BPS` (100 each: the 1 percent
the vault keeps on deposits and on payouts,
[docs/BRIDGE.md](../../docs/BRIDGE.md), "Fees"), `DELAY_SECONDS`, the
limits, `VAULT_OWNER` and `FIRST_DEPOSIT_ID`.

| Variable of `bridge-chain.sh` | Meaning |
|---|---|
| `KEYS` | directory with `guardian1.json` … `guardian3.json`; guardian 1's EVM key deploys and is the first owner |
| `DELAY_SECONDS` | the vault's timelock delay (default `172800`, 2 days, for the testnets; **604800 (7 days) or more on mainnet**; the vault accepts 1 to 30 days). Every later change of guardians, tokens, fees, limits or the delay, and lifting a pause, waits that long after it is proposed |
| `FEE_DEPOSIT_BPS`, `FEE_RELEASE_BPS` | fees in basis points (default 100 each, at most 1000); a release fee above 100 is set in steps of 100 |
| `MAX_RELEASE_USDC`, `MAX_RELEASE_USDT` | the vault's cap on one release, whole tokens (default 10000; 0 = none) |
| `PERIOD_LIMIT_USDC`, `PERIOD_LIMIT_USDT` | the vault's cap on the releases of one 24-hour window, whole tokens (default 100000; 0 = none). Both limits are converted with the decimals the token reports on chain |
| `FIRST_DEPOSIT_ID` | the id of the vault's first deposit (default `1`, for the first vault on a chain). A vault that **replaces** another on the same chain must start above every deposit id the guardians have on record for it: the largest `chains[].max_deposit_id` in `/api/status` of the three guardians, plus one (round up generously); the guardians refuse a replacement that does not ([docs/BRIDGE.md](../../docs/BRIDGE.md), "Replacing a vault") |
| `OWNER` | optional: the multisig the vault is handed to after the setup |
| `ANVIL_RPC` | the RPC of the `anvil` chain (default `http://127.0.0.1:8545`) |
| `ENV_DIR` | where `<chain>.env` is written (default `deploy/railway/chains`) |
| `FOUNDRY`, `BSC_GAS_PRICE_WEI`, `BSC_DEPLOY_RPC` | as before: Foundry's directory; BNB Chain's legacy gas price and deployment RPC |

`bridge-chain.sh check <chain>` shows the deployer's balance and, for a
chain already deployed, the vault's owner (and pending owner), whether the
setup is finalized, its delay, whether it is paused, its guardians,
threshold and fees, and per token whether it is allowed, its minimum,
`maxRelease`, `periodLimit` and `periodAvailable` (a vault from before the
hardened contract answers `n/a` for what it lacks). The two fee
keys are a record of what was deployed: `chains.sh` does not copy them
into the chains document, because the daemon reads the fees from the
contract on every scan and shows them in `/api/status` as
`deposit_fee_bps` and `release_fee_bps`; changing a fee is an owner
transaction on the vault, not a variable. `bridge-chain.sh check <chain>`
shows the deployer's native balance first. The current addresses are
those files.

`deploy/railway/chains.sh [chain ...]` turns the env files into the chains
document the daemon reads (the `-chains` file: every chain with its vault,
start block, confirmations and tokens), on one line, ready for the
`CHAINS_JSON` variable. With no arguments it takes every `chains/*.env`
alphabetically; name the chains to fix the order. **The first chain's
first token (USDC) is what a short `BRG1` memo pays out**, so name
`sepolia` first. Optional keys in an env file: `CONFIRMATIONS` (default
12), `PUBLIC_RPC` (what browsers get when `RPC` carries an API key),
`USDC_MIN_DEPOSIT` and `USDT_MIN_DEPOSIT` in token units (default 1 token),
`ROUTER` and `ROUTER_BLOCK`: the chain's `DepositRouter` and its
deployment block, which `bridge-chain.sh deploy` (or `router` for an
existing vault) writes, `L1_START_HEIGHT`: the first Argonmine height
whose burns the chain's vault pays (see "Replacing a vault" below), and
for the second security pass ([docs/BRIDGE.md](../../docs/BRIDGE.md),
"Finality and providers", "Release transactions", "Per-deposit cap"):
`RPCS` (more RPC providers, comma-separated; deposits are acted on only
when all of them and `RPC` agree; use providers of different companies),
`FINALITY` (`finalized` or `depth`; the default by chain id is
`finalized` on Sepolia, Base Sepolia, BNB testnet, Arbitrum Sepolia and
Amoy), `MAX_GAS_PRICE_GWEI` (cap on what a release pays per gas, fee
raises included), `MIN_GAS_BALANCE` (gas key balance, in the native coin,
below which the status warns) and `USDC_MAX_DEPOSIT` / `USDT_MAX_DEPOSIT`
(per-deposit cap in token units: larger deposits are held for the
guardians). `PUBLIC_RPC` is now the only RPC the status shows: without it
a chain has none in `/api/status`, never `RPC`. `chains.sh` copies them
into the document as `router`, `router_block`, `l1_start_height`, `rpcs`,
`finality`, `max_gas_price_gwei`, `min_gas_balance`, `max_deposit` and
`public_rpc`; a chain with a router gets **deposit
addresses** (users send USDC or USDT from any wallet or exchange to an
address of their Argonmine address, and the leader sweeps it into the
vault; [docs/BRIDGE.md](../../docs/BRIDGE.md), "Deposit addresses"), and a
chain without `ROUTER` simply has none. Git Bash and `python3` are enough;
no `jq`.


### Deployed vaults (testnets, 2026-10-26)

From `deploy/railway/chains/*.env`; every vault is owned by the leader guardian's key and allows both mock tokens (public `mint`). **These vaults predate the hardened `contracts/Vault.sol`** (no timelock, setup finalization or per-token release limits; the daemon reads their missing views as "no limit"): redeploy each with `bridge-chain.sh deploy` before launch, following "Replacing a vault" below.

| Chain | Vault | USDC (decimals) | USDT (decimals) | Start block | Fee deposit / release |
|---|---|---|---|---|---|
| Arbitrum Sepolia (421614) | `0x81FBF76bFdC8e4a5F0c447D43a7032de3753d317` | `0xD76B4777C17aBB9413714d53dE8b17153900C2E9` (6) | `0x913d757E2f3F5E2d5994e20860062Ad2c8C20019` (6) | 323082008 | 1.0% / 1.0% |
| Base Sepolia (84532) | `0x81FBF76bFdC8e4a5F0c447D43a7032de3753d317` | `0xD76B4777C17aBB9413714d53dE8b17153900C2E9` (6) | `0x913d757E2f3F5E2d5994e20860062Ad2c8C20019` (6) | 50560526 | 1.0% / 1.0% |
| BNB Chain Testnet (97) | `0x5eECDCD8d10d7065955D6C6f2a10d8F318D36524` | `0x913d757E2f3F5E2d5994e20860062Ad2c8C20019` (6) | `0x81FBF76bFdC8e4a5F0c447D43a7032de3753d317` (18) | 139074721 | 1.0% / 1.0% |
| Ethereum Sepolia (11155111) | `0xE77DbB4d59c67e85a7D8497c59d6Fe0D919FBE3f` | `0x9fFb481eAAe54296B9B4EC1Ae58564584e42B6F6` (6) | `0xb5A8458072817cb2fA7add0f8Da310D3439b728e` (6) | 11999334 | 1.0% / 1.0% |

### Bridge variables

Besides `RAILWAY_DOCKERFILE_PATH`, `ARGONMINE_ROLE=bridge`,
`ARGONMINE_NETWORK`, `NODE_RPC`, `RPC_TOKEN` and `PORT` as above:

| Variable | Meaning |
|---|---|
| `GUARDIAN_KEY` | the JSON written by `argonmine-bridge keygen`, on one line; `start.sh` writes it to `/data/guardian.json` with mode 0600. Different on each service. |
| `GAS_KEY` | leader only, **required**: the JSON written by `argonmine-bridge gaskey` (or any JSON with `evm_private`), on one line; `start.sh` writes it to `/data/gas-key.json` (mode 0600), passes `-gas-key` and removes it from the environment. The key that sends every release and sweep: never a guardian key or a vault owner (the daemon refuses both), with a small balance of native gas on every chain; `/api/status` shows its address (`gas_address`) and per chain its balance (`gas_balance`, `gas_low`) |
| `GUARDIANS` | the three guardians' L1 public keys, comma-separated; the same on every service |
| `ARGONMINE_BRIDGE_SECRET` | shared secret of the guardian-to-guardian API, **at least 32 characters** (`openssl rand -hex 32`); the same on every service. `start.sh` writes it to `/data/bridge-secret` (mode 0600), passes `-secret-file`, and removes it, `GUARDIAN_KEY` and `CHAINS_JSON` from the daemon's environment. It is a second layer only: the followers take requests signed by the leader's key and nothing else |
| `BRIDGE_MODE` | `leader` on `bridge`, `follower` on the others |
| `PEERS` | leader only: `http://bridge-g2.railway.internal:8395,http://bridge-g3.railway.internal:8395` |
| `LEADER_PUBKEY` | followers only, **required**: the leader guardian's L1 public key (the one in `bridge`'s `GUARDIAN_KEY`, one of `GUARDIANS`). A follower signs only requests signed by this key; a follower without it does not start |
| `MAX_ISSUE`, `MAX_ISSUE_DAY` | the most uARG one issuance creates and the most a guardian signs over any 24 hours (defaults `10000` and `100000`; `0` = no limit). Set the same values on all three services. A deposit above `MAX_ISSUE` waits, with the reason in its `error` and counted in `held` in `/api/status`; one that would pass the daily limit waits until the window frees up |
| `CHAINS_JSON` | the chains document on one line, the output of `deploy/railway/chains.sh sepolia …`; the same on every service. `start.sh` writes it to `/data/chains.json` with mode 0600 and passes `-chains /data/chains.json`. It carries, per chain, the RPC URL, explorer, native symbol, vault, start block (the vault's deployment block: public RPCs do not serve older logs) and confirmations, and per token the address, decimals, `mint` (the page shows a "Get 100 test USDC" faucet) and `min_deposit`. |
| `ASSET_SYMBOL` | `UARG` (the default) |
| `THRESHOLD` | `2` signatures to release from a vault |
| `L1_CONFIRMATIONS` | `6` on the testnet: the floor of every burn's payout depth; the daemon's default is 30. EVM confirmations are per chain in `CHAINS_JSON` (12 from `chains.sh`). |
| `L1_CONFIRMATION_TIERS` | the payout depth by burn size, `amount:blocks` in uARG (default `1000:6,10000:30,100000:120`; set it empty to use `L1_CONFIRMATIONS` for every burn). A burn above the last tier waits for `argonmine-bridge approve-release` on every guardian. The same on all three services |
| `L1_REORG_PAUSE` | an Argonmine reorganisation undoing more than this many blocks pauses every issuance and release on that guardian until its operator resumes it (default `3`; `0` = never) |
| `RELEASE_TIMEOUT`, `ISSUE_REBROADCAST`, `STUCK_AFTER` | a release not mined after `RELEASE_TIMEOUT` (`5m`) is sent again with the same nonce at fees 12.5 percent higher; an issuance missing from the node's mempool is broadcast again after `ISSUE_REBROADCAST` (`10m`); anything waiting longer than `STUCK_AFTER` (`1h`) is listed under `stuck` in `/api/status` and logged once an hour |
| `ISSUE_INTERVAL`, `SCAN_INTERVAL` | `1m` and `10s` |

The single-chain variables of the first deployment (`EVM_RPC`, `VAULT`,
`TOKEN`, `EVM_START`, `TOKEN_DECIMALS`, `TOKEN_SYMBOL`, `TOKEN_MINT`,
`MIN_DEPOSIT`, `EVM_CONFIRMATIONS`) are gone; everything they carried is in
`CHAINS_JSON`. Delete them from the services.

### Going live

Everything above is in place; what remains needs money and is done by hand.

1. Gas on every chain: the leader guardian's EVM address
   `0x084A0c27404CaA063D077680E7A041A572ceb0dC` deploys each vault (and
   is its first owner), so it needs a little of the native coin to deploy;
   the **gas key** (`argonmine-bridge gaskey gas.json`, set as `GAS_KEY` on
   `bridge`) pays every release and sweep afterwards, so it needs a little
   on each chain the bridge serves: Sepolia ETH, Base Sepolia ETH, tBNB on
   the BNB testnet, and so on; about 0.05 of each is plenty, and
   `gas_low` in `/api/status` says when to top it up. The followers only
   sign and need no gas. Check with `KEYS=<directory with
   guardian1..3.json> deploy/railway/bridge-chain.sh check <chain>`.
2. Deploy the contracts, one chain at a time:
   `KEYS=<key directory> deploy/railway/bridge-chain.sh deploy sepolia`,
   then `base-sepolia`, `bsc-testnet`, … (add `FOUNDRY=<directory with
   forge and cast>` if they are not on PATH, `OWNER=<multisig>` to hand
   each vault to a multisig, and `DELAY_SECONDS` / `MAX_RELEASE_*` /
   `PERIOD_LIMIT_*` if the defaults do not fit). Each run writes
   `deploy/railway/chains/<chain>.env`; `check <chain>` shows the result.
3. Build the chains document, set it on the three services and deploy
   them, followers first:

   ```
   CHAINS=$(deploy/railway/chains.sh sepolia base-sepolia bsc-testnet)
   for s in bridge bridge-g2 bridge-g3; do
     railway variable set "CHAINS_JSON=$CHAINS" -s $s --skip-deploys --json
   done
   railway up -s bridge-g2 -d
   railway up -s bridge-g3 -d
   railway up -s bridge -d
   railway up -s web -d          # picks up BRIDGE_URL
   ```

   The daemon rejects a document that lists a chain id twice, a token
   address twice on one chain, decimals outside 0..18, or a chain without
   tokens, and logs the reason.
4. Fund the custody address above with a few testnet ARG for issuance
   fees (send from the app's wallet or mine to it). `GET /api/status` on the
   bridge shows the float as `custody_arg`; the first deposit is issued
   once it is non-zero.
5. Try it: on the bridge page, pick a chain and a token, "Get 100 test
   USDC" (the page switches the wallet to that chain, adding it if the
   wallet does not know it), then deposit to a `targ1…` address. The
   issuance appears after the chain's confirmations and the next batch (at
   most a minute). `/api/status` lists every chain with its fees and its
   reserves per token, split into what backs uARG (`obligations`) and the
   fees collected (`surplus`), and, if a chain's RPC or the leader's gas
   is a problem, the reason in that chain's `error`. A chain with a
   router also shows `router` (empty if its check failed, with the reason
   in `router_error`) and `deposit_addresses`, the number of addresses
   the leader watches there. On the Deposit tab, "No wallet? Get a deposit
   address" shows the address of a `targ1…` address on the chosen chain;
   send it test tokens from any wallet and follow it there or with
   `GET /api/deposit-address/status?chain=<id>&to=<targ1…>`. The leader
   pays the gas of every sweep, so it needs native gas on those chains as
   it does for releases.

Adding a chain later: deploy it (step 2), rebuild `CHAINS_JSON` with the
new chain named after the existing ones, set it on all three services and
redeploy them, followers first. The daemon keeps its state and scans the
new chain from its start block. Removing a chain from the document leaves
its deposits and redemptions on record but stops processing them.

**Upgrading to the hardened daemon** (state format 4, coordinator
authentication): set `LEADER_PUBKEY` on `bridge-g2` and `bridge-g3`, make
sure `ARGONMINE_BRIDGE_SECRET` has at least 32 characters (a new value on
all three services if not), set `MAX_ISSUE` and `MAX_ISSUE_DAY` if the
defaults do not fit, then deploy the followers first and the leader last,
at a moment when no deposit is `issued` (in flight): an issuance broadcast
by the old daemon carries no deposit references, so once mined only the
leader can settle it.
The daemon migrates `/data/bridge-state` from format 3 in place: deposits,
redemptions and issuances are kept, and only the custody coin set is
rebuilt by rescanning Argonmine from genesis (a few minutes, logged). A
directory of format 2 or older is never wiped any more: its records are
moved under `a/<format>/` and kept. Do not delete `/data/bridge-state`:
besides the bookkeeping it holds each guardian's signing memory (every
issuance it signed, by deposit) and the burns it signed releases for,
which is what keeps a guardian from signing the same money twice.
Hand-off codes are now ten characters (`K7M2Q-X9RT4`) and work once; the
desktop app must accept them (it checked for six digits).

**Upgrading to the second security pass** (finality, providers, payout
tiers, pauses, gas key): create the gas key (`argonmine-bridge gaskey
gas.json`), fund its address on every chain and set `GAS_KEY` on `bridge`
(the leader does not start without it); set `L1_CONFIRMATION_TIERS`,
`L1_REORG_PAUSE`, `RELEASE_TIMEOUT`, `ISSUE_REBROADCAST` and `STUCK_AFTER`
on all three services if the defaults do not fit; rebuild `CHAINS_JSON`
with `chains.sh` (the env files now carry `PUBLIC_RPC`; add `RPCS` with a
second provider of another company where you have one); deploy the
followers first and the leader last, at a moment when no release is
`released` (in flight): a release sent by the old daemon is followed only
by its receipt. The state directory is used as it is (no format change).
The deposits of a chain are issued once final: on the testnets with the
`finalized` tag that is about 15 to 30 minutes after a deposit instead of
12 blocks. Then redeploy the vaults with the hardened contract (below).

**Pauses and other operator commands.** A guardian that paused (`paused`
in its `/api/status`: a deep Argonmine reorganisation, or an issued
deposit that left its EVM chain) acts again only when its operator
resumes it, from a shell in its container:

```
railway ssh -s bridge-g2
argonmine-bridge -network testnet -datadir /data -key /data/guardian.json -listen 127.0.0.1:8395 resume
```

(`resume l1`, `resume evm/<chain id>` for one scope; `approve-release
<burn txid>` lets a burn above the last payout tier be paid;
`mark-unclaimable <chain id>#<id> refunded` after refunding a held deposit
by hand). The command is signed with that guardian's key and taken only
from inside the container; with the daemon stopped it edits the state
directory directly. Do it on every guardian that paused, after reading
docs/BRIDGE.md, "Operator commands".

### Replacing a vault

A burn names a chain, not a vault, and a new vault does not know which
burns the old one paid. So, before the new vault goes into `CHAINS_JSON`:

1. Read the current Argonmine height (`/api/status`, `l1_height`) and put
   `L1_START_HEIGHT=<that height + 1>` into the chain's env file next to
   the new `VAULT` (and the new `START_BLOCK`). Burns below it are never
   paid from the new vault; those still unpaid in the old vault need a
   human (they show as `unreleasable` with the reason).
2. Rebuild `CHAINS_JSON` with `chains.sh`, set it on all three services
   and redeploy, followers first.

The guardians also protect themselves if the step is forgotten: a guardian
that sees a chain's vault change in its configuration starts a new epoch
at its current Argonmine height and pays no older burn from the new vault,
and none of them ever signs a burn for a second release (another vault,
token, recipient or amount) once it has signed one. Both depend on the
guardian's state, so a vault replaced together with a rebuilt state relies
on `L1_START_HEIGHT` alone.

Rotating a guardian means a new key file, a new `GUARDIANS` value on all
three services (and a new `LEADER_PUBKEY` on the followers if the leader
changes), a new vault on every chain (the custody address changes with
the guardian set), the env files in `chains/` deleted before `deploy` is
run again and `L1_START_HEIGHT` added to each new one as above, and a new
`CHAINS_JSON`.

## Mainnet

Create a second project, set `ARGONMINE_NETWORK=mainnet`, `ARGONMINE_HRP=arg`,
`RPC_PORT=9332`, a TCP proxy on 9333, and follow `docs/LAUNCH.md`.
