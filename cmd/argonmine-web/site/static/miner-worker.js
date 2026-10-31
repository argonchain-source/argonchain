// Runs the WebAssembly miner in a Web Worker so the page never blocks.
// Messages in: {id, type: start|stop|stats|newWallet|addressOf|wallet, ...args}.
// Messages out: {type: 'ready'} once, {type: 'error', message} on failure,
// {type: 'refused', message} when the work server sent a job this miner
// will not mine (see below), and {id, result} or {id, error} for every request.
'use strict';

// ---- job check ----
// The miner (Go, in WebAssembly) fetches its jobs with fetch(). Every job
// passes through checkJob first: it is mined only if its coinbase pays this
// miner's address (and the referrer the miner chose, at most the share the
// page showed), and hashing that coinbase up the job's merkle branches gives
// the roots inside the header the miner will hash. Otherwise the miner gets
// an error instead of the job and the page is told why. The hashing follows
// the consensus code (consensus/hash.go, merkle.go, tx.go): tagged SHA-256,
// txid over the coinbase without its witness, wtxid over all of it.

var K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2]);

// sha256 of a Uint8Array (small inputs only: a coinbase, a few hashes).
function sha256(msg) {
  var H = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
  var len = msg.length, total = ((len + 9 + 63) >> 6) << 6, buf = new Uint8Array(total);
  buf.set(msg); buf[len] = 0x80;
  var dv = new DataView(buf.buffer), bits = len * 8;
  dv.setUint32(total - 8, Math.floor(bits / 4294967296)); dv.setUint32(total - 4, bits >>> 0);
  var W = new Uint32Array(64), i, a, b, c, d, e, f, g, h, t1, t2;
  for (var off = 0; off < total; off += 64) {
    for (i = 0; i < 16; i++) W[i] = dv.getUint32(off + 4 * i);
    for (i = 16; i < 64; i++) {
      var x = W[i - 15], y = W[i - 2];
      W[i] = (W[i - 16] + (((x >>> 7) | (x << 25)) ^ ((x >>> 18) | (x << 14)) ^ (x >>> 3)) + W[i - 7] + (((y >>> 17) | (y << 15)) ^ ((y >>> 19) | (y << 13)) ^ (y >>> 10))) | 0;
    }
    a = H[0]; b = H[1]; c = H[2]; d = H[3]; e = H[4]; f = H[5]; g = H[6]; h = H[7];
    for (i = 0; i < 64; i++) {
      t1 = (h + (((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7))) + ((e & f) ^ (~e & g)) + K[i] + W[i]) | 0;
      t2 = ((((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10))) + ((a & b) ^ (a & c) ^ (b & c))) | 0;
      h = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
    }
    H[0] += a; H[1] += b; H[2] += c; H[3] += d; H[4] += e; H[5] += f; H[6] += g; H[7] += h;
  }
  var out = new Uint8Array(32), ov = new DataView(out.buffer);
  for (i = 0; i < 8; i++) ov.setUint32(4 * i, H[i]);
  return out;
}

function concat(parts) {
  var n = 0, i; for (i = 0; i < parts.length; i++) n += parts[i].length;
  var out = new Uint8Array(n), o = 0;
  for (i = 0; i < parts.length; i++) { out.set(parts[i], o); o += parts[i].length; }
  return out;
}
var enc = new TextEncoder();
// taggedHash = SHA256(SHA256(tag) || SHA256(tag) || data...), as in BIP-340.
function taggedHash(tag) {
  var th = sha256(enc.encode(tag));
  return sha256(concat([th, th].concat(Array.prototype.slice.call(arguments, 1))));
}
function fromHex(s, n) {
  s = String(s == null ? '' : s);
  if (!/^(?:[0-9a-f]{2})*$/.test(s) || (n !== undefined && s.length !== 2 * n)) throw new Error('malformed hex');
  var out = new Uint8Array(s.length / 2);
  for (var i = 0; i < out.length; i++) out[i] = parseInt(s.substr(2 * i, 2), 16);
  return out;
}
function toHex(b) { var s = ''; for (var i = 0; i < b.length; i++) s += (b[i] < 16 ? '0' : '') + b[i].toString(16); return s; }
function same(a, b) { if (a.length !== b.length) return false; for (var i = 0; i < a.length; i++) if (a[i] !== b[i]) return false; return true; }

