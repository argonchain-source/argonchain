#!/bin/sh
# Deploys the bridge contracts to one EVM chain and records their addresses for
# the Railway services (deploy/railway/README.md, section "Bridge"). POSIX sh;
# on Windows run it from Git Bash.
#
#   deploy/railway/bridge-chain.sh check  <chain>   chain, deployer, native balance;
#                                                    for a deployed vault: its delay,
#                                                    setup state, owner, pause, fees and
#                                                    per-token limits
#   deploy/railway/bridge-chain.sh deploy <chain>   MockUSDC, the USDT mock, the hardened
#                                                    Vault (timelocked admin) and its
#                                                    DepositRouter; allows both tokens,
#                                                    sets the fees and the release limits,
#                                                    finalizes the setup, hands the vault
#                                                    to OWNER if set; writes
#                                                    deploy/railway/chains/<chain>.env
#   deploy/railway/bridge-chain.sh router <chain>   only the DepositRouter, for a chain
#                                                    deployed before it existed: reads
#                                                    VAULT from chains/<chain>.env and
#                                                    appends ROUTER and ROUTER_BLOCK to it;
#                                                    refuses if ROUTER is already there
#
# Chains: sepolia, base-sepolia, bsc-testnet, arbitrum-sepolia, polygon-amoy, anvil.
# The mocks are for testnets only; a mainnet deployment uses the real USDC and
# USDT and must not be done with this script.
#
# Variables:
#   KEYS             directory holding guardian1.json, guardian2.json and
#                    guardian3.json from `argonmine-bridge keygen`
#   FOUNDRY          directory with forge and cast, if they are not on PATH
#   FEE_DEPOSIT_BPS  bridge fee on deposits in basis points, default 100 (1 percent)
#   FEE_RELEASE_BPS  bridge fee on releases in basis points, default 100 (1 percent)
#                    Both are set with the vault's setFees and must be at most
#                    1000 (10 percent); a release fee above 100 is set in steps
#                    of 100, the most the vault lets it rise per change.
#   DELAY_SECONDS    the vault's timelock delay: how long every later change of
#                    guardians, tokens, fees, limits or the delay itself, and
#                    lifting a pause, waits after it is proposed. Default
#                    172800 (2 days) for the testnets; use 604800 (7 days) or
#                    more on mainnet. The vault accepts 86400..2592000.
#   MAX_RELEASE_USDC, MAX_RELEASE_USDT
#                    the vault's cap on one release of that token, in whole
#                    tokens (default 10000; 0 = none)
#   PERIOD_LIMIT_USDC, PERIOD_LIMIT_USDT
#                    the vault's cap on the releases of that token in one
#                    24-hour window, in whole tokens (default 100000; 0 = none).
#                    Both are converted with the decimals the token reports on
#                    chain and set with setLimits before the setup is finalized.
#   FIRST_DEPOSIT_ID the id of the vault's first deposit (its constructor's
#                    firstDepositId), default 1 for the first vault on a chain.
#                    The guardians name a deposit by its chain and id alone, so
#                    a vault that REPLACES another on the same chain must start
#                    above every deposit id they have on record for it: the
#                    largest chains[].max_deposit_id in /api/status of the
#                    guardians, plus one (round it up generously). A guardian
#                    refuses a replacement vault that does not (docs/BRIDGE.md,
#                    "Replacing a vault"). 1..4611686018427387904 (2^62).
#   OWNER            optional: the address the vault is handed to after the
#                    setup (a multisig of independent parties before real
#                    funds). transferOwnership(OWNER) is sent; OWNER must then
#                    call acceptOwnership() (done here on anvil, whose accounts
#                    are unlocked). Without it the deployer stays the owner.
#   ANVIL_RPC        the anvil chain's RPC (default http://127.0.0.1:8545)
#   ENV_DIR          where <chain>.env is written and read (default
#                    deploy/railway/chains)
#
# The deployer is guardian1's EVM key: it pays for the deployment and is the
# vault's first owner (the router has no owner). On anvil the first unlocked
# account deploys instead and is minted 1,000,000 of each mock token. None of
# these keys sends the bridge's release transactions: the leader does that
# from its own gas key (GAS_KEY, deploy/railway/README.md).
set -eu

