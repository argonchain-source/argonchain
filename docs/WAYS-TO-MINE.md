# Ways to mine

You can mine ARG on a phone, on a computer, or on a rented server. All
three pay the same wallet, and you can combine them. This guide takes each
one step by step and says what to expect from it. For what mining pays,
when you can spend it and how long a block takes, see
[Mining](docs/MINING.md).

## Which one is for you?

| | Phone or tablet | Computer | Rented server (VPS) |
|---|---|---|---|
| How | The browser app | The browser app, or the node | The node, as a service |
| Attempts per second | The fewest | More | The most for the money, and around the clock |
| Runs while | The app is open on the screen | The tab or the program is open | Always |
| Costs | Battery and some warmth | Electricity | A monthly fee |
| Setup | Two minutes | Two minutes for the app, ten for the node | Fifteen minutes, in a terminal |
| Suits | Taking part, and following your wallet | Most people | Steady mining without keeping a computer on |

What all three have in common:

- **One wallet.** Create it in the app and keep the 24 words safe. Every
  device and server mines to its address, and the app shows what each one
  finds.
- **The same work.** A phone, a laptop and a server all hash real block
  headers with Argon2id; a block from any of them pays the full reward.
- **The same lock.** A reward can be spent 100 blocks after the block that
  paid it, about 17 hours at 10-minute blocks. Until then the app shows it
  as **Maturing**.
- **Your chance is your share.** The Mine page shows your hashrate, the
  network's, and the **Expected time to a block** from live figures.
  Finding a block is random: two blocks in a day and then nothing for a
  week is normal.

## On a phone or tablet

The app runs in the phone's browser. Nothing is installed from an app
store, and the 24 words never leave the phone.

