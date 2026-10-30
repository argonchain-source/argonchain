// Command argonmine-bridge runs one guardian of the federated bridge
// (docs/BRIDGE.md). Subcommands:
//
//	argonmine-bridge keygen <file>      create this guardian's key file and print its public keys
//	argonmine-bridge gaskey <file>      create the leader's gas key file (it sends releases and sweeps)
//	argonmine-bridge address            print the custody address for -guardians
//	argonmine-bridge                    run (leader with -leader, follower otherwise)
//
// Operator commands, signed with this guardian's key (-key) and sent to the
// daemon on this host (-listen), or applied to its state directory when it
// is stopped (docs/BRIDGE.md, "Operator commands"):
//
//	argonmine-bridge resume [all|l1|evm/<chain id>]         lift pauses
//	argonmine-bridge approve-release <burn txid>            pay a burn above the last payout tier
//	argonmine-bridge mark-unclaimable <chain id>#<id> [why] a held deposit refunded by hand is never issued
//	argonmine-bridge reset-vault <chain id>                 drop a pending or refused candidate vault, checked again next round
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"argonmine/bridge"
	"argonmine/consensus"
	"argonmine/wallet"
)

type keyFile struct {
	L1Private  string `json:"l1_private"`
	L1Public   string `json:"l1_public"` // 33-byte compressed
	EVMPrivate string `json:"evm_private"`
	EVMAddress string `json:"evm_address"`
}

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
	network := flag.String("network", "mainnet", "mainnet, testnet or regtest")
	dataDir := flag.String("datadir", defaultDataDir(), "node data directory (rpc.cookie); bridge state lives beside it")
	nodeRPC := flag.String("node-rpc", "", "this chain's node RPC URL (default http://127.0.0.1:<port>)")
	nodeToken := flag.String("node-token", "", "node RPC token (default: <datadir>/<network>/rpc.cookie)")
	chainsPath := flag.String("chains", "", "JSON file listing the EVM chains, their vaults and accepted tokens (docs/BRIDGE.md, Configuration)")
	assetSymbol := flag.String("asset-symbol", "UARG", "symbol of the asset issued on this chain")
	guardians := flag.String("guardians", "", "comma-separated compressed public keys of all guardians (hex)")
	keyPath := flag.String("key", "", "this guardian's key file from `keygen` (omit to run as observer)")
	leader := flag.Bool("leader", false, "coordinate issuance and payouts (exactly one guardian)")
	peers := flag.String("peers", "", "comma-separated base URLs of the other guardians (leader only)")
	leaderPub := flag.String("leader-pubkey", "", "the leader guardian's L1 public key, one of -guardians (required on a follower: it takes guardian requests signed by this key only)")
	secretFlag := flag.String("secret", "", "shared secret for the guardian API, at least 32 characters; prefer ARGONMINE_BRIDGE_SECRET or -secret-file (a flag shows in the process list)")
	secretFile := flag.String("secret-file", "", "file holding the shared secret for the guardian API")
	maxIssue := flag.String("max-issue", "10000", "most uARG one issuance may create (0 = no limit); larger deposits wait for the guardians")
	maxIssueDay := flag.String("max-issue-day", "100000", "most uARG this guardian signs for over any 24 hours (0 = no limit)")
	threshold := flag.Int("threshold", 2, "guardian signatures required to release from a vault")
	l1Confs := flag.Uint("l1-confirmations", 30, "confirmations on this chain before a burn is paid out (the floor of every payout tier)")
	tiersFlag := flag.String("l1-confirmation-tiers", "1000:6,10000:30,100000:120", "payout depth by burn size, amount:blocks in uARG, increasing; a burn above the last tier waits for approve-release (empty: -l1-confirmations for all)")
	reorgPause := flag.Uint("l1-reorg-pause", 3, "an Argonmine reorganisation undoing more than this many blocks pauses every issuance and release until `resume` (0: never)")
	gasKeyPath := flag.String("gas-key", "", "leader: file with the EVM key that sends release and sweep transactions (from `gaskey`, or a key file's evm_private); never a guardian key or the vault owner")
	releaseTimeout := flag.Duration("release-timeout", 5*time.Minute, "how long a release transaction may stay unmined before it is sent again at higher fees")
	issueRebroadcast := flag.Duration("issue-rebroadcast", 10*time.Minute, "how long an issuance may be neither mined nor in the mempool before it is broadcast again")
	stuckAfter := flag.Duration("stuck-after", time.Hour, "items waiting longer than this are listed under stuck in /api/status and logged once an hour")
	listen := flag.String("listen", "127.0.0.1:8395", "HTTP listen address (page, API, guardian endpoints)")
	ipHeader := flag.String("client-ip-header", "", "request header naming the client address when a proxy is in front, e.g. X-Forwarded-For (its right-most entry, the one the proxy appended, is used); empty: the TCP peer address, or the right-most X-Forwarded-For entry when the peer is a loopback, private or 100.64.0.0/10 address")
	issueEvery := flag.Duration("issue-interval", 2*time.Minute, "how often the leader batches issuances and payouts")
	scanEvery := flag.Duration("scan-interval", 10*time.Second, "how often every chain is scanned")
	flag.Parse()

	switch flag.Arg(0) {
	case "keygen":
		keygen(flag.Arg(1))
		return
	case "gaskey":
		gaskeygen(flag.Arg(1))
		return
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
	netDir := *dataDir
	if params != consensus.MainNet {
		netDir = filepath.Join(netDir, params.Name)
	}
	switch flag.Arg(0) {
	case "resume", "approve-release", "mark-unclaimable", "reset-vault":
		operatorCommand(params, *keyPath, *listen, filepath.Join(netDir, "bridge-state"), flag.Args())
		return
	}
	gkeys, err := bridge.ParseGuardianKeys(splitList(*guardians))
	if err != nil || len(gkeys) == 0 {
		log.Fatal("-guardians must list every guardian's compressed public key")
	}

	var l1Key *btcec.PrivateKey
	var evmKey *secp256k1.PrivateKey
	if *keyPath != "" {
		kf, err := loadKeys(*keyPath)
		if err != nil {
			log.Fatal(err)
		}
		l1Key, evmKey = kf.l1, kf.evm
	}
	custody, err := bridge.NewCustody(params.HRP, gkeys, l1Key)
	if err != nil {
		log.Fatal(err)
	}
	if flag.Arg(0) == "address" {
		fmt.Println(custody.Address())
		return
	}

	var leaderKey []byte
	if *leaderPub != "" {
		if leaderKey, err = hex.DecodeString(strings.TrimSpace(*leaderPub)); err != nil || len(leaderKey) != 33 || !custody.IsSigner(leaderKey) {
			log.Fatal("-leader-pubkey must be the leader's compressed L1 public key, one of -guardians")
		}
	}
	if !*leader && l1Key != nil && leaderKey == nil {
		log.Fatal("a follower needs -leader-pubkey: the leader guardian's L1 public key (one of -guardians); it signs nothing for anyone else")
	}
	secret, err := loadSecret(*secretFlag, *secretFile)
	if err != nil {
		log.Fatal(err)
	}
	if ((*leader && len(splitList(*peers)) > 0) || (!*leader && l1Key != nil)) && len(secret) < bridge.MinSecretLen {
		log.Fatalf("the shared secret (ARGONMINE_BRIDGE_SECRET, -secret-file or -secret) must be at least %d characters", bridge.MinSecretLen)
	}
	tiers, err := bridge.ParseTiers(*tiersFlag)
	if err != nil {
		log.Fatalf("-l1-confirmation-tiers: %v", err)
	}
	var gasKey *secp256k1.PrivateKey
	if *gasKeyPath != "" {
		if gasKey, err = loadGasKey(*gasKeyPath); err != nil {
			log.Fatalf("-gas-key: %v", err)
		}
		if evmKey != nil && bridge.AddressOf(gasKey.PubKey()) == bridge.AddressOf(evmKey.PubKey()) {
			log.Fatal("-gas-key is this guardian's own EVM key: use a dedicated key (argonmine-bridge gaskey <file>)")
		}
	}
	if *leader && gasKey == nil {
		log.Fatal("the leader needs -gas-key: a dedicated EVM key with a little gas on every chain, which sends the release and sweep transactions (argonmine-bridge gaskey <file> creates one)")
	}
	if !*leader && gasKey != nil {
		log.Print("-gas-key is ignored on a follower: only the leader sends transactions")
		gasKey = nil
	}
	limit, err := parseLimit("-max-issue", *maxIssue)
	if err != nil {
		log.Fatal(err)
	}
	dayLimit, err := parseLimit("-max-issue-day", *maxIssueDay)
	if err != nil {
		log.Fatal(err)
	}

	if *nodeRPC == "" {
		*nodeRPC = fmt.Sprintf("http://127.0.0.1:%d", params.RPCPort)
	}
	if *nodeToken == "" {
		if b, err := os.ReadFile(filepath.Join(netDir, "rpc.cookie")); err == nil {
			*nodeToken = strings.TrimSpace(string(b))
		}
	}
	if *chainsPath == "" {
		log.Fatal("-chains is required: a JSON file listing the EVM chains, vaults and tokens (docs/BRIDGE.md)")
	}
	chains, err := bridge.LoadChains(*chainsPath)
	if err != nil {
		log.Fatal(err)
	}
	evms := map[uint64]bridge.EVM{}
	extra := map[uint64][]bridge.EVM{}
	for _, c := range chains {
		// The first provider sends (the leader's gas key); every provider is
		// read, and deposits need them all to agree.
		evms[c.ID] = bridge.NewRPCEVM(c.RPCs[0], c.ID, c.Vault, gasKey)
		for _, u := range c.RPCs[1:] {
			extra[c.ID] = append(extra[c.ID], bridge.NewRPCEVM(u, c.ID, c.Vault, nil))
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	svc, err := bridge.New(bridge.Config{
		Params: params, Node: wallet.NewClient(*nodeRPC, *nodeToken), Chains: chains, EVMs: evms, ExtraEVMs: extra, Custody: custody, EVMKey: evmKey,
		Leader: *leader, Peers: splitList(*peers), Secret: secret, LeaderKey: leaderKey, Threshold: *threshold,
		L1Confirmations: uint32(*l1Confs), AssetSymbol: *assetSymbol,
		IssueInterval: *issueEvery, ScanInterval: *scanEvery,
		MaxIssue: limit, MaxIssueDay: dayLimit,
		ReleaseTiers: tiers, L1ReorgPause: uint32(*reorgPause),
		ReleaseTimeout: *releaseTimeout, IssueRebroadcast: *issueRebroadcast, StuckAfter: *stuckAfter,
		ClientIPHeader: strings.TrimSpace(*ipHeader),
		StatePath:      filepath.Join(netDir, "bridge-state"), Logf: log.Printf,
	})
	if err != nil {
		log.Fatal(err)
	}
	svc.Start()
	srv := newHTTPServer(*listen, svc.Handler())
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	role := "follower"
	if *leader {
		role = "leader"
	}
	var desc []string
	for _, c := range chains {
		var syms []string
		for _, t := range c.Tokens {
			syms = append(syms, t.Symbol)
		}
		router := ""
		if c.HasRouter() {
			router = ", deposit addresses via router " + bridge.HexAddress(c.Router)
		}
		desc = append(desc, fmt.Sprintf("%s (%d) vault %s%s, finality %s, %d provider(s): %s", c.Name, c.ID, bridge.HexAddress(c.Vault), router, c.Finality, len(c.RPCs), strings.Join(syms, ", ")))
	}
	if gasKey != nil {
		desc = append(desc, "gas key "+bridge.HexAddress(bridge.AddressOf(gasKey.PubKey())))
	}
	limits := "no issuance limits"
	if limit > 0 || dayLimit > 0 {
		limits = fmt.Sprintf("issuance limits %s per issuance, %s per 24 h (0 = none)", wallet.FormatAmount(limit), wallet.FormatAmount(dayLimit))
	}
	log.Printf("bridge %s on %s · issuer %s · asset %s (%s) · %d guardians, threshold %d · %s · chains: %s", role, *listen, custody.Address(), svc.Asset(), *assetSymbol, custody.N(), *threshold, limits, strings.Join(desc, "; "))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	srv.Close()
	svc.Stop()
	log.Print("bridge stopped")
}

// HTTP server timeouts. A guardian request from the leader waits up to
// bridge.PeerTimeout for its answer (a follower verifies what it is asked
// to sign first, one signature at a time), so a handler may take that
// long: the write timeout, which runs from the end of the request headers
// to the end of the answer, leaves room above it.
const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 30 * time.Second // bodies are small: a guardian request, a withdrawal, an operator command
	httpWriteTimeout      = bridge.PeerTimeout + 30*time.Second
	httpIdleTimeout       = 120 * time.Second
)

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: httpReadHeaderTimeout, ReadTimeout: httpReadTimeout,
		WriteTimeout: httpWriteTimeout, IdleTimeout: httpIdleTimeout, MaxHeaderBytes: 64 << 10}
}

