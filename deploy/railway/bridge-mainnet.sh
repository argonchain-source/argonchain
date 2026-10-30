#!/bin/sh
# Deploys the bridge's Vault and DepositRouter to one EVM MAINNET with the
# real USDC and USDT, and records them for the Railway services
# (deploy/railway/README.md, "Bridge"). POSIX sh; on Windows run it from Git
# Bash. The testnet script (bridge-chain.sh) deploys mock tokens and must not
# be used here.
#
#   deploy/railway/bridge-mainnet.sh check  <chain>   deployer balance, tokens as the chain reports them
#   deploy/railway/bridge-mainnet.sh deploy <chain>   Vault + DepositRouter, tokens allowed, fees set,
#                                                      setup finalized, ownership offered to OWNER;
#                                                      writes deploy/railway/chains-mainnet/<chain>.env
#
# Chains: ethereum, base, arbitrum, bsc. The token addresses below were
# checked on 2026-10-28 against Circle's and Tether's published lists and on
# chain (name, symbol, decimals, supply); the script checks symbol and
# decimals on chain again before allowing a token. Base has no USDT from
# Tether, so it serves USDC only. BNB Chain's USDC and USDT are the Binance-
# issued tokens with 18 decimals.
#
# Variables:
#   KEYS          directory with guardian1.json..guardian3.json (argonmine-bridge keygen)
#                 and deployer.json (the deploying key: never a guardian, never the gas key)
#   OWNER         required for deploy: the address the vault is handed to (transferOwnership);
#                 it must then call acceptOwnership() on each vault
#   DELAY_SECONDS the vault's timelock delay, default 604800 (7 days)
#   FOUNDRY       directory with forge and cast, if they are not on PATH
#   FEE_DEPOSIT_BPS, FEE_RELEASE_BPS   default 100 each (1 percent)
# Release limits are left at 0 (none), by the owner's decision of 2026-10-28.
set -eu

KEYS="${KEYS:?set KEYS to the directory with guardian1..3.json and deployer.json}"
[ -z "${FOUNDRY:-}" ] || PATH="$FOUNDRY:$PATH"
HERE=$(cd "$(dirname "$0")" && pwd)
CONTRACTS="$HERE/../../contracts"
THRESHOLD=2
DELAY_SECONDS="${DELAY_SECONDS:-604800}"
FEE_DEPOSIT_BPS="${FEE_DEPOSIT_BPS:-100}"
FEE_RELEASE_BPS="${FEE_RELEASE_BPS:-100}"
FEE_STEP_BPS=100
OWNER="${OWNER:-}"
ENV_DIR="${ENV_DIR:-$HERE/chains-mainnet}"

usage() { echo "usage: $0 <check|deploy> <ethereum|base|arbitrum|bsc>" >&2; exit 2; }
ACTION="${1:-}"; CHAIN="${2:-}"
case "$ACTION" in check | deploy) ;; *) usage ;; esac

USDT=""; USDT_DECIMALS=""; GAS_ARGS=""
case "$CHAIN" in
  ethereum)
    NAME="Ethereum"; CHAIN_ID=1; RPC=https://ethereum-rpc.publicnode.com; EXPLORER=https://etherscan.io; NATIVE_SYMBOL=ETH
    USDC=0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48; USDC_DECIMALS=6
    USDT=0xdAC17F958D2ee523a2206206994597C13D831ec7; USDT_DECIMALS=6
    MIN_WHOLE=10 ;; # a release on Ethereum costs real gas
  base)
    NAME="Base"; CHAIN_ID=8453; RPC=https://base-rpc.publicnode.com; EXPLORER=https://basescan.org; NATIVE_SYMBOL=ETH
    USDC=0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913; USDC_DECIMALS=6
    MIN_WHOLE=2 ;;
  arbitrum)
    NAME="Arbitrum One"; CHAIN_ID=42161; RPC=https://arb1.arbitrum.io/rpc; EXPLORER=https://arbiscan.io; NATIVE_SYMBOL=ETH
    USDC=0xaf88d065e77c8cC2239327C5EDb3A432268e5831; USDC_DECIMALS=6
    USDT=0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9; USDT_DECIMALS=6 # Tether's USDT0
    MIN_WHOLE=2 ;;
  bsc)
    NAME="BNB Chain"; CHAIN_ID=56; RPC=https://bsc-rpc.publicnode.com; EXPLORER=https://bscscan.com; NATIVE_SYMBOL=BNB
    USDC=0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d; USDC_DECIMALS=18
    USDT=0x55d398326f99059fF775485246999027B3197955; USDT_DECIMALS=18
    GAS_ARGS="--legacy"
    MIN_WHOLE=2 ;;
  *) usage ;;
