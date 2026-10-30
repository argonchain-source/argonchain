// Command argonmine-wallet is the command-line wallet. It talks to a running
// argonmined over RPC.
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"argonmine/bridge"
	"argonmine/consensus"
	"argonmine/ui"
	"argonmine/wallet"
)

const usage = `argonmine-wallet [flags] <command> [args]

Commands:
  create                      create a new wallet and print its 24-word backup phrase
  restore "<24 words>"        create a wallet file from a backup phrase
  address                     hand out a new receive address
  addresses                   list receive addresses handed out so far
  sync                        scan the node for new blocks
  balance                     show spendable / immature / pending balances (syncs first)
  send <address> <amount> [feerate] [memo-hex]   send ARG (amount like 1.25; feerate in units/byte, default 1; memo for bridge deposits)
  assets                      list the assets this wallet holds (uARG and others), with their ids
  send-asset <asset> <address> <amount> [feerate]   send units of an asset; <asset> is an asset id, a symbol the wallet holds,
                              or UARG for the bridge's uARG (named by -uarg-asset or -bridge-url; look-alikes are refused);
                              -memo adds a memo, such as the one the bridge's Withdraw page gives with a withdrawal address
  swap-address <chain-id> <token> <0x-address>   print the swap address that pays uARG out as <token> (USDC, USDT)
                              on that EVM chain to that address; send uARG to it with send-asset uarg
  redeem <amount> <0x-address> [chain-id:token-address] [feerate]   burn uARG with a memo so the bridge pays USDC or USDT
                              to that EVM address; the route names the chain and token, e.g. 84532:0x036c... (default:
                              the bridge's first chain and token). A swap address does the same with no memo.
  history                     list transactions
  utxos                       list coins
  mnemonic                    print the backup phrase (requires password)

Flags:
`

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Argonmine")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".argonmine")
}

