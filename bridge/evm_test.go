package bridge

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

func TestKeccakAndSelectors(t *testing.T) {
	// keccak256("") is the well-known constant.
	empty := Keccak256(nil)
	if hex.EncodeToString(empty[:]) != "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470" {
		t.Fatal("keccak256 empty")
	}
	// ERC-20 transfer and approve selectors are the well-known values.
	if sel := Selector("transfer(address,uint256)"); hex.EncodeToString(sel[:]) != "a9059cbb" {
		t.Fatal("selector")
	}
	if hex.EncodeToString(SelMint[:]) != "40c10f19" {
		t.Errorf("mint selector %x", SelMint)
	}
	if hex.EncodeToString(SelApprove[:]) != "095ea7b3" {
		t.Fatal("approve selector")
	}
}

func TestReleaseCallEncoding(t *testing.T) {
	var token, to [20]byte
	token[19], to[19] = 0xBB, 0xAA
	var burn [32]byte
	burn[0] = 0xDD
	sigs := [][]byte{bytes.Repeat([]byte{1}, 65), bytes.Repeat([]byte{2}, 65)}
	data := EncodeReleaseCall(token, to, big.NewInt(123456789), burn, sigs)
	if !bytes.Equal(data[:4], selRelease[:]) {
		t.Fatal("selector")
	}
	if selRelease != Selector("release(address,address,uint256,bytes32,bytes[])") {
		t.Fatal("release selector")
	}
	if data[4+31] != 0xBB || data[4+32+31] != 0xAA {
		t.Fatal("token and to words")
	}
	// Head: 5 words; offset of bytes[] = 0xa0.
	if data[4+32*4+31] != 0xa0 {
		t.Fatalf("array offset word: %x", data[4+32*4:4+32*5])
	}
	tail := data[4+32*5:]
	if new(big.Int).SetBytes(tail[:32]).Int64() != 2 {
		t.Fatal("array length")
	}
	// First element offset = 2*32 = 64, second = 64 + 32 + 96 (65 padded to 96).
	if new(big.Int).SetBytes(tail[32:64]).Int64() != 64 || new(big.Int).SetBytes(tail[64:96]).Int64() != 64+32+96 {
		t.Fatalf("element offsets %x %x", tail[32:64], tail[64:96])
	}
	if new(big.Int).SetBytes(tail[96:128]).Int64() != 65 || tail[128] != 1 || tail[128+64] != 1 || tail[128+65] != 0 {
		t.Fatal("first element")
	}
	if len(data) != 4+32*5+32+64+2*(32+96) {
		t.Fatalf("total length %d", len(data))
	}
}

func TestReleaseDigestSignRecover(t *testing.T) {
	key, _ := secp256k1.GeneratePrivateKey()
	var vault, token, to [20]byte
	vault[0], token[0], to[0] = 1, 3, 2
	var burn [32]byte
	burn[31] = 9
	digest := ReleaseDigest(8453, vault, token, to, big.NewInt(5e8), burn)
	sig := SignDigest(key, digest)
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		t.Fatalf("signature shape v=%d", sig[64])
	}
	// Low-s as the contract requires.
	s := new(big.Int).SetBytes(sig[32:64])
	halfN := new(big.Int).Rsh(secp256k1.S256().N, 1)
	if s.Cmp(halfN) > 0 {
		t.Fatal("high-s signature")
	}
	addr, err := RecoverSigner(digest, sig)
	if err != nil || addr != AddressOf(key.PubKey()) {
		t.Fatal("recover")
	}
	// A different amount, token or chain gives a different digest, so the signature fails.
	other := ReleaseDigest(8453, vault, token, to, big.NewInt(6e8), burn)
	if a, _ := RecoverSigner(other, sig); a == AddressOf(key.PubKey()) {
		t.Fatal("signature valid for another amount")
	}
	if a, _ := RecoverSigner(ReleaseDigest(8453, vault, to, to, big.NewInt(5e8), burn), sig); a == AddressOf(key.PubKey()) {
		t.Fatal("signature valid for another token")
	}
	if a, _ := RecoverSigner(ReleaseDigest(1, vault, token, to, big.NewInt(5e8), burn), sig); a == AddressOf(key.PubKey()) {
		t.Fatal("signature valid for another chain")
	}
}

