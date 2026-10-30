// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title Vault: the EVM side of the inbound Argonmine bridge (docs/BRIDGE.md).
/// @notice Holds allowlisted stablecoins (USDC, USDT) that back uARG on the
///         Argonmine chain one to one: every unit of every allowed token is one
///         dollar, so uARG stays one dollar. A deposit here is paid out as uARG
///         by the guardians; a uARG burn on Argonmine is paid out from here, in
///         the token the burner named, with `threshold` guardian signatures.
///         One vault is deployed per EVM chain. ARG itself never crosses this
///         bridge.
///
///         A bridge fee in basis points may be taken on deposits and on
///         releases. The fee stays in the vault as *surplus*: the part of the
///         balance above `obligations`, the amount that backs the uARG issued
///         for that token. Only the owner can withdraw surplus, and never more
///         than it, so the backing cannot be touched.
///
///         Releases are capped per token: `maxRelease[token]` per release and
///         `periodLimit[token]` per window of `PERIOD` seconds, both in gross
///         token units (0 = no limit).
///
///         Administration (contracts/README.md, "Admin model"):
///         - Guardians, tokens, fees, release limits, the timelock delay and
///           lifting a pause change only through the timelock: `propose*`
///           records the change, the matching `execute*` applies it no earlier
///           than `delay` seconds later (at least one day) and no later than
///           `GRACE_PERIOD` after that, and `cancel` drops it.
///         - A threshold of guardians may `veto` a pending proposal. The next
///           proposal of the same change is an owner override: it waits
///           `OVERRIDE_DELAY` (twice `MAX_DELAY`) and cannot be vetoed, so
///           guardians can delay the owner but never freeze the vault.
///         - Until the one-way `finalizeSetup`, the owner may also set tokens,
///           fees and limits at once (`setToken`, `setFees`, `setLimits`) to
///           configure a fresh deployment.
///         - `pause` is immediate and open to the owner and to every guardian.
///         - `withdrawSurplus` is immediate: it cannot touch the backing.
///         - A release fee may rise by at most `MAX_RELEASE_FEE_STEP_BPS` per
///           change.
///         The owner must be a multisig of independent parties before the
///         vault holds real funds.
/// @dev Self-contained so the whole contract can be reviewed in one sitting.
///      The vault never calls out except to tokens, and holds nothing but
///      tokens. Token calls tolerate mainnet USDT, whose `transfer` and
///      `transferFrom` return nothing. Only plain stablecoins belong on the
///      allowlist: a fee-on-transfer or rebasing token would break the one to
///      one backing.
interface IERC20 {
    function balanceOf(address who) external view returns (uint256);
}

