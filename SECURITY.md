# Security policy

## Reporting a vulnerability

Please do not post anything that could affect consensus, funds or node
availability publicly. Email **security@argonmine.com** with:

- what the bug is and which part it affects (the consensus rules, chain state, peer-to-peer networking, the mempool, a wallet, the RPC, the explorer, the bridge and its contracts, the swap service, the app);
- how to reproduce it (a block or transaction hex, a request, or steps);
- your assessment of impact.

We aim to acknowledge within 48 hours and to give a fix timeline within 7
days. We credit reporters in the release notes unless they prefer otherwise.
The project has no funds for a paid bounty programme yet; that will change
only when it does, and will be announced here first.

## How the code is reviewed

The source is public at [github.com/argonchain-source/argonchain](https://github.com/argonchain-source/argonchain),
with its tests, so a report can point at the line. There is no external audit. Instead, every part that holds or moves money
(the chain rules, the bridge and its contracts, the wallets, the swap
service) goes through independent in-house review passes, and a finding
counts only once a test proves it; the fix keeps that test as a regression
test. Before the mainnet launch, four in-house reviews were done on
2026-10-28, each followed by a second pass over its fixes. Findings and
their fixes are listed in the
[Security review](https://docs.argonmine.com/security-review), section
"Round of 2026-10-28".

Be clear about what that means. The bridge runs on mainnet and holds real
USDC and USDT without an external audit, and without caps on issuance per
deposit or per day. Withdrawals wait for more Argonmine blocks the larger
they are, and one above 100,000 uARG waits for the guardians' approval;
that protects against reorganisations, not against a bug. A bug the reviews
missed could cost what the vaults hold. Bridge only what you can afford to
have at risk. An external audit is planned once there are funds for it.

## Scope

In scope: anything in this project that can create coins, steal or freeze
coins, split the chain, crash or stall nodes remotely, or leak wallet secrets.

Out of scope: denial of service that requires more hashrate than the honest
network, issues in third-party dependencies already disclosed upstream,
social engineering.

## Supported versions

Only the latest release. Consensus changes are scheduled at an activation
height and announced in advance. Depending on the change, nodes on old
versions stop following the chain at that height, or have the blocks they
mine rejected. The treasury rule from mainnet block 300 (release 1.1.0) is
of the second kind: nodes on 1.0.0 keep following the chain, but a node
that mines must run 1.1.0 before block 300.

## Disclosure

We coordinate disclosure: fix first, then publish the advisory with the
reporter's credit. For consensus bugs that require a network-wide upgrade we
may delay details until most nodes have updated.