1. Open [app.argonmine.com](https://app.argonmine.com) in Chrome, Safari,
   Samsung Internet or Firefox.
2. **Create a new wallet** and write down the 24 words, or **Log in with
   your 24 words** if you already have a wallet.
3. Open the **Mine** page and press **Start mining**.

Then make it comfortable to leave running:

- **Put it on the home screen.** Under **More**, choose **Install the app**
  (on an iPhone or iPad: Safari's Share button, then **Add to Home
  Screen**). The app then opens full-screen like any other app, and keeps
  your wallet.
- **Keep the screen on.** In **Settings**, turn on **Keep the screen on**.
  Phones slow down or stop a browser in the background, so mining only
  counts while the app is in front. Dim the screen to save battery.
- **Start when the app opens.** Also in Settings. Mining then resumes
  whenever you open the app, without pressing Start.
- **Processor cores.** The app uses 4 by default. If the phone gets
  uncomfortably warm, lower it under Settings, **Mining**.
- **Plug it in.** Mining uses as much power as a game. Leave the phone on
  a charger, out of direct sunlight and not under a pillow or in a bag.

What to expect: a phone makes the fewest attempts of the three, and fewer
still when it is warm. On a busy network it can wait weeks for a block.
Treat phone mining as taking part and as a way to watch your wallet, and
let a computer or a server do the heavier work.

## On a computer

A laptop or desktop has two ways to mine. The app is the quickest; the
node makes more attempts per core and does not depend on the explorer.

### With the browser app

1. Open [app.argonmine.com](https://app.argonmine.com), create a wallet
   or log in, and press **Start mining** on the Mine page.
2. Under **Settings**, **Mining**, choose the **Processor cores**. Half of
   them is the default, which leaves the computer responsive. Use more
   when the computer has nothing else to do.
3. Turn on **Start when the app opens**, and keep the tab open. Closing
   the tab stops mining; a reload pauses it for a moment, then it resumes.

Keep a laptop plugged in and set the computer not to sleep while mining:
on Windows under **Settings, System, Power**, on macOS under **System
Settings, Battery** or **Energy**. The lid can stay open; the screen may
turn off.

The browser runs the miner as WebAssembly, which makes fewer attempts per
core than the node's native code. Install the app from the browser's
address bar (Chrome and Edge offer **Install**) to run it in its own
window.

### With the node

The node is a single program with a built-in miner. It keeps its own copy
of the chain, builds its own blocks, and keeps mining if the explorer is
ever down. It is the right choice for a computer that mines most of the
day.

1. **Download release 1.1.0** for your system and check its checksum,
   exactly as [Run a node, Download](docs/NODE.md#download) describes,
   or [build it from source](docs/NODE.md#build-from-source).
   Releases before 1.1.0 cannot mine from block 300.
2. **Install it** with the script in the archive
   ([Install](docs/NODE.md#install)): `sh install.sh` on Linux and macOS,
   `install-windows.ps1` on Windows.
3. **Take your address** from the app: Wallet, **Receive**. Rewards then
   show up in the app under your 24 words.
4. **Start mining** in a terminal:

   ```
   argonmined -mine arg1… -threads 4 -tag "my laptop"
   ```

   `-threads` is how many cores mine; the default is all but one. Each
   thread needs about 8 MiB of memory. `-tag` is optional: a short label
   of your choice written into the blocks you find.

5. **Watch the log.** Every 60 seconds a `status:` line shows the
   height, the hashrate and the blocks found. On first start the node
   downloads the chain first, which takes a few minutes.

Stop it with Ctrl+C. To mine again, run the same command. When you
upgrade, stop the node, replace the programs, and start it again; see
[Upgrades](docs/NODE.md#upgrades).

Rewards from a node pay no referral share, and the node needs a correct
clock: a clock more than two hours fast produces blocks the network
refuses.

## On a rented server (VPS)

A virtual server at a hosting provider can run the node and its miner
around the clock. It needs no graphics card and no special hardware:
mining is limited by memory speed, which every server has.

Before you rent one:

- **Read the provider's terms.** Many providers forbid or limit
  cryptocurrency mining on their servers. Find the clause before you
  start, not after.
- **Count the cost.** A server costs a fee every month, and ARG has no
  market price. Rent what you are happy to spend on taking part, and
  check the Mine page's expected time to a block before you add cores.
- **Pick cores and memory, not disk.** The chain is small. A plan with 2
  to 4 cores, 2 GB of memory and 20 GB of disk is plenty, on x86-64 or
  ARM64. Cores that are shared with other customers make fewer attempts
  than dedicated ones; a plan with dedicated cores is worth more per
  core.
- **Choose Linux.** The steps below are for Ubuntu 22.04 or 24.04 and
  Debian 12, and assume you can log in with SSH.

### Set it up

1. **Download and check the release.** On the server:

   ```
   sudo apt update && sudo apt install -y unzip curl
   curl -LO https://docs.argonmine.com/downloads/argonmine-1.1.0-linux-amd64.zip
   curl -LO https://docs.argonmine.com/downloads/SHA256SUMS
   sha256sum -c SHA256SUMS --ignore-missing
   ```

   The last command must print `OK`. If you would rather compile it
   yourself, see [Build from source](docs/NODE.md#build-from-source). On an ARM server use
   `argonmine-1.1.0-linux-arm64.zip` instead.

2. **Install the node** under its own user, with its data in
   `/var/lib/argonmine`:

   ```
   unzip argonmine-1.1.0-linux-amd64.zip
   cd argonmine-1.1.0-linux-amd64
   sudo useradd -r -m -d /var/lib/argonmine argonmine
   sudo install -m 755 argonmined /usr/local/bin/argonmined
   ```

3. **Create the service.** Put your address after `-mine`, the number of
   threads after `-threads`, and a name after `-tag`:

   ```
   sudo tee /etc/systemd/system/argonmined.service > /dev/null <<'EOF'
   [Unit]
   Description=Argonmine node and miner
   After=network-online.target
   Wants=network-online.target

   [Service]
   User=argonmine
   Group=argonmine
   ExecStart=/usr/local/bin/argonmined -network mainnet -datadir /var/lib/argonmine -listen :9333 -mine arg1… -threads 4 -tag "my server"
   Restart=always
   RestartSec=5
   NoNewPrivileges=true
   ProtectSystem=strict
   ProtectHome=true
   ReadWritePaths=/var/lib/argonmine
   PrivateTmp=true
   LimitNOFILE=65536

   [Install]
   WantedBy=multi-user.target
   EOF
   sudo systemctl daemon-reload
   sudo systemctl enable --now argonmined
   ```

   The service starts with the server and restarts if it ever stops. Use
   as many threads as the plan has cores; leave one free if the server
   does anything else.

4. **Watch it work:**

   ```
   journalctl -u argonmined -f
   ```

   The node downloads the chain, then a `status:` line every 60 seconds
   shows the height, the number of peers, the hashrate and the blocks
   found. Press Ctrl+C to stop watching; the service keeps running.

5. **Open the peer port** if the server has a firewall, so that other
   nodes can connect to yours. The RPC port stays closed; the node only
   answers it on the server itself.

   ```
   sudo ufw allow 9333/tcp
   ```

6. **Keep the clock right.** `timedatectl` must say
   `System clock synchronized: yes`. Hosting providers set this up; if
   yours did not, `sudo timedatectl set-ntp true`.

### Keep it running

- **Change the threads or the address** by editing the `ExecStart` line
  in `/etc/systemd/system/argonmined.service`, then
  `sudo systemctl daemon-reload && sudo systemctl restart argonmined`.
- **Upgrade** by stopping the service, installing the new `argonmined`
  with the same `install` command, and starting it again. Release notes
  say when an upgrade is required ([Upgrades](docs/NODE.md#upgrades)).
- **Several servers** can mine to the same address; each one fetches
  nothing from the others and every block pays the same wallet.
- **Back up nothing on the server.** It holds no keys: the address only
  receives. Your 24 words stay with you.

How many attempts to expect: measured on cloud servers with shared cores,
about 700 attempts per second with 4 threads and about 1,000 with 8. Per
thread, that is a wide range, from about 20 to 200 attempts per second
depending on the processor and above all on how fast its memory is.
Adding threads beyond what the memory can feed adds little, so compare the
`status:` hashrate at 2, 4 and all cores before settling on a number.

## Checking that it pays you

- **The Mine page** shows the hashrate of this device, the blocks it has
  found and the network figures.
- **The Dashboard** adds up everything your address has mined, from every
  device and server.
- **The explorer** shows every block, who it paid, and your address's
  full history: [explorer.argonmine.com](https://explorer.argonmine.com).
- **The miners map** on the website counts browser miners by country.
  Nodes mine without asking the explorer for work, so they count from the
  blocks they find, without a country.

If a block does not appear within a few times the expected time, check
the hashrate first: a phone in the background, a laptop that went to
sleep, or a server whose service stopped all show as zero. See
[Troubleshooting](docs/GETTING-STARTED.md#troubleshooting) for the app
and [Run a node](docs/NODE.md#troubleshooting) for the node.