contract Vault {
    address public owner;
    address public pendingOwner;
    bool public paused;
    bool public finalized; // setup is over: tokens, fees and limits change only through the timelock
    uint16 public depositFeeBps; // fee on deposits in basis points, kept as surplus
    uint16 public releaseFeeBps; // fee on releases in basis points, kept as surplus
    uint256 public threshold;
    uint256 public delay; // timelock delay in seconds, MIN_DELAY..MAX_DELAY
    mapping(address => bool) public isGuardian;
    address[] public guardians;
    /// @notice The id of this vault's first deposit (a constructor argument).
    ///         The guardians name a deposit by its chain and id alone, so a
    ///         vault that replaces another on the same chain must start above
    ///         every deposit id the earlier vaults used (docs/BRIDGE.md,
    ///         "Replacing a vault"); the first vault on a chain starts at 1.
    uint256 public immutable firstDepositId;
    uint256 public depositCount; // the id of the last deposit (firstDepositId - 1 before the first); one counter for all tokens
    mapping(bytes32 => bool) public released; // burnId -> paid out
    mapping(address => bool) public isAllowed; // token -> accepted for deposit and release
    mapping(address => uint256) public minDeposit; // token -> smallest deposit in token units (gross)
    mapping(address => uint256) public obligations; // token -> units that back the uARG issued for it
    mapping(address => uint256) public maxRelease; // token -> cap on one release in token units (gross), 0 = none
    mapping(address => uint256) public periodLimit; // token -> cap on the releases of one window (gross), 0 = none
    mapping(address => uint256) public periodStart; // token -> start of its current release window
    mapping(address => uint256) public periodReleased; // token -> gross released in the window at periodStart
    mapping(bytes32 => uint256) public proposalEta; // proposal id -> earliest execution time, 0 = none
    /// @notice proposal id -> how many times it has been proposed. With the
    ///         eta it names one proposal instance in `vetoDigest`, so a veto
    ///         never applies to a later proposal of the same change.
    mapping(bytes32 => uint256) public proposalNonce;
    /// @notice proposal id -> its last proposal was vetoed: the next one is
    ///         an owner override (`overrideOf`).
    mapping(bytes32 => bool) public vetoed;
    /// @notice proposal id -> the pending proposal is an owner override: it
    ///         waits `OVERRIDE_DELAY` instead of `delay` and cannot be vetoed.
    mapping(bytes32 => bool) public overrideOf;
    address[] private _tokens; // every token ever allowed, in insertion order

    /// @dev At most 2^62: that leaves 2^62 ids, every one of them within the
    ///      2^63 - 1 an issuance's deposit references can name.
    uint256 public constant MAX_FIRST_DEPOSIT_ID = 2 ** 62;
    uint16 public constant MAX_FEE_BPS = 1000; // 10 percent
    uint16 public constant MAX_RELEASE_FEE_STEP_BPS = 100; // largest rise of releaseFeeBps in one change
    uint256 public constant PERIOD = 1 days; // length of a release window
    uint256 public constant MIN_DELAY = 1 days;
    uint256 public constant MAX_DELAY = 30 days;
    uint256 public constant GRACE_PERIOD = 14 days; // a proposal expires this long after its eta
    /// @notice The delay of an owner override: the proposal that follows a
    ///         veto of the same change.
    uint256 public constant OVERRIDE_DELAY = 2 * MAX_DELAY;
    uint256 private constant BPS = 10000;

    /// @param amount the net credited to the depositor as uARG; `fee` is the part kept as surplus.
    event Deposited(
        uint256 indexed id, address indexed token, address indexed from, uint256 amount, uint256 fee, string to
    );
    /// @param amount the gross value of the burn; `to` received `amount - fee` and `fee` stayed as surplus.
    event Released(bytes32 indexed burnId, address indexed token, address indexed to, uint256 amount, uint256 fee);
    event FeesSet(uint16 depositBps, uint16 releaseBps);
    event SurplusWithdrawn(address indexed token, address indexed to, uint256 amount);
    event TokenSet(address indexed token, bool allowed, uint256 minDeposit);
    event GuardiansSet(address[] guardians, uint256 threshold);
    event LimitsSet(address indexed token, uint256 maxRelease, uint256 periodLimit);
    event DelaySet(uint256 delay);
    event Paused(bool paused);
    event SetupFinalized();
    /// @param id keccak256(data), the proposal's id.
    /// @param data the calldata of the `execute*` call that applies the change,
    ///        so a watcher can decode what is proposed with the vault's ABI.
    /// @param eta the earliest time the change can be executed.
    event Proposed(bytes32 indexed id, bytes data, uint256 eta);
    event Executed(bytes32 indexed id);
    event Cancelled(bytes32 indexed id);
    event OwnershipTransferStarted(address indexed to);
    event OwnershipTransferred(address indexed to);

    error NotOwner();
    error NotOwnerOrGuardian();
    error IsPaused();
    error NotPaused();
    error BadGuardians();
    error BadToken();
    error TokenNotAllowed();
    error AlreadyReleased();
    error BadSignatures();
    error BadDestination();
    error AmountTooSmall();
    error LimitExceeded();
    error TransferFailed();
    error BadFee();
    error FeeStepTooLarge();
    error NoSurplus();
    error BadDelay();
    error BadFirstDepositId();
    error AlreadyFinalized();
    error AlreadyProposed();
    error NotProposed();
    error TooEarly();
    error ProposalExpired();
    error NotVetoable();

    modifier onlyOwner() {
        if (msg.sender != owner) revert NotOwner();
        _;
    }

    modifier whenNotPaused() {
        if (paused) revert IsPaused();
        _;
    }

    modifier duringSetup() {
        if (finalized) revert AlreadyFinalized();
        _;
    }

    /// @param _guardians the guardians' EVM addresses, distinct and non-zero.
    /// @param _threshold signatures needed to release, 1.._guardians.length.
    /// @param _delay the timelock delay in seconds, MIN_DELAY..MAX_DELAY.
    /// @param _firstDepositId the id of the first deposit, 1..MAX_FIRST_DEPOSIT_ID:
    ///        1 for the first vault on a chain; above every deposit id of the
    ///        vaults it replaces for a later one (their `depositCount()` + 1).
    constructor(address[] memory _guardians, uint256 _threshold, uint256 _delay, uint256 _firstDepositId) {
        _checkDelay(_delay);
        if (_firstDepositId == 0 || _firstDepositId > MAX_FIRST_DEPOSIT_ID) revert BadFirstDepositId();
        owner = msg.sender;
        delay = _delay;
        firstDepositId = _firstDepositId;
        depositCount = _firstDepositId - 1;
        emit DelaySet(_delay);
        _setGuardians(_guardians, _threshold);
    }

    // ---- deposits ----

    /// @notice Deposit `amount` of `token` to receive uARG at `to`, a bech32m
    ///         Argonmine address. Approve the vault to spend `amount` first.
    ///         `amount` is gross: the deposit fee is taken from it and the
    ///         rest (the `amount` in the event) is what the guardians pay out
    ///         as uARG and what is added to `obligations`. The minimum deposit
    ///         applies to the gross amount. The guardians validate `to`; a
    ///         deposit to an invalid address cannot be paid out and needs
    ///         manual handling.
    /// @return id the deposit's id, unique across all tokens of this vault:
    ///         firstDepositId for the first deposit, then one more each time.
    function deposit(address token, uint256 amount, string calldata to) external whenNotPaused returns (uint256 id) {
        if (!isAllowed[token]) revert TokenNotAllowed();
        if (amount == 0 || amount < minDeposit[token]) revert AmountTooSmall();
        uint256 len = bytes(to).length;
        if (len == 0 || len > 128) revert BadDestination();
        uint256 fee = amount * depositFeeBps / BPS; // floors in the depositor's favour
        uint256 net = amount - fee;
        id = ++depositCount;
        obligations[token] += net;
        emit Deposited(id, token, msg.sender, net, fee, to); // reverted with the transfer if that fails
        _call(token, abi.encodeWithSelector(0x23b872dd, msg.sender, address(this), amount)); // transferFrom
    }

    // ---- releases ----

    /// @notice The hash a guardian signs to approve a release: the EIP-191
    ///         personal-sign prefix over
    ///         keccak256(abi.encode(chainId, vault, token, to, amount, burnId)),
    ///         `amount` being the gross value of the burn.
    function releaseDigest(address token, address to, uint256 amount, bytes32 burnId) public view returns (bytes32) {
        bytes32 inner = keccak256(abi.encode(block.chainid, address(this), token, to, amount, burnId));
        return keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", inner));
    }

    /// @notice Pay out a uARG burn on Argonmine worth `amount` of `token` to
    ///         `to`. `amount` is gross: the release fee is taken from it, `to`
    ///         receives the rest, and `obligations` drops by the full `amount`
    ///         (floored at zero). Reverts with `LimitExceeded` when `amount`
    ///         is above `maxRelease[token]` or would take the releases of the
    ///         current window above `periodLimit[token]` (both gross, 0 = no
    ///         limit); such a burn can be paid in a later window or once the
    ///         limits are raised.
    /// @param burnId the burn transaction id on Argonmine (replay protection:
    ///        each burnId is paid at most once, whatever the token).
    /// @param sigs 65-byte (r,s,v) guardian signatures over `releaseDigest`,
    ///        ordered by strictly increasing signer address, at least
    ///        `threshold` of them.
    function release(address token, address to, uint256 amount, bytes32 burnId, bytes[] calldata sigs)
        external
        whenNotPaused
    {
        if (!isAllowed[token]) revert TokenNotAllowed();
        if (released[burnId]) revert AlreadyReleased();
        if (to == address(0)) revert BadDestination();
        if (amount == 0) revert AmountTooSmall();
        uint256 cap = maxRelease[token];
        if (cap != 0 && amount > cap) revert LimitExceeded();
        if (sigs.length < threshold) revert BadSignatures();
        bytes32 digest = releaseDigest(token, to, amount, burnId);
        address last = address(0);
        for (uint256 i = 0; i < sigs.length; i++) {
            address signer = _recover(digest, sigs[i]);
            // forge-lint: disable-next-line(require-revert-in-loop)
            if (signer <= last || !isGuardian[signer]) revert BadSignatures();
            last = signer;
        }
        _countRelease(token, amount);
        released[burnId] = true;
        uint256 fee = amount * releaseFeeBps / BPS;
        uint256 owed = obligations[token];
        obligations[token] = owed > amount ? owed - amount : 0;
        emit Released(burnId, token, to, amount, fee); // reverted with the transfer if that fails
        _call(token, abi.encodeWithSelector(0xa9059cbb, to, amount - fee)); // transfer
    }

    /// @notice The gross amount of `token` that can still be released in the
    ///         current window under `periodLimit[token]`; type(uint256).max
    ///         when the token has no period limit. A single release is also
    ///         bounded by `maxRelease[token]`.
    function periodAvailable(address token) external view returns (uint256) {
        uint256 limit = periodLimit[token];
        if (limit == 0) return type(uint256).max;
        // forge-lint: disable-next-line(block-timestamp)
        uint256 used = block.timestamp >= periodStart[token] + PERIOD ? 0 : periodReleased[token];
        return used >= limit ? 0 : limit - used;
    }

    /// @dev Adds `amount` to `token`'s release window, opening a new window of
    ///      `PERIOD` seconds when the current one has ended, and enforces
    ///      `periodLimit`. Windows are counted whether or not a limit is set,
    ///      so a limit set mid-window sees the releases already made in it.
    function _countRelease(address token, uint256 amount) internal {
        uint256 used = periodReleased[token];
        // forge-lint: disable-next-line(block-timestamp)
        if (block.timestamp >= periodStart[token] + PERIOD) {
            periodStart[token] = block.timestamp;
            used = 0;
        }
        used += amount;
        uint256 limit = periodLimit[token];
        if (limit != 0 && used > limit) revert LimitExceeded();
        periodReleased[token] = used;
    }

    // ---- reserve, obligations and surplus ----

    /// @notice Amount of `token` held: obligations plus surplus.
    function reserve(address token) external view returns (uint256) {
        return IERC20(token).balanceOf(address(this));
    }

    /// @notice The part of the vault's `token` balance above what backs the
    ///         uARG issued for it (the collected fees, plus anything sent to
    ///         the vault outside `deposit`). Floored at zero.
    function surplus(address token) public view returns (uint256) {
        uint256 held = IERC20(token).balanceOf(address(this));
        uint256 owed = obligations[token];
        return held > owed ? held - owed : 0;
    }

    /// @notice Withdraw up to `surplus(token)` of `token` to `to`. The backing
    ///         (`obligations`) can never be withdrawn this way, so this is not
    ///         timelocked. Works whether or not `token` is currently allowed,
    ///         so the surplus of a token removed from the allowlist can still
    ///         be swept.
    function withdrawSurplus(address token, address to, uint256 amount) external onlyOwner {
        if (to == address(0)) revert BadDestination();
        if (amount > surplus(token)) revert NoSurplus();
        emit SurplusWithdrawn(token, to, amount); // reverted with the transfer if that fails
        _call(token, abi.encodeWithSelector(0xa9059cbb, to, amount)); // transfer
    }

    /// @notice The tokens currently accepted, in the order they were first allowed.
    function tokens() external view returns (address[] memory list) {
        uint256 n = 0;
        for (uint256 i = 0; i < _tokens.length; i++) {
            if (isAllowed[_tokens[i]]) n++;
        }
        list = new address[](n);
        uint256 j = 0;
        for (uint256 i = 0; i < _tokens.length; i++) {
            if (isAllowed[_tokens[i]]) list[j++] = _tokens[i];
        }
    }

    function guardianCount() external view returns (uint256) {
        return guardians.length;
    }

    function _recover(bytes32 digest, bytes calldata sig) internal pure returns (address) {
        if (sig.length != 65) return address(0);
        bytes32 r = bytes32(sig[0:32]);
        bytes32 s = bytes32(sig[32:64]);
        uint8 v = uint8(sig[64]);
        if (v < 27) v += 27;
        if (v != 27 && v != 28) return address(0);
        if (uint256(s) > 0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0) return address(0);
        return ecrecover(digest, v, r, s);
    }

    /// @dev Token call with SafeERC20 semantics: the call must succeed and
    ///      return either nothing (mainnet USDT) or the boolean true.
    function _call(address token, bytes memory data) internal {
        (bool ok, bytes memory ret) = token.call(data);
        if (!ok) revert TransferFailed();
        if (ret.length == 0) {
            if (token.code.length == 0) revert TransferFailed();
        } else if (ret.length < 32 || !abi.decode(ret, (bool))) {
            revert TransferFailed();
        }
    }

    // ---- pause ----

    /// @notice Stop deposits and releases at once. Open to the owner and to
    ///         every guardian, so any one of them can halt a theft in
    ///         progress. Also drops a pending unpause proposal (an override
    ///         one too), so lifting a pause always takes a fresh
    ///         `proposeUnpause` and the full delay. Guardians who keep
    ///         pausing are removed with `proposeGuardians` first, which a
    ///         pause does not touch and a veto only delays.
    function pause() external {
        if (msg.sender != owner && !isGuardian[msg.sender]) revert NotOwnerOrGuardian();
        paused = true;
        emit Paused(true);
        bytes32 id = keccak256(abi.encodeCall(this.executeUnpause, ()));
        if (proposalEta[id] != 0) {
            delete proposalEta[id];
            delete overrideOf[id];
            emit Cancelled(id);
        }
    }

    // ---- timelock ----
    //
    // A proposal's id is keccak256 of the calldata of the `execute*` call
    // that applies it (selector and arguments), so the same change proposed
    // twice has the same id and different changes never share one. Every
    // `propose*` checks its arguments, records `proposalEta[id] = now +
    // delay`, emits `Proposed(id, data, eta)` and returns the id. The matching
    // `execute*`, called with the same arguments by the owner, applies the
    // change from `eta` until `eta + GRACE_PERIOD`, checking the arguments
    // again against the state at that time. `cancel(id)` drops a pending
    // proposal. An expired proposal can be proposed again. After a `veto`,
    // the next proposal of the id is an owner override: `eta = now +
    // OVERRIDE_DELAY`, and `veto` refuses it.
    //
    // Timestamps are compared on the scale of days (the delay, the grace
    // period and the release windows), so the few seconds a block producer
    // can shift `block.timestamp` do not matter; hence the lint exemptions.

    /// @notice Propose replacing the guardian set and the release threshold.
    function proposeGuardians(address[] calldata newGuardians, uint256 newThreshold)
        external
        onlyOwner
        returns (bytes32 id)
    {
        _checkGuardians(newGuardians, newThreshold);
        id = _propose(abi.encodeCall(this.executeGuardians, (newGuardians, newThreshold)));
    }

    /// @notice Apply a `proposeGuardians` proposal whose delay has passed.
    function executeGuardians(address[] calldata newGuardians, uint256 newThreshold) external onlyOwner {
        _execute(abi.encodeCall(this.executeGuardians, (newGuardians, newThreshold)));
        _setGuardians(newGuardians, newThreshold);
    }

    /// @notice Propose allowing or disallowing `token` and setting its
    ///         minimum deposit in token units.
    function proposeToken(address token, bool allowed, uint256 minimum) external onlyOwner returns (bytes32 id) {
        _checkToken(token);
        id = _propose(abi.encodeCall(this.executeToken, (token, allowed, minimum)));
    }

    /// @notice Apply a `proposeToken` proposal whose delay has passed.
    function executeToken(address token, bool allowed, uint256 minimum) external onlyOwner {
        _execute(abi.encodeCall(this.executeToken, (token, allowed, minimum)));
        _setToken(token, allowed, minimum);
    }

    /// @notice Propose new deposit and release fees in basis points; see `setFees`.
    function proposeFees(uint16 depositBps, uint16 releaseBps) external onlyOwner returns (bytes32 id) {
        _checkFees(depositBps, releaseBps);
        id = _propose(abi.encodeCall(this.executeFees, (depositBps, releaseBps)));
    }

    /// @notice Apply a `proposeFees` proposal whose delay has passed. The
    ///         release fee step is checked against the fee at this time.
    function executeFees(uint16 depositBps, uint16 releaseBps) external onlyOwner {
        _execute(abi.encodeCall(this.executeFees, (depositBps, releaseBps)));
        _setFees(depositBps, releaseBps);
    }

    /// @notice Propose new release limits for `token`; see `setLimits`.
    function proposeLimits(address token, uint256 perRelease, uint256 perPeriod)
        external
        onlyOwner
        returns (bytes32 id)
    {
        if (token == address(0)) revert BadToken();
        id = _propose(abi.encodeCall(this.executeLimits, (token, perRelease, perPeriod)));
    }

    /// @notice Apply a `proposeLimits` proposal whose delay has passed.
    function executeLimits(address token, uint256 perRelease, uint256 perPeriod) external onlyOwner {
        _execute(abi.encodeCall(this.executeLimits, (token, perRelease, perPeriod)));
        _setLimits(token, perRelease, perPeriod);
    }

    /// @notice Propose lifting the pause; only while paused. A later `pause`
    ///         cancels the proposal.
    function proposeUnpause() external onlyOwner returns (bytes32 id) {
        if (!paused) revert NotPaused();
        id = _propose(abi.encodeCall(this.executeUnpause, ()));
    }

    /// @notice Apply a `proposeUnpause` proposal whose delay has passed.
    function executeUnpause() external onlyOwner {
        _execute(abi.encodeCall(this.executeUnpause, ()));
        paused = false;
        emit Paused(false);
    }

    /// @notice Propose a new timelock delay, MIN_DELAY..MAX_DELAY seconds.
    ///         Raising and lowering it both wait for the current delay.
    function proposeDelay(uint256 newDelay) external onlyOwner returns (bytes32 id) {
        _checkDelay(newDelay);
        id = _propose(abi.encodeCall(this.executeDelay, (newDelay)));
    }

    /// @notice Apply a `proposeDelay` proposal whose delay has passed.
    ///         Proposals already pending keep their eta.
    function executeDelay(uint256 newDelay) external onlyOwner {
        _execute(abi.encodeCall(this.executeDelay, (newDelay)));
        delay = newDelay;
        emit DelaySet(newDelay);
    }

    /// @notice The hash guardians sign to veto the pending proposal `id`: the
    ///         EIP-191 personal-sign prefix over keccak256(abi.encode(
    ///         "Argonmine/vault/veto", chainId, vault, id, proposalEta[id],
    ///         proposalNonce[id])). It names the proposal instance pending
    ///         now, so signatures made for one proposal never veto a later
    ///         proposal of the same change.
    function vetoDigest(bytes32 id) public view returns (bytes32) {
        bytes32 inner = keccak256(
            abi.encode("Argonmine/vault/veto", block.chainid, address(this), id, proposalEta[id], proposalNonce[id])
        );
        return keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", inner));
    }

    /// @notice Drop the pending proposal `id` with `threshold` guardian
    ///         signatures over `vetoDigest(id)`, ordered by strictly
    ///         increasing signer address; anyone may submit them. The
    ///         guardians can so stop a change made with a stolen owner key
    ///         (new guardians, lifted limits, a shorter delay, an unpause)
    ///         during its delay, while no single guardian can block the
    ///         owner's changes on its own. The owner's next proposal of the
    ///         same change is an override (`overrideOf`): it waits
    ///         `OVERRIDE_DELAY` and this function refuses it
    ///         (`NotVetoable`), so a veto delays a change but never blocks it
    ///         for good.
    function veto(bytes32 id, bytes[] calldata sigs) external {
        if (proposalEta[id] == 0) revert NotProposed();
        if (overrideOf[id]) revert NotVetoable();
        if (sigs.length < threshold) revert BadSignatures();
        bytes32 digest = vetoDigest(id);
        address last = address(0);
        for (uint256 i = 0; i < sigs.length; i++) {
            address signer = _recover(digest, sigs[i]);
            // forge-lint: disable-next-line(require-revert-in-loop)
            if (signer <= last || !isGuardian[signer]) revert BadSignatures();
            last = signer;
        }
        delete proposalEta[id];
        vetoed[id] = true;
        emit Cancelled(id);
    }

    /// @notice Drop the pending (or expired) proposal `id`.
    function cancel(bytes32 id) external onlyOwner {
        if (proposalEta[id] == 0) revert NotProposed();
        delete proposalEta[id];
        delete overrideOf[id];
        emit Cancelled(id);
    }

    /// @dev The proposal after a veto of the same id is an owner override:
    ///      it waits OVERRIDE_DELAY and cannot be vetoed. Any other waits
    ///      `delay` and can (an expired override proposed again included).
    function _propose(bytes memory data) internal returns (bytes32 id) {
        id = keccak256(data);
        uint256 pending = proposalEta[id];
        // forge-lint: disable-next-line(block-timestamp)
        if (pending != 0 && block.timestamp <= pending + GRACE_PERIOD) revert AlreadyProposed();
        bool ov = vetoed[id];
        if (ov) delete vetoed[id];
        overrideOf[id] = ov;
        proposalNonce[id] += 1;
        uint256 eta = block.timestamp + (ov ? OVERRIDE_DELAY : delay);
        proposalEta[id] = eta;
        emit Proposed(id, data, eta);
    }

    function _execute(bytes memory data) internal {
        bytes32 id = keccak256(data);
        uint256 eta = proposalEta[id];
        if (eta == 0) revert NotProposed();
        // forge-lint: disable-next-line(block-timestamp)
        if (block.timestamp < eta) revert TooEarly();
        // forge-lint: disable-next-line(block-timestamp)
        if (block.timestamp > eta + GRACE_PERIOD) revert ProposalExpired();
        delete proposalEta[id];
        delete overrideOf[id];
        emit Executed(id);
    }

    // ---- setup (before finalizeSetup only) ----

    /// @notice Allow or disallow `token` for deposits and releases and set its
    ///         minimum deposit in token units, at once. Setup only; afterwards
    ///         use `proposeToken`. A disallowed token stays in the vault until
    ///         it is allowed again; releases of it are refused.
    function setToken(address token, bool allowed, uint256 minimum) external onlyOwner duringSetup {
        _setToken(token, allowed, minimum);
    }

    /// @notice Set the deposit and release fees in basis points (100 = one
    ///         percent), each at most `MAX_FEE_BPS`, at once. The release fee
    ///         may rise by at most `MAX_RELEASE_FEE_STEP_BPS` per change, here
    ///         as through `proposeFees`. Setup only; afterwards use
    ///         `proposeFees`. Fees apply from the next transaction on.
    function setFees(uint16 depositBps, uint16 releaseBps) external onlyOwner duringSetup {
        _setFees(depositBps, releaseBps);
    }

    /// @notice Set `token`'s release limits in gross token units, at once:
    ///         `perRelease` caps one release and `perPeriod` the releases of
    ///         one window of `PERIOD` seconds; 0 removes a limit. Setup only;
    ///         afterwards use `proposeLimits`.
    function setLimits(address token, uint256 perRelease, uint256 perPeriod) external onlyOwner duringSetup {
        _setLimits(token, perRelease, perPeriod);
    }

    /// @notice End the setup for good: from now on tokens, fees and limits
    ///         change only through the timelock, like everything else.
    function finalizeSetup() external onlyOwner duringSetup {
        finalized = true;
        emit SetupFinalized();
    }

    // ---- changes ----

    function _setToken(address token, bool allowed, uint256 minimum) internal {
        _checkToken(token);
        if (allowed && !_listed(token)) _tokens.push(token);
        isAllowed[token] = allowed;
        minDeposit[token] = minimum;
        emit TokenSet(token, allowed, minimum);
    }

    function _checkToken(address token) internal view {
        if (token == address(0) || token.code.length == 0) revert BadToken();
    }

    function _listed(address token) internal view returns (bool) {
        for (uint256 i = 0; i < _tokens.length; i++) {
            if (_tokens[i] == token) return true;
        }
        return false;
    }

    function _setFees(uint16 depositBps, uint16 releaseBps) internal {
        _checkFees(depositBps, releaseBps);
        depositFeeBps = depositBps;
        releaseFeeBps = releaseBps;
        emit FeesSet(depositBps, releaseBps);
    }

    /// @dev The release fee is what a user who has already burned uARG pays,
    ///      so it may only creep up; lowering it is never limited.
    function _checkFees(uint16 depositBps, uint16 releaseBps) internal view {
        if (depositBps > MAX_FEE_BPS || releaseBps > MAX_FEE_BPS) revert BadFee();
        if (releaseBps > uint256(releaseFeeBps) + MAX_RELEASE_FEE_STEP_BPS) revert FeeStepTooLarge();
    }

    function _setLimits(address token, uint256 perRelease, uint256 perPeriod) internal {
        if (token == address(0)) revert BadToken();
        maxRelease[token] = perRelease;
        periodLimit[token] = perPeriod;
        emit LimitsSet(token, perRelease, perPeriod);
    }

    function _setGuardians(address[] memory list, uint256 newThreshold) internal {
        _checkGuardians(list, newThreshold);
        for (uint256 i = 0; i < guardians.length; i++) {
            isGuardian[guardians[i]] = false;
        }
        delete guardians;
        for (uint256 i = 0; i < list.length; i++) {
            isGuardian[list[i]] = true;
            guardians.push(list[i]);
        }
        threshold = newThreshold;
        emit GuardiansSet(list, newThreshold);
    }

    /// @dev A guardian set is non-empty, without the zero address or
    ///      duplicates, and the threshold is 1..its size.
    function _checkGuardians(address[] memory list, uint256 newThreshold) internal pure {
        uint256 n = list.length;
        if (n == 0 || newThreshold == 0 || newThreshold > n) revert BadGuardians();
        for (uint256 i = 0; i < n; i++) {
            // forge-lint: disable-next-line(require-revert-in-loop)
            if (list[i] == address(0)) revert BadGuardians();
            for (uint256 j = 0; j < i; j++) {
                // forge-lint: disable-next-line(require-revert-in-loop)
                if (list[j] == list[i]) revert BadGuardians();
            }
        }
    }

    function _checkDelay(uint256 newDelay) internal pure {
        if (newDelay < MIN_DELAY || newDelay > MAX_DELAY) revert BadDelay();
    }

    // ---- ownership ----

    /// @notice Offer ownership to `to`, who must call `acceptOwnership`;
    ///         address(0) cancels a pending offer. Not timelocked: a new owner
    ///         faces the same timelock. Before the vault holds real funds,
    ///         transfer ownership to a multisig of independent parties.
    // forge-lint: disable-next-item(missing-zero-check)
    function transferOwnership(address to) external onlyOwner {
        pendingOwner = to;
        emit OwnershipTransferStarted(to);
    }

    function acceptOwnership() external {
        if (msg.sender != pendingOwner) revert NotOwner();
        // forge-lint: disable-next-line(missing-events-access-control)
        owner = pendingOwner;
        pendingOwner = address(0);
        emit OwnershipTransferred(owner);
    }
}
