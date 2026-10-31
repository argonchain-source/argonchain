// argonmine-swap runs the native swap service (docs/SWAP.md): a maker that
// trades ARG, wARG and uARG with wallets, one transaction per swap that
// both sides sign.
//
//	argonmine-swap -network testnet -wallet swap.wallet -node-rpc http://127.0.0.1:19332 -node-token … -bridge-url https://bridge… -price 0.10
//	argonmine-swap -network testnet -wallet swap.wallet init      create the maker's wallet and print its backup phrase and address
//	argonmine-swap -network testnet -wallet swap.wallet address   print the maker's address
//
// The wallet password comes from -password-file or the SWAP_PASSWORD
// environment variable; with SWAP_MNEMONIC set and no wallet file yet, the
// wallet is restored from that phrase. Both variables are removed from the
// environment once read.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"argonmine/consensus"
	"argonmine/swap"
	"argonmine/wallet"
)

func main() {
	network := flag.String("network", "mainnet", "mainnet, testnet or regtest")
	walletPath := flag.String("wallet", "swap.wallet", "the maker's wallet file")
	passwordFile := flag.String("password-file", "", "file holding the wallet password (else SWAP_PASSWORD)")
	listen := flag.String("listen", "127.0.0.1:8410", "HTTP listen address")
	nodeRPC := flag.String("node-rpc", "", "node RPC URL (default: the network's local RPC port)")
	nodeToken := flag.String("node-token", "", "node RPC token")
	bridgeURL := flag.String("bridge-url", "", "bridge whose /api/status names the uARG asset id")
	uargAsset := flag.String("uarg-asset", "", "the uARG asset id (64 hex); overrides -bridge-url")
	price := flag.String("price", "", "uARG per ARG before the spread, e.g. 0.10 (required)")
	spread := flag.Uint("spread-bps", 100, "spread on each side of ARG and wARG against uARG, in basis points")
	wrapFee := flag.Uint("wrap-fee-bps", 0, "fee on ARG to wARG and back, in basis points")
	maxARG := flag.String("max-arg", "10000", "largest swap paid in ARG")
	maxWARG := flag.String("max-warg", "10000", "largest swap paid in wARG")
	maxUARG := flag.String("max-uarg", "1000", "largest swap paid in uARG")
	minIn := flag.String("min", "0.01", "smallest swap, in any coin")
	feeRate := flag.Int64("fee-rate", 1, "network fee the maker pays, units per byte")
	splitARG := flag.String("split-arg", "100", "keep the maker's ARG in coins of about this size, so swaps in one block do not wait for each other (0: off)")
	splitUARG := flag.String("split-uarg", "10", "keep the maker's uARG in coins of about this size, so swaps paying uARG in one block do not wait for each other (0: off)")
	ledger := flag.String("ledger", "", "the wARG ledger file (default: next to the wallet)")
	treasury := flag.String("treasury", "", "address the swap fees are paid to (default: the network's treasury, if it has one; \"none\": keep them)")
	sweepMin := flag.String("fee-sweep-min", "1", "earned fees of a coin paid to the treasury at once")
	ipHeader := flag.String("client-ip-header", "", "request header naming the client address when a proxy is in front (right-most entry), e.g. X-Forwarded-For")
	closed := flag.String("closed", "", "close every swap with this message (the service keeps answering /api/info, with no pairs)")
	closedPairs := flag.String("closed-pairs", "", "close these directions only, e.g. ARG>UARG,WARG>UARG (left out of /api/info; their quotes are refused)")
	flag.Parse()

	closedSet, err := swap.ParseClosedPairs(*closedPairs)
	if err != nil {
		log.Fatalf("-closed-pairs: %v", err)
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
		log.Fatalf("unknown network %q", *network)
	}
	if *nodeRPC == "" {
		*nodeRPC = fmt.Sprintf("http://127.0.0.1:%d", params.RPCPort)
	}

	password := os.Getenv("SWAP_PASSWORD")
	if *passwordFile != "" {
		b, err := os.ReadFile(*passwordFile)
		if err != nil {
			log.Fatal(err)
		}
		password = strings.TrimRight(string(b), "\r\n")
	}
	mnemonic := strings.TrimSpace(os.Getenv("SWAP_MNEMONIC"))
	os.Unsetenv("SWAP_PASSWORD")
	os.Unsetenv("SWAP_MNEMONIC")
	if len(password) < 12 {
		log.Fatal("the wallet password (SWAP_PASSWORD or -password-file) must be at least 12 characters")
	}

	switch flag.Arg(0) {
	case "init":
		if _, err := os.Stat(*walletPath); err == nil {
			log.Fatalf("%s exists; refusing to overwrite it", *walletPath)
		}
		w, m, err := wallet.Create(*walletPath, params, password, "")
		if err != nil {
			log.Fatal(err)
		}
		addr, _ := w.ReceiveAddress()
		fmt.Printf("backup phrase (write it down; it controls the maker's coins):\n%s\naddress %s\n", m, addr)
		return
	case "address":
		w, err := wallet.Open(*walletPath, params, password)
		if err != nil {
			log.Fatal(err)
		}
		addr, _ := w.ReceiveAddress()
		fmt.Println(addr)
		return
	case "":
	default:
		log.Fatalf("unknown command %q (init, address, or none to serve)", flag.Arg(0))
	}

	w, err := wallet.Open(*walletPath, params, password)
	if errors.Is(err, os.ErrNotExist) && mnemonic != "" {
		w, err = wallet.Restore(*walletPath, params, mnemonic, "", password)
	}
	if err != nil {
		log.Fatalf("open wallet: %v", err)
	}
	node := wallet.NewClient(*nodeRPC, *nodeToken)
	w.SetClient(node)

	uarg := strings.ToLower(strings.TrimSpace(*uargAsset))
	if uarg == "" && *bridgeURL != "" {
		for i := 0; uarg == "" && i < 30; i++ {
			if uarg, err = bridgeAsset(*bridgeURL); err != nil {
				log.Printf("bridge %s: %v; retrying", *bridgeURL, err)
				time.Sleep(10 * time.Second)
			}
		}
	}
	uid, err := consensus.HashFromHex(uarg)
	if err != nil || uid == consensus.NativeAsset {
		log.Fatal("the uARG asset id is required: -uarg-asset or a reachable -bridge-url")
	}
	p, ok := new(big.Rat).SetString(strings.TrimSpace(*price))
	if !ok || p.Sign() <= 0 {
		log.Fatal("-price must be a positive number of uARG per ARG, e.g. 0.10")
	}
	amt := func(name, s string) consensus.Amount {
		a, err := wallet.ParseAmount(s)
		if err != nil {
			log.Fatalf("-%s: %v", name, err)
		}
		return a
	}
	amtOrZero := func(name, s string) consensus.Amount {
		if strings.TrimSpace(s) == "0" {
			return 0
		}
		return amt(name, s)
	}
	if *ledger == "" {
		*ledger = *walletPath + ".ledger.json"
	}
	// The swap fees go to the treasury of the chain's 1% rule (consensus/treasury.go).
	switch strings.TrimSpace(*treasury) {
	case "none":
		*treasury = ""
	case "":
		if len(params.TreasuryKey) == consensus.P2PKPayloadLen {
			a, err := wallet.EncodeAddress(params.HRP, consensus.OutputTypeP2PK, params.TreasuryKey)
			if err != nil {
				log.Fatal(err)
			}
			*treasury = a
		}
	}
	if *treasury != "" {
		log.Printf("swap fees go to the treasury %s (paid out from %s of a coin)", *treasury, *sweepMin)
	}
	m, err := swap.New(swap.Config{Params: params, Node: node, Wallet: w, UARG: uid, Price: p, SpreadBps: uint32(*spread), WrapFeeBps: uint32(*wrapFee),
		MaxIn: map[string]consensus.Amount{swap.ARG: amt("max-arg", *maxARG), swap.WARG: amt("max-warg", *maxWARG), swap.UARG: amt("max-uarg", *maxUARG)},
		MinIn: amt("min", *minIn), FeeRate: consensus.Amount(*feeRate), SplitARG: amtOrZero("split-arg", *splitARG), SplitUARG: amtOrZero("split-uarg", *splitUARG), LedgerPath: *ledger, Treasury: *treasury, FeeSweepMin: amt("fee-sweep-min", *sweepMin), ClientIPHeader: *ipHeader, Closed: strings.TrimSpace(*closed), ClosedPairs: closedSet, Logf: log.Printf})
	if err != nil {
		log.Fatal(err)
	}
	go m.Run(context.Background())
	srv := &http.Server{Addr: *listen, Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("swap maker for %s on http://%s · address %s · wARG %s · uARG %s · price %s uARG per ARG, spread %d bps, wrap fee %d bps",
		params.Name, *listen, m.Address(), m.WARG(), uid, p.FloatString(8), *spread, *wrapFee)
	log.Fatal(srv.ListenAndServe())
}

// bridgeAsset reads the uARG asset id from a bridge's /api/status.
func bridgeAsset(url string) (string, error) {
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Get(strings.TrimRight(url, "/") + "/api/status")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	var st struct {
		Asset string `json:"asset"`
	}
	if err := json.Unmarshal(b, &st); err != nil || len(st.Asset) != 64 {
		return "", errors.New("no asset id in the bridge status")
	}
	return strings.ToLower(st.Asset), nil
}
