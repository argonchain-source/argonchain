package wallet_test

import (
	"path/filepath"
	"testing"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
	"argonmine/rpc"
	"argonmine/wallet"
)

func startNode(t *testing.T) *wallet.Client {
	t.Helper()
	ch, err := chain.Open(consensus.RegTest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := mempool.New(ch, mempool.DefaultConfig())
	srv := rpc.New(rpc.Config{Listen: "127.0.0.1:0", Token: "tok"}, rpc.Deps{Params: consensus.RegTest, Chain: ch, Pool: pool, Started: time.Now(), Logf: t.Logf})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Stop()
		ch.Close()
	})
	return wallet.NewClient("http://"+srv.Addr(), "tok")
}

func TestWalletEndToEnd(t *testing.T) {
	client := startNode(t)
	dir := t.TempDir()

	// Wallet A: create, get an address, mine 101 blocks to it.
	wa, mnemonic, err := wallet.Create(filepath.Join(dir, "a.json"), consensus.RegTest, "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	wa.SetClient(client)
	addrA, err := wa.NewAddress()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call("generatetoaddress", 101, addrA); err != nil {
		t.Fatal(err)
	}
	h, err := wa.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if h != 101 {
		t.Fatalf("synced %d", h)
	}
	spendable, immature, locked := wa.Balance(h)
	// Coinbase 1 (height 1) matures when 101 blocks exist: 101+1-1 = 101 >= 100 -> spendable; height 2 also (100).
	if spendable != 100*consensus.UnitsPerCoin || immature != 99*50*consensus.UnitsPerCoin || locked != 0 {
		t.Fatalf("balance: %d %d %d", spendable, immature, locked)
	}
	if len(wa.History()) != 101 {
		t.Fatal("history")
	}

	// Wallet B receives 30 ARG from A.
	wb, _, err := wallet.Create(filepath.Join(dir, "b.json"), consensus.RegTest, "pw2", "")
	if err != nil {
		t.Fatal(err)
	}
	wb.SetClient(client)
	addrB, _ := wb.NewAddress()
	txid, err := wa.Send(addrB, 30*consensus.UnitsPerCoin, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Inputs are locked until confirmed.
	spendable, _, locked = wa.Balance(h)
	if locked != 50*consensus.UnitsPerCoin || spendable != 50*consensus.UnitsPerCoin {
		t.Fatalf("after send: spendable %d locked %d", spendable, locked)
	}
	// Insufficient funds error for too much.
	if _, err := wa.Send(addrB, 60*consensus.UnitsPerCoin, 1); err == nil {
		t.Fatal("overspend accepted")
	}

	// Confirm it.
	if _, err := client.Call("generatetoaddress", 1, addrA); err != nil {
		t.Fatal(err)
	}
	h, _ = wa.Sync()
	hb, _ := wb.Sync()
	if h != 102 || hb != 102 {
		t.Fatal("sync heights")
	}
	sb, _, _ := wb.Balance(hb)
	if sb != 30*consensus.UnitsPerCoin {
		t.Fatalf("B balance %d", sb)
	}
	sa, _, la := wa.Balance(h)
	// A: 50 (coinbase 2) + change (50 - 30 - fee) ; coinbase 3 now mature too (102+1-3 = 100).
	fee := consensus.Amount(0)
	for _, r := range wa.History() {
		if r.TxID == txid {
			fee = r.Fee
			if r.Height != 102 || r.Net != -(30*consensus.UnitsPerCoin+fee) {
				t.Fatalf("history record %+v", r)
			}
		}
	}
	if fee <= 0 || fee > 1000 {
		t.Fatalf("fee %d", fee)
	}
	want := 50*consensus.UnitsPerCoin + 50*consensus.UnitsPerCoin + (50*consensus.UnitsPerCoin - 30*consensus.UnitsPerCoin - fee)
	if sa != want || la != 0 {
		t.Fatalf("A balance %d want %d locked %d", sa, want, la)
	}

	// Restore A from its mnemonic into a new file: finds the same coins
	// (receive address 0 and change address 0 are within the lookahead).
	wr, err := wallet.Restore(filepath.Join(dir, "r.json"), consensus.RegTest, mnemonic, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	wr.SetClient(client)
	if _, err := wr.Sync(); err != nil {
		t.Fatal(err)
	}
	sr, ir, _ := wr.Balance(102)
	_, ia, _ := wa.Balance(102)
	if sr != sa || ir != ia {
		t.Fatalf("restored balance %d/%d vs %d/%d", sr, ir, sa, ia)
	}
	if addrs := wr.Addresses(); len(addrs) != 1 || addrs[0] != addrA {
		t.Fatalf("restored addresses %v", addrs)
	}

	// Reopen with the password; wrong password fails.
	if _, err := wallet.Open(filepath.Join(dir, "a.json"), consensus.RegTest, "nope"); err == nil {
		t.Fatal("wrong password accepted")
	}
	wa2, err := wallet.Open(filepath.Join(dir, "a.json"), consensus.RegTest, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if s2, _, _ := wa2.Balance(102); s2 != sa {
		t.Fatal("state not persisted")
	}
	if _, err := wallet.Open(filepath.Join(dir, "a.json"), consensus.TestNet, "pw"); err != wallet.ErrWrongNetwork {
		t.Fatal("network check")
	}
}
