# Fees

This page lists every fee in Argonmine in one place: the rate, who pays it,
where it goes, who can change it and how you can check it yourself. The
rates are the ones in force on mainnet since its launch on
28 October 2026, 12:00 UTC, except the treasury share, which applies from
block 300.

## Is there a fee on mining rewards?

**Yes, from block 300.** From mainnet block 300, every block pays 1% of the
block subsidy to the project treasury: 0.5 ARG of today's 50 ARG, so the
miner receives 49.5 ARG plus the fees. The share halves with the subsidy.
This is a consensus rule: a block that leaves the treasury out is invalid.
Transaction fees are not touched; they stay with the miner, as in Bitcoin.
Blocks 1 to 299 paid the miner in full. There is no premine: the
treasury's coins are mined like every other coin. This was decided on
28 October 2026.

The only other part of a block that can go to someone else is a referral
share, and only if you accepted an invite yourself (see
[Referral share](#referral-share) below).

## The treasury

| Item | Value |
|---|---|
| Address | `arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2` |
| Key | Held offline by the project. |
| Receives | 1% of the subsidy of every block from block 300, paid by the chain inside the block. The swap service pays its spread to the treasury automatically, and the project moves the bridge fees there. |
| How to check | The address is public. Anyone can watch it on the [explorer](https://explorer.argonmine.com/address/arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2). From block 300, every block's reward transaction shows its output to this address. |

Running a node that does not mine earns nothing from the protocol. The
treasury may fund public seed nodes later.

## Every fee at a glance

| Fee | Rate | Who pays | Where it goes |
|---|---|---|---|
| [Network fee](#network-fee) | Set by the sender. The app pays 2 units per byte, about 0.000004 ARG for a simple payment. | Whoever sends a transaction | The miner of the block that includes it |
| [Bridge deposit fee](#bridge-fees) | 1% of the deposit | You, when you deposit USDC or USDT | The treasury. It collects in the bridge vault on that chain as surplus, and the project moves it to the treasury regularly |
| [Bridge withdrawal fee](#bridge-fees) | 1% of the withdrawal | You, when you withdraw uARG to USDC or USDT | The treasury, the same way as the deposit fee |
| [Swap spread](#swap-spread) | 1% on each swap between ARG or wARG and uARG | You, as a slightly lower amount received | The treasury, paid out automatically once a coin's earned fees reach 1 ARG or 1 uARG |
| [Wrap fee](#swap-spread) | 0% between ARG and wARG | Nobody today | n/a |
| [Referral share](#referral-share) | 5% of each block you mine, after the treasury's share, only if you accepted an invite | You, out of your own reward | The person who invited you, inside the block |
| [Treasury share](#is-there-a-fee-on-mining-rewards) | 1% of the block subsidy, from block 300. Fees are not touched | Every miner, by consensus rule | [The treasury](#the-treasury), inside the block |

From block 300 the miner keeps the rest of each block: 99% of the subsidy (49.5 ARG at
today's subsidy) and every transaction fee, less any referral share.

## Network fee

Every transaction pays a small fee in ARG. It goes to the miner who puts the
transaction in a block. This is how miners are paid once block rewards
become small.

- **Rate.** The sender chooses it, in units per byte (1 ARG =
  100,000,000 units). Nodes relay a transaction only if it pays at least 1
  unit per byte. A transaction with more than two outputs of uARG, wARG or
  another asset adds 546 units for each extra such output, so spam with
  many tiny asset outputs costs about what it would in ARG.
- **What the app pays.** The browser wallet pays 2 units per byte. A simple
  payment is about 200 bytes, so its fee is about 400 units
  (0.000004 ARG). The app shows the exact fee before you confirm.
- **Paid in ARG only,** also when you send uARG or wARG. If your wallet holds
  no ARG, the app first swaps the smallest allowed amount of your uARG or
  wARG to ARG (0.01 by default). That ARG pays this fee and many later ones.
  The swap spread applies to that small swap.
- **Swaps.** The swap service pays the network fee of every swap
  transaction itself.
- **Who controls it.** You, through your wallet. Node operators set the
  relay minimum. Miners choose which transactions to include, and nodes put
  the best-paying first.
- **How to check.** Every transaction page on the
  [explorer](https://explorer.argonmine.com) shows the fee and the fee rate.
  A block's reward transaction shows the subsidy plus all fees paid to the
  miner, less the treasury's 1% of the subsidy from block 300.

## Bridge fees

The bridge charges **1% on deposits and 1% on withdrawals.** Both are taken
by the vault contract on the EVM chain, in the token you deposit or
receive.

- **On a deposit,** the vault keeps 1% and credits the rest. You receive
  uARG for the net amount. Example: deposit 100 USDC, receive 99 uARG.
- **On a withdrawal,** the vault keeps 1% of the payout and sends you the
  rest. Example: withdraw 100 uARG, receive 99 USDC.
- **Rounding** always favours you: a fee on a tiny amount rounds down,
  so you never pay more than 1%. A 6-decimal token cannot pay fractions
  below 0.000001; that dust, less than a millionth of a dollar, is not paid
  out.
- **Minimums.** The smallest deposit and the smallest withdrawal are 10
  USDC or USDT on Ethereum, where gas is expensive, and 2 on Base,
  Arbitrum One and BNB Chain. These are minimums, not fees.
- **Where it goes.** To the treasury. The fee first collects in the vault
  as **surplus**: the part of the vault's balance above what backs the uARG
  in circulation, and the project moves it to the treasury regularly. The vault
  tracks that backing (its `obligations`) for each token, and the surplus is
  never needed to back uARG. Only the vault's owner can withdraw surplus
  (`withdrawSurplus`), and never more than the surplus. The backing can
  leave the vault only as a withdrawal signed by the guardians.
- **Who controls the rate.** The vault's owner sets the rates in the
  contract. The contract caps each fee at 10%. A fee change waits behind
  the vault's timelock, 7 days on every mainnet vault, so you see it coming,
  and the withdrawal fee can rise by at most 1 percentage point per change.
  The owner is currently one wallet of the project, not a key on a
  server. A multisig owner is planned (see below).
- **Network fees and gas.** These are not bridge fees; they go to the
  networks involved.
  - Depositing from an EVM wallet costs you that chain's gas for the
    approval and the deposit.
  - With a deposit address you pay only the gas of your own transfer to
    it. The bridge pays the gas to move it into the vault.
  - A withdrawal costs you no EVM gas: the bridge pays the gas of every
    payout. Your payment of uARG to the withdrawal address is an ordinary
    Argonmine transaction and pays the usual network fee.
- **How to check.**
  - The bridge page shows the fee and the amount you will receive before
    you confirm.
  - `https://bridge.argonmine.com/api/status` lists `deposit_fee_bps` and
    `release_fee_bps` for every chain (100 means 1%), the backing and the
    surplus per token, and the totals `backing_units` and `surplus_units`.
  - Each vault's `depositFeeBps()`, `releaseFeeBps()`, `obligations(token)`,
    `surplus(token)` and `owner()` can be read on that chain's block
    explorer. The vault addresses are listed in
    [Bridge, Chains and tokens](docs/BRIDGE.md#chains-and-tokens).
  - Every deposit and payout event on the vault records its fee.

Details: [Bridge, Fees](docs/BRIDGE.md#fees).

## Swap spread

The swap service trades ARG, wARG and uARG with your wallet on the
Argonmine chain. Its price includes a spread.

- **Rate.** 1% on each swap between ARG and uARG, and between wARG and
  uARG, in either direction. Swaps between ARG and wARG have a wrap fee,
  which is 0% today.
- **How it works.** You receive 1% less than the plain price would give.
  Example: at a price of 0.10 uARG per ARG, swapping 100 ARG gives
  9.9 uARG instead of 10.
- **Where it goes.** To the treasury, automatically. The swap service
  books each swap's 1% in the coin you paid (ARG for ARG and wARG, uARG
  for uARG), counts it as earned once the swap has 6 confirmations, and
  pays it to the treasury address in one payment once a coin's earned fees
  reach 1 ARG or 1 uARG. Fees not yet paid out are never used for swaps.
- **Who controls it.** The operator of the swap service, through its
  settings (`-spread-bps`, `-wrap-fee-bps` and the price). The price is a
  setting, not a market: 1 ARG = 0.10 uARG before the spread.
- **When it starts.** The swap service starts trading once it holds mined
  ARG that has unlocked, about a day after launch.
- **How to check.** The app shows the rate and the amount you receive before
  you confirm, and your wallet checks the swap transaction against that
  quote before it signs. The service's current settings are public at
  `https://app.argonmine.com/swap/api/info` (`spread_bps`, `wrap_fee_bps`,
  and the `rate` of each pair after fees).

Details: [Swaps](docs/SWAP.md).

## Referral share

- **Rate.** 5% of each block you mine, counting the subsidy and the
  transaction fees. From block 300 it is 5% of what remains after the
  treasury's 1% of the subsidy. It applies only if you opened someone's
  invite link and pressed **Accept**.
- **Who pays.** You, the miner. It comes out of your reward. No new coins are
  created for it.
- **Where it goes.** To the person who invited you, as a second output of
  the block's reward transaction. It is paid by the chain the moment the
  block is mined. Nobody holds it in between.
- **Who controls it.** You decide whether to accept, and you can remove the
  inviter at any time on the Dashboard. The rate is set by the operator of
  the work server (the explorer's `-referral-share`, at most 50%). It
  applies only to browser miners who get their work from the explorer.
- **How to check.** The Mine page shows the share while it applies. On the
  explorer, a block mined with a referral has an output to the inviter in
  its reward transaction, beside the miner's (and, from block 300, the
  treasury's).

Details: [Mining, Invite links](docs/MINING.md#invite-links-and-referral-shares).

## Where the fees go next

The bridge surplus can be withdrawn only by the vault owner, currently one
wallet of the project, and the swap spread collects in the swap service's
wallet. The project moves both to the treasury regularly. The plan is:

- **A treasury on each EVM chain** whose keys are not on any server: a
  multisig wallet (Safe) that needs 2 of 3 signatures, with the keys on
  separate devices. The bridge surplus is withdrawn only to it.
- **Vault ownership** moves to that multisig.
- **Public fee records:** a Treasury page on the explorer listing every fee
  received and every payment out, per coin and chain.

The [Roadmap](docs/ROADMAP.md) tracks these items.
