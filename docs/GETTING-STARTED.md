# Getting started

The Argonmine app runs in a web browser, on a phone or a computer, with
nothing to install. In about a minute you can create a wallet and start
mining. This page walks through the app screen by screen and explains every
figure it shows.

## Before you start

- Any current browser works: Chrome, Edge, Firefox, Safari, and their mobile versions.
- Mining uses the processor. A laptop on battery or a phone will run warm. Plug in when you can.
- The app runs on mainnet, which launched on 28 October 2026, 12:00 UTC. Your address starts with `arg1`.
- Coins on mainnet have real value. A payment cannot be undone, and nobody can restore lost 24 words.
- In the first day after launch, **Swap** and the bridge are not working yet. Each starts once it holds mined ARG that has unlocked, about a day after launch. A bridge deposit made before then waits in the vault and is issued then.

## 1. Create a wallet

Open the app. It offers two choices.

**Create a new wallet.** The app generates a 24-word recovery phrase inside
your browser and shows it at once. Write the words down, in order, on paper.
They are the only way back to the wallet if this browser loses it: the
explorer never sees them, and there is no account to recover them from.
Confirm that you wrote them down, type back four words at the positions the
app asks for, then choose a password. The app keeps the words in this
browser only locked under that password, and asks for it before a payment
when the wallet has been idle for 15 minutes. Settings, **Show recovery
phrase**, shows the words again after the password.

**Log in with your 24 words.** Already have a wallet? Enter its 24 words
and choose a password for this browser. The same words open the same coins
anywhere. **Log out** in Settings removes the wallet from the browser; the
24 words bring it back.

Your address is shown on the Mine page under "Rewards go to", on the Wallet
page with a QR code, and in Settings.

> The 24 words are a standard BIP-39 phrase, so the same words open the same
> address and coins in every Argonmine wallet.

## 2. Start mining

Press **Start mining**. The miner loads into a background worker in the tab,
asks the explorer for a block template that pays your address, and starts
hashing. You can switch to other pages of the app while it runs. From
block 300, every block you find pays 1% of its subsidy to the project
treasury and the rest to you, with all its fees (see [Fees](docs/FEES.md)).

A few things to know:

- **Keep the tab open.** A browser tab can only work while it exists. Closing it stops the miner.
- **Reloading is fine.** A reload restarts the tab, so mining stops for a second and then resumes by itself.
- **Phones throttle background tabs.** Keep the app in the foreground. In Settings, "Keep the screen awake" stops the phone from sleeping while mining.
- **Start when the app opens** in Settings makes every visit start mining without a press.

## 3. Reading the Mine page

| Figure | Meaning |
|---|---|
| Hashrate | Attempts per second this tab is making. The chart shows the last three minutes. |
| Found this session | Blocks this tab has solved since you pressed Start. |
| Hashes | Attempts since you pressed Start. |
| Rejected | Solved blocks the network refused, usually because someone else solved the same height first. Rare. |
| Running | Time since you pressed Start. |
| Working on block | The height your current attempts would create. |
| Block reward | The subsidy of the next block, in ARG. From block 300, 1% of it goes to the project treasury; the miner keeps the rest and all fees. |
| Network hashrate | Attempts per second across the whole network, estimated from recent blocks. |
| Your share | Your hashrate divided by the network's, including yours. |
| Expected time to a block | On average, how long until this tab finds a block at the current rates. Blocks are random; the real wait can be much shorter or longer. |
| Referral share | Shown only if you opened the app through an invite link. The fixed share of your rewards that goes to the inviter. |
| Blocks mined | All blocks ever paid to this address, with when the last one was found. |
| Mined in total | Everything those blocks paid, including referral rewards you received as an inviter. |
| Matured | Rewards that are now spendable. |
| Maturing | Rewards still locked, with a bar for the one that unlocks next. |
| Blocks you found | The most recent blocks paid to this address, each with the moment it unlocks. |