KEYS="${KEYS:-$HOME/.argonmine-bridge-keys}"
[ -z "${FOUNDRY:-}" ] || PATH="$FOUNDRY:$PATH"
HERE=$(cd "$(dirname "$0")" && pwd)
CONTRACTS="$HERE/../../contracts"
ANVIL_ACCOUNT=0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266
THRESHOLD=2
USDC_UNIT=1000000 # one USDC in token units (6 decimals)
FEE_DEPOSIT_BPS="${FEE_DEPOSIT_BPS:-100}" # setFees: 100 = 1 percent of each deposit kept as surplus
FEE_RELEASE_BPS="${FEE_RELEASE_BPS:-100}" # setFees: 100 = 1 percent of each release kept as surplus
MAX_FEE_BPS=1000                          # the vault's limit (10 percent); setFees reverts with BadFee above it
FEE_STEP_BPS=100                          # the most the release fee may rise in one change (FeeStepTooLarge above it)
DELAY_SECONDS="${DELAY_SECONDS:-172800}"  # the timelock delay: 2 days on the testnets, 7 days or more on mainnet
MIN_DELAY=86400
MAX_DELAY=2592000
MAX_RELEASE_USDC="${MAX_RELEASE_USDC:-10000}"
MAX_RELEASE_USDT="${MAX_RELEASE_USDT:-10000}"
PERIOD_LIMIT_USDC="${PERIOD_LIMIT_USDC:-100000}"
PERIOD_LIMIT_USDT="${PERIOD_LIMIT_USDT:-100000}"
OWNER="${OWNER:-}"
FIRST_DEPOSIT_ID="${FIRST_DEPOSIT_ID:-1}" # the vault's first deposit id: above every id of the vaults it replaces
ZERO=0x0000000000000000000000000000000000000000

usage() {
  echo "usage: $0 <check|deploy|router> <sepolia|base-sepolia|bsc-testnet|arbitrum-sepolia|polygon-amoy|anvil>" >&2
  exit 2
}

ACTION="${1:-}"
CHAIN="${2:-}"
case "$ACTION" in check | deploy | router) ;; *) usage ;; esac

# Built-in chain table. USDT_UNIT is one USDT in token units: BNB Chain
# stablecoins have 18 decimals, so bsc-testnet gets the 18-decimal mock.
# The RPCs are public endpoints without keys, so they are also what the page
# hands to wallets (PUBLIC_RPC).
case "$CHAIN" in
  sepolia)
    NAME="Ethereum Sepolia"; CHAIN_ID=11155111; RPC=https://ethereum-sepolia-rpc.publicnode.com
    NATIVE_SYMBOL=ETH; EXPLORER=https://sepolia.etherscan.io
    USDT_CONTRACT=MockUSDT; USDT_DECIMALS=6; USDT_UNIT=1000000 ;;
  base-sepolia)
    NAME="Base Sepolia"; CHAIN_ID=84532; RPC=https://base-sepolia-rpc.publicnode.com
    NATIVE_SYMBOL=ETH; EXPLORER=https://sepolia.basescan.org
    USDT_CONTRACT=MockUSDT; USDT_DECIMALS=6; USDT_UNIT=1000000 ;;
  bsc-testnet)
    NAME="BNB Chain Testnet"; CHAIN_ID=97; RPC=https://bsc-testnet-rpc.publicnode.com
    NATIVE_SYMBOL=BNB; EXPLORER=https://testnet.bscscan.com
    # BNB Chain validators drop transactions below their floor gas price; the RPC's estimate is lower.
    GAS_ARGS="--legacy --gas-price ${BSC_GAS_PRICE_WEI:-10000000000}"
    # A public RPC's mempool can hold a stuck low-fee attempt; transactions go
    # through the official endpoint, which does not know it.
    DEPLOY_RPC=${BSC_DEPLOY_RPC:-https://data-seed-prebsc-1-s1.bnbchain.org:8545}
    USDT_CONTRACT=MockUSD18; USDT_DECIMALS=18; USDT_UNIT=1000000000000000000 ;;
  arbitrum-sepolia)
    NAME="Arbitrum Sepolia"; CHAIN_ID=421614; RPC=https://arbitrum-sepolia-rpc.publicnode.com
    NATIVE_SYMBOL=ETH; EXPLORER=https://sepolia.arbiscan.io
    USDT_CONTRACT=MockUSDT; USDT_DECIMALS=6; USDT_UNIT=1000000 ;;
  polygon-amoy)
    NAME="Polygon Amoy"; CHAIN_ID=80002; RPC=https://polygon-amoy-bor-rpc.publicnode.com
    NATIVE_SYMBOL=POL; EXPLORER=https://amoy.polygonscan.com
    USDT_CONTRACT=MockUSDT; USDT_DECIMALS=6; USDT_UNIT=1000000 ;;
  anvil)
    NAME="Anvil (local)"; CHAIN_ID=31337; RPC="${ANVIL_RPC:-http://127.0.0.1:8545}"
    NATIVE_SYMBOL=ETH; EXPLORER=
    USDT_CONTRACT=MockUSDT; USDT_DECIMALS=6; USDT_UNIT=1000000 ;;
  "") usage ;;
  *) echo "unknown chain '$CHAIN'" >&2; usage ;;
