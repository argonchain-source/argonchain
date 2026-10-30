// Package miner builds block templates and searches for proof of work.
// It is the engine behind the mining app and the node's -mine flag.
package miner

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"argonmine/chain"
	"argonmine/consensus"
	"argonmine/mempool"
)

const (
	// templateRefresh bounds how long a template is mined before rebuilding
	// (picks up new transactions and advances the timestamp).
	templateRefresh = 30 * time.Second
)

var ErrNoPayout = errors.New("miner: no payout output configured")

// Miner mines continuously on the chain tip.
type Miner struct {
	p         *consensus.Params
	ch        *chain.Chain
	pool      *mempool.Pool
	payout    consensus.TxOut
	tag       []byte
	threads   int
	logf      func(string, ...any)
	broadcast func(*consensus.Block)

	mu        sync.Mutex
	cancelCur context.CancelFunc
	stop      context.CancelFunc
	wg        sync.WaitGroup
	running   atomic.Bool
	hashes    atomic.Uint64
	found     atomic.Uint64
	startedAt time.Time

	rateMu     sync.Mutex
	rateT      time.Time
	rateH      uint64
	rateWindow float64
}

// New creates a miner paying to payout. threads <= 0 means all CPUs but one.
func New(ch *chain.Chain, pool *mempool.Pool, payout consensus.TxOut, threads int, logf func(string, ...any)) *Miner {
	if threads <= 0 {
		threads = runtime.NumCPU() - 1
		if threads < 1 {
			threads = 1
		}
	}
	if logf == nil {
		logf = log.Printf
	}
	m := &Miner{p: ch.Params(), ch: ch, pool: pool, payout: payout, threads: threads, logf: logf, tag: []byte("argonmine-miner")}
	ch.Subscribe(func(*chain.Node) { m.restartTemplate() })
	return m
}

// SetBroadcast sets the hook called with every block this miner finds and
// the chain accepts (the node passes p2p announcement here).
func (m *Miner) SetBroadcast(fn func(*consensus.Block)) { m.broadcast = fn }

// SetTag sets the coinbase miner tag (<= 100 bytes).
func (m *Miner) SetTag(tag []byte) {
	if len(tag) > consensus.MaxCoinbaseTagLen {
		tag = tag[:consensus.MaxCoinbaseTagLen]
	}
	m.tag = append([]byte{}, tag...)
}

// SetThreads changes the thread count for the next template.
func (m *Miner) SetThreads(n int) {
	if n < 1 {
		n = 1
	}
	m.mu.Lock()
	m.threads = n
	m.mu.Unlock()
	m.restartTemplate()
}

// Threads returns the configured thread count.
func (m *Miner) Threads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.threads
}

// Running reports whether the mining loop is active.
func (m *Miner) Running() bool { return m.running.Load() }

// Hashes returns the total hash attempts since Start.
func (m *Miner) Hashes() uint64 { return m.hashes.Load() }

// BlocksFound returns the number of blocks accepted since Start.
func (m *Miner) BlocksFound() uint64 { return m.found.Load() }

// HashRate returns the recent hash rate in hashes per second.
func (m *Miner) HashRate() float64 {
	m.rateMu.Lock()
	defer m.rateMu.Unlock()
	return m.rateWindow
}

func (m *Miner) restartTemplate() {
	m.mu.Lock()
	if m.cancelCur != nil {
		m.cancelCur()
	}
	m.mu.Unlock()
}

// Start begins mining in the background.
func (m *Miner) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running.Load() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.stop = cancel
	m.running.Store(true)
	m.startedAt = time.Now()
	m.hashes.Store(0)
	m.rateMu.Lock()
	m.rateT, m.rateH, m.rateWindow = time.Now(), 0, 0
	m.rateMu.Unlock()
	m.wg.Add(2)
	go m.loop(ctx)
	go m.rateLoop(ctx)
	m.logf("miner: started with %d threads", m.threads)
}

// Stop halts mining and waits for workers to exit.
func (m *Miner) Stop() {
	m.mu.Lock()
	if !m.running.Load() {
		m.mu.Unlock()
		return
	}
	m.stop()
	if m.cancelCur != nil {
		m.cancelCur()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.running.Store(false)
	m.logf("miner: stopped")
}

func (m *Miner) rateLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			h := m.hashes.Load()
			m.rateMu.Lock()
			dt := now.Sub(m.rateT).Seconds()
			if dt > 0 {
				inst := float64(h-m.rateH) / dt
				if m.rateWindow == 0 {
					m.rateWindow = inst
				} else {
					m.rateWindow = 0.7*m.rateWindow + 0.3*inst
				}
			}
			m.rateT, m.rateH = now, h
			m.rateMu.Unlock()
		}
	}
}