Rewards unlock 100 blocks after the block that paid them, about 17 hours at
the target block interval. While few people mine, blocks can come more
slowly than every 10 minutes, and the wait is longer (see
[Mining at launch](docs/MINING.md#mining-at-launch)). The lock exists so
that a short chain reorganisation cannot make an already-spent reward
disappear.

## 4. The Wallet page

The balance card shows the amount you can spend now. Below it, "maturing"
is what mining has paid that is still locked. **Receive** shows your address
and its QR code; anyone can send ARG to it, and mining rewards arrive there
too. The Tokens tab lists ARG, uARG and wARG; the Activity tab is a short
version of History, with payments still waiting for a block at the top.

**Send** pays ARG, uARG or wARG to another Argonmine address: paste the
address, pick the coin, enter the amount, check the fee and press Send. The
payment is signed in this browser and confirms with the next block. The
network fee is paid in ARG; a wallet that holds only uARG or wARG pays it
with the smallest swap to ARG, shown in the review.

**Swap** trades ARG, wARG and uARG with each other on this chain; the swap
service pays the network fee. Moving uARG to or from USDC and USDT on other
chains is done on the bridge.

## 5. The Dashboard

Your account at a glance:

- **Holdings:** ARG, uARG and wARG you can spend now, with ARG still unlocking.
- **Four totals:** blocks mined, earned from mining, referral earnings, and
  rewards still unlocking, with the time to the next unlock.
- **Activity:** every transaction that touched your address, grouped by day,
  with a link to the explorer. Filters show mined, received, sent or swaps;
  **Show more** loads older entries.
- **Invite friends:** your invite link, with Share and Copy. When a friend
  mines with it, a share of each block they find (5% today, of what remains
  after the treasury's 1% from block 300) comes to you inside the block. It
  comes out of their reward; nothing extra is created. If you came through a link, it shows who invited you, with a
  button to remove them.
- **Miner:** your hashrate, the network's, and the expected time to a block.

## 6. Settings

| Section | What is there |
|---|---|
| Account | Your address, with Copy and its QR code. |
| Security | Lock now, show your 24 words (after your password), and check that a written copy of your words belongs to this wallet. |
| Mining | Start when the app opens. Keep the screen on while mining. The mining server in use. |
| Appearance | System, light or dark. |
| About | The network and the app version. |
| Log out | Removes the wallet from this browser. The coins stay on the chain; your 24 words log in again. |

The password only locks the wallet in this browser. Whoever has the 24
words has the coins, and nobody else does.

## 7. Phones and tablets

Everything above works on mobile browsers. Expect a lower hashrate than a
laptop, and expect the phone to warm up. Turn on "Keep the screen awake" and
leave the app in the foreground; mobile browsers slow down or stop background
tabs to save battery. Mining pauses while the device is asleep and resumes
when the app is visible again.

## Troubleshooting

| Message or symptom | What it means |
|---|---|
| "The work server is not reachable right now. Retrying." | The explorer that supplies block templates is down or your connection dropped. The miner keeps retrying and the message clears by itself. |
| "This browser cannot run the miner" | The browser does not support WebAssembly or web workers. Update it, or use another browser. |
| Rejected count goes up | Your solved block arrived after someone else's at the same height. Nothing is lost except that attempt. |
| Balance or history does not update | Both come from the explorer. The red banner at the top says when it is unreachable. |
| Hashrate is zero while "Mining" | The tab is in the background and the browser has throttled it. Bring it to the front. |
| I closed the tab | Mining stopped. Reopen the app and press Start, or turn on "Start when the app opens". |
| I lost the 24 words | While the wallet is still in this browser, Settings, **Show recovery phrase**, shows them after your password: write them down now. Once the wallet is gone from every browser, the coins at that address cannot be spent by anyone. |

## Where to go next

- [Mining](docs/MINING.md): what you earn, when you can spend it, how long a
  block takes, and how invite links work.
- [Ways to mine](docs/WAYS-TO-MINE.md): a phone, a computer or a rented
  server, step by step, and what to expect from each.
- [Fees](docs/FEES.md): every fee, who receives it and how to check it.
- [Bridge](docs/BRIDGE.md): deposit USDC or USDT for uARG, and withdraw it.
- [FAQ](docs/FAQ.md): short answers to common questions.
