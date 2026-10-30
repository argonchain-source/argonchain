// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

interface IToken {
    function balanceOf(address who) external view returns (uint256);
    function allowance(address holder, address spender) external view returns (uint256);
}

interface IVault {
    function deposit(address token, uint256 amount, string calldata to) external returns (uint256 id);
}

/// @title DepositRouter: deposit addresses for the Argonmine bridge vault.
/// @notice Lets anyone deposit into the vault without connecting a wallet.
///         Every Argonmine address `to` has one deposit address on this
///         chain, `depositAddress(to)`: the CREATE2 address of a `Forwarder`
///         this router deploys with salt `keccak256(bytes(to))`. A user sends
///         an allowlisted stablecoin there with a plain transfer, from any
///         wallet or exchange; then anyone (in practice the bridge leader)
///         calls `sweep(token, to)`, which deploys the forwarder if needed,
///         pulls its whole balance of `token` and deposits it into the vault
///         credited to `to`. The vault takes its deposit fee and emits
///         `Deposited` as for any deposit, with the router as `from`.
///
///         Sweeping is open to everyone and harmless: it can only move what
///         sits at the address derived from `to` into the vault credited to
///         that same `to`. There is no owner, no pause and no other entry
///         point; the vault's own allowlist, minimum deposit and pause apply.
///         A sweep the vault refuses reverts as a whole, so the funds stay at
///         the deposit address until a later sweep succeeds (for instance
///         once a second transfer lifts the balance above the minimum).
/// @dev The salt is the exact bytes of `to`, so the daemon and the page must
///      derive the address from the canonical (lowercase bech32m) form. The
///      address is
///      `address(uint160(uint256(keccak256(0xff ‖ router ‖ keccak256(bytes(to)) ‖ forwarderCodeHash()))))`
///      and can be computed off-chain without a call. Token calls tolerate
///      mainnet USDT, which returns nothing from `transfer` and `approve`
///      and refuses to change a non-zero allowance to another non-zero one.
contract DepositRouter {
    address public immutable vault;

    /// @param forwarder the deposit address swept, `depositAddress(to)`.
    /// @param amount the gross amount handed to the vault; the vault's
    ///        `Deposited` event with id `depositId` carries the net and fee.
    event Swept(address indexed token, address indexed forwarder, uint256 amount, uint256 depositId, string to);

    error BadVault();
    error BadDestination();
    error NothingToSweep();
    error TransferFailed();

    /// @param _vault the Vault deposits are made into; must be a contract.
    constructor(address _vault) {
        if (_vault == address(0) || _vault.code.length == 0) revert BadVault();
        vault = _vault;
    }

    /// @notice The deposit address of the Argonmine address `to` on this
    ///         chain: the CREATE2 address of its `Forwarder`, deployed or not.
    ///         Reverts with `BadDestination` for a `to` that `sweep` would
    ///         refuse (empty or longer than 128 bytes), so the contract never
    ///         hands out an address whose funds could not be swept.
    function depositAddress(string calldata to) public view returns (address) {
        uint256 len = bytes(to).length;
        if (len == 0 || len > 128) revert BadDestination();
        bytes32 h = keccak256(abi.encodePacked(bytes1(0xff), address(this), keccak256(bytes(to)), forwarderCodeHash()));
        return address(uint160(uint256(h)));
    }

    /// @notice Deposit everything of `token` held at `depositAddress(to)`
    ///         into the vault, credited to `to`. Deploys the forwarder on the
    ///         first sweep of that address. Anyone may call it. Reverts with
    ///         `NothingToSweep` when the deposit address holds none of
    ///         `token`, and with the vault's error (`TokenNotAllowed`,
    ///         `AmountTooSmall`, `IsPaused`, ...) when the vault refuses the
    ///         deposit; the funds then stay where they are.
    /// @return id the vault's deposit id.
    function sweep(address token, string calldata to) external returns (uint256 id) {
        address forwarder = depositAddress(to); // checks `to`
        if (forwarder.code.length == 0) new Forwarder{salt: keccak256(bytes(to))}();
        uint256 amount = Forwarder(forwarder).pull(token);
        if (amount == 0) revert NothingToSweep();
        // Mainnet USDT refuses to change a non-zero allowance to another
        // non-zero one. `deposit` consumes the approval in full, so the
        // allowance is normally zero already.
        if (IToken(token).allowance(address(this), vault) != 0) {
            _call(token, abi.encodeWithSelector(0x095ea7b3, vault, 0)); // approve
        }
        _call(token, abi.encodeWithSelector(0x095ea7b3, vault, amount)); // approve
        id = IVault(vault).deposit(token, amount, to); // bubbles the vault's revert
        // The id is only known after the deposit; the vault calls out only to the token.
        // forge-lint: disable-next-line(reentrancy-events)
        emit Swept(token, forwarder, amount, id, to);
    }

    /// @notice keccak256 of the Forwarder's creation code, the init code hash
    ///         in the CREATE2 address (the forwarder takes no constructor
    ///         arguments), so off-chain code can derive deposit addresses.
    function forwarderCodeHash() public pure returns (bytes32) {
        return keccak256(type(Forwarder).creationCode);
    }

    /// @dev Token call with SafeERC20 semantics: the call must succeed and
    ///      return either nothing (mainnet USDT) or the boolean true. `token`
    ///      has code: the forwarder's `balanceOf` call already decoded.
    function _call(address token, bytes memory data) internal {
        (bool ok, bytes memory ret) = token.call(data);
        if (!ok || (ret.length != 0 && (ret.length < 32 || !abi.decode(ret, (bool))))) revert TransferFailed();
    }
}

/// @title Forwarder: the contract behind one deposit address.
/// @notice Deployed by the router with CREATE2 on the first sweep of its
///         address and kept minimal, since there is one per Argonmine address
///         per chain. Only the router can move what it holds, and only to the
///         router, which deposits it into the vault in the same transaction.
///         It does not accept the chain's native coin once deployed; native
///         coin sent to the address before that cannot be recovered.
contract Forwarder {
    address public immutable router;

    error NotRouter();
    error TransferFailed();

    constructor() {
        router = msg.sender;
    }

    /// @notice Send this forwarder's whole balance of `token` to the router.
    /// @return amount what was sent, 0 when there was nothing.
    // forge-lint: disable-next-item(missing-zero-check)
    function pull(address token) external returns (uint256 amount) {
        if (msg.sender != router) revert NotRouter();
        amount = IToken(token).balanceOf(address(this));
        if (amount != 0) {
            (bool ok, bytes memory ret) = token.call(abi.encodeWithSelector(0xa9059cbb, msg.sender, amount)); // transfer
            if (!ok || (ret.length != 0 && (ret.length < 32 || !abi.decode(ret, (bool))))) revert TransferFailed();
        }
    }
}