func (m *Miner) loop(ctx context.Context) {
	defer m.wg.Done()
	for ctx.Err() == nil {
		tctx, cancel := context.WithTimeout(ctx, templateRefresh)
		m.mu.Lock()
		m.cancelCur = cancel
		threads := m.threads
		m.mu.Unlock()

		blk, err := BuildTemplate(m.ch, m.pool, m.payout, m.tag, time.Now().Unix())
		if err != nil {
			m.logf("miner: template: %v", err)
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if Solve(tctx, m.p, blk, threads, &m.hashes) {
			if _, err := m.ch.ProcessBlock(blk, time.Now().Unix()); err != nil {
				if !errors.Is(err, chain.ErrDuplicateBlock) {
					m.logf("miner: block rejected: %v", err)
				}
			} else {
				m.found.Add(1)
				m.logf("miner: found block %s at height %d (%d txs)", blk.Header.Hash(), blk.Txs[0].LockHeight, len(blk.Txs))
				if m.broadcast != nil {
					m.broadcast(blk)
				}
			}
		}
		cancel()
	}
}

// BuildTemplate assembles a full block on the current tip: coinbase paying
// subsidy + fees to payout (less the treasury's share, which it pays the
// treasury), pool transactions by fee rate, correct roots,
// bits and timestamp. Only the nonce remains to be found.
func BuildTemplate(ch *chain.Chain, pool *mempool.Pool, payout consensus.TxOut, tag []byte, now int64) (*consensus.Block, error) {
	if payout.Type != consensus.OutputTypeP2PK || len(payout.Payload) != consensus.P2PKPayloadLen {
		return nil, ErrNoPayout
	}
	p := ch.Params()
	tip := ch.Tip()
	height := tip.Height + 1

	cb := &consensus.Tx{
		Version:    consensus.TxVersion,
		LockHeight: height,
		Inputs:     []consensus.TxIn{{PrevOut: consensus.NullOutPoint}},
		Outputs:    []consensus.TxOut{{Amount: 0, Type: payout.Type, Payload: append([]byte{}, payout.Payload...)}},
		Witnesses:  [][]byte{append([]byte{}, tag...)},
	}
	// From the network's treasury height, 1% of the subsidy goes to the
	// treasury (consensus/treasury.go); the miner keeps the rest and the fees.
	treasury, payTreasury := consensus.TreasuryOutput(p, height)
	if payTreasury {
		cb.Outputs = append(cb.Outputs, treasury)
	}
	// Reserve: header + count varint + coinbase.
	budget := consensus.MaxBlockSize - consensus.HeaderSize - 9 - cb.SerializedSize()
	txs, fees := pool.Select(budget)
	value, err := consensus.AddAmounts(consensus.Subsidy(height), fees)
	if err != nil {
		return nil, err
	}
	if payTreasury {
		value -= treasury.Amount
	}
	cb.Outputs[0].Amount = value

	blk := &consensus.Block{Txs: append([]*consensus.Tx{cb}, txs...)}
	txRoot, wRoot, err := blk.ComputeRoots()
	if err != nil {
		return nil, err
	}
	t := uint64(now)
	if mtp := chain.MedianTimePast(tip); t <= mtp {
		t = mtp + 1
	}
	blk.Header = consensus.Header{
		Version:     consensus.HeaderVersion,
		PrevHash:    tip.Hash,
		TxRoot:      txRoot,
		WitnessRoot: wRoot,
		Time:        t,
		Bits:        consensus.NextBits(p, tip.Header.Time, tip.Height),
	}
	return blk, nil
}

// Solve searches for a nonce satisfying the header's target using the given
// number of threads. It returns true (with blk.Header.Nonce set) on success
// or false when ctx is cancelled. counter, if non-nil, is incremented per
// attempt.
func Solve(ctx context.Context, p *consensus.Params, blk *consensus.Block, threads int, counter *atomic.Uint64) bool {
	target, err := consensus.CompactToTarget(blk.Header.Bits)
	if err != nil || target.Cmp(p.PowLimit) > 0 {
		return false
	}
	if threads < 1 {
		threads = 1
	}
	var start [8]byte
	rand.Read(start[:])
	base := binary.LittleEndian.Uint64(start[:])

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan uint64, 1)
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(offset uint64) {
			defer wg.Done()
			hdr := blk.Header
			for n := base + offset; ctx.Err() == nil; n += uint64(threads) {
				hdr.Nonce = n
				pow := consensus.HashToBig(consensus.PowHash(hdr.Serialize()))
				if counter != nil {
					counter.Add(1)
				}
				if pow.Cmp(target) <= 0 {
					select {
					case result <- n:
					default:
					}
					cancel()
					return
				}
			}
		}(uint64(i))
	}
	wg.Wait()
	select {
	case n := <-result:
		blk.Header.Nonce = n
		return true
	default:
		return false
	}
}

// MineOne builds a template and mines until a block is found and accepted,
// or ctx ends. Used by regtest's generatetoaddress.
func MineOne(ctx context.Context, ch *chain.Chain, pool *mempool.Pool, payout consensus.TxOut, threads int) (*consensus.Block, error) {
	for ctx.Err() == nil {
		blk, err := BuildTemplate(ch, pool, payout, []byte("generate"), time.Now().Unix())
		if err != nil {
			return nil, err
		}
		if !Solve(ctx, ch.Params(), blk, threads, nil) {
			break
		}
		if _, err := ch.ProcessBlock(blk, time.Now().Unix()); err != nil {
			if errors.Is(err, chain.ErrDuplicateBlock) {
				continue
			}
			return nil, err
		}
		return blk, nil
	}
	return nil, ctx.Err()
}
