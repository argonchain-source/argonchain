## Run a seed on a rented VPS

Most public seeds will run on a rented virtual server rather than on a
machine at home: a seed needs a public address that rarely changes and a
machine that is never switched off. A few things differ from running a
node on your own hardware.

**Check the provider's terms before you mine.** Many VPS providers
prohibit proof-of-work mining in their terms, even on plans where running
a plain node is allowed, because mining pins shared processor cores at
full load for weeks. A seed does not need to mine: run it without
`-mine`, as in the systemd unit above, and it stays within the terms of
most providers. If you want the same machine to mine, check the terms
first and expect to need a dedicated server rather than a shared VPS.

**Harden the box in the first hour.** A rented server is reachable from
the whole internet from the moment it is created, and password guessing
starts within minutes.

- Log in with an SSH key, then turn password logins off
  (`PasswordAuthentication no` in `/etc/ssh/sshd_config`, or your
  provider's key-only reinstall option).
- Set the firewall to deny incoming connections by default, and allow
  only SSH and the P2P port (9333). The RPC port stays on localhost, as
  described in [Firewall and NAT](#firewall-and-nat).
- Turn on automatic security updates for the operating system, so the
  box stays patched while it runs unattended.

**Use the release archive, not a build from source.** A public seed
should report the current release version in `getnetworkinfo`, because
other operators use that version to judge what the network is running. A
build from source stamps the version written in the source tree, which
can lag the latest release archive. Follow [Download](#download) and
check the archive against `SHA256SUMS`.

**Keep the previous binary when you upgrade.** A seed is expected to be
up, so make an upgrade reversible: copy the running `argonmined` aside
before you replace it, and if the new version does not start or does not
catch up with the explorer (see [Monitoring](#monitoring)), put the old
binary back and restart while you work out what went wrong.

**Ask to be listed once the seed is proven.** An open port alone brings
few incoming peers (see [Firewall and NAT](#firewall-and-nat)): a seed
only starts helping new nodes once it is in the built-in list and the
DNS seed. When yours has run synced and reachable for a day or so, open
an issue on the project repository with:

- the address and port, and whether the address is static;
- the version shown by `getnetworkinfo`;
- the cores, memory, and disk, and the region the server is in;
- that it runs without mining, if it does.

Expect the listing itself to land in a later release, because the
built-in list ships with the program.
