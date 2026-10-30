# Run a node

This guide is for operators: people who want to verify the chain on their
own machine, run a public seed, mine with several processor cores, or run
an explorer. You do not need a node to use Argonmine. The app at
[app.argonmine.com](https://app.argonmine.com) does everything a user
needs, including mining.

## What does a node do?

A node is the program `argonmined`. It:

- keeps a full copy of the chain on disk;
- checks every block and transaction against the
  [consensus rules](../docs/SPEC.md) before it accepts them;
- passes valid blocks and transactions on to other nodes;
- keeps a pool of transactions waiting for a block (the mempool);
- answers programs on the same machine through a JSON-RPC interface;
- optionally mines, paying the rewards to an address you choose.

A node holds no wallet keys and earns nothing by itself. There is no reward
for running one in the protocol; the project treasury may fund public seed
nodes later. What it gives you is independence: your node checks every
rule itself, so you do not have to trust anyone else's explorer. Every
public node also makes the network harder to split or censor.

## Before you start

**Mainnet is live.** It launched on 28 October 2026, 12:00 UTC. The node starts on
mainnet unless you say otherwise, and finds peers through the built-in
seed `altaria.proxy.rlwy.net:12347`:

```
argonmined
```

The commands in this guide run on mainnet. The public testnet is retired:
for experiments, use a private regtest chain
([Mining with a node](#mining-with-a-node)).

**Getting the software.** Download the release archive for your system
and check it, as described in [Download](#download), then follow
[Install](#install).

## Hardware requirements

The figures below follow from the consensus limits (1,000,000-byte blocks,
one block every 10 minutes, 8 MiB per Argon2id hash) and the node's own
limits (a 300 MB mempool, 8 outbound and up to 125 inbound peers).

| Resource | Private node | Public seed node | Why |
|---|---|---|---|
| CPU | 1 core, any 64-bit processor from the last ten years | 2 cores or more | Checking a block's proof of work costs one Argon2id hash: about 40 ms of one core on a recent desktop processor. A year of blocks (52,560) takes about 35 minutes of one core to check. Mining uses every thread you give it. |
| Memory | 1 GB free | 2 GB | Each proof-of-work check, and each mining thread, uses 8 MiB. The mempool may use up to 300 MB when full. Each peer has bounded send queues. |
| Disk | 10 GB free, SSD preferred | 100 GB, SSD | See the growth estimate below. The database writes to disk after every block, so an SSD helps. |
| Network | Any broadband connection | A public IP address and an open TCP port, always on | See bandwidth below. |
| Clock | Synchronised with NTP | Synchronised with NTP | A node rejects blocks more than 2 hours ahead of its own clock. There is no network time adjustment. |

**Disk growth.** Each block is stored with its undo data (to roll it back in
a reorganisation) and indexes.

| Situation | Growth |
|---|---|
| Today, with nearly empty blocks | Under 50 MB a year |
| Every block full (the most the rules allow) | About 53 GB a year of block data, about 80 GB a year with undo data and indexes |

The explorer, if you run one, keeps its own address index next to the
node's data and adds to this.

**Bandwidth.** While blocks are nearly empty, a node sends and receives
little: headers, pings and the occasional transaction. The first sync
downloads the whole chain once. With full blocks, expect about 300 MB a day
downloaded (each transaction arrives once on its own and once inside its
block), and uploads that grow with the number of peers: a seed with 50
active peers could upload several GB a day.

## Supported operating systems

Release archives are built for six targets:

| Operating system | Processors |
|---|---|
| Windows 10 and 11 | x86-64 (amd64), ARM64 |
| Linux | x86-64 (amd64), ARM64 |
| macOS | Intel (amd64), Apple silicon (arm64) |

Other platforms are not built or tested.

## Download

The current release is **1.1.0**. It adds the treasury rule from block 300
and is required for mining from that block (see [Upgrades](#upgrades)).
Download the archive for your system:

| System | Archive |
|---|---|
| Windows, x86-64 | [argonmine-1.1.0-windows-amd64.zip](https://docs.argonmine.com/downloads/argonmine-1.1.0-windows-amd64.zip) |
| Windows, ARM64 | [argonmine-1.1.0-windows-arm64.zip](https://docs.argonmine.com/downloads/argonmine-1.1.0-windows-arm64.zip) |
| Linux, x86-64 | [argonmine-1.1.0-linux-amd64.zip](https://docs.argonmine.com/downloads/argonmine-1.1.0-linux-amd64.zip) |
| Linux, ARM64 | [argonmine-1.1.0-linux-arm64.zip](https://docs.argonmine.com/downloads/argonmine-1.1.0-linux-arm64.zip) |
| macOS, Intel | [argonmine-1.1.0-darwin-amd64.zip](https://docs.argonmine.com/downloads/argonmine-1.1.0-darwin-amd64.zip) |
| macOS, Apple silicon | [argonmine-1.1.0-darwin-arm64.zip](https://docs.argonmine.com/downloads/argonmine-1.1.0-darwin-arm64.zip) |

Download the checksum file,
[SHA256SUMS](https://docs.argonmine.com/downloads/SHA256SUMS), into the
same folder.

**Check the archive before you unzip it.** In the folder that holds both
files:

```
sha256sum -c SHA256SUMS --ignore-missing          # Linux
shasum -a 256 -c SHA256SUMS --ignore-missing      # macOS
```

Each command must print `OK` for your archive. On Windows, in PowerShell:

```
Get-FileHash .\argonmine-1.1.0-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS
```

The hash of the first command must equal the line of `SHA256SUMS` for the
same file (case does not matter). If it does not, delete the archive and
download it again.

**The programs are not code-signed yet.** Your system may warn the first
time you run one:

- **Windows** may show "Windows protected your PC". Choose **More info**,
  then **Run anyway**.
- **macOS** may refuse to open a program from an unidentified developer.
  After checking the checksum, remove the quarantine flag from each
  program, for example `xattr -d com.apple.quarantine argonmined`, or
  allow it in **System Settings**, **Privacy & Security**.
- **Linux and macOS:** if a program does not start with "permission
  denied", make it executable with `chmod +x argonmined` (and the same for
  the other programs).

## Install

Each archive contains:

| File | What it is |
|---|---|
| `argonmined` | The node. |
| `argonmine-wallet` | The command-line wallet. |
| `argonmine-explorer` | The block explorer and work server. |
| `genesis` | Reproduces the genesis blocks. |
| `README.txt` | A short start guide. |
| `SECURITY.md` | How to report a security problem. |
| `docs/NODE.md`, `docs/SPEC.md` | This guide and the consensus specification. |
| `install.sh` (Linux, macOS) or `install-windows.ps1` (Windows) | Installs the programs for your user account. |

On Windows the programs end in `.exe`.

1. Unzip the archive you checked.
2. Install the programs for your user account (no administrator rights
   needed):
   - **Linux and macOS:** `sh install.sh` copies the four programs to
     `~/.local/bin`. Add that folder to your `PATH` if the script asks.
   - **Windows:** `powershell -ExecutionPolicy Bypass -File install-windows.ps1`
     copies the programs to `%LOCALAPPDATA%\Programs\Argonmine` and adds
     that folder to your user `PATH`.

   Run either script with `--uninstall` (Linux, macOS) or `-Uninstall`
   (Windows) to remove the programs. Your chain data and wallets are kept.
3. Open a new terminal and run `argonmined`.

### Build from source

The source is at [github.com/argonchain-source/argonchain](https://github.com/argonchain-source/argonchain).
With Go 1.26 or newer installed:

```
git clone https://github.com/argonchain-source/argonchain
cd argonchain
go build ./cmd/argonmined ./cmd/argonmine-wallet ./cmd/argonmine-explorer
go test ./...
```

The tests take a few minutes; they cover the chain rules, the networking,
the wallets, the bridge and the swap. `scripts/release.sh` builds the
release archives for every platform the same way the published ones are
built, so you can compare your build with the checksums in `SHA256SUMS`.
The browser miner (`cmd/argonmine-webminer`) is compiled to WebAssembly
by `deploy/railway/Dockerfile`; the website does not need it to run a node.

### As a systemd service (Linux)

To run the node as a service, give it a dedicated `argonmine` user with
its data in `/var/lib/argonmine`. The unit below restarts the node if it
stops and limits what it can write to. In the unzipped folder:

```
sudo useradd -r -m -d /var/lib/argonmine argonmine
sudo install -m 755 argonmined /usr/local/bin/argonmined
sudo tee /etc/systemd/system/argonmined.service > /dev/null <<'EOF'
[Unit]
Description=Argonmine full node
After=network-online.target
Wants=network-online.target

[Service]
User=argonmine
Group=argonmine
ExecStart=/usr/local/bin/argonmined -network mainnet -datadir /var/lib/argonmine -listen :9333
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
journalctl -u argonmined -f
```

The unit starts mainnet, with its data in `/var/lib/argonmine` and P2P on
port 9333. To add flags such as `-mine`, edit the `ExecStart` line in
`/etc/systemd/system/argonmined.service`, then run `sudo systemctl
daemon-reload` and `sudo systemctl restart argonmined`.

## Start the node

```
argonmined                                   # mainnet
argonmined -network regtest -listen none     # a private chain on this machine only
```

On first start the node:

1. creates its data directory and opens the database;
2. writes a random RPC token to `rpc.cookie` in that directory;
3. finds peers through the built-in seed list and any DNS seeds;
4. downloads and checks the headers first, then the blocks.

The log shows each step:

```
argonmined 1.1.0 starting on mainnet, datadir /home/you/.argonmine
chain: height 0 tip e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6
rpc: token written to /home/you/.argonmine/rpc.cookie
p2p: handshake with … (/argonmined:1.1.0/, height 120)
status: height 120 peers 8 mempool 0
```

A `status` line follows every 60 seconds with the height, the number of
peers and the mempool size (and, when mining, the hashrate and blocks
found). Stop the node with Ctrl+C or `SIGTERM`. It closes the database
cleanly and deletes `rpc.cookie`.

## Networks and ports

| | mainnet | regtest |
|---|---|---|
| Status | Public, live since 28 October 2026, 12:00 UTC | Private, on your machine |
| Address prefix | `arg1` | `rarg1` |
| P2P port | 9333 | 29333 |
| RPC port | 9332 | 29332 |
| Built-in seeds | `altaria.proxy.rlwy.net:12347` | none |
| Proof-of-work floor | 262,144 (2^18) attempts per block, `bits` `0x1E400000` | about 2, fixed |
| Block target | 10 minutes, ASERT difficulty | fixed difficulty |
| Genesis hash | `e378ed19…cac8f6` | `b3682483…97959d` |

Each network has its own message prefix, so nodes on different networks
cannot talk to each other. `getblockchaininfo` returns the genesis hash, so
you can confirm which chain you are on: on mainnet it must be
`e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6`.

The programs still know the retired public testnet (`-network testnet`,
prefix `targ1`, ports 19333 and 19332), but no public nodes run it any
more.

## Configuration

`argonmined` has no configuration file. Everything is a command-line flag.
Under systemd, put the flags in the unit file.

### Flags

| Flag | Default | What it does |
|---|---|---|
| `-network` | `mainnet` | `mainnet`, `testnet` or `regtest`. |
| `-datadir` | see [Data directory](#data-directory) | Where the node keeps its data. Testnet and regtest use a subfolder named after the network. |
| `-listen` | `:<P2P port>` | Address for incoming P2P connections, on every interface. `none` accepts no incoming connections; the node still connects out. |
| `-connect` | none | Comma-separated `host:port` peers to stay connected to. |
| `-seeds` | none | Comma-separated extra `host:port` seed peers, added to the built-in list. |
| `-dnsseed` | none | Comma-separated extra DNS seed host names. Their A and AAAA records are used with the network's P2P port. |
| `-rpc` | `127.0.0.1:<RPC port>` | Address of the RPC interface. Keep it on localhost. |
| `-rpctoken` | random | The RPC token. Without it, a new random token is written to `rpc.cookie` at every start. |
| `-mine` | off | Start the built-in miner, paying this address. |
| `-threads` | number of cores minus one | Mining threads. |
| `-tag` | `argonmine-miner` | Text placed in the reward transaction of blocks you mine, at most 100 bytes. |
| `-maxinbound` | 125 | Most incoming P2P connections. When full, the least useful incoming peer makes room for a newcomer. |
| `-behind-proxy` | off | Incoming connections arrive through a TCP proxy or load balancer (a hosting platform's TCP proxy, a cloud load balancer): never ban or cap incoming peers by address. See [Connection limits](#connection-limits). |
| `-ban-local-addrs` | off | Treat loopback, private and carrier-grade NAT source addresses as separate peers, so bans and the per-address cap apply. For private networks where every peer has its own address. |
| `-assetoutputfee` | 546 | Relay policy: extra fee, in units, for every asset output beyond the first two in a transaction. |

Fixed limits that have no flag: 8 outbound connections, at most 3 incoming
connections from one address, and a 300 MB mempool.

### Data directory

| Operating system | Default data directory |
|---|---|
| Windows | `%LOCALAPPDATA%\Argonmine` |
| Linux | `~/.argonmine` |
| macOS | `~/.argonmine` |
| systemd unit | `/var/lib/argonmine` |

**Mainnet uses the data directory itself.** Regtest, and the retired
testnet, use a subfolder:

| Network | Folder (Linux example) |
|---|---|
| mainnet | `~/.argonmine/` |
| regtest | `~/.argonmine/regtest/` |
| testnet (retired) | `~/.argonmine/testnet/` |

If you ran a testnet node, its data sits in `testnet/` and is not used on
mainnet. You can delete that folder; back up any `wallet.json` in it first
if you want to keep it.

Inside each network's folder:

| Path | What it holds | Back it up? |
|---|---|---|
| `chain/` | The database (LevelDB): blocks, undo data, the coin set, the block and transaction indexes. | No. It can be downloaded again. |
| `peers.json` | Addresses of known peers. | No. |
| `rpc.cookie` | The RPC token, readable only by the node's user. Exists while the node runs. | No. |
| `wallet.json` | The command-line wallet, if you use it, encrypted with your password. | Optional. The 24 words are the real backup. |
| `explorer-index/` | The explorer's address index, if you run the explorer with the same data directory. | No. It rebuilds itself. |

### The RPC token

Every RPC request must carry the token, either as a bearer token or as the
password of HTTP basic authentication (any user name):

```
TOKEN=$(cat ~/.argonmine/rpc.cookie)
curl -s -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":1,"method":"getblockchaininfo","params":[]}' \
  http://127.0.0.1:9332/
```

On Windows PowerShell:

```
$token = Get-Content "$env:LOCALAPPDATA\Argonmine\rpc.cookie"
Invoke-RestMethod -Uri http://127.0.0.1:9332/ -Method Post `
  -Headers @{Authorization = "Bearer $token"} `
  -Body '{"jsonrpc":"2.0","id":1,"method":"getblockchaininfo","params":[]}'
```

The random token changes at every start. The command-line wallet and the
explorer read the cookie by themselves when they use the same data
directory. If another program needs a fixed token, pass `-rpctoken`. A token
on the command line is visible to other users of the machine in the process
list, so on a shared machine prefer the cookie.

The token gives full control of the node, including broadcasting
transactions, but never access to any wallet's keys: the node has none.

## Firewall and NAT

| Port | Direction | Open it? |
|---|---|---|
| P2P (9333) | Incoming TCP | Only if you want incoming peers, for example on a seed. A node works without it. |
| Any port | Outgoing TCP | Yes. Peers may listen on other ports (the public seed uses 12347). |
| 53 | Outgoing UDP and TCP | Yes, for DNS seeds. |
| RPC (9332) | Incoming | **Never** to the internet. |
| Explorer (8380) | Incoming | Only behind a reverse proxy with TLS. |

**Behind a home router.** The node does not open ports on your router by
itself (no UPnP or NAT-PMP). Without a port forward it connects out to 8
peers and works normally. To accept incoming peers, forward TCP 9333 on
the router to the machine running the node, and allow it in the machine's
firewall.

**The node does not announce its own address.** Other nodes learn about a
node only when it is in their seed list or DNS seed, or when an operator
connects to it with `-connect` or `-seeds`. An open port alone brings few
incoming peers. That is why seed nodes are listed by hand.

**Remote RPC access.** Use an SSH tunnel
(`ssh -L 9332:127.0.0.1:9332 you@host`) rather than binding `-rpc` to a
public address. The RPC uses plain HTTP and has no TLS of its own.

## Run a seed node

A seed node is the first contact for new nodes. It needs to be reachable
and always on:

- a public IP address or a stable DNS name, with the P2P port open;
- at least 2 cores, 2 GB of memory and an SSD;
- a synchronised clock (chrony or ntpd);
- `-behind-proxy` if incoming connections pass through a TCP proxy or load
  balancer, because every peer then arrives from the proxy's address;
- monitoring (see [Monitoring](#monitoring)).

Seeds are published in two places: the list built into `argonmined` and a
DNS name whose records list every seed. Today there is one public seed,
`altaria.proxy.rlwy.net:12347`, and no DNS seed yet. Seeds on more hosting
providers, listed in `seed.argonmine.com`, are planned after launch, so a
seed you run helps. Check from another machine that yours accepts
connections:

```
nc -vz <your ip> 9333
```

The [Seed nodes](https://docs.argonmine.com/seed-nodes) page lists the
public services and a checklist for every seed.

## Mining with a node

The built-in miner builds blocks from the node's own chain and mempool and
pays the reward, plus fees, to the address you give it. From block 300 each
block also pays 1% of its subsidy to the project treasury, as consensus
requires; the fees stay with you. Mining needs release 1.1.0 or later from
block 300:

```
argonmined -mine arg1… -threads 4 -tag "my miner"
```

- **Which address?** Any address of the network. If you use the app, take
  the address from its **Receive** screen; rewards then show up in the app
  under your 24 words.
- **Threads.** The default is the number of cores minus one. Each thread
  uses about 8 MiB of memory. Expect roughly 25 attempts per second per core
  on a recent desktop processor.
- **Checking it works.** `getmininginfo` returns `mining`, `hashrate`,
  `threads` and `blocksfound`, and the 60-second `status` line shows the
  hashrate and blocks found.
- **Rewards** can be spent 100 blocks (about 17 hours at 10-minute blocks)
  after the block that paid them. See
  [Mining](https://docs.argonmine.com/mining) for what to expect, and
  [Mining at launch](https://docs.argonmine.com/mining#mining-at-launch)
  for the first days.
- **No referral share** applies to blocks your node mines.
- A miner with a clock more than 2 hours fast produces blocks that other
  nodes refuse. Keep NTP running.

For a quick private chain, use regtest. Blocks there take about two
attempts, and `generatetoaddress` mines on request:

```
argonmined -network regtest -listen none
curl -s -u x:$(cat ~/.argonmine/regtest/rpc.cookie) \
  -d '{"jsonrpc":"2.0","id":1,"method":"generatetoaddress","params":[101,"rarg1…"]}' \
  http://127.0.0.1:29332/
```

## Monitoring

Poll these RPC methods every minute or so:

| Check | Method and field | Healthy |
|---|---|---|
| The node answers | `uptime` | Seconds since start, increasing. |
| Height | `getblockchaininfo` → `blocks` | Grows about 6 blocks an hour, and matches other nodes or the explorer (`nodeHeight` in `https://explorer.argonmine.com/api/stats`). |
| Tip age | `getblockchaininfo` → `time` (the tip's timestamp) | Usually less than an hour old. Over 2 hours means a stall or a node that is behind. |
| Catching up | `getblockchaininfo` → `initialblockdownload` | `false` once caught up. It is `true` while the node downloads from a peer that is ahead. |
| Same chain | `getblockchaininfo` → `bestblockhash` | The same on all your nodes at the same height. |
| Peers | `getnetworkinfo` → `connections`, and `getpeerinfo` | At least 1. A node with an open port usually has 8 outgoing plus incoming peers. |
| Mempool | `getmempoolinfo` → `size`, `bytes` | Well under the 300 MB cap. |
| Mining | `getmininginfo` → `mining`, `hashrate` | `true` and above 0 when you mine. |

Also watch, outside the node: free disk space, the clock offset reported by
NTP, and memory use.

Alert when:

- peers stay at 0 for more than 10 minutes;
- the height has not changed for 2 hours (while the network is small,
  this can also mean nobody is mining);
- your nodes report different block hashes at the same height for more
  than a few blocks;
- free disk space falls below 10%;
- the clock is more than a few seconds off.

A minimal check from a shell:

```
TOKEN=$(cat ~/.argonmine/rpc.cookie)
for m in getblockchaininfo getnetworkinfo; do
  curl -s -H "Authorization: Bearer $TOKEN" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$m\",\"params\":[]}" \
    http://127.0.0.1:9332/
done
```

**The public network.** `https://explorer.argonmine.com/api/health`
answers 200 with `"status": "ok"` while the public explorer follows its
node and blocks keep coming. It answers 503 with `"status": "down"` and a
list of `problems` when the explorer cannot reach its node, its index is
more than 3 blocks behind, or no block has arrived for an hour. It also
returns `nodeHeight`, `indexedHeight` and `lastBlockAge` (seconds). Point
a free uptime monitor at it. An explorer you run yourself answers the same
path.

## Backups

The node itself holds no keys, so very little needs a backup.

| What | Back it up? |
|---|---|
| The 24 words of every wallet you mine or receive to | **Yes, on paper, kept safe.** They are the wallet. |
| `wallet.json` of the command-line wallet | Optional. It is encrypted, and the 24 words restore it. |
| Your flags or unit file, and a fixed `-rpctoken` if you use one | Yes. |
| `chain/` | No. A new node downloads it again. To keep a copy that speeds up a rebuild, stop the node first, then copy the folder. Never copy the database while the node runs. |
| `peers.json`, `rpc.cookie`, `explorer-index/` | No. |

Bridge guardians and the swap service keep their own state and keys. See
[Bridge: State](https://docs.argonmine.com/bridge#state) and
[Swaps](https://docs.argonmine.com/swaps).

## Upgrades

**Release 1.1.0 and block 300.** Release 1.1.0 adds the treasury rule:
from mainnet block 300, every block must pay 1% of the block subsidy to
the project treasury
(`arg1q0tg6xr2g9glakgl06t0ymf5aut5ac5xjqnvvmqjf2xp7dvllmakqna8cd2`). A
node that mines must run 1.1.0 before block 300, or the blocks it finds
will be rejected. A node that does not mine keeps following the chain on
1.0.0, because the rule only turns away blocks that leave the treasury
out; upgrade it anyway, with the steps below.

1. Read the release notes. Consensus changes are announced in advance with
   an activation height. Depending on the change, a node that is not
   upgraded in time stops following the chain at that height, or, as with
   the treasury rule above, has the blocks it mines rejected.
2. Download the new archive and check it against `SHA256SUMS`
   ([Download](#download)).
3. Stop the node (Ctrl+C or `sudo systemctl stop argonmined`). Wait for
   the `bye` line in the log. Optionally copy the `chain/` folder.
4. Replace the programs with the new ones: run the new archive's install
   script, or under systemd copy the new `argonmined` to
   `/usr/local/bin/argonmined` again.
5. Start the node and watch the log. A version that changes the database
   upgrades it in place on first start; for example, the transaction index
   is built once when it is missing. The explorer rebuilds its index by
   itself when the index format changes, which can take a while.
6. Check `getblockchaininfo` and `getnetworkinfo` (`version`).

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| No peers | Outgoing connections blocked, or the seed is down. | Allow outgoing TCP. Look for `p2p: connect …` errors in the log. Add a known node with `-connect host:port`. |
| A node that ran on the testnet finds no peers | It still runs with `-network testnet`, and the testnet is retired. | Remove `-network testnet` from its command or unit file. Mainnet uses the data directory itself, not `testnet/`. |
| Peers connect and drop at once (`p2p: disconnected …`, or `… is on a different chain, disconnecting`) | The other node is on a different network, or on an old copy of a chain whose genesis block changed. | Use the same `-network` on both sides, and the same release. |
| Height does not move, `initialblockdownload` stays `true` | The sync peer is slow or not useful, or the clock is wrong. | Check the clock first. Check `getpeerinfo` for peers with a higher `startheight`. Restart, or add a known good node with `-connect`. |
| Log shows `timestamp too far in the future` | Your clock is more than 2 hours behind. | Turn on NTP and restart. |
| Blocks you mine are refused by other nodes | Your clock is more than 2 hours ahead, or you mine to an address of another network. | Fix the clock. Use an address with the network's prefix. |
| `-mine address:` error at start | The address belongs to another network or is mistyped. | Use an `arg1…` address on mainnet (`rarg1…` on regtest). A `targ1…` address is from the retired testnet. |
| RPC answers `401 unauthorized` | Wrong token. The random token changes at every start. | Read `rpc.cookie` again, or set `-rpctoken`. |
| RPC connection refused | The node is not running, or you used another network's port. | Check the ports table. Mainnet RPC is 9332. |
| `address already in use` at start | Another node or program uses the port. | Stop it, or give this node other ports with `-listen` and `-rpc`. Two nodes on one machine also need different `-datadir` folders. |
| Disk full | The database cannot write. The node logs `chain: storage` errors and stops accepting blocks, or exits. It never marks a block invalid because of a disk error. | Free space and restart. If the database is damaged, stop the node, delete `chain/` and let it download the chain again. |
| High memory use | A full mempool (up to 300 MB) or many peers. | Lower `-maxinbound`. |
| Explorer shows `Node unavailable` | The explorer cannot reach the node's RPC. | Check the explorer's `-rpc` and `-token`, and that the node runs. |

## Connection limits

- **Inbound slots.** At most `-maxinbound` incoming connections, accepted at
  most 10 per second (bursts of 20). When every slot is taken a newcomer is
  not refused: an incoming peer is evicted instead. A peer that never sent a
  new block or transaction goes first, then the one least recently useful,
  the longest connected among equals. The four peers with the best ping, the
  four that most recently delivered a new block, and the peer the node is
  syncing from are never evicted.
- **Per address.** At most 3 incoming connections from one address, and a
  peer that misbehaves has its address banned for 24 hours, but only when
  the address identifies the peer. Behind a TCP proxy every incoming peer
  arrives from the proxy's address, so one bad message would lock everyone
  out: start such a node with `-behind-proxy`, and misbehaviour then only
  disconnects the one connection. Loopback, private (10/8, 172.16/12,
  192.168/16, fc00::/7), link-local and carrier-grade NAT (100.64/10)
  sources are treated the same way by default; `-ban-local-addrs` turns
  bans and the cap back on for them.
- **Sync peers.** The node prefers to download from peers it dialled. A peer
  that claims a greater height must answer the first `getheaders` within 15
  seconds and then deliver headers the node lacks; otherwise it is dropped
  and, when its address identifies it, not chosen again for an hour.
  Transaction announcements are skipped only while the tip is more than two
  hours old and the sync peer is really delivering new headers, so a false
  height claim cannot switch transaction relay off.

## Transaction relay limits

- **Fetching.** At most 500 transactions per `getdata` and 1,000
  outstanding per peer. A transaction is asked of one peer at a time; if it
  does not arrive within 60 seconds, or arrives with a bad witness, the next
  peer that announced it is asked.
- **Serving.** Answers to `getdata` come from a separate worker per peer, in
  order, at most 4 MB ahead of the connection, so a peer asking for a lot
  slows down only its own answers and two busy nodes cannot stall each
  other. Blocks are always served. A transaction is served to a peer only
  after it was announced to that peer, and once per announcement.
- **Pending transactions.** On connect, and when a caught-up peer sends
  `mempool`, the node announces up to 10,000 pending transactions, parents
  before children and the best fee rate first, in `inv` messages of at most
  1,000. A `mempool` request is answered once per 2 minutes per address (per
  connection when the address is shared, and then at most ten at once and
  one every 10 seconds after that across all such connections).
  Transactions fetched from these announcements count against 64 MB per
  address per 10 minutes (256 MB shared by all connections from shared
  addresses).
- **Orphans.** A transaction that arrives before its parent waits in an
  orphan pool (100 transactions, 5 MB, 5 minutes, random eviction) and
  enters the mempool when the parent arrives or a new block confirms its
  inputs.

The mempool's own rules (fees, dust, asset outputs, chains of unconfirmed
transactions) are in
[Specification §10](../docs/SPEC.md#10-policy-non-consensus-node-defaults).

## The RPC

JSON-RPC 2.0 over HTTP `POST`, one request per call, at most 4 MB per
request. Every request carries the token (see [The RPC token](#the-rpc-token)).

| Method | Returns |
|---|---|
| `help` | The method list |
| `uptime` | Seconds since start |
| `getblockchaininfo` | `chain`, `blocks` (height), `bestblockhash`, `difficulty`, `nextbits`, `time` and `mediantime` of the tip, `chainwork`, `initialblockdownload`, `genesis`, and `subsidy` (units) of the next block |
| `getblockcount` | Height of the tip |
| `getbestblockhash` | Hash of the tip |
| `getdifficulty` | Difficulty of the next block, relative to the proof-of-work floor |
| `getblockhash <height>` | Block hash at a height on the main chain |
| `getblockheader <hash>` | Decoded header with confirmations and work |
| `getblock <hash> [verbosity]` | The block as hex (0), decoded (1, the default), or decoded with transactions (2) |
| `getrawtransaction <txid> [block hash]` | A transaction from the mempool or the chain, decoded and as hex, with its block, height and confirmations. With a block hash, it looks in that block |
| `decoderawtransaction <hex>` | Decode a transaction without broadcasting it |
| `gettxout <txid> <index>` | An unspent output with its confirmations, or null |
| `getrawmempool` | Transaction ids waiting for a block |
| `getmempoolinfo` | `size` (transactions) and `bytes` in the mempool |
| `sendrawtransaction <hex>` | Check, add to the mempool and relay; returns the txid |
| `getblocktemplate` | Everything a miner needs for the next block |
| `submitblock <hex>` | Check and connect a mined block |
| `getmininginfo` | `blocks`, `difficulty`, `pooledtx`, `chain`, `mining`, `hashrate`, `threads`, `blocksfound`, `powalgorithm` |
| `getpeerinfo` | Connected peers: address, direction, user agent, start height, ban score, ping, bytes in and out |
| `getnetworkinfo` | `version`, `protocolversion`, `network`, `connections`, `listening` |
| `validateaddress <address>` | Whether an address is valid on this network |
| `generatetoaddress <n> <address>` | Mine n blocks at once (regtest only) |

Decoded transactions include each output's type, address and amount, each
input's previous output, and for version-2 transactions the asset id of
every non-ARG output and any issuance.

## The command-line wallet

`argonmine-wallet` is a wallet for operators. It talks to your node over
the RPC. Its keys come from a 24-word phrase and are stored encrypted under
a password in `wallet.json` in the network's folder. Flags go before the
command.

```
argonmine-wallet create                 # prints the 24 words once
argonmine-wallet restore "<24 words>"
argonmine-wallet address                # a new receive address
argonmine-wallet balance                # spendable, maturing, pending
argonmine-wallet send arg1… 1.25        # amount in ARG
argonmine-wallet assets                 # uARG, wARG and other assets held
argonmine-wallet -bridge-url https://bridge.argonmine.com \
  -memo K7M2-X9RT send-asset UARG arg1qswap… 10    # pay a bridge withdrawal
argonmine-wallet history
argonmine-wallet utxos
argonmine-wallet mnemonic               # prints the 24 words; needs the password
```

The wallet runs on mainnet unless you pass `-network`.

| Flag | What it does |
|---|---|
| `-network`, `-datadir` | As for the node. |
| `-wallet <file>` | Another wallet file. |
| `-rpc <url>`, `-token` | The node's RPC, if it is not the local default. |
| `-password` | The wallet password. Better: the `ARGONMINE_WALLET_PASSWORD` variable, or type it when asked. |
| `-passphrase` | An optional BIP-39 passphrase, at `create` or `restore` only. |
| `-bridge-url`, `-uarg-asset` | How the wallet learns which asset is the real uARG. An asset is never chosen by its symbol alone. |
| `-memo` | A memo for `send-asset`, such as the one the bridge's Withdraw page gives. |

The 24 words from the app restore here with the same first address, so the
same coins can be spent from either. Run `argonmine-wallet` with no
arguments for the full command list.

## The explorer and work server

```
argonmine-explorer -listen 127.0.0.1:8380
```

The explorer follows its node over the RPC, keeps an address index next to
the node's data, and serves pages for blocks, transactions, addresses,
assets and the mempool, with charts. It is also the work server for browser
miners ([Mining](https://docs.argonmine.com/mining#the-work-protocol)). It
holds no coins.

| Flag | Default | What it does |
|---|---|---|
| `-network` | `mainnet` | Which network. |
| `-datadir` | as for the node | The node's data directory, for `rpc.cookie`. The index is kept in `explorer-index/` there. |
| `-rpc` | `http://127.0.0.1:<RPC port>` | The node's RPC. |
| `-token` | from `rpc.cookie` | The RPC token. |
| `-listen` | `127.0.0.1:8380` | HTTP address. Put a reverse proxy with TLS in front of it. |
| `-referral-share` | `5` | Percent of a block a browser miner gives to the inviter they accepted (at most 50; `0` turns referrals off). |
| `-allow-origin` | local addresses | Comma-separated web app origins whose pages may submit mining solutions; `*` allows any. |
| `-client-ip-header` | none | The header that names the client's address behind a proxy, such as `X-Forwarded-For`, for the per-client limits. |
| `-bridge-url`, `-uarg-asset` | none | Which asset is the bridge's real uARG. Look-alikes are marked unverified. |
| `-swap-url` | none | The swap service whose `/api/info` names the real wARG issuer. |

### JSON API

| Endpoint | Returns |
|---|---|
| `GET /api/stats` | Network, indexed and node height, hashrate, supply, blocks submitted by browser miners |
| `GET /api/health` | For uptime monitors: 200 and `"status": "ok"` while the explorer follows its node and blocks keep coming; 503, `"status": "down"` and `problems` when the node is unreachable, the index is more than 3 blocks behind, or no block has arrived for an hour. With `nodeHeight`, `indexedHeight` and `lastBlockAge` |
| `GET /api/charts?n=288` | Hashrate, difficulty, block interval and supply over the last n blocks (24 to 2,016) |
| `GET /api/address/{address}` | Balance, spendable, maturing, totals, referral totals, assets and the recent history with coinbase and referral flags |
| `GET /api/utxos/{address}` | The address's unspent coins (at most 1,000) with height, asset and coinbase flag; `spentBy` for a coin a waiting transaction spends; `pending`, its unconfirmed outputs (`own` when every input was its own coin, its change); `mempool`, the waiting transactions that touch it. What the browser wallet spends from |
| `GET /api/asset/{id}` | One asset by its id: symbol, issuer, issued, burned and supply, whether it is a look-alike (`unverified`), and its newest transactions |
| `GET /api/assets` | Every asset the index knows, with the same fields but without the transactions |
| `POST /api/tx` | Body `{"hex": signed transaction}`: checked and handed to the node; returns `{"txid"}`. A transaction spending a coin that a waiting one already spends is refused (409), so a payment never quietly replaces another. Any origin may call it; one a second per client, bursts of 10 |
| `GET /api/work?address=…[&ref=…]` | A block template for a browser miner |
| `POST /api/work/submit` | Submit a solved block |
| `GET /qr.svg?d=…` | A QR code as SVG |

Every API response except `/api/work/submit` allows requests from any
origin, so the app can run on a different host. Every route has a
per-client rate limit.

## Security checklist

- Check every archive against `SHA256SUMS` before you run it.
- Keep the RPC on `127.0.0.1`. Use an SSH tunnel for remote access.
- Run the node as its own unprivileged user (the systemd unit above does).
- Keep the clock synchronised.
- Behind a TCP proxy or load balancer, set `-behind-proxy`.
- Put the explorer behind a reverse proxy with TLS. It does not handle TLS
  itself.
- Keep the 24 words of any wallet offline. The node never needs them.
- Report security problems as described in the
  [Security policy](../SECURITY.md).