esac
PUBLIC_RPC=$RPC
RPC="${RPC_OVERRIDE:-$RPC}" # a local fork for a rehearsal (anvil --fork-url); the env file keeps the public RPC
ENVFILE="$ENV_DIR/$CHAIN.env"

field() { sed -n 's/.*"'"$2"'": *"\([^"]*\)".*/\1/p' "$1" | head -n 1 | tr -d '\r'; }
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
num() { sed 's/ .*//'; }
units() { z=""; i=0; while [ "$i" -lt "$2" ]; do z="${z}0"; i=$((i + 1)); done; echo "$1$z"; }

for i in 1 2 3; do [ -f "$KEYS/guardian$i.json" ] || { echo "missing $KEYS/guardian$i.json" >&2; exit 2; }; done
[ -f "$KEYS/deployer.json" ] || { echo "missing $KEYS/deployer.json" >&2; exit 2; }
G1=$(field "$KEYS/guardian1.json" evm_address); G2=$(field "$KEYS/guardian2.json" evm_address); G3=$(field "$KEYS/guardian3.json" evm_address)
[ -n "$G1" ] && [ -n "$G2" ] && [ -n "$G3" ] || { echo "guardian key files have no evm_address" >&2; exit 2; }
DEPLOYER_KEY=$(field "$KEYS/deployer.json" evm_private)
[ -n "$DEPLOYER_KEY" ] || { echo "$KEYS/deployer.json has no evm_private" >&2; exit 2; }
case "$DEPLOYER_KEY" in 0x*) ;; *) DEPLOYER_KEY="0x$DEPLOYER_KEY" ;; esac
command -v cast >/dev/null 2>&1 || { echo "cast/forge not found: set FOUNDRY" >&2; exit 2; }
DEPLOYER=$(cast wallet address --private-key "$DEPLOYER_KEY" | tr -d '\r')
AUTH="--private-key $DEPLOYER_KEY"
for G in "$G1" "$G2" "$G3"; do
  [ "$(lower "$G")" != "$(lower "$DEPLOYER")" ] || { echo "the deployer is a guardian key; use a separate deployer.json" >&2; exit 2; }
done

call() { cast call "$@" --rpc-url "$RPC" | tr -d '\r'; }
send() {
  # shellcheck disable=SC2086
  OUT=$(cast send --rpc-url "$RPC" $AUTH $GAS_ARGS "$@" 2>&1) || { echo "$OUT" >&2; exit 1; }
}
create() {
  label=$1; shift
  echo "deploying $label..."
  # shellcheck disable=SC2086
  OUT=$(forge create --rpc-url "$RPC" $AUTH $GAS_ARGS --broadcast --json "$@" 2>&1) || { echo "$OUT" >&2; exit 1; }
  ADDR=$(echo "$OUT" | sed -n 's/.*"deployedTo": *"\([^"]*\)".*/\1/p' | head -n 1 | tr -d '\r')
  TXHASH=$(echo "$OUT" | sed -n 's/.*"transactionHash": *"\([^"]*\)".*/\1/p' | head -n 1 | tr -d '\r')
  [ -n "$ADDR" ] && [ -n "$TXHASH" ] || { echo "$OUT" >&2; exit 1; }
  echo "$label: $ADDR"
}
# token_ok ADDRESS WANT_SYMBOL WANT_DECIMALS: the chain agrees.
token_ok() {
  S=$(call "$1" "symbol()(string)" | tr -d '"'); D=$(call "$1" "decimals()(uint8)" | num)
  case "$2:$S" in USDC:USDC | USDT:USDT | USDT:USD*0) ;; *) echo "$1 reports symbol '$S', expected $2" >&2; exit 1 ;; esac
  [ "$D" = "$3" ] || { echo "$1 reports $D decimals, expected $3" >&2; exit 1; }
  echo "token $1: symbol $S, $D decimals"
}

REPORTED=$(cast chain-id --rpc-url "$RPC" | tr -d '\r')
[ "$REPORTED" = "$CHAIN_ID" ] || { echo "$RPC is chain $REPORTED, not $NAME ($CHAIN_ID)" >&2; exit 1; }

