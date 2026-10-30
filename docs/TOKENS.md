# Assets on Argonmine: ARG, wARG, uARG

The Argonmine chain carries three coins. The app and the command-line
wallet hold and send all three, and the explorer shows them.

| Coin | What it is | Where it comes from |
|---|---|---|
| **ARG** | The native coin. At most 21 million will ever exist. | Created only by mining, as block rewards. ARG is never issued by anyone and never leaves the Argonmine chain. Network fees are always paid in ARG. |
| **wARG** | Wrapped ARG, one for one. | Issued by the swap service when you swap ARG for it, and burned when you swap it back. The service holds the ARG behind every wARG in circulation and never pays that ARG out, so wARG can always be swapped back. See [Swaps](docs/SWAP.md). |
| **uARG** | A dollar token with 8 decimals. | Issued by the bridge when you deposit USDC or USDT on Ethereum, Base, Arbitrum One or BNB Chain. It is redeemable one for one for USDC or USDT through the bridge, less the 1% withdrawal fee, subject to the risks in [Bridge, Risks](docs/BRIDGE.md#risks). |

## The asset layer

wARG and uARG are assets of the chain itself, not entries in a database.
A version-2 transaction can carry assets other than ARG; each asset's id is
the hash of its issuer's key and its symbol, so only that key can create
units of it. Every node checks that no transaction creates units without
the issuer's signature, and that no units appear or vanish in a transfer.
The full rules are in [Specification §3.5](docs/SPEC.md).

## Telling the real coins from look-alikes

Anyone can issue an asset with the symbol UARG or WARG, but it gets a
different asset id. The wallets and the explorer recognise uARG by the
bridge's asset id and wARG by the swap service's, never by the name, and
show any other asset with those symbols as unverified, with no dollar value.

## More

- [Specification §3.5](docs/SPEC.md): the asset rules.
- [Bridge](docs/BRIDGE.md): deposits, withdrawals and the risks of uARG.
- [Swaps](docs/SWAP.md): wARG and swaps between the three coins.
- [Fees](docs/FEES.md): every fee, including the bridge fee and the swap spread.
