// Command argonmined is the full node: chain state, P2P, mempool, JSON-RPC and an
// optional built-in miner.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/miner"
	"argonmine/p2p"
	"argonmine/rpc"
	"argonmine/wallet"
)

// version is set at release time with -ldflags "-X main.version=<version>".
var version = "1.1.0"

// Default seed peers per network. Mainnet is filled in at launch (docs/LAUNCH.md
// step 7). The testnet is retired; it has no public seed.
var defaultSeeds = map[string][]string{
	"mainnet": {"altaria.proxy.rlwy.net:12347"}, // the Railway node (P2P on container port 19333 behind the TCP proxy)
	"testnet": {},                               // retired on 2026-10-28: no public nodes
	"regtest": {},
}

// Default DNS seeds per network: hostnames whose A/AAAA records list seed
// nodes. Filled in once the project has a domain.
var defaultDNSSeeds = map[string][]string{
	"mainnet": {},
	"testnet": {},
	"regtest": {},
}

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Argonmine")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "argonmine-data"
	}
	return filepath.Join(home, ".argonmine")
}

func main() {
	var (
		network   = flag.String("network", "mainnet", "mainnet, testnet or regtest")
		dataDir   = flag.String("datadir", defaultDataDir(), "data directory (a per-network subdirectory is used)")
		listen    = flag.String("listen", "", "P2P listen address (default :<network port>; 'none' to disable)")
		connect   = flag.String("connect", "", "comma-separated peers to stay connected to")
		seeds     = flag.String("seeds", "", "comma-separated extra seed peers")
		dnsSeeds  = flag.String("dnsseed", "", "comma-separated extra DNS seed hostnames")
		rpcListen = flag.String("rpc", "", "RPC listen address (default 127.0.0.1:<network rpc port>)")
		rpcToken  = flag.String("rpctoken", "", "RPC bearer token (default: random, written to <datadir>/rpc.cookie)")
		mineTo    = flag.String("mine", "", "start the built-in miner paying to this address")
		threads   = flag.Int("threads", 0, "miner threads (default: CPUs-1)")
		tag       = flag.String("tag", "", "coinbase miner tag (<=100 bytes)")

		behindProxy   = flag.Bool("behind-proxy", false, "inbound P2P connections arrive through a TCP proxy or load balancer (e.g. Railway): never ban or cap inbound peers by address")
		banLocalAddrs = flag.Bool("ban-local-addrs", false, "treat loopback/private/CGNAT inbound addresses as distinct peers (bans and the per-address cap apply); for private networks")
		maxInbound    = flag.Int("maxinbound", 125, "maximum inbound P2P connections; when full, an idle inbound peer is evicted for a newcomer")
		assetOutFee   = flag.Int64("assetoutputfee", int64(mempool.DefaultConfig().AssetOutputFee), "relay policy: extra fee (units) per asset output beyond the first two in a transaction")
	)
	flag.Parse()

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

	dir := *dataDir
	if params != consensus.MainNet {
		dir = filepath.Join(dir, params.Name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("datadir: %v", err)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("argonmined %s starting on %s, datadir %s", version, params.Name, dir)

	ch, err := chain.Open(params, filepath.Join(dir, "chain"))
	if err != nil {
		log.Fatalf("chain: %v", err)
	}
	tip := ch.Tip()
	log.Printf("chain: height %d tip %s", tip.Height, tip.Hash)

	poolCfg := mempool.DefaultConfig()
	if *assetOutFee < 0 {
		log.Fatalf("-assetoutputfee must not be negative")
	}
	poolCfg.AssetOutputFee = consensus.Amount(*assetOutFee)
	pool := mempool.New(ch, poolCfg)

	// P2P
	p2pCfg := p2p.Config{Params: params, DataDir: dir, UserAgent: "/argonmined:" + version + "/",
		BehindProxy: *behindProxy, LocalAddrsAreHosts: *banLocalAddrs, MaxInbound: *maxInbound}
	if *behindProxy {
		log.Printf("p2p: behind a proxy: inbound peers are never banned or capped by address")
	}
	switch *listen {
	case "none":
	case "":
		p2pCfg.Listen = fmt.Sprintf(":%d", params.P2PPort)
	default:
		p2pCfg.Listen = *listen
	}
	p2pCfg.Seeds = append(p2pCfg.Seeds, defaultSeeds[params.Name]...)
	p2pCfg.Seeds = append(p2pCfg.Seeds, splitList(*seeds)...)
	p2pCfg.DNSSeeds = append(p2pCfg.DNSSeeds, defaultDNSSeeds[params.Name]...)
	p2pCfg.DNSSeeds = append(p2pCfg.DNSSeeds, splitList(*dnsSeeds)...)
	p2pCfg.Connect = splitList(*connect)
	net := p2p.New(p2pCfg, ch, pool)
	if err := net.Start(); err != nil {
		log.Fatalf("p2p: %v", err)
	}

	// Miner (optional)
	var m *miner.Miner
	if *mineTo != "" {
		payout, err := wallet.DecodeAddress(params.HRP, *mineTo)
		if err != nil {
			log.Fatalf("-mine address: %v", err)
		}
		m = miner.New(ch, pool, payout, *threads, log.Printf)
		if *tag != "" {
			m.SetTag([]byte(*tag))
		}
		m.SetBroadcast(func(*consensus.Block) {}) // tips are announced by the chain subscription
		m.Start()
	}

	// RPC
	token := *rpcToken
	if token == "" {
		var b [24]byte
		rand.Read(b[:])
		token = hex.EncodeToString(b[:])
	}
	cookie := filepath.Join(dir, "rpc.cookie")
	if err := os.WriteFile(cookie, []byte(token), 0o600); err != nil {
		log.Fatalf("rpc cookie: %v", err)
	}
	rpcAddr := *rpcListen
	if rpcAddr == "" {
		rpcAddr = fmt.Sprintf("127.0.0.1:%d", params.RPCPort)
	}
	rpcSrv := rpc.New(rpc.Config{Listen: rpcAddr, Token: token}, rpc.Deps{
		Params: params, Chain: ch, Pool: pool, P2P: net, Miner: m, Version: version, Started: time.Now(), Logf: log.Printf,
	})
	if err := rpcSrv.Start(); err != nil {
		log.Fatalf("rpc: %v", err)
	}
	log.Printf("rpc: token written to %s", cookie)

	// Periodic status line.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				tip := ch.Tip()
				line := fmt.Sprintf("status: height %d peers %d mempool %d", tip.Height, net.PeerCount(), pool.Count())
				if m != nil {
					line += fmt.Sprintf(" hashrate %.1f H/s found %d", m.HashRate(), m.BlocksFound())
				}
				log.Print(line)
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")
	close(stop)
	if m != nil {
		m.Stop()
	}
	rpcSrv.Stop()
	net.Stop()
	if err := ch.Close(); err != nil {
		log.Printf("chain close: %v", err)
	}
	os.Remove(cookie)
	log.Print("bye")
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
