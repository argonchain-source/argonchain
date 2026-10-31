#!/bin/sh
# Entry point of deploy/railway/Dockerfile. Picks the program from ARGONMINE_ROLE
# and builds its flags from environment variables, so one image serves every role.
set -eu

NET="${ARGONMINE_NETWORK:-mainnet}"
ROLE="${ARGONMINE_ROLE:?set ARGONMINE_ROLE to node, explorer, web, bridge, swap or miner}"
EXTRA="${ARGONMINE_ARGS:-}"

case "$ROLE" in
  node)
    # P2P on the network's default port (dual-stack), RPC on the private network only.
    # shellcheck disable=SC2086
    exec argonmined -network "$NET" -datadir /data \
      -rpc ":${RPC_PORT:-19332}" -rpctoken "${RPC_TOKEN:?RPC_TOKEN is required}" \
      ${ARGONMINE_SEEDS:+-seeds "$ARGONMINE_SEEDS"} \
      ${ARGONMINE_DNSSEED:+-dnsseed "$ARGONMINE_DNSSEED"} \
      $EXTRA
    ;;
  miner)
    # A pool miner (or, with -solo in ARGONMINE_ARGS, a solo one) paying
    # MINE_ADDRESS through the explorer; on Railway the explorer is reached
    # over the private network.
    # shellcheck disable=SC2086
    exec argonmine-miner -network "$NET" \
      -server "${MINE_SERVER:-http://explorer.railway.internal:8380}" \
      -address "${MINE_ADDRESS:?MINE_ADDRESS is required}" \
      -threads "${MINE_THREADS:-4}" \
      $EXTRA
    ;;
  explorer)
    # With POOL_WALLET_PASSWORD set the explorer also runs the mining pool,
    # paying out from a wallet on the volume (created from POOL_MNEMONIC the
    # first time; docs/MINING.md, "Pool mining").
    # shellcheck disable=SC2086
    exec argonmine-explorer -network "$NET" -datadir /data \
      -rpc "${NODE_RPC:?NODE_RPC is required, e.g. http://node.railway.internal:19332}" \
      -token "${RPC_TOKEN:?RPC_TOKEN is required}" \
      -listen ":${PORT:-8380}" \
      ${POOL_WALLET_PASSWORD:+-pool-wallet /data/pool-wallet.json} \
      $EXTRA
    ;;
  web)
    # One service, four names (README.md, Domains): argonmine.com is the
    # landing page (SITE_URL), app.argonmine.com the app (APP_URL) and
    # docs.argonmine.com the docs (DOCS_URL); www. redirects to the site.
    # shellcheck disable=SC2086
    exec argonmine-web -listen ":${PORT:-8370}" -repo /srv \
      -hrp "${ARGONMINE_HRP:-arg}" \
      -explorer "${EXPLORER_URL:?EXPLORER_URL is required}" \
      ${BRIDGE_URL:+-bridge "$BRIDGE_URL"} \
      ${APP_URL:+-app "$APP_URL"} \
      ${SITE_URL:+-site "$SITE_URL"} \
      ${DOCS_URL:+-docs "$DOCS_URL"} \
      ${SITE_DOMAIN:+-domain "$SITE_DOMAIN"} \
      ${SWAP_UPSTREAM:+-swap-upstream "$SWAP_UPSTREAM"} \
      ${HOLD_ARG:+-hold-arg "$HOLD_ARG"} \
      ${DOWNLOADS_UPSTREAM:+-downloads-upstream "$DOWNLOADS_UPSTREAM"} \
      $EXTRA
    ;;
  bridge)
    # One guardian of the federated bridge (docs/BRIDGE.md). The key file from
    # `argonmine-bridge keygen` arrives as the GUARDIAN_KEY variable, the chain
    # set (every EVM chain with its vault and accepted tokens, the document that
    # deploy/railway/chains.sh prints) as CHAINS_JSON, and the shared secret of
    # the guardian API (at least 32 characters, the same on every guardian) as
    # ARGONMINE_BRIDGE_SECRET. All three are written to the volume, readable by
    # this container only, and taken out of the environment before the daemon
    # starts. A follower takes guardian requests signed by the leader's key and
    # nothing else: LEADER_PUBKEY, the leader's L1 public key, one of GUARDIANS.
    # The leader sends the release and sweep transactions from its own gas key
    # (GAS_KEY: the JSON written by argonmine-bridge gaskey, never a guardian
    # key or a vault owner), written to the volume and unset the same way.
    : "${ARGONMINE_BRIDGE_SECRET:?ARGONMINE_BRIDGE_SECRET is required (same value on every guardian, at least 32 characters)}"
    umask 077
    printf '%s\n' "${GUARDIAN_KEY:?GUARDIAN_KEY is required (the JSON written by argonmine-bridge keygen)}" > /data/guardian.json
    chmod 600 /data/guardian.json
    printf '%s\n' "${CHAINS_JSON:?CHAINS_JSON is required (the chains document printed by deploy/railway/chains.sh, on one line)}" > /data/chains.json
    chmod 600 /data/chains.json
    printf '%s\n' "$ARGONMINE_BRIDGE_SECRET" > /data/bridge-secret
    chmod 600 /data/bridge-secret
    LEAD=""
    case "${BRIDGE_MODE:-follower}" in
      leader)
        printf '%s\n' "${GAS_KEY:?GAS_KEY is required on the leader (the JSON written by argonmine-bridge gaskey: a dedicated EVM key with a little gas on every chain)}" > /data/gas-key.json
        chmod 600 /data/gas-key.json
        LEAD="-leader -gas-key /data/gas-key.json -peers ${PEERS:?PEERS is required when BRIDGE_MODE=leader, e.g. http://bridge-g2.railway.internal:8395,http://bridge-g3.railway.internal:8395}" ;;
      follower) LEAD="-leader-pubkey ${LEADER_PUBKEY:?LEADER_PUBKEY is required on a follower: the L1 public key of the leader guardian, one of GUARDIANS}" ;;
      *) echo "unknown BRIDGE_MODE '$BRIDGE_MODE' (leader or follower)" >&2; exit 2 ;;
    esac
    unset GUARDIAN_KEY CHAINS_JSON ARGONMINE_BRIDGE_SECRET GAS_KEY
    # Railway's edge appends each client's address to X-Forwarded-For: the
    # key of the page's per-client limits (-client-ip-header).
    # shellcheck disable=SC2086
    exec argonmine-bridge -network "$NET" -datadir /data \
      -node-rpc "${NODE_RPC:?NODE_RPC is required, e.g. http://node.railway.internal:19332}" \
      -node-token "${RPC_TOKEN:?RPC_TOKEN is required}" \
      -chains /data/chains.json \
      -asset-symbol "${ASSET_SYMBOL:-UARG}" \
      -guardians "${GUARDIANS:?GUARDIANS is required (comma-separated L1 public keys of all guardians)}" \
      -key /data/guardian.json -threshold "${THRESHOLD:-2}" \
      -secret-file /data/bridge-secret \
      -l1-confirmations "${L1_CONFIRMATIONS:-6}" \
      -l1-confirmation-tiers "${L1_CONFIRMATION_TIERS-1000:6,10000:30,100000:120}" \
      -l1-reorg-pause "${L1_REORG_PAUSE:-3}" \
      -release-timeout "${RELEASE_TIMEOUT:-5m}" -issue-rebroadcast "${ISSUE_REBROADCAST:-10m}" \
      -stuck-after "${STUCK_AFTER:-1h}" \
      -max-issue "${MAX_ISSUE:-10000}" -max-issue-day "${MAX_ISSUE_DAY:-100000}" \
      -issue-interval "${ISSUE_INTERVAL:-1m}" -scan-interval "${SCAN_INTERVAL:-10s}" \
      -listen ":${PORT:-8395}" -client-ip-header X-Forwarded-For \
      $LEAD \
      $EXTRA
    ;;
  swap)
    # The native swap service (docs/SWAP.md): the maker's wallet lives on the
    # volume, restored from SWAP_MNEMONIC on the first start and opened with
    # SWAP_PASSWORD; both are read by the daemon and taken out of its
    # environment. The uARG asset id comes from the bridge, or from
    # UARG_ASSET (64 hex) when it is set, so the swap never waits for one.
    : "${SWAP_PASSWORD:?SWAP_PASSWORD is required (at least 12 characters)}"
    umask 077
    # One wallet file per network: the testnet's kept its original name.
    WALLET=/data/swap.wallet
    [ "$NET" = testnet ] || WALLET="/data/swap-$NET.wallet"
    exec argonmine-swap -network "$NET" -wallet "$WALLET" \
      -listen ":${PORT:-8410}" \
      -node-rpc "${NODE_RPC:?NODE_RPC is required, e.g. http://node.railway.internal:19332}" \
      -node-token "${RPC_TOKEN:?RPC_TOKEN is required}" \
      -bridge-url "${BRIDGE_URL:?BRIDGE_URL is required (the bridge whose /api/status names the uARG asset id)}" \
      ${UARG_ASSET:+-uarg-asset "$UARG_ASSET"} \
      -price "${SWAP_PRICE:?SWAP_PRICE is required (uARG per ARG, e.g. 0.10)}" \
      -spread-bps "${SWAP_SPREAD_BPS:-100}" -wrap-fee-bps "${SWAP_WRAP_FEE_BPS:-0}" \
      ${SWAP_CLOSED:+-closed "$SWAP_CLOSED"} \
      -client-ip-header X-Forwarded-For
    ;;
  *)
    echo "unknown ARGONMINE_ROLE '$ROLE' (node, explorer, web, bridge, swap or miner)" >&2
    exit 2
    ;;
esac