func TestAddressOfKnownKey(t *testing.T) {
	// Private key 1 -> the well-known address 0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf.
	k := secp256k1.PrivKeyFromBytes([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	if HexAddress(AddressOf(k.PubKey())) != "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf" {
		t.Fatalf("address %s", HexAddress(AddressOf(k.PubKey())))
	}
	a, err := ParseAddress("0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf")
	if err != nil || HexAddress(a) != "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf" {
		t.Fatal("parse")
	}
	if _, err := ParseAddress("0x1234"); err == nil {
		t.Fatal("short address accepted")
	}
}

func TestDecodeDepositLog(t *testing.T) {
	var from, token [20]byte
	from[19], token[19] = 7, 9
	to := "rarg1qexampleaddress"
	// Deposited(id, token, from, amount, fee, to): amount is the net credited,
	// fee what the vault kept; the string follows at offset 96.
	data := append(wordUint(big.NewInt(247500000)), wordUint(big.NewInt(2500000))...)
	data = append(data, wordUint(big.NewInt(96))...)
	data = append(data, wordUint(big.NewInt(int64(len(to))))...)
	data = append(data, padRight([]byte(to))...)
	topics := []string{"0x" + hex.EncodeToString(topicDeposit[:]), "0x" + hex.EncodeToString(wordUint(big.NewInt(42))), "0x" + hex.EncodeToString(word(token[:])), "0x" + hex.EncodeToString(word(from[:]))}
	ev, err := DecodeDepositLog(topics, "0x"+hex.EncodeToString(data))
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != 42 || ev.From != from || ev.Token != token || ev.Amount.Int64() != 247500000 || ev.Fee.Int64() != 2500000 || ev.To != to {
		t.Fatalf("decoded %+v", ev)
	}
	if _, err := DecodeDepositLog(topics[:3], "0x"); err == nil {
		t.Fatal("short topics")
	}
	// The old layout (amount, offset, string) is not a Deposited log any more.
	old := append(wordUint(big.NewInt(250000000)), wordUint(big.NewInt(64))...)
	old = append(old, wordUint(big.NewInt(int64(len(to))))...)
	old = append(old, padRight([]byte(to))...)
	if ev, err := DecodeDepositLog(topics, "0x"+hex.EncodeToString(old)); err == nil && ev.To == to {
		t.Fatal("old layout decoded as new")
	}
	if topicDeposit != Keccak256([]byte("Deposited(uint256,address,address,uint256,uint256,string)")) {
		t.Fatal("Deposited topic")
	}
	if SelDeposit != Selector("deposit(address,uint256,string)") {
		t.Fatal("deposit selector")
	}
	if selDepositFeeBps != Selector("depositFeeBps()") || selReleaseFeeBps != Selector("releaseFeeBps()") || selObligations != Selector("obligations(address)") {
		t.Fatal("fee view selectors")
	}
}

func TestDecodeReleasedLog(t *testing.T) {
	var token, to [20]byte
	token[19], to[19] = 9, 8
	var burn [32]byte
	burn[0], burn[31] = 0xAB, 0xCD
	data := append(wordUint(big.NewInt(50_000_000)), wordUint(big.NewInt(500_000))...)
	topics := []string{"0x" + hex.EncodeToString(topicReleased[:]), "0x" + hex.EncodeToString(burn[:]), "0x" + hex.EncodeToString(word(token[:])), "0x" + hex.EncodeToString(word(to[:]))}
	ev, err := DecodeReleasedLog(topics, "0x"+hex.EncodeToString(data))
	if err != nil {
		t.Fatal(err)
	}
	if ev.BurnID != burn || ev.Token != token || ev.To != to || ev.Amount.Int64() != 50_000_000 || ev.Fee.Int64() != 500_000 {
		t.Fatalf("decoded %+v", ev)
	}
	if _, err := DecodeReleasedLog(topics, "0x"+hex.EncodeToString(data[:32])); err == nil {
		t.Fatal("short data")
	}
	topics[0] = "0x" + hex.EncodeToString(topicDeposit[:])
	if _, err := DecodeReleasedLog(topics, "0x"+hex.EncodeToString(data)); err == nil {
		t.Fatal("wrong topic")
	}
	if topicReleased != Keccak256([]byte("Released(bytes32,address,address,uint256,uint256)")) {
		t.Fatal("Released topic")
	}
}

func TestFeeOf(t *testing.T) {
	// 1% of 100 USDC is 1 USDC; of 50 USDC, 0.5; rounding is down, so a fee
	// on 99 units at 1% is 0 and on 150 units is 1.
	cases := []struct {
		amount int64
		bps    uint16
		want   int64
	}{{100_000_000, 100, 1_000_000}, {50_000_000, 100, 500_000}, {99, 100, 0}, {150, 100, 1}, {100_000_000, 0, 0}, {0, 100, 0}, {12_345_678, 25, 30_864}, {1, 10000, 1}}
	for _, c := range cases {
		if got := FeeOf(big.NewInt(c.amount), c.bps); got.Int64() != c.want {
			t.Errorf("FeeOf(%d, %d) = %d, want %d", c.amount, c.bps, got.Int64(), c.want)
		}
	}
	if FeeOf(nil, 100).Sign() != 0 {
		t.Fatal("nil amount")
	}
	// 18 decimals: 1% of 2.123456789012345678 rounds down to 0.021234567890123456.
	v, _ := new(big.Int).SetString("2123456789012345678", 10)
	if got := FeeOf(v, 100); got.String() != "21234567890123456" {
		t.Fatalf("18-decimal fee %s", got)
	}
}

func TestTokenAmounts(t *testing.T) {
	cases := map[string]uint64{"1": 1_000_000, "0.5": 500_000, "12.345678": 12_345_678, "0": 0, "1000000": 1_000_000_000_000}
	for in, want := range cases {
		got, err := ParseTokenAmount(in, 6)
		if err != nil || got != want {
			t.Fatalf("%q: got %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1.2345678", "abc", "1.2.3"} {
		if _, err := ParseTokenAmount(bad, 6); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if FormatTokenAmount(12_345_678, 6) != "12.345678" || FormatTokenAmount(1_000_000, 6) != "1" || FormatTokenAmount(0, 6) != "0" || FormatTokenAmount(500_000, 6) != "0.5" {
		t.Fatal("format")
	}
}

func TestLegacyTxSigning(t *testing.T) {
	// Known vector from EIP-155: nonce 9, gasPrice 20 gwei, gas 21000, to 0x3535..., value 1e18, chain 1,
	// private key 0x4646...46 -> raw tx 0xf86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a764000080 25 a0 28ef... a0 67cb...
	keyBytes, _ := hex.DecodeString("4646464646464646464646464646464646464646464646464646464646464646")
	key := secp256k1.PrivKeyFromBytes(keyBytes)
	var to [20]byte
	for i := range to {
		to[i] = 0x35
	}
	raw := SignLegacyTx(key, 1, 9, big.NewInt(20_000_000_000), 21000, to, new(big.Int).SetUint64(1_000_000_000_000_000_000), nil)
	want := "f86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a764000080" +
		"25a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"
	if hex.EncodeToString(raw) != want {
		t.Fatalf("raw tx mismatch:\n got %s\nwant %s", hex.EncodeToString(raw), want)
	}
}

func TestRLP(t *testing.T) {
	if !bytes.Equal(rlpBytes([]byte{0x7f}), []byte{0x7f}) || !bytes.Equal(rlpBytes([]byte{0x80}), []byte{0x81, 0x80}) {
		t.Fatal("single byte")
	}
	if !bytes.Equal(rlpUint(big.NewInt(0)), []byte{0x80}) || !bytes.Equal(rlpUint(big.NewInt(1024)), []byte{0x82, 0x04, 0x00}) {
		t.Fatal("ints")
	}
	long := bytes.Repeat([]byte{'a'}, 56)
	enc := rlpBytes(long)
	if enc[0] != 0xb8 || enc[1] != 56 || len(enc) != 58 {
		t.Fatal("long string")
	}
	if !bytes.Equal(rlpList(rlpBytes([]byte("cat")), rlpBytes([]byte("dog"))), []byte{0xc8, 0x83, 'c', 'a', 't', 0x83, 'd', 'o', 'g'}) {
		t.Fatal("list")
	}
}