esac
ENV_DIR="${ENV_DIR:-$HERE/chains}"
ENVFILE="$ENV_DIR/$CHAIN.env"

whole() { # whole NAME VALUE: a non-negative whole number or exit
  case "$2" in
    *[!0-9]* | "") echo "$1 must be a whole number, got '$2'" >&2; exit 2 ;;
  esac
}
for F in "$FEE_DEPOSIT_BPS" "$FEE_RELEASE_BPS"; do
  case "$F" in
    *[!0-9]* | "") echo "FEE_DEPOSIT_BPS and FEE_RELEASE_BPS must be whole numbers of basis points, got '$F'" >&2; exit 2 ;;
  esac
  [ "$F" -le "$MAX_FEE_BPS" ] || { echo "fee of $F bps is above the vault's limit of $MAX_FEE_BPS bps (10 percent)" >&2; exit 2; }
done
whole DELAY_SECONDS "$DELAY_SECONDS"
[ "$DELAY_SECONDS" -ge "$MIN_DELAY" ] && [ "$DELAY_SECONDS" -le "$MAX_DELAY" ] || { echo "DELAY_SECONDS must be $MIN_DELAY..$MAX_DELAY (1 to 30 days), got $DELAY_SECONDS" >&2; exit 2; }
whole FIRST_DEPOSIT_ID "$FIRST_DEPOSIT_ID"
case "$FIRST_DEPOSIT_ID" in 0*) echo "FIRST_DEPOSIT_ID must be 1 or more, without leading zeros, got '$FIRST_DEPOSIT_ID'" >&2; exit 2 ;; esac
[ "${#FIRST_DEPOSIT_ID}" -le 19 ] && [ "$FIRST_DEPOSIT_ID" -le 4611686018427387904 ] || { echo "FIRST_DEPOSIT_ID must be at most 4611686018427387904 (2^62), got $FIRST_DEPOSIT_ID" >&2; exit 2; }
whole MAX_RELEASE_USDC "$MAX_RELEASE_USDC"; whole MAX_RELEASE_USDT "$MAX_RELEASE_USDT"
whole PERIOD_LIMIT_USDC "$PERIOD_LIMIT_USDC"; whole PERIOD_LIMIT_USDT "$PERIOD_LIMIT_USDT"
if [ -n "$OWNER" ]; then
  case "$OWNER" in 0x????????????????????????????????????????) ;; *) echo "OWNER must be a 0x address, got '$OWNER'" >&2; exit 2 ;; esac
fi

# field FILE KEY: one string value from a flat JSON key file (no jq needed).
field() { sed -n 's/.*"'"$2"'": *"\([^"]*\)".*/\1/p' "$1" | head -n 1 | tr -d '\r'; }

# envval KEY: the last value of KEY in the chain's env file, empty if absent.
envval() { sed -n "s/^$1=//p" "$ENVFILE" | tail -n 1 | tr -d '\r' | sed 's/^"//; s/"$//'; }

# The router needs no guardian addresses, and on anvil no key at all.
if [ "$ACTION" != router ]; then
  for i in 1 2 3; do
    [ -f "$KEYS/guardian$i.json" ] || { echo "missing $KEYS/guardian$i.json (set KEYS to the directory with the key files)" >&2; exit 2; }
  done
  G1=$(field "$KEYS/guardian1.json" evm_address)
  G2=$(field "$KEYS/guardian2.json" evm_address)
  G3=$(field "$KEYS/guardian3.json" evm_address)
  [ -n "$G1" ] && [ -n "$G2" ] && [ -n "$G3" ] || { echo "guardian key files have no evm_address field" >&2; exit 2; }
elif [ "$CHAIN" != anvil ]; then
  [ -f "$KEYS/guardian1.json" ] || { echo "missing $KEYS/guardian1.json (set KEYS to the directory with the key files)" >&2; exit 2; }
