# FAQ

Short answers, with links to the pages that go deeper.

## Mining

**Is mining in a browser real mining?**
Yes. The tab hashes real block headers with Argon2id against the real
target, and a solution is a real block that pays the reward to your
address (from block 300, less 1% of the subsidy to the project treasury).
It is slower than a desktop miner with many threads, but it is the
same work. See [Mining](docs/MINING.md).

**Why Argon2id and not SHA-256?**
Each Argon2id attempt needs 8 MiB of memory. That makes memory bandwidth
the limit instead of raw silicon, so a phone or laptop stays within reach
of purpose-built hardware. SHA-256 mining is dominated by ASIC farms, and a
phone earns nothing there.

**How much will I earn?**
Your share of blocks is your hashrate divided by the network's. The Mine
page shows the expected time to a block from live figures. Blocks are
random: the expected time is an average, not a schedule. From block 300,
each block pays its miner 49.5 ARG plus its fees at today's subsidy.

**Why can't I spend a reward right away?**
Rewards unlock 100 blocks after they are mined, about 17 hours. A reorg
could otherwise remove a block whose reward was already spent. The app
shows locked rewards as "maturing" with the moment each one unlocks.

**Refreshing the page stopped mining. Is that normal?**
A reload destroys the tab's worker, so mining stops for a moment and then
resumes automatically. Closing the tab stops mining until you open the app
again, or turn on "Start when the app opens" in Settings.

**Can I mine on several devices with the same address?**
Yes. Every device fetches its own templates and every block pays the same
address. There is no conflict.

**What is "Rejected"?**
A solved block the network turned down, almost always because another
miner's block at the same height arrived first. Only that attempt is lost.

**Is there a pool?**
No, by design. The browser app mines whole blocks on its own, with the
explorer supplying templates and forwarding solutions. Nobody holds funds
in between and there is no payout to wait for.

## Wallet and keys

**Where are my keys?**
When you create a wallet in the app, the 24 words are generated in your
browser. The app keeps them in that browser only, encrypted under your
password. Nothing that could spend your coins is ever sent to a server.

**What if I lose the 24 words?**
The coins at that address cannot be spent by anyone, ever. Write the words
on paper when they are shown, and use **Check your backup** in Settings to
check your copy. While the wallet is still in your browser, **Show recovery
phrase** in Settings shows the words again after your password.

**How do I withdraw uARG to USDC or USDT?**
Open **Withdraw** on the bridge page. Paste the EVM wallet address to be
paid, enter the amount of uARG, pick the network and token, and press
**Continue**. The page shows an address (it starts with `arg1qswap`), its
QR code and a **memo** such as `K7M2-X9RT`.

Send exactly that amount of uARG to the address from any Argonmine wallet,
with the memo. In the app: **Send**, paste the address, and a **Memo** field
appears; paste the memo there. The bridge page shows the payment within
seconds and follows it: Waiting for deposit, Deposit detected, Processing
withdrawal, Completed. A withdrawal of up to 1,000 uARG takes about an
hour; larger ones wait longer (see [Bridge](docs/BRIDGE.md)). The smallest
withdrawal is 10 USDC or USDT on Ethereum and 2 on the other chains. You
can close the page: opening the same link again shows where it is.

If you send less, what arrived is paid out, and you can send the rest with
the same memo. If you send more, all of it is paid out. A payment without
the memo still arrives at the same EVM address. Send only uARG: anything
else sent to that address is lost, which is why the Argonmine wallets
refuse to send it.

**What can I send from the browser app?**
ARG, uARG and wARG, to any Argonmine address. Swap trades the three with
each other; USDC and USDT on other chains go through the bridge. The browser signs the payment itself: the 24
words are kept there only locked under your password, and the key never
leaves the wallet's worker. Nothing goes to a server but the signed
payment.

**Is there a password or two-factor login?**
There is no account, so there is nothing to log in to. The password only
unlocks the wallet in the browser that keeps it; the 24 words are the whole
wallet. Anyone with them has the coins; nobody without them does. Forgot
the password? Forget the wallet in Settings and open it again with the 24
words.

**Can I use the same wallet on several devices?**
Yes. Log in with the same 24 words on each device. They all open the same
address and the same coins.

## The coin

**Is there a premine?**
No. Every ARG that will ever exist is created as a block reward, starting
from a genesis block whose coinbase is not spendable. That includes the
treasury's share below: it is mined too.

