// Package workclient implements the miner side of the solo work protocol. It
// depends only on the consensus package, so it also compiles to WebAssembly
// for the browser app.
//
// Work protocol (HTTP + JSON), deliberately simple so phones and browsers can
// implement it:
//
//	GET  /api/work?address=<payout address>[&ref=<referrer address>] -> Job
//	POST /api/work/submit {"job_id","nonce","address"}              -> Result
//
// With ref, the server builds the coinbase with two outputs: the miner's
// reward minus a fixed share, and that share to the referrer. The job says
// which share applies, so the miner can show it. There is no other source of
// referral rewards: the supply is fixed and nothing is created for them.
//
// A job is a complete block template whose coinbase pays the requesting
// address. The miner searches nonces until the Argon2id hash of the header
// is at or below the block target, then submits the nonce; the server
// assembles the block and hands it to the network. There are no shares and
// no intermediary: whoever solves the block gets the reward (less, from the
// treasury height, the treasury's 1% of the subsidy; consensus/treasury.go).
package workclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"argonmine/consensus"
)

// Job is one block template handed to a miner.
type Job struct {
	ID           string `json:"job_id"`
	HeaderPrefix string `json:"header_prefix"` // hex of the first 112 header bytes (all but the nonce)
	Target       string `json:"target"`        // 64 hex chars, big-endian block target
	Height       uint32 `json:"height"`
	Network      string `json:"network"`
	Algorithm    string `json:"algorithm"`
	Referrer     string `json:"referrer,omitempty"`     // address receiving the referral share, if any
	ReferralPct  int    `json:"referral_pct,omitempty"` // percent of the block reward paid to the referrer
}

// Submit is a miner's solution for a job.
type Submit struct {
	JobID   string `json:"job_id"`
	Nonce   uint64 `json:"nonce"`
	Address string `json:"address"`
}