fi

command -v cast >/dev/null 2>&1 || { echo "cast/forge not found: add Foundry to PATH or set FOUNDRY=<its bin directory>" >&2; exit 2; }

# AUTH is passed unquoted to forge and cast. The private key is never printed.
if [ "$CHAIN" = anvil ]; then
  DEPLOYER=$ANVIL_ACCOUNT
  AUTH="--unlocked --from $DEPLOYER"
else
  DEPLOYER_KEY=$(field "$KEYS/guardian1.json" evm_private)
  [ -n "$DEPLOYER_KEY" ] || { echo "$KEYS/guardian1.json has no evm_private field" >&2; exit 2; }
  case "$DEPLOYER_KEY" in 0x*) ;; *) DEPLOYER_KEY="0x$DEPLOYER_KEY" ;; esac
  DEPLOYER=$(cast wallet address --private-key "$DEPLOYER_KEY" | tr -d '\r')
  AUTH="--private-key $DEPLOYER_KEY"
fi

rpc_chain() { cast chain-id --rpc-url "$RPC" | tr -d '\r'; }

# create LABEL FILE:CONTRACT [forge create args...]: sets ADDR and TXHASH.
create() {
  label=$1; shift
  echo "deploying $label..."
  # shellcheck disable=SC2086
  OUT=$(forge create --rpc-url "${DEPLOY_RPC:-$RPC}" $AUTH ${GAS_ARGS:-} --broadcast --json "$@" 2>&1) || { echo "$OUT" >&2; exit 1; }
  ADDR=$(echo "$OUT" | sed -n 's/.*"deployedTo": *"\([^"]*\)".*/\1/p' | head -n 1 | tr -d '\r')
  TXHASH=$(echo "$OUT" | sed -n 's/.*"transactionHash": *"\([^"]*\)".*/\1/p' | head -n 1 | tr -d '\r')
  [ -n "$ADDR" ] && [ -n "$TXHASH" ] || { echo "$OUT" >&2; echo "could not find the $label address in the forge output" >&2; exit 1; }
  echo "$label: $ADDR"
}

# send TO SIG ARGS...: a transaction from the deployer, failing loudly.
send() {
  # shellcheck disable=SC2086
  OUT=$(cast send --rpc-url "${DEPLOY_RPC:-$RPC}" $AUTH ${GAS_ARGS:-} "$@" 2>&1) || { echo "$OUT" >&2; exit 1; }
}

call() { cast call "$@" --rpc-url "${DEPLOY_RPC:-$RPC}" | tr -d '\r'; }

# num: the first word of a cast answer ("1000000 [1e6]" -> "1000000").
num() { sed 's/ .*//'; }

# call_or_na ARGS...: like call, or "n/a" when the contract has no such view
# (a vault deployed before the hardened one).
call_or_na() {
  R=$(cast call "$@" --rpc-url "${DEPLOY_RPC:-$RPC}" 2>/dev/null | tr -d '\r')
  if [ -n "$R" ]; then echo "$R" | num; else echo n/a; fi
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# units WHOLE DECIMALS: whole tokens in token units (WHOLE followed by DECIMALS zeros).
units() {
  if [ "$1" = 0 ]; then echo 0; return; fi
  z=""; i=0
  while [ "$i" -lt "$2" ]; do z="${z}0"; i=$((i + 1)); done
  echo "$1$z"
}

# deploy_router VAULT: deploys DepositRouter(VAULT) from the contracts
# directory and checks that vault() reads back; sets ROUTER and ROUTER_BLOCK.
deploy_router() {
  create DepositRouter DepositRouter.sol:DepositRouter --constructor-args "$1"
  ROUTER=$ADDR
  ROUTER_BLOCK=$(cast receipt "$TXHASH" blockNumber --rpc-url "${DEPLOY_RPC:-$RPC}" | tr -d '\r')
  case "$ROUTER_BLOCK" in
    *[!0-9]* | "") echo "could not read the router's block number from the receipt of $TXHASH: '$ROUTER_BLOCK'" >&2; exit 1 ;;
  esac
  V=$(call "$ROUTER" "vault()(address)")
  [ "$(lower "$V")" = "$(lower "$1")" ] || { echo "vault() on the router returned '$V', expected $1" >&2; exit 1; }
  H=$(call "$ROUTER" "forwarderCodeHash()(bytes32)")
  echo "router ok: vault $V, forwarder code hash $H, block $ROUTER_BLOCK"
}

