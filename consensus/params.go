package consensus

import "math/big"

// Params describes one network (SPEC §11). Every field is consensus-critical
// except the ports.
type Params struct {
	Name    string
	HRP     string // bech32m human-readable part
	Magic   [4]byte
	P2PPort uint16
	RPCPort uint16

	PowLimitBits uint32   // easiest allowed compact target
	PowLimit     *big.Int // decoded PowLimitBits
	// ConstantDifficulty makes every block use PowLimit (regtest only).
	ConstantDifficulty bool

	// Genesis block, fully specified. GenesisTime and AnchorTarget are the
	// ASERT anchor; ChainTag (= genesis hash) separates signature domains.
	Genesis      *Block
	GenesisHash  Hash
	GenesisTime  uint64
	AnchorTarget *big.Int
	ChainTag     Hash

	// Treasury (treasury.go): from TreasuryHeight on, every coinbase pays
	// TreasuryBps of the block subsidy to TreasuryKey (a P2PK x-only key).
	// TreasuryHeight 0: no treasury on this network.
	TreasuryHeight uint32
	TreasuryKey    []byte
}

// genesisSpec holds the mined values for a network's genesis block.
type genesisSpec struct {
	time    uint64
	bits    uint32
	nonce   uint64
	message string
}

// BuildGenesisBlock constructs the deterministic genesis block: a single
// coinbase with lock_height 0, an empty miner tag, and one NULLDATA output
// carrying the message. Its coinbase never enters the UTXO set.
func BuildGenesisBlock(time uint64, bits uint32, nonce uint64, message string) *Block {
	cb := &Tx{
		Version:    TxVersion,
		LockHeight: 0,
		Inputs:     []TxIn{{PrevOut: NullOutPoint}},
		Outputs:    []TxOut{{Amount: 0, Type: OutputTypeNullData, Payload: []byte(message)}},
		Witnesses:  [][]byte{{}},
	}
	blk := &Block{Txs: []*Tx{cb}}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		panic(err)
	}
	blk.Header = Header{
		Version:     HeaderVersion,
		PrevHash:    Hash{},
		TxRoot:      txRoot,
		WitnessRoot: wRoot,
		Time:        time,
		Bits:        bits,
		Nonce:       nonce,
	}
	return blk
}

func newParams(name, hrp string, magic [4]byte, p2p, rpc uint16, powLimitBits uint32, constant bool, g genesisSpec) *Params {
	limit, err := CompactToTarget(powLimitBits)
	if err != nil {
		panic("consensus: bad PowLimitBits for " + name + ": " + err.Error())
	}
	anchor, err := CompactToTarget(g.bits)
	if err != nil {
		panic("consensus: bad genesis bits for " + name + ": " + err.Error())
	}
	if anchor.Cmp(limit) > 0 {
		panic("consensus: genesis target easier than the proof-of-work limit for " + name)
	}
	blk := BuildGenesisBlock(g.time, g.bits, g.nonce, g.message)
	h := blk.Header.Hash()
	return &Params{
		Name:               name,
		HRP:                hrp,
		Magic:              magic,
		P2PPort:            p2p,
		RPCPort:            rpc,
		PowLimitBits:       powLimitBits,
		PowLimit:           limit,
		ConstantDifficulty: constant,
		Genesis:            blk,
		GenesisHash:        h,
		GenesisTime:        g.time,
		AnchorTarget:       anchor,
		ChainTag:           h,
	}
}

// GenesisMessage is embedded in every network's genesis NULLDATA output.
const GenesisMessage = "Argonmine genesis: every coin is mined, none are given."

// The nonce values below are produced by cmd/genesis and verified by
// TestGenesisProofOfWork. A network whose nonce is still 0 has not been mined.
var (
	// Each network has its own genesis time, so setting mainnet's for the
	// launch (docs/LAUNCH.md) cannot change the live testnet's genesis.
	// Because ASERT's schedule is anchored at genesis, mainnet's final
	// genesis time MUST be the actual launch moment: a genesis in the past
	// keeps difficulty at the minimum until the chain "catches up"; one in
	// the future blocks all mining. Mainnet launched at 2026-10-28 12:00 UTC; the
	// testnet and regtest stay at 2026-10-25 00:00 UTC.
	mainnetGenesisTime uint64 = 1_793_188_800 // 2026-10-28 12:00:00 UTC, the launch moment
	testnetGenesisTime uint64 = 1_792_886_400 // fixed: the public testnet's genesis
	regtestGenesisTime uint64 = 1_792_886_400 // fixed: the regtest genesis

	// Mainnet's easiest target is 2^238: an expected 2^18 (262,144) Argon2id
	// evaluations per block, so a side-chain header costs an attacker real
	// work and the first blocks after launch cannot be minted by one core in
	// seconds. A small honest network (~1 kH/s) still finds blocks in minutes.
	MainNet = newParams("mainnet", "arg", [4]byte{0x41, 0x52, 0x47, 0x4D}, 9333, 9332,
		0x1E400000, false,
		genesisSpec{time: mainnetGenesisTime, bits: 0x1E400000, nonce: 77492, message: GenesisMessage}).
		withTreasury(mainnetTreasuryHeight, mainnetTreasuryKey)
	// mainnet genesis hash e378ed192099aa7693a063071926aff1415165557b9e1edebfdbbe65a1cac8f6

	TestNet = newParams("testnet", "targ", [4]byte{0x54, 0x41, 0x52, 0x47}, 19333, 19332,
		0x1F7FFFFF, false,
		genesisSpec{time: testnetGenesisTime, bits: 0x1F7FFFFF, nonce: 180, message: GenesisMessage})
	// testnet genesis hash 81fa0f3bcdf66c3de91ce8d6676b747584354141fff3ec88d06f0208217cff32

	RegTest = newParams("regtest", "rarg", [4]byte{0x52, 0x41, 0x52, 0x47}, 29333, 29332,
		0x207FFFFF, true,
		genesisSpec{time: regtestGenesisTime, bits: 0x207FFFFF, nonce: 6, message: GenesisMessage})
	// regtest genesis hash b3682483241531d67d67cf78f11462e8c4be637da746cd1ffbe60636d197959d
)