func main() {
	fs := flag.NewFlagSet("argonmine-wallet", flag.ExitOnError)
	network := fs.String("network", "mainnet", "mainnet, testnet or regtest")
	dataDir := fs.String("datadir", defaultDataDir(), "node data directory (for rpc.cookie and default wallet path)")
	walletPath := fs.String("wallet", "", "wallet file (default <datadir>/<network>/wallet.json)")
	rpcURL := fs.String("rpc", "", "node RPC URL (default http://127.0.0.1:<port>)")
	token := fs.String("token", "", "RPC token (default: read <datadir>/<network>/rpc.cookie)")
	password := fs.String("password", "", "wallet password (or set ARGONMINE_WALLET_PASSWORD; prompted otherwise)")
	passphrase := fs.String("passphrase", "", "optional BIP-39 passphrase (create/restore only)")
	uargFlag := fs.String("uarg-asset", "", "the bridge's uARG asset id (64 hex); redeem and send-asset UARG use only this asset")
	bridgeURL := fs.String("bridge-url", "", "bridge URL whose /api/status names the uARG asset id (used when -uarg-asset is not given)")
	memoFlag := fs.String("memo", "", "send-asset: a memo for the payment, such as the withdrawal memo the bridge's Withdraw page gives (at most 80 characters)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}

	var params *consensus.Params
	switch *network {
	case "mainnet":
		params = consensus.MainNet
	case "testnet":
		params = consensus.TestNet
	case "regtest":
		params = consensus.RegTest
	default:
		fatal("unknown network %q", *network)
	}
	netDir := *dataDir
	if params != consensus.MainNet {
		netDir = filepath.Join(netDir, params.Name)
	}
	if *walletPath == "" {
		*walletPath = filepath.Join(netDir, "wallet.json")
	}
	if *rpcURL == "" {
		*rpcURL = fmt.Sprintf("http://127.0.0.1:%d", params.RPCPort)
	}
	if *token == "" {
		if b, err := os.ReadFile(filepath.Join(netDir, "rpc.cookie")); err == nil {
			*token = strings.TrimSpace(string(b))
		}
	}
	client := wallet.NewClient(*rpcURL, *token)

	pw := func() string {
		if *password != "" {
			return *password
		}
		if p := os.Getenv("ARGONMINE_WALLET_PASSWORD"); p != "" {
			return p
		}
		fmt.Fprint(os.Stderr, "wallet password: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n")
	}

	// Which asset is uARG is never decided by its symbol: anyone can issue an
	// asset called UARG. The id comes from -uarg-asset or the bridge.
	var uargID string
	var uargDone bool
	uarg := func(required bool) string {
		if !uargDone {
			uargDone = true
			switch id := strings.ToLower(strings.TrimSpace(*uargFlag)); {
			case id != "":
				if !isHexID(id) {
					fatal("-uarg-asset must be a 64-character hex asset id")
				}
				uargID = id
			case *bridgeURL != "":
				id, err := fetchBridgeAsset(*bridgeURL)
				if err != nil {
					if required {
						fatal("could not learn the uARG asset id from the bridge at %s: %v", *bridgeURL, err)
					}
					fmt.Fprintf(os.Stderr, "warning: the bridge at %s did not say which asset is uARG (%v); every UARG is shown as unverified\n", *bridgeURL, err)
				}
				uargID = id
			}
			ui.SetCanonicalAsset("UARG", uargID)
		}
		if required && uargID == "" {
			fatal("which asset is the bridge's uARG? Pass -uarg-asset <asset id> or -bridge-url <bridge URL>; an asset is never picked by the symbol UARG alone")
		}
		return uargID
	}

	open := func() *wallet.Wallet {
		w, err := wallet.Open(*walletPath, params, pw())
		if err != nil {
			fatal("open wallet: %v", err)
		}
		w.SetClient(client)
		return w
	}

	switch args[0] {
	case "create":
		w, mnemonic, err := wallet.Create(*walletPath, params, pw(), *passphrase)
		if err != nil {
			fatal("create: %v", err)
		}
		fmt.Printf("wallet created: %s\n\nBACKUP PHRASE (write it down, it is the only way to recover your coins):\n\n  %s\n\n", *walletPath, mnemonic)
		w.SetClient(client)
		addr, _ := w.NewAddress()
		fmt.Printf("first receive address: %s\n", addr)
	case "restore":
		if len(args) < 2 {
			fatal("usage: restore \"<24 words>\"")
		}
		mnemonic := strings.Join(args[1:], " ")
		if !wallet.ValidateMnemonic(mnemonic) {
			fatal("invalid mnemonic")
		}
		w, err := wallet.Restore(*walletPath, params, mnemonic, *passphrase, pw())
		if err != nil {
			fatal("restore: %v", err)
		}
		w.SetClient(client)
		fmt.Printf("wallet restored: %s\nscanning chain...\n", *walletPath)
		h, err := w.Sync()
		if err != nil {
			fatal("sync: %v", err)
		}
		uarg(false)
		printBalance(w, h)
	case "address":
		w := open()
		addr, err := w.NewAddress()
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(addr)
	case "addresses":
		w := open()
		for _, a := range w.Addresses() {
			fmt.Println(a)
		}
	case "sync":
		w := open()
		h, err := w.Sync()
		if err != nil {
			fatal("sync: %v", err)
		}
		fmt.Printf("synced to height %d\n", h)
	case "balance":
		w := open()
		h, err := w.Sync()
		if err != nil {
			fatal("sync: %v", err)
		}
		uarg(false)
		printBalance(w, h)
	case "send":
		if len(args) < 3 {
			fatal("usage: send <address> <amount> [feerate]")
		}
		amount, err := wallet.ParseAmount(args[2])
		if err != nil {
			fatal("amount: %v", err)
		}
		feeRate := consensus.Amount(1)
		if len(args) > 3 {
			fr, err := strconv.ParseInt(args[3], 10, 64)
			if err != nil || fr < 1 || fr > 1_000_000 {
				fatal("feerate must be a whole number of units per byte (1 to 1000000)")
			}
			feeRate = consensus.Amount(fr)
		}
		var memo []byte
		if len(args) > 4 {
			memo, err = hex.DecodeString(args[4])
			if err != nil || len(memo) == 0 || len(memo) > consensus.MaxNullDataLen {
				fatal("memo must be 1 to 80 bytes of hex")
			}
		}
		if bridge.IsSwapAddress(params.HRP, args[1]) {
			fatal("%s is a swap address: only uARG can be sent to it (send-asset uarg %s <amount>); ARG sent there is lost", args[1], args[1])
		}
		w := open()
		if _, err := w.Sync(); err != nil {
			fatal("sync: %v", err)
		}
		txid, err := w.SendManyMemo([]wallet.Recipient{{Address: args[1], Amount: amount}}, memo, feeRate)
		if err != nil {
			fatal("send: %v", err)
		}
		fmt.Printf("sent %s ARG to %s\ntxid %s\n", wallet.FormatAmount(amount), args[1], txid)
	case "assets":
		uarg(false)
		w := open()
		if _, err := w.Sync(); err != nil {
			fatal("sync: %v", err)
		}
		bal := w.AssetBalances()
		if len(bal) == 0 {
			fmt.Println("no assets held (ARG is shown by 'balance')")
		}
		for _, b := range bal {
			lock := ""
			if b.Locked > 0 {
				lock = fmt.Sprintf("  pending %s", wallet.FormatAmount(b.Locked))
			}
			fmt.Printf("%-8s %18s%s  id %s\n", assetLabel(b), wallet.FormatAmount(b.Confirmed), lock, b.Asset)
		}
	case "send-asset":
		if len(args) < 4 {
			fatal("usage: send-asset <asset> <address> <amount> [feerate]")
		}
		amount, err := wallet.ParseAmount(args[3])
		if err != nil {
			fatal("amount: %v", err)
		}
		feeRate := parseFeeRate(args, 4)
		w := open()
		if _, err := w.Sync(); err != nil {
			fatal("sync: %v", err)
		}
		genuine := ""
		if strings.EqualFold(strings.TrimSpace(args[1]), "uarg") {
			genuine = uarg(true)
		} else {
			uarg(false)
		}
		b := findAsset(w, args[1], genuine)
		if d, ok := bridge.ParseSwapAddress(params.HRP, args[2]); ok {
			if genuine == "" || b.Asset != genuine {
				fatal("%s is a swap address: only the bridge's uARG can be sent to it (send-asset uarg ...); anything else is lost", args[2])
			}
			fmt.Printf("swap address: the bridge pays %s on chain %d to %s, less its fee, once the payment has enough confirmations\n", d.Symbol, d.ChainID, bridge.ChecksumAddress(d.To))
		}
		memo := strings.TrimSpace(*memoFlag)
		if len(memo) > consensus.MaxNullDataLen {
			fatal("-memo can be at most %d characters", consensus.MaxNullDataLen)
		}
		if c := bridge.ParseWithdrawCode(memo); c != "" {
			memo = bridge.FormatWithdrawCode(c)
		}
		txid, err := w.SendAssetMemo(b.Asset, args[2], amount, []byte(memo), feeRate)
		if err != nil {
			fatal("send-asset: %v", err)
		}
		fmt.Printf("sent %s %s to %s\ntxid %s\n", wallet.FormatAmount(amount), assetLabel(b), args[2], txid)
	case "swap-address":
		if len(args) < 4 {
			fatal("usage: swap-address <chain-id> <token> <0x-address>")
		}
		chainID, err := strconv.ParseUint(args[1], 10, 64)
		if err != nil || chainID == 0 {
			fatal("chain-id must be an EVM chain id, e.g. 11155111")
		}
		to, err := bridge.ParseAddress(args[3])
		if err != nil {
			fatal("0x-address: %v", err)
		}
		addr, err := bridge.SwapAddress(params.HRP, bridge.SwapDest{ChainID: chainID, Symbol: args[2], To: to})
		if err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%s\nuARG sent to this address is paid out as %s on chain %d to %s (if the bridge serves that chain and token; see its /api/status)\n", addr, strings.ToUpper(args[2]), chainID, bridge.ChecksumAddress(to))
	case "redeem":
		const redeemUsage = "usage: redeem <amount> <0x-address> [chain-id:token-address] [feerate]"
		if len(args) < 3 {
			fatal(redeemUsage)
		}
		amount, err := wallet.ParseAmount(args[1])
		if err != nil {
			fatal("amount: %v", err)
		}
		evm, err := bridge.ParseAddress(args[2])
		if err != nil {
			fatal("the payout address must be a 0x EVM address: %v", err)
		}
		// A "chain-id:token-address" route picks where the bridge pays out
		// (BRG2 memo); without it the bridge pays on its default chain and
		// token (BRG1). The colon keeps the route distinct from a fee rate.
		rest := args[3:]
		memo, route := bridge.EncodeMemo(evm), "the bridge's default chain and token (24-byte BRG1 memo)"
		if len(rest) >= 1 && strings.Contains(rest[0], ":") {
			idStr, tokStr, _ := strings.Cut(rest[0], ":")
			chainID, err := strconv.ParseUint(idStr, 10, 64)
			if err != nil || chainID == 0 {
				fatal("the route must be <chain-id>:<token-address>, e.g. 84532:0x036c…; chain id must be a whole number above 0")
			}
			token, err := bridge.ParseAddress(tokStr)
			if err != nil {
				fatal("the route must be <chain-id>:<token-address>; the token must be a 0x contract address: %v", err)
			}
			if token == [20]byte{} {
				fatal("the token must be a contract address, not the zero address")
			}
			memo = bridge.EncodeRedeemMemo(chainID, token, evm)
			route = fmt.Sprintf("token %s on chain %d (52-byte BRG2 memo)", bridge.HexAddress(token), chainID)
			rest = rest[1:]
		} else if len(rest) >= 1 && (isDigits(rest[0]) && len(strings.TrimLeft(rest[0], "0")) > 7 || strings.HasPrefix(strings.ToLower(rest[0]), "0x")) {
			fatal("the payout route is one argument, <chain-id>:<token-address>\n%s", redeemUsage)
		}
		if len(rest) > 1 {
			fatal(redeemUsage)
		}
		feeRate := parseFeeRate(rest, 0)
		w := open()
		if _, err := w.Sync(); err != nil {
			fatal("sync: %v", err)
		}
		b := findAsset(w, "UARG", uarg(true))
		txid, err := w.BurnAsset(b.Asset, amount, memo, feeRate)
		if err != nil {
			fatal("redeem: %v", err)
		}
		fmt.Printf("burned %s %s for payout to %s\npayout route: %s\nredemption id (txid) %s\nthe bridge pays out after the burn has enough confirmations, minus the vault's release fee (shown on the bridge page); track it there by this id\n", wallet.FormatAmount(amount), assetLabel(b), bridge.HexAddress(evm), route, txid)
	case "history":
		uarg(false)
		w := open()
		if _, err := w.Sync(); err != nil {
			fatal("sync: %v", err)
		}
		for _, r := range w.History() {
			when := "pending"
			if r.Height > 0 {
				when = fmt.Sprintf("%d %s", r.Height, time.Unix(int64(r.Time), 0).Format("2006-01-02 15:04"))
			}
			extra := ""
			for id, n := range r.Assets {
				label := id[:8] + "…"
				if info, ok := w.AssetInfoFor(id); ok && info.Symbol != "" {
					label = ui.DisplaySymbol(info.Symbol, id)
				}
				extra += fmt.Sprintf("  %+s %s", wallet.FormatAmount(n), label)
			}
			fmt.Printf("%-24s %+16s  fee %s%s  %s\n", when, wallet.FormatAmount(r.Net), wallet.FormatAmount(r.Fee), extra, r.TxID)
		}
	case "utxos":
		uarg(false)
		w := open()
		for _, u := range w.UTXOs() {
			lock := ""
			if u.LockedBy != "" {
				lock = " (pending spend)"
			}
			cb := ""
			if u.Coinbase {
				cb = " coinbase"
			}
			unit := "ARG"
			if u.Asset != "" {
				unit = u.Asset[:8] + "…"
				if info, ok := w.AssetInfoFor(u.Asset); ok && info.Symbol != "" {
					unit = ui.DisplaySymbol(info.Symbol, u.Asset)
				}
			}
			fmt.Printf("%s:%d %s %s height %d%s%s\n", u.TxID, u.Index, wallet.FormatAmount(u.Amount), unit, u.Height, cb, lock)
		}
	case "mnemonic":
		w := open()
		fmt.Println(w.Mnemonic())
	default:
		fatal("unknown command %q", args[0])
	}
}

func printBalance(w *wallet.Wallet, tip uint32) {
	s, i, l := w.Balance(tip)
	fmt.Printf("height     %d\nspendable  %s\nimmature   %s\npending    %s\n", tip, wallet.FormatAmount(s), wallet.FormatAmount(i), wallet.FormatAmount(l))
	for _, b := range w.AssetBalances() {
		fmt.Printf("%-10s %s\n", assetLabel(b), wallet.FormatAmount(b.Confirmed))
	}
}

// assetLabel is the symbol shown for a held asset ("uARG" only for the
// bridge's asset id, "UARG (unverified)" for a look-alike), or a short id
// when the wallet has not seen the issuance.
func assetLabel(b wallet.AssetBalance) string {
	if b.Symbol != "" {
		return ui.DisplaySymbol(b.Symbol, b.Asset)
	}
	return b.Asset[:8] + "…"
}

// findAsset is pickAsset over the wallet's holdings; it exits on failure.
func findAsset(w *wallet.Wallet, q, genuine string) wallet.AssetBalance {
	b, err := pickAsset(w.AssetBalances(), q, genuine)
	if err != nil {
		fatal("%v", err)
	}
	return b
}

// pickAsset resolves an asset id, "UARG" (the bridge's asset genuine and
// nothing else) or another symbol to one of the holdings. Symbols that
// imitate the chain's coins (UARG, WARG, ARG) never select an asset by
// symbol: a look-alike is reachable by its id only.
func pickAsset(bal []wallet.AssetBalance, q, genuine string) (wallet.AssetBalance, error) {
	q = strings.TrimSpace(q)
	ql := strings.ToLower(q)
	if ql == "uarg" {
		if genuine == "" {
			return wallet.AssetBalance{}, fmt.Errorf("which asset is the bridge's uARG? Pass -uarg-asset <asset id> or -bridge-url <bridge URL>")
		}
		fakes := 0
		for _, b := range bal {
			if strings.EqualFold(b.Asset, genuine) {
				return b, nil
			}
			if strings.EqualFold(b.Symbol, "UARG") {
				fakes++
			}
		}
		if fakes > 0 {
			return wallet.AssetBalance{}, fmt.Errorf("this wallet holds no uARG (the bridge's asset %s); the %d asset(s) here called UARG are look-alikes from other issuers and are not used", genuine, fakes)
		}
		return wallet.AssetBalance{}, fmt.Errorf("this wallet holds no uARG (the bridge's asset %s)", genuine)
	}
	if isHexID(ql) {
		for _, b := range bal {
			if strings.EqualFold(b.Asset, ql) {
				return b, nil
			}
		}
		return wallet.AssetBalance{}, fmt.Errorf("no asset %s in this wallet; run 'assets' to see what it holds", ql)
	}
	if reservedSymbol(q) {
		return wallet.AssetBalance{}, fmt.Errorf("%s is not picked by symbol: anyone can issue an asset called that. Use the asset id (see 'assets')", strings.ToUpper(q))
	}
	var matches []wallet.AssetBalance
	for _, b := range bal {
		if !reservedSymbol(b.Symbol) && strings.EqualFold(b.Symbol, q) {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(bal) == 0 {
			return wallet.AssetBalance{}, fmt.Errorf("this wallet holds no assets")
		}
		return wallet.AssetBalance{}, fmt.Errorf("no asset %q in this wallet; run 'assets' to see what it holds", q)
	default:
		return wallet.AssetBalance{}, fmt.Errorf("%d assets are called %s here; use the asset id instead (see 'assets')", len(matches), q)
	}
}

// reservedSymbol reports whether a symbol imitates the chain's coins.
func reservedSymbol(sym string) bool {
	switch strings.ToUpper(strings.TrimSpace(sym)) {
	case "UARG", "WARG", "ARG":
		return true
	}
	return false
}

func isHexID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

// fetchBridgeAsset reads the uARG asset id from a bridge's /api/status.
func fetchBridgeAsset(bridgeURL string) (string, error) {
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Get(strings.TrimRight(strings.TrimSpace(bridgeURL), "/") + "/api/status")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var st struct {
		Asset string `json:"asset"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return "", err
	}
	id := strings.ToLower(strings.TrimSpace(st.Asset))
	if !isHexID(id) {
		return "", fmt.Errorf("the status names no asset id")
	}
	return id, nil
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseFeeRate(args []string, i int) consensus.Amount {
	if len(args) <= i {
		return 1
	}
	fr, err := strconv.ParseInt(args[i], 10, 64)
	if err != nil || fr < 1 || fr > 1_000_000 {
		fatal("feerate must be a whole number of units per byte (1 to 1000000)")
	}
	return consensus.Amount(fr)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