// Result is the server's verdict.
type Result struct {
	Accepted bool   `json:"accepted"`
	Stale    bool   `json:"stale"`
	Hash     string `json:"hash,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// HeaderPrefixLen is the header size without the trailing 8-byte nonce.
const HeaderPrefixLen = consensus.HeaderSize - 8

// Client mines whole blocks against a work server. It needs no chain data.
type Client struct {
	url      string
	address  string
	referrer string
	threads  int
	hc       *http.Client
	logf     func(string, ...any)

	mu       sync.Mutex
	stop     context.CancelFunc
	wg       sync.WaitGroup
	running  atomic.Bool
	hashes   atomic.Uint64
	blocks   atomic.Uint64
	rejected atomic.Uint64
	lastErr  atomic.Value
	height   atomic.Uint32
	rate     atomic.Value // float64
}

// New creates a miner paying to address, fetching work from serverURL.
func New(serverURL, address string, threads int, logf func(string, ...any)) *Client {
	if threads < 1 {
		threads = 1
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &Client{url: serverURL, address: address, threads: threads, hc: &http.Client{Timeout: 30 * time.Second}, logf: logf}
	c.lastErr.Store("")
	c.rate.Store(float64(0))
	return c
}

// SetReferrer sets the address that receives the referral share of blocks
// this miner finds. Takes effect from the next job. Empty disables it.
func (c *Client) SetReferrer(addr string) {
	c.mu.Lock()
	c.referrer = strings.ToLower(strings.TrimSpace(addr))
	c.mu.Unlock()
}

// Referrer returns the configured referral address.
func (c *Client) Referrer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.referrer
}

func (c *Client) Running() bool     { return c.running.Load() }
func (c *Client) Hashes() uint64    { return c.hashes.Load() }
func (c *Client) Blocks() uint64    { return c.blocks.Load() }
func (c *Client) Rejected() uint64  { return c.rejected.Load() }
func (c *Client) Height() uint32    { return c.height.Load() }
func (c *Client) LastError() string { return c.lastErr.Load().(string) }
func (c *Client) Threads() int      { return c.threads }

// HashRate is the recent hashes per second.
func (c *Client) HashRate() float64 {
	if !c.running.Load() {
		return 0
	}
	return c.rate.Load().(float64)
}

// Start begins mining in the background.
func (c *Client) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running.Load() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.stop = cancel
	c.running.Store(true)
	c.wg.Add(2)
	go c.loop(ctx)
	go c.rateLoop(ctx)
}

// Stop ends mining and waits for the workers.
func (c *Client) Stop() {
	c.mu.Lock()
	if c.stop != nil {
		c.stop()
	}
	c.mu.Unlock()
	c.wg.Wait()
	c.running.Store(false)
	c.rate.Store(float64(0))
}

func (c *Client) rateLoop(ctx context.Context) {
	defer c.wg.Done()
	last, lastT := c.hashes.Load(), time.Now()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			h := c.hashes.Load()
			dt := now.Sub(lastT).Seconds()
			if dt > 0 {
				inst := float64(h-last) / dt
				prev := c.rate.Load().(float64)
				if prev == 0 {
					c.rate.Store(inst)
				} else {
					c.rate.Store(prev*0.7 + inst*0.3)
				}
			}
			last, lastT = h, now
		}
	}
}

func (c *Client) loop(ctx context.Context) {
	defer c.wg.Done()
	var last *Job
	for ctx.Err() == nil {
		job, err := c.fetchJob(ctx)
		if err != nil {
			c.logf("work: %v", err)
			c.lastErr.Store(friendlyError(err))
			if last != nil {
				// Keep working on the last job instead of standing idle: it
				// stays valid until the tip moves, and a solution for a job
				// that went stale is refused by the server, nothing worse.
				// Then ask again.
				jctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				c.mineJob(jctx, last)
				cancel()
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		c.lastErr.Store("")
		c.height.Store(job.Height)
		last = job
		// Refresh the template regularly so new transactions and new tips are
		// picked up; a solved job ends the round early.
		jctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		c.mineJob(jctx, job)
		cancel()
	}
}

func (c *Client) fetchJob(ctx context.Context) (*Job, error) {
	q := "/api/work?address=" + url.QueryEscape(c.address)
	if r := c.Referrer(); r != "" {
		q += "&ref=" + url.QueryEscape(r)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("work: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (c *Client) submit(ctx context.Context, s Submit) (*Result, error) {
	body, _ := json.Marshal(s)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/work/submit", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("work: bad response: %s", bytes.TrimSpace(data))
	}
	return &r, nil
}

// friendlyError turns transport failures into a sentence a miner can act on;
// server verdicts are passed through unchanged.
func friendlyError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "work: ") && !strings.Contains(msg, "fetch") && !strings.Contains(msg, "dial") && !strings.Contains(msg, "connection") {
		return msg
	}
	return "The work server is not reachable right now. Retrying."
}

// mineJob searches nonces for one job until ctx ends or a block is found.
func (c *Client) mineJob(ctx context.Context, job *Job) {
	prefix, err := hex.DecodeString(job.HeaderPrefix)
	if err != nil || len(prefix) != HeaderPrefixLen {
		c.lastErr.Store("work: bad header prefix")
		time.Sleep(time.Second)
		return
	}
	target, ok := new(big.Int).SetString(job.Target, 16)
	if !ok || target.Sign() <= 0 {
		c.lastErr.Store("work: bad target")
		time.Sleep(time.Second)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var start [8]byte
	rand.Read(start[:])
	base := binary.LittleEndian.Uint64(start[:])
	var wg sync.WaitGroup
	for i := 0; i < c.threads; i++ {
		wg.Add(1)
		go func(offset uint64) {
			defer wg.Done()
			hdr := make([]byte, consensus.HeaderSize)
			copy(hdr, prefix)
			for n, i := base+offset, 0; ctx.Err() == nil; n, i = n+uint64(c.threads), i+1 {
				if i%yieldEvery == yieldEvery-1 {
					yield()
				}
				binary.LittleEndian.PutUint64(hdr[HeaderPrefixLen:], n)
				pow := consensus.HashToBig(consensus.PowHash(hdr))
				c.hashes.Add(1)
				if pow.Cmp(target) > 0 {
					continue
				}
				res, err := c.submit(ctx, Submit{JobID: job.ID, Nonce: n, Address: c.address})
				if err != nil {
					if ctx.Err() == nil {
						c.lastErr.Store(err.Error())
					}
					continue
				}
				switch {
				case res.Accepted:
					c.blocks.Add(1)
					c.logf("work: block %s found at height %d", res.Hash, job.Height)
					cancel()
				case res.Stale:
					cancel()
				default:
					c.rejected.Add(1)
					c.lastErr.Store("block rejected: " + res.Reason)
					cancel()
				}
			}
		}(uint64(i))
	}
	wg.Wait()
}