type loadedKeys struct {
	l1  *btcec.PrivateKey
	evm *secp256k1.PrivateKey
}

func loadKeys(path string) (*loadedKeys, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, err
	}
	l1b, err := hex.DecodeString(kf.L1Private)
	if err != nil || len(l1b) != 32 {
		return nil, fmt.Errorf("key file: bad l1_private")
	}
	evmb, err := hex.DecodeString(kf.EVMPrivate)
	if err != nil || len(evmb) != 32 {
		return nil, fmt.Errorf("key file: bad evm_private")
	}
	l1, _ := btcec.PrivKeyFromBytes(l1b)
	return &loadedKeys{l1: l1, evm: secp256k1.PrivKeyFromBytes(evmb)}, nil
}

func keygen(path string) {
	if path == "" {
		log.Fatal("usage: argonmine-bridge keygen <file>")
	}
	if _, err := os.Stat(path); err == nil {
		log.Fatalf("%s already exists; refusing to overwrite a guardian key", path)
	}
	l1, err := btcec.NewPrivateKey()
	if err != nil {
		log.Fatal(err)
	}
	var eb [32]byte
	rand.Read(eb[:])
	evm := secp256k1.PrivKeyFromBytes(eb[:])
	kf := keyFile{
		L1Private:  hex.EncodeToString(l1.Serialize()),
		L1Public:   hex.EncodeToString(l1.PubKey().SerializeCompressed()),
		EVMPrivate: hex.EncodeToString(evm.Serialize()),
		EVMAddress: bridge.HexAddress(bridge.AddressOf(evm.PubKey())),
	}
	b, _ := json.MarshalIndent(kf, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("guardian keys written to %s (keep it secret, back it up offline)\n", path)
	fmt.Printf("share with the other guardians:\n  L1 public key: %s\n  EVM address:   %s\n", kf.L1Public, kf.EVMAddress)
}

// gasKeyFile is what `gaskey` writes.
type gasKeyFile struct {
	EVMPrivate string `json:"evm_private"`
	EVMAddress string `json:"evm_address"`
}

// loadGasKey reads the gas key: a JSON file with evm_private (from `gaskey`,
// or a key file of `keygen`), or a file holding just the hex private key.
func loadGasKey(path string) (*secp256k1.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hexKey := strings.TrimSpace(string(b))
	var kf gasKeyFile
	if json.Unmarshal(b, &kf) == nil && kf.EVMPrivate != "" {
		hexKey = kf.EVMPrivate
	}
	kb, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(hexKey, "0x"), "0X"))
	if err != nil || len(kb) != 32 {
		return nil, errors.New("the file holds no 32-byte EVM private key (evm_private)")
	}
	return secp256k1.PrivKeyFromBytes(kb), nil
}

