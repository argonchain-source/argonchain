# Running public seed nodes

New nodes find the network through seed nodes. The goal is at least three,
on different providers and continents, published two ways:

1. **DNS seeds**: an A/AAAA record set `seed.<domain>` that returns the IPs
   of all seed nodes. Nodes resolve the DNS seeds they know on start (add
   more with `-dnsseed`).
2. **Built-in seeds**: the same addresses in the list built into
   `argonmined`, as a fallback if DNS is blocked. The list changes with a
   release.

A seed is an ordinary node that is always on and reachable. Hardware,
ports, monitoring and troubleshooting are in [Run a node](../docs/NODE.md).

## Set up a seed (Linux)

1. Download the Linux archive for your processor and check it against
   `SHA256SUMS` ([Run a node, Download](../docs/NODE.md#download)), or
   build it from the [source](https://github.com/argonchain-source/argonchain).
2. Unzip it and install the node as a systemd service, with the unit file
   given in [Run a node](../docs/NODE.md#as-a-systemd-service-linux).
3. Open TCP 9333 (mainnet P2P) in the machine's and the provider's
   firewall.
4. If you run the explorer too, it listens on 8380; put Caddy or nginx
   with TLS in front of it.

**Behind a TCP proxy or load balancer** every inbound peer arrives from
the proxy's address, so run the node with `-behind-proxy`; otherwise one
misbehaving peer could get the proxy's address banned and three
connections would fill its per-address cap. See
[Connection limits](../docs/NODE.md#connection-limits). Platforms that
only expose HTTP cannot be P2P seeds.

## Hosting map (one domain)

| Host | Service | Notes |
|---|---|---|
| `domain.com` | the website | The front page. The app is the product for users; node operators download the release archives ([Run a node, Download](../docs/NODE.md#download)). |
| `docs.domain.com` | the same web server (requests for this host get the docs) | The rendered documentation at `/<page>`, and the release archives at `/downloads/`; `/docs/<page>` on the other hosts redirects here. |
| `app.domain.com` | the same web server (requests for this host get the app) | The browser app: creates a wallet in the browser, mines whole blocks with the WebAssembly miner using templates from the explorer, shows balance and history from the explorer, links to docs, explorer and bridge. |
| `explorer.domain.com` | `argonmine-explorer -listen 0.0.0.0:8380` | Behind a TLS reverse proxy; one instance per network. Also the work server for browser miners (`/api/work`): it builds block templates from its node and submits solved blocks, holding no funds. |
| `bridge.domain.com` | the bridge page and its public API | The goal is guardians on separate hosts and providers, under separate operators; today the project runs all three ([Bridge, Risks](../docs/BRIDGE.md#risks)). Followers expose `/guardian/*` only to the leader (firewall or mTLS). |
| `seed.domain.com` | DNS A/AAAA records | List every seed node's IP. |

Mainnet uses `argonmine.com` for the website and `app.`, `docs.`,
`explorer.` and `bridge.argonmine.com` for the rest. The public testnet
that used the same names is retired.

| Host | What runs there today |
|---|---|
| `argonmine.com` | The landing page. |
| `app.argonmine.com` | The browser app, with the swap service behind `/swap/api/`. |
| `docs.argonmine.com` | This documentation, and the release archives at `/downloads/`. |
| `explorer.argonmine.com` | The mainnet explorer and work server. `/api/health` is the uptime check. |
| `bridge.argonmine.com` | The mainnet bridge page and its public API (Ethereum, Base, Arbitrum One, BNB Chain). |
| `altaria.proxy.rlwy.net:12347` | The mainnet seed node. It is the built-in default seed in `argonmined`. |

There is one seed node today, and no `seed.argonmine.com` DNS seed yet.
Seeds on more providers, listed in DNS, are planned after launch.

Put Caddy or nginx in front of every HTTP service for TLS; none of them
terminates TLS itself.

## Checklist per seed

- `argonmined` reachable: `nc -vz <ip> 9333` from outside.
- Behind a TCP proxy or load balancer: `-behind-proxy` is set.
- Clock synced (chrony/ntpd). A node more than two hours off rejects valid blocks.
- Disk: the chain grows at most ~1 MB per 10 minutes (about 50 GB/year at full blocks).
- Monitor with the RPC `getpeerinfo` and `getblockchaininfo`; alert if peers drop to 0 or height stalls for 2 hours.
- For the public network as a whole, point an uptime monitor at `https://explorer.argonmine.com/api/health`: 200 while the explorer follows its node and blocks keep coming, 503 with the reasons when the node is unreachable, the index falls behind or no block has arrived for an hour.
- Keep the RPC bound to localhost.
