// Command argonmine-miner mines through a work server: the explorer's pool
// (the default) or its solo work endpoint. It needs no chain data and no
// node, only the address to be paid and a few CPU cores. It is what the
// browser app runs, as a program.
//
//	argonmine-miner -address arg1... -threads 4
//	argonmine-miner -address arg1... -solo
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"argonmine/consensus"
	"argonmine/wallet"
	"argonmine/workclient"
)

func main() {
	server := flag.String("server", "https://explorer.argonmine.com", "work server (the explorer)")
	address := flag.String("address", "", "address paid for the work (required)")
	threads := flag.Int("threads", 0, "mining threads (default: all cores but one)")
	solo := flag.Bool("solo", false, "mine whole blocks that pay the address directly instead of pool shares")
	ref := flag.String("ref", "", "solo only: referrer address paid the referral share of blocks found")
	network := flag.String("network", "mainnet", "mainnet, testnet or regtest (the address must match)")
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
	addr := strings.ToLower(strings.TrimSpace(*address))
	if _, err := wallet.DecodeAddress(params.HRP, addr); err != nil {
		fmt.Fprintf(os.Stderr, "-address: not a valid %s address\n", params.Name)
		os.Exit(2)
	}
	if *threads < 1 {
		*threads = runtime.NumCPU() - 1
		if *threads < 1 {
			*threads = 1
		}
	}
	url := strings.TrimRight(strings.TrimSpace(*server), "/")
	mode := "pool"
	if *solo {
		mode = "solo"
	} else {
		url += "/pool"
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	c := workclient.New(url, addr, *threads, log.Printf)
	if *solo && *ref != "" {
		c.SetReferrer(*ref)
	}
	c.Start()
	log.Printf("miner: %s mining to %s through %s with %d threads", mode, addr, url, *threads)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			errText := ""
			if e := c.LastError(); e != "" {
				errText = " error: " + e
			}
			log.Printf("status: height %d hashrate %.1f H/s shares %d blocks %d rejected %d%s", c.Height(), c.HashRate(), c.Shares(), c.Blocks(), c.Rejected(), errText)
		case <-sig:
			c.Stop()
			log.Print("miner: stopped")
			return
		}
	}
}