// ---- addresses: bech32m, data = [output type] || base32(payload) (wallet/address.go) ----
var CHARSET = 'qpzry9x8gf2tvdw0s3jn54khce6mua7l', BECH32M = 0x2bc830a3;
function polymod(v) {
  var G = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3], chk = 1;
  for (var i = 0; i < v.length; i++) {
    var top = chk >>> 25; chk = ((chk & 0x1ffffff) << 5) ^ v[i];
    for (var j = 0; j < 5; j++) if ((top >>> j) & 1) chk ^= G[j];
  }
  return chk >>> 0;
}
function hrpExpand(h) { var out = [], i; for (i = 0; i < h.length; i++) out.push(h.charCodeAt(i) >> 5); out.push(0); for (i = 0; i < h.length; i++) out.push(h.charCodeAt(i) & 31); return out; }
function convertBits(data, from, to, pad) {
  var acc = 0, bits = 0, out = [], maxv = (1 << to) - 1;
  for (var i = 0; i < data.length; i++) {
    acc = (acc << from) | data[i]; bits += from;
    while (bits >= to) { bits -= to; out.push((acc >>> bits) & maxv); }
    acc &= (1 << bits) - 1;
  }
  if (pad) { if (bits > 0) out.push((acc << (to - bits)) & maxv); }
  else if (bits >= from || ((acc << (to - bits)) & maxv)) return null;
  return out;
}
// decodeAddress returns {hrp, type, payload} or throws.
function decodeAddress(addr) {
  addr = String(addr || '').toLowerCase();
  var sep = addr.lastIndexOf('1');
  if (addr.length < 8 || addr.length > 90 || sep < 1 || sep + 7 > addr.length) throw new Error('malformed address');
  var hrp = addr.slice(0, sep), data = [];
  for (var i = sep + 1; i < addr.length; i++) { var d = CHARSET.indexOf(addr[i]); if (d < 0) throw new Error('malformed address'); data.push(d); }
  if (polymod(hrpExpand(hrp).concat(data)) !== BECH32M) throw new Error('bad address checksum');
  data = data.slice(0, -6);
  var payload = convertBits(data.slice(1), 5, 8, false);
  if (!payload || data[0] !== 0 || payload.length !== 32) throw new Error('unsupported address');
  return { hrp: hrp, type: data[0], payload: new Uint8Array(payload) };
}
function encodeAddress(hrp, type, payload) {
  var data = [type].concat(convertBits(Array.prototype.slice.call(payload), 8, 5, true));
  var mod = polymod(hrpExpand(hrp).concat(data, [0, 0, 0, 0, 0, 0])) ^ BECH32M;
  for (var i = 0; i < 6; i++) data.push((mod >>> (5 * (5 - i))) & 31);
  return hrp + '1' + data.map(function (d) { return CHARSET[d]; }).join('');
}

// ---- transactions (consensus/encoding.go, tx.go) ----
function Reader(b) { this.b = b; this.o = 0; }
Reader.prototype.need = function (n) { if (this.o + n > this.b.length) throw new Error('truncated coinbase'); };
Reader.prototype.u8 = function () { this.need(1); return this.b[this.o++]; };
Reader.prototype.u32 = function () { this.need(4); var v = new DataView(this.b.buffer, this.b.byteOffset + this.o, 4).getUint32(0, true); this.o += 4; return v; };
Reader.prototype.i64 = function () { this.need(8); var v = new DataView(this.b.buffer, this.b.byteOffset + this.o, 8).getBigInt64(0, true); this.o += 8; return v; };
Reader.prototype.bytes = function (n) { this.need(n); var v = this.b.subarray(this.o, this.o + n); this.o += n; return v; };
Reader.prototype.varint = function () {
  var v = 0, shift = 1;
  for (var i = 0; i < 9; i++) {
    var b = this.u8(); v += (b & 0x7f) * shift; shift *= 128;
    if (!(b & 0x80)) { if (i > 0 && b === 0) throw new Error('non-minimal varint'); return v; }
  }
  throw new Error('varint too long');
};

