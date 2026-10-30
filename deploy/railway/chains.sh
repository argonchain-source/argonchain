#!/bin/sh
# Prints the bridge's chains document (the -chains file; docs/BRIDGE.md,
# "Configuration") on one line, built from the per-chain files that
# deploy/railway/bridge-chain.sh writes to deploy/railway/chains/<chain>.env:
#
#   NAME, CHAIN_ID, RPC, EXPLORER, NATIVE_SYMBOL, VAULT, START_BLOCK, MINT,
#   USDC, USDC_DECIMALS, USDT, USDT_DECIMALS          (either token may be absent)
#
# and, optionally, CONFIRMATIONS (default 12), PUBLIC_RPC (what browsers get
# and what /api/status shows; without it the status shows no RPC for the
# chain, never RPC, which may carry an API key), USDC_MIN_DEPOSIT /
# USDT_MIN_DEPOSIT in token units (default 1 token), ROUTER / ROUTER_BLOCK:
# the chain's DepositRouter and its deployment block, which turn on deposit
# addresses for that chain (a chain without ROUTER has none), and
# L1_START_HEIGHT: the first Argonmine height whose burns the chain's vault
# pays (set it to the current Argonmine height plus one before replacing a
# vault; docs/BRIDGE.md, "Replacing a vault").
#
# Hardening (docs/BRIDGE.md, "Finality and providers", "Release
# transactions", "Per-deposit cap"), all optional:
#
#   RPCS                more RPC providers, comma-separated ("rpcs"): deposits are
#                       acted on only when RPC and all of these agree
#   FINALITY            "finalized" or "depth" ("finality"); default by chain id
#   MAX_GAS_PRICE_GWEI  cap on release gas prices, gwei ("max_gas_price_gwei")
#   MIN_GAS_BALANCE     gas key balance below which the status warns, in the
#                       native coin, "0.01" ("min_gas_balance")
#   USDC_MAX_DEPOSIT / USDT_MAX_DEPOSIT
#                       per-deposit cap in token units ("max_deposit"): larger
#                       deposits are held for the guardians
#
#   deploy/railway/chains.sh                       every chains/*.env, alphabetically
#   deploy/railway/chains.sh sepolia base-sepolia  these, in this order
#
# The first chain's first token (USDC before USDT) is what a short BRG1 memo
# pays out, so name the chains in the order you want. Use the output as the
# CHAINS_JSON variable of every bridge service:
#
#   railway variable set "CHAINS_JSON=$(deploy/railway/chains.sh sepolia)" -s bridge --skip-deploys
#
# POSIX sh plus python3 (no jq); on Windows run it from Git Bash.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DIR="${CHAINS_DIR:-$HERE/chains}"
PY=python3
command -v python3 >/dev/null 2>&1 || PY=python
command -v "$PY" >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }

[ -d "$DIR" ] || { echo "no chain directory $DIR (run deploy/railway/bridge-chain.sh <chain> first)" >&2; exit 2; }

# The directory and the chain names go to Python as arguments, so paths with
# spaces survive and no shell word-splitting is involved.
"$PY" -I - "$DIR" "$@" <<'EOF'
import json, os, sys

def read_env(path):
    kv = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            kv[k.strip()] = v.strip().strip('"').strip("'")
    return kv

d, names = sys.argv[1], sys.argv[2:]
if not names:
    names = sorted(f[:-4] for f in os.listdir(d) if f.endswith(".env"))
if not names:
    sys.exit(f"no chain files in {d} (run deploy/railway/bridge-chain.sh <chain> first)")
chains = []
for name in names:
    path = os.path.join(d, name + ".env")
    if not os.path.isfile(path):
        sys.exit(f"missing {path} (run deploy/railway/bridge-chain.sh {name} first)")
    kv = read_env(path)
    missing = [k for k in ("NAME", "CHAIN_ID", "RPC", "VAULT") if not kv.get(k)]
    if missing:
        sys.exit(f"{path}: missing {', '.join(missing)}")
    mint = kv.get("MINT", "").lower() not in ("", "0", "false", "no")
    tokens = []
    for sym in ("USDC", "USDT"):
        addr = kv.get(sym)
        if not addr:
            continue
        t = {"symbol": sym, "address": addr, "decimals": int(kv.get(sym + "_DECIMALS") or 6), "mint": mint}
        if kv.get(sym + "_MIN_DEPOSIT"):
            t["min_deposit"] = kv[sym + "_MIN_DEPOSIT"]
        if kv.get(sym + "_MAX_DEPOSIT"):
            t["max_deposit"] = kv[sym + "_MAX_DEPOSIT"]
        tokens.append(t)
    if not tokens:
        sys.exit(f"{path}: neither USDC nor USDT is set")
    c = {
        "name": kv["NAME"], "chain_id": int(kv["CHAIN_ID"]), "rpc": kv["RPC"],
        "explorer": kv.get("EXPLORER", ""), "native_symbol": kv.get("NATIVE_SYMBOL") or "ETH",
        "vault": kv["VAULT"], "start_block": int(kv.get("START_BLOCK") or 0),
        "confirmations": int(kv.get("CONFIRMATIONS") or 12), "tokens": tokens,
    }
    if kv.get("PUBLIC_RPC"):
        c["public_rpc"] = kv["PUBLIC_RPC"]
    rpcs = [u.strip() for u in kv.get("RPCS", "").split(",") if u.strip()]
    if rpcs:
        c["rpcs"] = rpcs
    if kv.get("FINALITY"):
        if kv["FINALITY"] not in ("finalized", "depth"):
            sys.exit(f"{path}: FINALITY must be finalized or depth")
        c["finality"] = kv["FINALITY"]
    if kv.get("MAX_GAS_PRICE_GWEI"):
        try:
            g = float(kv["MAX_GAS_PRICE_GWEI"])
        except ValueError:
            sys.exit(f"{path}: MAX_GAS_PRICE_GWEI must be a number of gwei")
        c["max_gas_price_gwei"] = int(g) if g == int(g) else g
    if kv.get("MIN_GAS_BALANCE"):
        c["min_gas_balance"] = kv["MIN_GAS_BALANCE"]
    if kv.get("ROUTER"):
        c["router"] = kv["ROUTER"]
        if kv.get("ROUTER_BLOCK"):
            c["router_block"] = int(kv["ROUTER_BLOCK"])
    if kv.get("L1_START_HEIGHT"):
        c["l1_start_height"] = int(kv["L1_START_HEIGHT"])
    chains.append(c)
ids = [c["chain_id"] for c in chains]
if len(ids) != len(set(ids)):
    sys.exit("the same chain id appears in two files")
print(json.dumps({"chains": chains}, separators=(",", ":")))
EOF
