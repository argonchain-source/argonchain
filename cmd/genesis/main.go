// Command genesis mines genesis blocks. With no flags it verifies or mines
// the nonces for the networks compiled into consensus/params.go. With -time
// it mines fresh genesis blocks for a chosen launch time and prints the
// values to paste into params.go; this is the mainnet launch procedure
// (see docs/LAUNCH.md).
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"argonmine/consensus"
)

// mineHeader searches nonces from start upwards on every CPU and reports
// progress to stderr every two minutes.
func mineHeader(h consensus.Header, p *consensus.Params, start uint64, threads int) (uint64, time.Duration) {
	began := time.Now()
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	var found atomic.Uint64
	var done atomic.Bool
	var hashes atomic.Uint64
	var wg sync.WaitGroup
	for w := 0; w < threads; w++ {
		wg.Add(1)
		go func(offset uint64) {
			defer wg.Done()
			hdr := h
			for n := start + offset; !done.Load(); n += uint64(threads) {
				hdr.Nonce = n
				hashes.Add(1)
				if consensus.CheckProofOfWork(&hdr, p.PowLimit) == nil {
					if done.CompareAndSwap(false, true) {
						found.Store(n)
					}
					return
				}
			}
		}(uint64(w))
	}
	progress := time.NewTicker(2 * time.Minute)
	defer progress.Stop()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	for {
		select {
		case <-finished:
			return found.Load(), time.Since(began)
		case <-progress.C:
			el := time.Since(began).Seconds()
			fmt.Fprintf(os.Stderr, "progress: %d hashes, %.0f H/s, %s elapsed\n", hashes.Load(), float64(hashes.Load())/el, time.Since(began).Round(time.Second))
		}
	}
}

func main() {
	launch := flag.Int64("time", 0, "genesis time (unix seconds) to mine for; 0 = verify/mine the compiled-in parameters")
	message := flag.String("message", consensus.GenesisMessage, "genesis NULLDATA message (<= 80 bytes)")
	start := flag.Uint64("start", 0, "first nonce to try (run several instances with disjoint ranges to mine in parallel)")
	threads := flag.Int("threads", 0, "threads (default: all CPUs)")
	flag.Parse()

	all := []*consensus.Params{consensus.RegTest, consensus.TestNet, consensus.MainNet}
	var nets []*consensus.Params
	if flag.NArg() == 0 {
		nets = all
	}
	for _, a := range flag.Args() {
		switch a {
		case "mainnet":
			nets = append(nets, consensus.MainNet)
		case "testnet":
			nets = append(nets, consensus.TestNet)
		case "regtest":
			nets = append(nets, consensus.RegTest)
		default:
			fmt.Fprintf(os.Stderr, "unknown network %q\n", a)
			os.Exit(2)
		}
	}
	if len(*message) > consensus.MaxNullDataLen {
		fmt.Fprintln(os.Stderr, "message longer than 80 bytes")
		os.Exit(2)
	}

	if *launch != 0 {
		fmt.Printf("// genesis for time %d (%s)\n", *launch, time.Unix(*launch, 0).UTC().Format(time.RFC3339))
		for _, p := range nets {
			blk := consensus.BuildGenesisBlock(uint64(*launch), p.PowLimitBits, 0, *message)
			nonce, took := mineHeader(blk.Header, p, *start, *threads)
			blk.Header.Nonce = nonce
			fmt.Printf("%-8s nonce=%d hash=%s (%s)\n", p.Name, nonce, blk.Header.Hash(), took.Round(time.Millisecond))
		}
		fmt.Println("// paste: set mainnetGenesisTime to the time above and mainnet's nonce in consensus/params.go (never the testnet's or regtest's), then run go test ./consensus/")
		return
	}

	for _, p := range nets {
		hdr := p.Genesis.Header
		if consensus.CheckProofOfWork(&hdr, p.PowLimit) == nil {
			fmt.Printf("%-8s already mined: nonce=%d hash=%s\n", p.Name, hdr.Nonce, hdr.Hash())
			continue
		}
		nonce, took := mineHeader(hdr, p, *start, *threads)
		hdr.Nonce = nonce
		fmt.Printf("%-8s nonce=%d time=%d bits=0x%08X hash=%s pow=%s (%s)\n",
			p.Name, nonce, hdr.Time, hdr.Bits, hdr.Hash(), consensus.PowHash(hdr.Serialize()), took.Round(time.Millisecond))
	}
}