// parseCoinbase decodes a version-1 coinbase: one null input, its outputs,
// one witness (the tag). noWitness is where the witness starts.
function parseCoinbase(b) {
  var r = new Reader(b), tx = { outputs: [] };
  tx.version = r.u32(); tx.lockHeight = r.u32();
  if (tx.version !== 1) throw new Error('coinbase version ' + tx.version);
  if (r.varint() !== 1) throw new Error('the coinbase must have one input');
  var prev = r.bytes(32), idx = r.u32();
  for (var i = 0; i < 32; i++) if (prev[i] !== 0) throw new Error('not a coinbase input');
  if (idx !== 0xffffffff) throw new Error('not a coinbase input');
  var n = r.varint();
  if (n < 1 || n > 3) throw new Error('the coinbase has ' + n + ' outputs');
  for (i = 0; i < n; i++) {
    var amount = r.i64(), type = r.u8(), plen = r.varint();
    if (plen > 80) throw new Error('output payload too long');
    tx.outputs.push({ amount: amount, type: type, payload: r.bytes(plen) });
  }
  tx.noWitness = r.o;
  var wlen = r.varint();
  if (wlen > 100) throw new Error('coinbase tag too long');
  r.bytes(wlen);
  if (r.o !== b.length) throw new Error('trailing bytes after the coinbase');
  return tx;
}

function fold(id, branch) {
  var acc = taggedHash('Argonmine/merkle/leaf', id);
  if (!Array.isArray(branch) || branch.length > 32) throw new Error('bad merkle branch');
  for (var i = 0; i < branch.length; i++) acc = taggedHash('Argonmine/merkle/node', acc, fromHex(branch[i], 32));
  return acc;
}

// compactTarget decodes header bits (consensus/compact.go) as a BigInt.
function compactTarget(bits) {
  var exp = bits >>> 24, mant = BigInt(bits & 0x007fffff);
  return exp <= 3 ? mant >> BigInt(8 * (3 - exp)) : mant << BigInt(8 * (exp - 3));
}

// The treasury (consensus/treasury.go, TreasuryBps and the network's
// TreasuryHeight and TreasuryKey): from height, the coinbase's last output
// pays exactly bps/10000 of the block subsidy to key. A Go test keeps these
// values equal to the consensus ones.
var TREASURY = { arg: { height: 300, key: '7ad1a30d482a3fdb23efd2de4da69de2e9dc50d204d8cd82495183e6b3ffdf6c', bps: 100 } };
function subsidyAt(h) { var halvings = Math.floor(h / 210000); return halvings >= 64 ? 0n : 5000000000n >> BigInt(halvings); }
function treasuryDue(hrp, h) { var t = TREASURY[hrp]; return t && h >= t.height ? subsidyAt(h) * BigInt(t.bps) / 10000n : 0n; }