func gaskeygen(path string) {
	if path == "" {
		log.Fatal("usage: argonmine-bridge gaskey <file>")
	}
	if _, err := os.Stat(path); err == nil {
		log.Fatalf("%s already exists; refusing to overwrite a key", path)
	}
	var eb [32]byte
	rand.Read(eb[:])
	k := secp256k1.PrivKeyFromBytes(eb[:])
	kf := gasKeyFile{EVMPrivate: hex.EncodeToString(k.Serialize()), EVMAddress: bridge.HexAddress(bridge.AddressOf(k.PubKey()))}
	b, _ := json.MarshalIndent(kf, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("gas key written to %s\nfund this address with a little native gas on every chain the bridge serves (it pays for releases and sweeps; keep the balance low):\n  %s\n", path, kf.EVMAddress)
}

// operatorCommand runs resume, approve-release, mark-unclaimable or
// reset-vault: signed with this guardian's L1 key and sent to the daemon on
// this host, or, when no daemon answers there, applied to the stopped
// guardian's state directory.
func operatorCommand(params *consensus.Params, keyPath, listen, statePath string, args []string) {
	action, body := args[0], map[string]any{}
	switch action {
	case "reset-vault":
		if len(args) < 2 {
			log.Fatal("usage: argonmine-bridge [flags] reset-vault <chain id>")
		}
		chainID, err := strconv.ParseUint(args[1], 10, 64)
		if err != nil || chainID == 0 {
			log.Fatal("the chain is its chain id, as in /api/status")
		}
		body["chain_id"] = chainID
	case "resume":
		scope := "all"
		if len(args) > 1 {
			scope = args[1]
		}
		body["scope"] = scope
	case "approve-release":
		if len(args) < 2 {
			log.Fatal("usage: argonmine-bridge [flags] approve-release <burn txid>")
		}
		body["txid"] = args[1]
	case "mark-unclaimable":
		if len(args) < 2 {
			log.Fatal("usage: argonmine-bridge [flags] mark-unclaimable <chain id>#<deposit id> [reason]")
		}
		c, id, ok := strings.Cut(args[1], "#")
		chainID, err1 := strconv.ParseUint(c, 10, 64)
		depID, err2 := strconv.ParseUint(id, 10, 64)
		if !ok || err1 != nil || err2 != nil {
			log.Fatal("the deposit is <chain id>#<deposit id>, as in /api/status")
		}
		body["chain_id"], body["id"] = chainID, depID
		if len(args) > 2 {
			body["reason"] = strings.Join(args[2:], " ")
		}
		action = "deposit-unclaimable" // the daemon's name for it (bridge/ops.go)
	}
	raw, _ := json.Marshal(body)
	if keyPath == "" {
		log.Fatal("operator commands are signed with this guardian's key: pass -key <key file>")
	}
	kf, err := loadKeys(keyPath)
	if err != nil {
		log.Fatal(err)
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		log.Fatalf("-listen %q: %v", listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	path := "/admin/" + action
	hdr, err := bridge.AdminHeaders(params.Name, kf.l1, path, raw, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+net.JoinHostPort(host, port)+path, bytes.NewReader(raw))
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	var out []byte
	if err != nil {
		var opErr *net.OpError
		if !errors.As(err, &opErr) || opErr.Op != "dial" {
			log.Fatal(err)
		}
		// Nothing listens there: the daemon is stopped; act on its state.
		fmt.Printf("no daemon answers on %s; applying %s to %s\n", net.JoinHostPort(host, port), action, statePath)
		res, err := bridge.AdminOffline(statePath, action, raw, log.Printf)
		if err != nil {
			log.Fatal(err)
		}
		out, _ = json.MarshalIndent(res, "", "  ")
	} else {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			log.Fatalf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		var v any
		if json.Unmarshal(b, &v) == nil {
			out, _ = json.MarshalIndent(v, "", "  ")
		} else {
			out = b
		}
	}
	fmt.Println(string(out))
}

// loadSecret returns the shared secret: the ARGONMINE_BRIDGE_SECRET
// environment variable first, then -secret-file, then -secret (which shows
// in the process list, so it is only a fallback).
func loadSecret(flagValue, file string) (string, error) {
	if v := os.Getenv("ARGONMINE_BRIDGE_SECRET"); v != "" {
		if flagValue != "" || file != "" {
			log.Print("ARGONMINE_BRIDGE_SECRET is set: -secret and -secret-file are ignored")
		}
		return v, nil
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("-secret-file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if flagValue != "" {
		log.Print("the shared secret came from -secret, which other users of this host can see; use ARGONMINE_BRIDGE_SECRET or -secret-file")
	}
	return flagValue, nil
}

// parseLimit reads an issuance limit in uARG ("10000", "2500.5"; 0 = none).
func parseLimit(name, v string) (consensus.Amount, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "0" {
		return 0, nil
	}
	a, err := wallet.ParseAmount(v)
	if err != nil || a < 0 {
		return 0, fmt.Errorf("%s must be an amount of uARG (0 for no limit)", name)
	}
	return a, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