# show_vault VAULT TOKENS...: the vault's administration and per-token limits.
show_vault() {
  V=$1; shift
  echo "vault:     $V"
  echo "  owner:          $(call_or_na "$V" "owner()(address)") (pending: $(call_or_na "$V" "pendingOwner()(address)"))"
  echo "  setup:          finalized=$(call_or_na "$V" "finalized()(bool)") (tokens, fees and limits change only through the timelock once true)"
  echo "  deposit ids:    first $(call_or_na "$V" "firstDepositId()(uint256)") (n/a: a vault from before it, numbering from 1), last $(call_or_na "$V" "depositCount()(uint256)")"
  echo "  delay:          $(call_or_na "$V" "delay()(uint256)") seconds"
  echo "  paused:         $(call_or_na "$V" "paused()(bool)")"
  echo "  guardians:      $(call_or_na "$V" "guardianCount()(uint256)"), threshold $(call_or_na "$V" "threshold()(uint256)")"
  echo "  fees:           deposit $(call_or_na "$V" "depositFeeBps()(uint16)") bps, release $(call_or_na "$V" "releaseFeeBps()(uint16)") bps"
  for T in "$@"; do
    [ -n "$T" ] || continue
    D=$(call_or_na "$T" "decimals()(uint8)")
    PA=$(call_or_na "$V" "periodAvailable(address)(uint256)" "$T")
    [ "$PA" != 115792089237316195423570985008687907853269984665640564039457584007913129639935 ] || PA=unlimited
    echo "  token $T (decimals $D): allowed=$(call_or_na "$V" "isAllowed(address)(bool)" "$T") minDeposit=$(call_or_na "$V" "minDeposit(address)(uint256)" "$T")"
    echo "    maxRelease=$(call_or_na "$V" "maxRelease(address)(uint256)" "$T") periodLimit=$(call_or_na "$V" "periodLimit(address)(uint256)" "$T") periodAvailable=$PA (token units; 0 = no limit)"
  done
}