// checkJob returns '' when the job pays exp.payload (and, with a referral,
// exp.refPayload at most exp.refPct percent, and from the treasury height
// exactly the treasury's share to the treasury), or the reason it does not.
// exp: {hrp, payload, refPayload|null, refPct, pool}. For a pool job (exp.pool)
// the payload is the pool's address the page showed, no referral output is
// allowed, and the job's target is the share target: block_target must be
// the header's and the share target no harder than it.
function checkJob(job, exp) {
  try {
    if (!job || typeof job !== 'object') return 'the job is not readable';
    if (!job.coinbase || !Array.isArray(job.merkle_branch) || !Array.isArray(job.witness_branch)) return 'the job carries no proof of its coinbase (the work server is outdated or not genuine)';
    var hdr = fromHex(job.header_prefix, 112), raw = fromHex(job.coinbase), cb = parseCoinbase(raw), outs = cb.outputs.slice(), out0 = outs[0];
    if (cb.lockHeight !== Number(job.height)) return 'the coinbase is for height ' + cb.lockHeight + ', the job for ' + job.height;
    var due = treasuryDue(exp.hrp, cb.lockHeight);
    if (due > 0n) {
      var tre = outs[outs.length - 1];
      if (outs.length < 2 || tre.type !== 0 || !same(tre.payload, fromHex(TREASURY[exp.hrp].key, 32)) || tre.amount !== due)
        return 'the coinbase does not pay the treasury exactly its 1% of the block subsidy';
      outs.pop();
    }
    if (outs.length > 2) return 'the coinbase has more outputs than you, a referrer and the treasury';
    if (out0.type !== 0 || out0.payload.length !== 32 || !same(out0.payload, exp.payload))
      return 'the block reward goes to ' + (out0.type === 0 && out0.payload.length === 32 ? encodeAddress(exp.hrp, 0, out0.payload) : 'an output that is not an address') + (exp.pool ? ', not to the pool address shown' : ', not to your address');
    if (out0.amount < 0n) return 'the block reward is negative';
    if (exp.pool && outs.length !== 1) return 'part of the block reward goes to another output than the pool and the treasury';
    if (outs.length === 2) {
      var out1 = outs[1], total = out0.amount + out1.amount;
      if (!exp.refPayload) return 'part of the block reward goes to ' + (out1.type === 0 && out1.payload.length === 32 ? encodeAddress(exp.hrp, 0, out1.payload) : 'another output') + ', and you chose no referrer';
      if (out1.type !== 0 || !same(out1.payload, exp.refPayload)) return 'the referral share goes to ' + (out1.type === 0 && out1.payload.length === 32 ? encodeAddress(exp.hrp, 0, out1.payload) : 'another output') + ', not to the referrer you accepted';
      if (out1.amount < 0n || out1.amount * 100n > total * BigInt(exp.refPct)) return 'the referral share is more than the ' + exp.refPct + '% you accepted';
    }
    var txid = taggedHash('Argonmine/txid', raw.subarray(0, cb.noWitness)), wtxid = taggedHash('Argonmine/wtxid', raw);
    if (!same(fold(txid, job.merkle_branch), hdr.subarray(36, 68))) return 'the block header does not commit to this coinbase (transaction root)';
    if (!same(fold(wtxid, job.witness_branch), hdr.subarray(68, 100))) return 'the block header does not commit to this coinbase (witness root)';
    var bits = new DataView(hdr.buffer, hdr.byteOffset + 108, 4).getUint32(0, true);
    if (!/^[0-9a-f]{64}$/.test(String(job.target))) return 'the job\'s target is malformed';
    if (exp.pool) {
      if (!/^[0-9a-f]{64}$/.test(String(job.block_target)) || BigInt('0x' + job.block_target) !== compactTarget(bits)) return 'the job\'s block target does not match its header';
      if (BigInt('0x' + job.target) < BigInt('0x' + job.block_target)) return 'the share target is harder than the block target';
    } else if (BigInt('0x' + job.target) !== compactTarget(bits)) return 'the job\'s target does not match its header';
    return '';
  } catch (e) {
    return 'the job is malformed (' + (e && e.message || e) + ')';
  }
}

// jobFields is a checked job with only the fields the Go miner reads.
function jobFields(job) {
  var out = { job_id: String(job.job_id), header_prefix: String(job.header_prefix), target: String(job.target), height: Number(job.height), network: String(job.network || ''), algorithm: String(job.algorithm || '') };
  if (job.referrer) out.referrer = String(job.referrer);
  if (job.referral_pct) out.referral_pct = Number(job.referral_pct);
  if (job.block_target) out.block_target = String(job.block_target);
  return out;
}

// ---- fetch: jobs are checked, busy answers to submissions are retried ----
var expect = null, refused = '';
var realFetch = self.fetch ? self.fetch.bind(self) : null;
function urlOf(input) { return String(typeof input === 'string' ? input : (input && input.url) || ''); }
function isJobURL(u) { return /\/api\/work\?/.test(u); }
function isSubmitURL(u) { return /\/api\/work\/submit(?:$|\?)/.test(u); }
function guardedFetch(input, init) {
  var u = urlOf(input), method = String((init && init.method) || (input && input.method) || 'GET').toUpperCase();
  if (method === 'GET' && isJobURL(u)) {
    return realFetch(input, init).then(function (resp) {
      if (!resp.ok) return resp;
      return resp.text().then(function (text) {
        var job = null; try { job = JSON.parse(text); } catch (e) {}
        var why = expect ? checkJob(job, expect) : 'the miner has no address to check jobs against';
        // The miner gets the fields that were checked, re-encoded, never the
        // server's text: Go's decoder matches keys in any case and keeps the
        // last, so "HEADER_PREFIX" after "header_prefix" would be mined unseen.
        if (!why) { refused = ''; return new Response(JSON.stringify(jobFields(job)), { status: 200, headers: { 'Content-Type': 'application/json' } }); }
        refused = why;
        self.postMessage({ type: 'refused', message: why });
        // The Go client sees an error instead of the job and mines nothing.
        return new Response('job refused: ' + why, { status: 409, statusText: 'Refused' });
      });
    });
  }
  if (method === 'POST' && isSubmitURL(u)) {
    // A solved block is worth waiting for: "busy" (429) is retried.
    var tries = 0;
    var go = function () {
      return realFetch(input, init).then(function (resp) {
        if (resp.status !== 429 || ++tries > 6) return resp;
        var wait = Math.min(8, Number(resp.headers.get('Retry-After')) || 1) * 1000;
        return new Promise(function (r) { setTimeout(r, wait); }).then(go);
      });
    };
    return go();
  }
  return realFetch(input, init);
}