case "$ACTION" in
  check)
    echo "chain:     $NAME ($CHAIN_ID)"
    echo "deployer:  $DEPLOYER, balance $(cast balance "$DEPLOYER" --rpc-url "$RPC" --ether | tr -d '\r') $NATIVE_SYMBOL"
    echo "guardians: $G1 $G2 $G3 (threshold $THRESHOLD)"
    token_ok "$USDC" USDC "$USDC_DECIMALS"
    [ -z "$USDT" ] || token_ok "$USDT" USDT "$USDT_DECIMALS"
    echo "owner:     ${OWNER:-(not set: required for deploy)}"
    ;;
  deploy)
    case "$OWNER" in 0x????????????????????????????????????????) ;; *) echo "OWNER must be the 0x address to hand the vault to" >&2; exit 2 ;; esac
    [ "$(lower "$OWNER")" != "$(lower "$DEPLOYER")" ] || { echo "OWNER must not be the deployer" >&2; exit 2; }
    [ ! -f "$ENVFILE" ] || { echo "$ENVFILE exists: this chain is deployed (remove it only to deploy a replacement, see docs/BRIDGE.md)" >&2; exit 1; }
    [ "$(cast balance "$DEPLOYER" --rpc-url "$RPC" | tr -d '\r')" != 0 ] || { echo "the deployer $DEPLOYER has no $NATIVE_SYMBOL on $NAME" >&2; exit 1; }
    token_ok "$USDC" USDC "$USDC_DECIMALS"
    [ -z "$USDT" ] || token_ok "$USDT" USDT "$USDT_DECIMALS"
    mkdir -p "$ENV_DIR"
    cd "$CONTRACTS"
    create Vault Vault.sol:Vault --constructor-args "[$G1,$G2,$G3]" "$THRESHOLD" "$DELAY_SECONDS" 1
    VAULT=$ADDR
    START_BLOCK=$(cast receipt "$TXHASH" blockNumber --rpc-url "$RPC" | tr -d '\r')
    create DepositRouter DepositRouter.sol:DepositRouter --constructor-args "$VAULT"
    ROUTER=$ADDR
    ROUTER_BLOCK=$(cast receipt "$TXHASH" blockNumber --rpc-url "$RPC" | tr -d '\r')
    [ "$(lower "$(call "$ROUTER" "vault()(address)")")" = "$(lower "$VAULT")" ] || { echo "the router does not name the vault" >&2; exit 1; }

    USDC_MIN=$(units "$MIN_WHOLE" "$USDC_DECIMALS")
    send "$VAULT" "setToken(address,bool,uint256)" "$USDC" true "$USDC_MIN"
    if [ -n "$USDT" ]; then
      USDT_MIN=$(units "$MIN_WHOLE" "$USDT_DECIMALS")
      send "$VAULT" "setToken(address,bool,uint256)" "$USDT" true "$USDT_MIN"
    fi
    REL=0
    while [ $((FEE_RELEASE_BPS - REL)) -gt "$FEE_STEP_BPS" ]; do REL=$((REL + FEE_STEP_BPS)); send "$VAULT" "setFees(uint16,uint16)" "$FEE_DEPOSIT_BPS" "$REL"; done
    send "$VAULT" "setFees(uint16,uint16)" "$FEE_DEPOSIT_BPS" "$FEE_RELEASE_BPS"
    [ "$(call "$VAULT" "guardianCount()(uint256)")" = 3 ] || { echo "guardianCount is not 3" >&2; exit 1; }
    [ "$(call "$VAULT" "isAllowed(address)(bool)" "$USDC")" = true ] || { echo "USDC not allowed" >&2; exit 1; }
    send "$VAULT" "finalizeSetup()"
    [ "$(call "$VAULT" "finalized()(bool)")" = true ] || { echo "setup not finalized" >&2; exit 1; }
    send "$VAULT" "transferOwnership(address)" "$OWNER"
    [ "$(lower "$(call "$VAULT" "pendingOwner()(address)")")" = "$(lower "$OWNER")" ] || { echo "pendingOwner is not $OWNER" >&2; exit 1; }
    {
      printf 'NAME="%s"\nCHAIN_ID=%s\nRPC=%s\nPUBLIC_RPC=%s\nEXPLORER=%s\nNATIVE_SYMBOL=%s\n' "$NAME" "$CHAIN_ID" "$PUBLIC_RPC" "$PUBLIC_RPC" "$EXPLORER" "$NATIVE_SYMBOL"
      printf 'VAULT=%s\nSTART_BLOCK=%s\nROUTER=%s\nROUTER_BLOCK=%s\n' "$VAULT" "$START_BLOCK" "$ROUTER" "$ROUTER_BLOCK"
      printf 'USDC=%s\nUSDC_DECIMALS=%s\nUSDC_MIN_DEPOSIT=%s\n' "$USDC" "$USDC_DECIMALS" "$USDC_MIN"
      [ -z "$USDT" ] || printf 'USDT=%s\nUSDT_DECIMALS=%s\nUSDT_MIN_DEPOSIT=%s\n' "$USDT" "$USDT_DECIMALS" "$USDT_MIN"
      printf '# a record: timelock %s s, no release limits, owner offered to %s (must accept)\n' "$DELAY_SECONDS" "$OWNER"
      printf 'MINT=0\n'
    } > "$ENVFILE"
    echo "done on $NAME: vault $VAULT (block $START_BLOCK), router $ROUTER; ownership offered to $OWNER"
    cat "$ENVFILE"
    ;;
esac