**Is there a fee on mining?**
Yes, from block 300. Every mainnet block from block 300 on pays 1% of the
block subsidy to the project treasury: 0.5 ARG of today's 50 ARG, halving
with the subsidy. It is a consensus rule: a block that leaves the treasury
out is invalid. Transaction fees are not touched; they stay with the
miner. Blocks 1 to 299 paid the miner in full. With an invite, the 5%
referral share is of what remains after the treasury's 1%. The treasury
address is `arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2`; anyone can watch it on the
[explorer](https://explorer.argonmine.com/address/arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2).
Its key is held offline by the project. See [Fees](docs/FEES.md).

**What is the supply?**
A little under 21 million ARG: 50 ARG per block, halving every 210,000
blocks. See [Mining](docs/MINING.md).

**What are fees?**
Transactions pay a fee in ARG to the miner of their block. The default
relay policy asks for at least one unit per byte, a fraction of a cent.
The bridge is separate: its vaults keep 1 percent of every deposit and 1
percent of every withdrawal, enforced by the vault contract and shown on
the bridge page before you confirm. The project moves these bridge fees
and the swap spread to its treasury regularly. Every fee is listed in
[Fees](docs/FEES.md).

**What are wARG and uARG?**
Assets on the Argonmine chain besides ARG. uARG is a dollar token backed by
USDC and USDT held in the bridge vaults on Ethereum, Base, Arbitrum One and
BNB Chain. It is redeemable one for one for USDC or USDT through the
bridge, to the chain and token you choose, less the 1% withdrawal fee,
subject to the risks in [Bridge, Risks](docs/BRIDGE.md#risks). wARG is
wrapped ARG, one to one, issued by the swap service when you swap ARG for it and backed by the
ARG it holds. Swap in the app trades the three with each other. ARG itself
never leaves the chain. See [Assets](docs/TOKENS.md) and
[Swaps](docs/SWAP.md).

**Does ARG trade on other chains?**
No. The bridge brings value in and lets it out again as uARG; it does not
export ARG.

## The network

**Is this mainnet?**
Yes. Mainnet launched on 28 October 2026, 12:00 UTC and is the only public network.
Its addresses start with `arg1`, and its coins have real value. The app
shows the network in Settings, under About.

**What happened to the testnet?**
It is retired. Testnet coins and test bridge balances do not carry over to
mainnet. Your testnet 24 words still log in, but on mainnet they open a
new, empty `arg1` address, because the testnet used other keys.

**Why are Swap and the bridge not working yet?**
In the first day after launch, both wait for mined ARG. The swap service
needs unlocked ARG to trade, and the bridge needs it to pay the network fee
of each uARG issuance. Mined ARG unlocks 100 blocks after it is mined, so
both start about a day after launch. A deposit made before then waits in
the vault and is issued then.

**Does the explorer hold my coins?**
No. It reads the chain and serves pages and an API. The work server inside
it builds templates and forwards solved blocks; it has no wallet.

**Why no smart contracts?**
A scripting engine is the largest attack surface a chain can have. Argonmine
has two output types and one signature scheme, which is enough to move
value and carry assets, and small enough to review line by line. A safe
predicate layer is a candidate for later. See
[Specification §12](docs/SPEC.md).

**How do I run a node?**
You do not need one to use or mine Argonmine. For operators who want to
verify the chain themselves, [Run a node](docs/NODE.md) covers hardware,
installation, configuration and monitoring.

**What do I get for running a node?**
Nothing from the protocol, and that is deliberate: there is no masternode
reward, no staking and no fee share, because every one of those would be
coins created for something other than mining. What pays is mining, and
a node can mine too (see [Run a node](docs/NODE.md#mining-with-a-node)).
So the honest advice is: do not run a bare node for income. Bare nodes
still matter for the network, as seed nodes and as independent verifiers,
and are run by people and projects with a reason to keep the chain
healthy, exactly as on Bitcoin. The treasury may fund public seed nodes
later. A node that mines must run release 1.1.0 before block 300.

**Has the code been audited?**
Not by an outside firm. Internal reviews of the chain, node and network
code recorded 27 findings, all fixed. On 2026-10-28, before launch, four
more in-house reviews were done, each followed by a second pass over its
fixes. All of it is published in the
[Security review](docs/SECURITY-REVIEW.md) ("Round of 2026-10-28").

The bridge holds real money without an outside audit and without limits
per deposit, per withdrawal or per day, so a bug the reviews missed could
cost what the vaults hold. Bridge only what you can afford to have at
risk. An outside audit follows when funds allow (see the
[Roadmap](docs/ROADMAP.md)). To report a problem, see the
[Security policy](SECURITY.md).