// setExpect prepares the check for a miner paying address (with an optional
// referrer and the share the page showed for it), or, with pool, for pool
// jobs paying the pool address the page showed. It returns '' or why the
// addresses are unusable.
function setExpect(address, ref, refPct, pool) {
  try {
    var a = decodeAddress(address), e = { hrp: a.hrp, payload: a.payload, refPayload: null, refPct: 0, pool: !!pool };
    if (ref && !pool) {
      var r = decodeAddress(ref);
      if (r.hrp !== a.hrp) return 'the referrer is on another network';
      e.refPayload = r.payload;
      e.refPct = Math.max(0, Math.min(50, Math.floor(Number(refPct) || 0)));
    }
    expect = e; refused = '';
    return '';
  } catch (err) {
    return 'invalid address: ' + (err && err.message || err);
  }
}

self.argonmineJobCheck = { checkJob: checkJob, setExpect: setExpect, decodeAddress: decodeAddress, encodeAddress: encodeAddress, sha256: sha256, taggedHash: taggedHash, guardedFetch: guardedFetch };

// ---- the miner ----
if (typeof importScripts === 'function' && realFetch) {
  self.fetch = guardedFetch;
  var V = new URLSearchParams(self.location.search).get('v') || '';
  var ready = false;
  importScripts('/static/wasm_exec.js?v=' + encodeURIComponent(V));
  self.addEventListener('argonmineMinerReady', function () { ready = true; self.postMessage({ type: 'ready' }); });
  var go = new Go();
  WebAssembly.instantiateStreaming(realFetch('/static/miner.wasm?v=' + encodeURIComponent(V)), go.importObject)
    .then(function (r) { return go.run(r.instance); })
    .catch(function (e) { self.postMessage({ type: 'error', message: String(e && e.message || e) }); });
  self.onmessage = function (ev) {
    var m = ev.data || {};
    if (!ready) { self.postMessage({ id: m.id, error: 'miner not ready' }); return; }
    var M = self.argonmineMiner, result;
    try {
      switch (m.type) {
        case 'start':
          // No job is mined before the check knows whom it must pay: the
          // miner's own address, or in pool mode the pool's. The miner's
          // address still goes to the server, which credits the shares to it.
          if (m.pool) result = setExpect(m.poolAddress, '', 0, true) || M.start(m.server, m.address, '');
          else result = setExpect(m.address, m.ref || '', m.refPct) || M.start(m.server, m.address, m.ref || '');
          break;
        case 'stop': M.stop(); result = null; break;
        case 'stats':
          result = M.stats();
          if (refused && result) result.error = 'Job refused: ' + refused + '.';
          break;
        case 'newWallet': result = M.newWallet(m.hrp); break;
        case 'addressOf': result = M.addressOf(m.phrase, m.hrp); break;
        // The browser wallet's signer: JSON in, JSON out (wallet_js.go).
        case 'wallet': result = JSON.parse(M.wallet(String(m.op || ''), JSON.stringify(m.args || {}))); break;
        default: self.postMessage({ id: m.id, error: 'unknown request' }); return;
      }
      self.postMessage({ id: m.id, result: result === undefined ? null : result });
    } catch (e) {
      self.postMessage({ id: m.id, error: String(e && e.message || e) });
    }
  };
}
