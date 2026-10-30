//go:build js && wasm

// Command argonmine-webminer is the solo miner compiled to WebAssembly for the
// browser app. It exposes window.argonmineMiner with start(workServer,
// address[, referrer]), stop(), stats(), newWallet(hrp), addressOf(phrase,
// hrp) and wallet(op, argsJSON), the browser wallet's signer (wallet_js.go).
// Single-threaded by nature of Go/wasm.
package main

import (
	"fmt"
	"strings"
	"syscall/js"

	"argonmine/consensus"
	"argonmine/wallet"
	"argonmine/workclient"
)

var client *workclient.Client

func logf(format string, args ...any) {
	js.Global().Get("console").Call("log", "[miner] "+fmt.Sprintf(format, args...))
}

func start(this js.Value, args []js.Value) any {
	if len(args) < 2 {
		return "work server URL and payout address are required"
	}
	server := strings.TrimRight(strings.TrimSpace(args[0].String()), "/")
	addr := strings.ToLower(strings.TrimSpace(args[1].String()))
	if server == "" || addr == "" {
		return "work server URL and payout address are required"
	}
	if client != nil {
		go client.Stop() // see stop
	}
	client = workclient.New(server, addr, 1, logf)
	if len(args) >= 3 {
		client.SetReferrer(args[2].String())
	}
	client.Start()
	return nil
}

// stop ends mining without waiting for it. A callback from JavaScript that
// blocks while another goroutine waits on a JavaScript promise (a job fetch
// or a submission in flight) stalls the event loop that would settle the
// promise, and the Go runtime aborts with "all goroutines are asleep"; the
// miner then winds down in the background instead.
func stop(this js.Value, args []js.Value) any {
	if client != nil {
		go client.Stop()
	}
	return nil
}

func stats(this js.Value, args []js.Value) any {
	if client == nil {
		return js.ValueOf(map[string]any{"running": false})
	}
	return js.ValueOf(map[string]any{
		"running":  client.Running(),
		"hashrate": client.HashRate(),
		"hashes":   float64(client.Hashes()),
		"rejected": float64(client.Rejected()),
		"blocks":   float64(client.Blocks()),
		"height":   float64(client.Height()),
		"error":    client.LastError(),
	})
}

func paramsForHRP(hrp string) *consensus.Params {
	for _, p := range []*consensus.Params{consensus.MainNet, consensus.TestNet, consensus.RegTest} {
		if p.HRP == hrp {
			return p
		}
	}
	return nil
}

// newWallet(hrp) generates a fresh 24-word recovery phrase and returns it with
// the first receive address (m/86'/coin'/0'/0/0, the same path the desktop app
// uses, so the phrase restores there). Nothing is stored; the page shows the
// phrase once and keeps only the address.
func newWallet(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.ValueOf(map[string]any{"error": "address prefix required"})
	}
	p := paramsForHRP(args[0].String())
	if p == nil {
		return js.ValueOf(map[string]any{"error": "unknown address prefix"})
	}
	m, err := wallet.NewMnemonic()
	if err != nil {
		return js.ValueOf(map[string]any{"error": "no secure randomness available: " + err.Error()})
	}
	addr, err := firstAddress(m, p)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return js.ValueOf(map[string]any{"mnemonic": m, "address": addr})
}

// addressOf(mnemonic, hrp) returns the first receive address of a phrase, so a
// returning user can confirm a phrase they wrote down matches their address.
func addressOf(this js.Value, args []js.Value) any {
	if len(args) < 2 {
		return js.ValueOf(map[string]any{"error": "phrase and address prefix required"})
	}
	p := paramsForHRP(args[1].String())
	if p == nil {
		return js.ValueOf(map[string]any{"error": "unknown address prefix"})
	}
	addr, err := firstAddress(strings.Join(strings.Fields(strings.ToLower(args[0].String())), " "), p)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return js.ValueOf(map[string]any{"address": addr})
}

func firstAddress(mnemonic string, p *consensus.Params) (string, error) {
	seed, err := wallet.SeedFromMnemonic(mnemonic, "")
	if err != nil {
		return "", fmt.Errorf("that is not a valid 24-word phrase")
	}
	kc, err := wallet.NewKeyChain(seed, wallet.CoinTypeFor(p), 0)
	if err != nil {
		return "", err
	}
	k, err := kc.Derive(false, 0)
	if err != nil {
		return "", err
	}
	return k.Address(p.HRP), nil
}

func main() {
	js.Global().Set("argonmineMiner", js.ValueOf(map[string]any{
		"start":     js.FuncOf(start),
		"stop":      js.FuncOf(stop),
		"stats":     js.FuncOf(stats),
		"newWallet": js.FuncOf(newWallet),
		"addressOf": js.FuncOf(addressOf),
		"wallet":    js.FuncOf(walletCall),
	}))
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("argonmineMinerReady"))
	select {}
}
