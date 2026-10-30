Argonmine node

This archive is for people who run a node. To mine, send or swap, use the
app at https://app.argonmine.com instead: nothing to install.

Programs
  argonmined          the node
  argonmine-wallet    command-line wallet that talks to your node
  argonmine-explorer  block explorer for your node
  genesis             reproduces the genesis blocks

Install
  Linux, macOS:  sh install.sh          (copies the programs to ~/.local/bin)
  Windows:       powershell -ExecutionPolicy Bypass -File install-windows.ps1
                 (copies them to %LOCALAPPDATA%\Programs\Argonmine)

Start
  argonmined

The node joins mainnet and finds peers through its built-in seed. On
mainnet, getblockchaininfo must report the genesis hash
e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6.

Version 1.1.0. The programs are not code-signed yet. Check the archive
against https://docs.argonmine.com/downloads/SHA256SUMS before you run it.
Windows may say "Windows protected your PC" (choose More info, then Run
anyway); macOS may refuse to open the programs until you remove the
quarantine flag (xattr -d com.apple.quarantine <file>). Details are under
"Download" in docs/NODE.md.

Full guide: docs/NODE.md, or https://docs.argonmine.com/node
Source code: https://github.com/argonchain-source/argonchain
Consensus rules: docs/SPEC.md
Reporting a security problem: SECURITY.md