case "$ACTION" in
  check)
    echo "chain:     $NAME ($CHAIN_ID) via $RPC"
    REPORTED=$(rpc_chain)
    if [ "$REPORTED" = "$CHAIN_ID" ]; then echo "rpc:       reports chain $REPORTED, ok"
    else echo "rpc:       WARNING reports chain $REPORTED, expected $CHAIN_ID; deploy will refuse"; fi
    echo "deployer:  $DEPLOYER"
    echo "balance:   $(cast balance "$DEPLOYER" --rpc-url "$RPC" --ether | tr -d '\r') $NATIVE_SYMBOL"
    echo "guardians: $G1 $G2 $G3 (threshold $THRESHOLD)"
    if [ -f "$ENVFILE" ]; then
      echo "env file:  $ENVFILE exists; deploy will refuse (remove it to deploy again)"
      show_vault "$(envval VAULT)" "$(envval USDC)" "$(envval USDT)"
      if [ -n "$(envval ROUTER)" ]; then echo "router:    $(envval ROUTER) (block $(envval ROUTER_BLOCK))"
      else echo "router:    not deployed; 'router $CHAIN' deploys it for VAULT=$(envval VAULT)"; fi
    else
      echo "env file:  $ENVFILE not written yet; deploy would do:"
      echo "tokens:    MockUSDC (6 decimals), $USDT_CONTRACT ($USDT_DECIMALS decimals), minimum deposit 1 token each"
      echo "fees:      deposit $FEE_DEPOSIT_BPS bps, release $FEE_RELEASE_BPS bps (setFees; FEE_DEPOSIT_BPS/FEE_RELEASE_BPS to change)"
      echo "delay:     $DELAY_SECONDS seconds (DELAY_SECONDS; 604800 or more on mainnet)"
      echo "first id:  $FIRST_DEPOSIT_ID (FIRST_DEPOSIT_ID; a replacement vault starts above every chains[].max_deposit_id in the guardians' /api/status)"
      echo "limits:    USDC $MAX_RELEASE_USDC per release, $PERIOD_LIMIT_USDC per 24 h; USDT $MAX_RELEASE_USDT per release, $PERIOD_LIMIT_USDT per 24 h (whole tokens, 0 = none)"
      echo "owner:     ${OWNER:-the deployer $DEPLOYER} (OWNER to hand the vault to a multisig)"
    fi
    ;;
  deploy)
    REPORTED=$(rpc_chain)
    [ "$REPORTED" = "$CHAIN_ID" ] || { echo "$RPC is chain $REPORTED, not $NAME ($CHAIN_ID)" >&2; exit 1; }
    WEI=$(cast balance "$DEPLOYER" --rpc-url "$RPC" | tr -d '\r')
    [ "$CHAIN" = anvil ] || [ "$WEI" != "0" ] || { echo "deployer $DEPLOYER has no $NATIVE_SYMBOL on $NAME; fund it first, then run 'check'" >&2; exit 1; }
    [ ! -f "$ENVFILE" ] || { echo "$ENVFILE already exists; remove it to deploy again" >&2; exit 1; }
    mkdir -p "$ENV_DIR"
    cd "$CONTRACTS"
    echo "deploying to $NAME ($CHAIN_ID) from $DEPLOYER"

    create MockUSDC MockUSDC.sol:MockUSDC
    USDC=$ADDR
    create "$USDT_CONTRACT" "$USDT_CONTRACT.sol:$USDT_CONTRACT"
    USDT=$ADDR
    create Vault Vault.sol:Vault --constructor-args "[$G1,$G2,$G3]" "$THRESHOLD" "$DELAY_SECONDS" "$FIRST_DEPOSIT_ID"
    VAULT=$ADDR
    F=$(call "$VAULT" "firstDepositId()(uint256)" | num)
    [ "$F" = "$FIRST_DEPOSIT_ID" ] || { echo "firstDepositId() returned '$F', expected $FIRST_DEPOSIT_ID" >&2; exit 1; }
    START_BLOCK=$(cast receipt "$TXHASH" blockNumber --rpc-url "${DEPLOY_RPC:-$RPC}" | tr -d '\r')
    case "$START_BLOCK" in
      *[!0-9]* | "") echo "could not read the vault's block number from the receipt of $TXHASH: '$START_BLOCK'" >&2; exit 1 ;;
    esac
    deploy_router "$VAULT"

    # The setup, while the owner may still change everything at once.
    echo "allowing USDC ($USDC) with minimum deposit $USDC_UNIT and USDT ($USDT) with minimum deposit $USDT_UNIT..."
    send "$VAULT" "setToken(address,bool,uint256)" "$USDC" true "$USDC_UNIT"
    send "$VAULT" "setToken(address,bool,uint256)" "$USDT" true "$USDT_UNIT"
    echo "setting fees: deposit $FEE_DEPOSIT_BPS bps, release $FEE_RELEASE_BPS bps..."
    REL=0
    while [ $((FEE_RELEASE_BPS - REL)) -gt "$FEE_STEP_BPS" ]; do
      REL=$((REL + FEE_STEP_BPS)) # the release fee rises at most 100 bps per change
      send "$VAULT" "setFees(uint16,uint16)" "$FEE_DEPOSIT_BPS" "$REL"
    done
    send "$VAULT" "setFees(uint16,uint16)" "$FEE_DEPOSIT_BPS" "$FEE_RELEASE_BPS"
    for SYM in USDC USDT; do
      eval "T=\$$SYM; MR=\$MAX_RELEASE_$SYM; PL=\$PERIOD_LIMIT_$SYM"
      DEC=$(call "$T" "decimals()(uint8)" | num)
      whole "decimals() of $SYM" "$DEC"
      MRU=$(units "$MR" "$DEC"); PLU=$(units "$PL" "$DEC")
      echo "release limits for $SYM ($DEC decimals on chain): $MR per release = $MRU units, $PL per 24 h = $PLU units..."
      send "$VAULT" "setLimits(address,uint256,uint256)" "$T" "$MRU" "$PLU"
      M=$(call "$VAULT" "maxRelease(address)(uint256)" "$T" | num); P=$(call "$VAULT" "periodLimit(address)(uint256)" "$T" | num)
      [ "$M" = "$MRU" ] && [ "$P" = "$PLU" ] || { echo "limits of $SYM read back as $M/$P, expected $MRU/$PLU" >&2; exit 1; }
    done

    N=$(call "$VAULT" "guardianCount()(uint256)")
    [ "$N" = "3" ] || { echo "guardianCount() returned '$N', expected 3" >&2; exit 1; }
    LIST=$(call "$VAULT" "tokens()(address[])")
    N=$(printf '%s\n' "$LIST" | tr ',' '\n' | grep -c '0x' || true)
    [ "$N" = "2" ] || { echo "tokens() returned $LIST, expected 2 entries" >&2; exit 1; }
    for T in "$USDC" "$USDT"; do
      A=$(call "$VAULT" "isAllowed(address)(bool)" "$T")
      [ "$A" = "true" ] || { echo "isAllowed($T) returned '$A'" >&2; exit 1; }
    done
    M=$(call "$VAULT" "minDeposit(address)(uint256)" "$USDT" | num)
    [ "$M" = "$USDT_UNIT" ] || { echo "minDeposit(USDT) returned '$M', expected $USDT_UNIT" >&2; exit 1; }
    F=$(call "$VAULT" "depositFeeBps()(uint16)" | num)
    [ "$F" = "$FEE_DEPOSIT_BPS" ] || { echo "depositFeeBps() returned '$F', expected $FEE_DEPOSIT_BPS" >&2; exit 1; }
    F=$(call "$VAULT" "releaseFeeBps()(uint16)" | num)
    [ "$F" = "$FEE_RELEASE_BPS" ] || { echo "releaseFeeBps() returned '$F', expected $FEE_RELEASE_BPS" >&2; exit 1; }
    D=$(call "$VAULT" "delay()(uint256)" | num)
    [ "$D" = "$DELAY_SECONDS" ] || { echo "delay() returned '$D', expected $DELAY_SECONDS" >&2; exit 1; }

    echo "finalizing the setup: from now on tokens, fees and limits change only through the $DELAY_SECONDS-second timelock..."
    send "$VAULT" "finalizeSetup()"
    [ "$(call "$VAULT" "finalized()(bool)")" = "true" ] || { echo "finalized() is not true after finalizeSetup()" >&2; exit 1; }

    if [ "$CHAIN" = anvil ]; then
      # 1,000,000 tokens = the unit with six more zeros.
      send "$USDC" "mint(address,uint256)" "$DEPLOYER" "1000000${USDC_UNIT#1}"
      send "$USDT" "mint(address,uint256)" "$DEPLOYER" "1000000${USDT_UNIT#1}"
      echo "minted 1,000,000 of each mock to $DEPLOYER"
    fi

    OWNER_NOTE="the deployer $DEPLOYER owns the vault"
    if [ -n "$OWNER" ] && [ "$(lower "$OWNER")" != "$(lower "$DEPLOYER")" ]; then
      echo "handing the vault to $OWNER: transferOwnership..."
      send "$VAULT" "transferOwnership(address)" "$OWNER"
      PENDING=$(call "$VAULT" "pendingOwner()(address)")
      [ "$(lower "$PENDING")" = "$(lower "$OWNER")" ] || { echo "pendingOwner() returned '$PENDING', expected $OWNER" >&2; exit 1; }
      if [ "$CHAIN" = anvil ]; then
        # anvil's accounts are unlocked: accept for the new owner.
        # shellcheck disable=SC2086
        OUT=$(cast send --rpc-url "$RPC" --unlocked --from "$OWNER" "$VAULT" "acceptOwnership()" 2>&1) || { echo "$OUT" >&2; exit 1; }
        NOW=$(call "$VAULT" "owner()(address)")
        [ "$(lower "$NOW")" = "$(lower "$OWNER")" ] || { echo "owner() returned '$NOW' after acceptOwnership" >&2; exit 1; }
        OWNER_NOTE="$OWNER owns the vault (accepted)"
      else
        OWNER_NOTE="ownership offered to $OWNER: it must call acceptOwnership() on $VAULT (until then the deployer $DEPLOYER owns it)"
      fi
    fi

    {
      printf 'NAME="%s"\n' "$NAME"
      printf 'CHAIN_ID=%s\nRPC=%s\nPUBLIC_RPC=%s\nEXPLORER=%s\nNATIVE_SYMBOL=%s\n' "$CHAIN_ID" "$RPC" "$RPC" "$EXPLORER" "$NATIVE_SYMBOL"
      printf 'VAULT=%s\nUSDC=%s\nUSDC_DECIMALS=6\nUSDT=%s\nUSDT_DECIMALS=%s\n' "$VAULT" "$USDC" "$USDT" "$USDT_DECIMALS"
      printf 'FEE_DEPOSIT_BPS=%s\nFEE_RELEASE_BPS=%s\n' "$FEE_DEPOSIT_BPS" "$FEE_RELEASE_BPS"
      printf '# the vault: timelock delay (seconds), release limits in whole tokens (0 = none), owner; a record, not read by chains.sh\n'
      printf 'DELAY_SECONDS=%s\nMAX_RELEASE_USDC=%s\nPERIOD_LIMIT_USDC=%s\nMAX_RELEASE_USDT=%s\nPERIOD_LIMIT_USDT=%s\nVAULT_OWNER=%s\nFIRST_DEPOSIT_ID=%s\n' \
        "$DELAY_SECONDS" "$MAX_RELEASE_USDC" "$PERIOD_LIMIT_USDC" "$MAX_RELEASE_USDT" "$PERIOD_LIMIT_USDT" "${OWNER:-$DEPLOYER}" "$FIRST_DEPOSIT_ID"
      printf 'START_BLOCK=%s\nMINT=1\n' "$START_BLOCK"
      printf 'ROUTER=%s\nROUTER_BLOCK=%s\n' "$ROUTER" "$ROUTER_BLOCK"
    } > "$ENVFILE"
    echo
    echo "done on $NAME ($CHAIN_ID):"
    echo "  vault $VAULT (block $START_BLOCK): 3 guardians, threshold $THRESHOLD, timelock $DELAY_SECONDS s, deposit ids from $FIRST_DEPOSIT_ID, setup finalized"
    echo "  tokens USDC $USDC and $USDT_CONTRACT $USDT allowed (minimum 1 token each)"
    echo "  fees $FEE_DEPOSIT_BPS/$FEE_RELEASE_BPS bps; limits USDC $MAX_RELEASE_USDC/$PERIOD_LIMIT_USDC, USDT $MAX_RELEASE_USDT/$PERIOD_LIMIT_USDT (per release / per 24 h, whole tokens)"
    echo "  router $ROUTER (block $ROUTER_BLOCK)"
    echo "  $OWNER_NOTE"
    echo "wrote $ENVFILE:"
    cat "$ENVFILE"
    echo "next: configure the bridge services from this file (deploy/railway/README.md, Bridge)"
    ;;
  router)
    [ -f "$ENVFILE" ] || { echo "$ENVFILE not found; 'deploy $CHAIN' deploys the vault and the router together" >&2; exit 1; }
    if grep -q '^ROUTER=' "$ENVFILE" || grep -q '^ROUTER_BLOCK=' "$ENVFILE"; then
      echo "$ENVFILE already has ROUTER=$(envval ROUTER) (block $(envval ROUTER_BLOCK)); remove the ROUTER and ROUTER_BLOCK lines to deploy another" >&2
      exit 1
    fi
    FILE_CHAIN=$(envval CHAIN_ID)
    [ "$FILE_CHAIN" = "$CHAIN_ID" ] || { echo "$ENVFILE has CHAIN_ID=$FILE_CHAIN, expected $CHAIN_ID for $NAME" >&2; exit 1; }
    VAULT=$(envval VAULT)
    case "$VAULT" in 0x*) ;; *) echo "$ENVFILE has no VAULT address" >&2; exit 1 ;; esac
    REPORTED=$(rpc_chain)
    [ "$REPORTED" = "$CHAIN_ID" ] || { echo "$RPC is chain $REPORTED, not $NAME ($CHAIN_ID)" >&2; exit 1; }
    WEI=$(cast balance "$DEPLOYER" --rpc-url "$RPC" | tr -d '\r')
    [ "$CHAIN" = anvil ] || [ "$WEI" != "0" ] || { echo "deployer $DEPLOYER has no $NATIVE_SYMBOL on $NAME; fund it first, then run 'check'" >&2; exit 1; }
    CODE=$(cast code "$VAULT" --rpc-url "${DEPLOY_RPC:-$RPC}" | tr -d '\r')
    [ -n "$CODE" ] && [ "$CODE" != "0x" ] || { echo "no contract at VAULT=$VAULT on $NAME" >&2; exit 1; }
    cd "$CONTRACTS"
    echo "deploying the DepositRouter for vault $VAULT to $NAME ($CHAIN_ID) from $DEPLOYER"
    deploy_router "$VAULT"
    # Append on a line of its own even if the file lacks a final newline.
    [ -z "$(tail -c 1 "$ENVFILE")" ] || echo >> "$ENVFILE"
    printf 'ROUTER=%s\nROUTER_BLOCK=%s\n' "$ROUTER" "$ROUTER_BLOCK" >> "$ENVFILE"
    echo "appended to $ENVFILE:"
    tail -n 2 "$ENVFILE"
    ;;
esac
