/* EVM wallet helpers for the Argonmine apps' Swap in: connect an injected
   wallet (EIP-6963, window.ethereum as the fallback), switch it to a chain,
   read a token's balance and allowance, approve the bridge vault and call
   vault.deposit(token, amount, to). Calls are built by hand (no library) and
   every read goes through the connected wallet, so the page fetches nothing
   from other origins. The selectors are checked against the bridge package
   by a Go test. */
(function () {
  'use strict';
  const SEL = { approve: '095ea7b3', balanceOf: '70a08231', allowance: 'dd62ed3e', deposit: 'bfe07da6' };
  const POPULAR = ['io.metamask', 'io.rabby', 'com.coinbase.wallet', 'com.okex.wallet', 'com.trustwallet.app', 'me.rainbow', 'app.phantom', 'com.brave.wallet', 'io.zerion.wallet', 'app.backpack'];
  const st = { wallets: [], prov: null, info: null, account: '', chainId: null, subs: [] };
  const emit = () => st.subs.forEach(f => { try { f(); } catch (e) { } });

  // ---- discovery: every installed wallet announces itself (EIP-6963) ----
  window.addEventListener('eip6963:announceProvider', e => {
    const d = e.detail; if (!d || !d.provider || !d.info || st.wallets.some(w => w.info.uuid === d.info.uuid)) return;
    st.wallets.push(d); emit();
  });
  window.dispatchEvent(new Event('eip6963:requestProvider'));
  function wallets() {
    const list = st.wallets.slice().sort((a, b) => {
      const pa = POPULAR.indexOf(a.info.rdns), pb = POPULAR.indexOf(b.info.rdns);
      return (pa < 0 ? 99 : pa) - (pb < 0 ? 99 : pb) || String(a.info.name || '').localeCompare(String(b.info.name || ''));
    });
    if (!list.length && window.ethereum) list.push({ provider: window.ethereum, info: { uuid: 'injected', name: window.ethereum.isMetaMask ? 'MetaMask' : 'Browser wallet', rdns: '', icon: '' } });
    return list;
  }
  function has() { return wallets().length > 0; }
  // A wallet's own icon, only as an inline image (data:) or over https.
  function icon(w) { const s = String((w && w.info && w.info.icon) || ''); return /^data:image\/(png|svg\+xml|webp|jpeg|gif)[;,]/.test(s) || /^https:\/\//.test(s) ? s : ''; }
  function use(w) {
    if (!w || st.prov === w.provider) return;
    st.prov = w.provider; st.info = w.info; st.account = ''; st.chainId = null;
    try { if (w.info.rdns) localStorage.setItem('evm.wallet', w.info.rdns); } catch (e) { }
    const p = w.provider;
    if (p.on && !p.__argonmine) {
      p.__argonmine = true;
      p.on('accountsChanged', a => { if (st.prov !== p) return; st.account = String((a && a[0]) || '').toLowerCase(); emit(); });
      p.on('chainChanged', id => { if (st.prov !== p) return; st.chainId = parseInt(id, 16); emit(); });
    }
  }
  // The wallet to use without asking: the one used last, or the only one.
  function auto() {
    if (st.prov) return true;
    const list = wallets(); let last = ''; try { last = localStorage.getItem('evm.wallet') || ''; } catch (e) { }
    const w = list.find(x => last && x.info.rdns === last) || (list.length === 1 ? list[0] : null);
    if (w) { use(w); return true; }
    return false;
  }
  function req(method, params) {
    if (!st.prov) return Promise.reject(new Error('No wallet connected'));
    return st.prov.request({ method: method, params: params || [] });
  }
  async function connect() {
    if (!st.prov && !auto()) throw new Error('Choose a wallet');
    const a = await req('eth_requestAccounts');
    st.account = String((a && a[0]) || '').toLowerCase();
    if (!st.account) throw new Error((st.info && st.info.name || 'The wallet') + ' returned no account. Unlock it and try again.');
    try { st.chainId = parseInt(await req('eth_chainId'), 16); } catch (e) { st.chainId = null; }
    emit(); return st.account;
  }
  // Picks up a wallet that is already connected to this site, without a prompt.
  async function resume() {
    if (!auto()) return '';
    try { const a = await req('eth_accounts'); st.account = String((a && a[0]) || '').toLowerCase(); st.chainId = parseInt(await req('eth_chainId'), 16); } catch (e) { }
    emit(); return st.account;
  }
  function disconnect() { st.prov = null; st.info = null; st.account = ''; st.chainId = null; try { localStorage.removeItem('evm.wallet'); } catch (e) { } emit(); }
  // Switches the wallet to chain c ({chain_id, name, rpc, explorer, native_symbol}), adding it if the wallet does not know it.
  async function ensureChain(c) {
    const id = Number(c.chain_id), hex = '0x' + id.toString(16);
    try { if (parseInt(await req('eth_chainId'), 16) === id) { st.chainId = id; return; } } catch (e) { }
    try { await req('wallet_switchEthereumChain', [{ chainId: hex }]); }
    catch (e) {
      if (e && e.code === 4001) throw new Error('Switch your wallet to ' + c.name + ' to continue.');
      if (!c.rpc) throw new Error('Add ' + c.name + ' to your wallet, then try again.');
      const p = { chainId: hex, chainName: c.name, rpcUrls: [c.rpc], nativeCurrency: { name: c.native_symbol || 'ETH', symbol: c.native_symbol || 'ETH', decimals: 18 } };
      if (c.explorer) p.blockExplorerUrls = [c.explorer];
      await req('wallet_addEthereumChain', [p]);
    }
    st.chainId = parseInt(await req('eth_chainId'), 16); emit();
    if (st.chainId !== id) throw new Error('Your wallet is not on ' + c.name + '.');
  }
  // ---- calls, built by hand ----
  const hex32 = v => BigInt(v).toString(16).padStart(64, '0');
  const addr32 = a => String(a).toLowerCase().replace(/^0x/, '').padStart(64, '0');
  function strArg(s) { const b = new TextEncoder().encode(s), h = Array.prototype.map.call(b, x => x.toString(16).padStart(2, '0')).join(''); return hex32(b.length) + h.padEnd(Math.ceil(h.length / 64) * 64, '0'); }
  const num = r => (r && r !== '0x' ? BigInt(r) : 0n);
  async function balanceOf(token, owner) { return num(await req('eth_call', [{ to: token, data: '0x' + SEL.balanceOf + addr32(owner) }, 'latest'])); }
  async function allowance(token, owner, spender) { return num(await req('eth_call', [{ to: token, data: '0x' + SEL.allowance + addr32(owner) + addr32(spender) }, 'latest'])); }
  function approve(token, spender, v) { return req('eth_sendTransaction', [{ from: st.account, to: token, data: '0x' + SEL.approve + addr32(spender) + hex32(v) }]); }
  // vault.deposit(token, amount, to): the uARG goes to the Argonmine address `to`.
  function deposit(vault, token, v, to) { return req('eth_sendTransaction', [{ from: st.account, to: vault, data: '0x' + SEL.deposit + addr32(token) + hex32(v) + hex32(96) + strArg(to) }]); }
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  async function receipt(hash, tries) {
    for (let i = 0; i < (tries || 300); i++) { const r = await req('eth_getTransactionReceipt', [hash]); if (r) return r; await sleep(2000); }
    throw new Error('Not confirmed in time. The transaction is ' + hash.slice(0, 10) + '…');
  }
  async function blockNumber() { return num(await req('eth_blockNumber')); }
  // An error a person can act on: a refusal in the wallet reads as such.
  function why(e) {
    const c = e && (e.code || (e.data && e.data.code)), m = String((e && (e.shortMessage || e.message)) || e || '');
    if (c === 4001 || /user (rejected|denied)|rejected the request/i.test(m)) return 'You declined in your wallet. Nothing was sent.';
    if (c === -32002) return 'Your wallet already has a request open. Open it to continue.';
    if (/insufficient funds/i.test(m)) return 'Not enough for the network fee in your wallet.';
    return m.replace(/^Error: /, '').slice(0, 200) || 'The wallet did not answer.';
  }
  window.EVM = {
    wallets, has, icon, use, connect, resume, disconnect, ensureChain, balanceOf, allowance, approve, deposit, receipt, blockNumber, why,
    on: f => { st.subs.push(f); },
    get account() { return st.account; }, get chainId() { return st.chainId; }, get name() { return (st.info && st.info.name) || ''; }, get connected() { return !!(st.prov && st.account); },
  };
})();
