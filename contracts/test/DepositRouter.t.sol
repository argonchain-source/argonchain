// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Test.sol";
import "../Vault.sol";
import "../DepositRouter.sol";
import "../MockUSDC.sol";
import "../MockUSDT.sol";
import "../MockUSD18.sol";

/// Like mainnet USDT: returns nothing from `transfer` and `approve`, and
/// `approve` refuses to change a non-zero allowance to another non-zero one.
contract StrictApproveToken {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    function mint(address to, uint256 value) external {
        balanceOf[to] += value;
    }

    function transfer(address to, uint256 value) external {
        balanceOf[msg.sender] -= value;
        balanceOf[to] += value;
    }

    function approve(address spender, uint256 value) external {
        require(value == 0 || allowance[msg.sender][spender] == 0, "approve from non-zero");
        allowance[msg.sender][spender] = value;
    }

    function transferFrom(address from, address to, uint256 value) external {
        allowance[from][msg.sender] -= value;
        balanceOf[from] -= value;
        balanceOf[to] += value;
    }
}

/// A token whose `transfer` reports failure with `false` instead of reverting.
contract FalseTransferToken {
    function transfer(address, uint256) external pure returns (bool) {
        return false;
    }

    function balanceOf(address) external pure returns (uint256) {
        return 1e6;
    }
}

contract DepositRouterTest is Test {
    MockUSDC usdc;
    MockUSDT usdt;
    MockUSD18 usd18;
    Vault vault;
    DepositRouter router;
    address user = address(0xBEEF);
    address stranger = address(0xD00D);
    string constant DEST = "targ1qexampleaddress";
    string constant OTHER = "targ1qanotheraddress";

    function setUp() public {
        address[] memory gs = new address[](3);
        gs[0] = vm.addr(0xA11CE);
        gs[1] = vm.addr(0xB0B);
        gs[2] = vm.addr(0xC0FFEE);
        usdc = new MockUSDC();
        usdt = new MockUSDT();
        usd18 = new MockUSD18();
        vault = new Vault(gs, 2, 1 days, 1);
        vault.setToken(address(usdc), true, 1e6);
        vault.setToken(address(usdt), true, 1e6);
        vault.setToken(address(usd18), true, 1e18);
        vault.setFees(100, 100);
        router = new DepositRouter(address(vault));
        usdc.mint(user, 1000e6);
        usdt.mint(user, 1000e6);
        usd18.mint(user, 1000e18);
    }

    // ---- helpers ----

    /// The CREATE2 formula the daemon uses, computed without calling the router.
    function offchain(address deployer, string memory to) internal pure returns (address) {
        bytes32 h = keccak256(
            abi.encodePacked(bytes1(0xff), deployer, keccak256(bytes(to)), keccak256(type(Forwarder).creationCode))
        );
        return address(uint160(uint256(h)));
    }

    /// The vault-side checks after deposits of `gross` in total, with the 1 percent fee.
    function assertBacked(address token, uint256 gross) internal view {
        uint256 fee = gross / 100;
        assertEq(vault.reserve(token), gross);
        assertEq(vault.obligations(token), gross - fee);
        assertEq(vault.surplus(token), fee);
        assertEq(IToken(token).balanceOf(address(router)), 0);
        assertEq(IToken(token).allowance(address(router), address(vault)), 0);
    }

    // ---- deposit addresses ----

    function testConstructor() public {
        assertEq(router.vault(), address(vault));
        vm.expectRevert(DepositRouter.BadVault.selector);
        new DepositRouter(address(0));
        vm.expectRevert(DepositRouter.BadVault.selector);
        new DepositRouter(user);
    }

    function testDepositAddressIsDeterministicAndDiffersPerDestination() public view {
        address a = router.depositAddress(DEST);
        assertEq(router.depositAddress(DEST), a);
        assertTrue(a != address(0));
        assertTrue(router.depositAddress(OTHER) != a);
        assertTrue(router.depositAddress("targ1qexampleaddresS") != a); // the exact bytes, case included
        assertEq(a.code.length, 0); // nothing deployed until the first sweep
    }

    function testDepositAddressMatchesOffchainFormula() public view {
        assertEq(router.forwarderCodeHash(), keccak256(type(Forwarder).creationCode));
        assertEq(router.depositAddress(DEST), offchain(address(router), DEST));
        assertEq(router.depositAddress(OTHER), offchain(address(router), OTHER));
        assertEq(router.depositAddress("x"), offchain(address(router), "x"));
    }

    function testFuzzDepositAddressMatchesOffchainFormula(bytes memory to) public {
        if (to.length == 0 || to.length > 128) {
            vm.expectRevert(DepositRouter.BadDestination.selector);
            router.depositAddress(string(to));
        } else {
            assertEq(router.depositAddress(string(to)), offchain(address(router), string(to)));
        }
    }

    function testDepositAddressDiffersPerRouter() public {
        DepositRouter other = new DepositRouter(address(vault));
        assertTrue(other.depositAddress(DEST) != router.depositAddress(DEST));
        assertEq(other.depositAddress(DEST), offchain(address(other), DEST));
    }

    function testBadDestination() public {
        vm.expectRevert(DepositRouter.BadDestination.selector);
        router.depositAddress("");
        vm.expectRevert(DepositRouter.BadDestination.selector);
        router.depositAddress(string(new bytes(129)));
        vm.expectRevert(DepositRouter.BadDestination.selector);
        router.sweep(address(usdc), "");
        vm.expectRevert(DepositRouter.BadDestination.selector);
        router.sweep(address(usdc), string(new bytes(129)));

        // 128 bytes is the longest destination, as in the vault.
        string memory longest = string(new bytes(128));
        address a = router.depositAddress(longest);
        vm.prank(user);
        usdc.transfer(a, 2e6);
        router.sweep(address(usdc), longest);
        assertEq(usdc.balanceOf(a), 0);
    }

    // ---- sweeps ----

    function testSweepUsdc() public {
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usdc.transfer(a, 10e6);

        vm.expectEmit(true, true, true, true, address(vault));
        emit Vault.Deposited(1, address(usdc), address(router), 99e5, 1e5, DEST);
        vm.expectEmit(true, true, false, true, address(router));
        emit DepositRouter.Swept(address(usdc), a, 10e6, 1, DEST);
        vm.prank(stranger);
        uint256 id = router.sweep(address(usdc), DEST);

        assertEq(id, 1);
        assertEq(vault.depositCount(), 1);
        assertEq(usdc.balanceOf(a), 0);
        assertBacked(address(usdc), 10e6);
        assertTrue(a.code.length > 0);
        assertEq(Forwarder(a).router(), address(router));
    }

    function testSecondDepositSweepsAgain() public {
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usdc.transfer(a, 10e6);
        router.sweep(address(usdc), DEST);
        bytes32 code = a.codehash;

        // The forwarder is deployed now; the next transfer is swept through it.
        vm.prank(user);
        usdc.transfer(a, 5e6);
        vm.expectEmit(true, true, true, true, address(vault));
        emit Vault.Deposited(2, address(usdc), address(router), 495e4, 5e4, DEST);
        vm.expectEmit(true, true, false, true, address(router));
        emit DepositRouter.Swept(address(usdc), a, 5e6, 2, DEST);
        assertEq(router.sweep(address(usdc), DEST), 2);
        assertEq(a.codehash, code);
        assertEq(usdc.balanceOf(a), 0);
        assertBacked(address(usdc), 15e6);

        // A second destination gets its own forwarder and deposit id.
        address b = router.depositAddress(OTHER);
        vm.prank(user);
        usdc.transfer(b, 2e6);
        vm.expectEmit(true, true, true, true, address(vault));
        emit Vault.Deposited(3, address(usdc), address(router), 198e4, 2e4, OTHER);
        assertEq(router.sweep(address(usdc), OTHER), 3);
        assertBacked(address(usdc), 17e6);
    }

    function testSweepNonStandardUsdt() public {
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usdt.transfer(a, 7e6);
        vm.expectEmit(true, true, true, true, address(vault));
        emit Vault.Deposited(1, address(usdt), address(router), 693e4, 7e4, DEST);
        vm.expectEmit(true, true, false, true, address(router));
        emit DepositRouter.Swept(address(usdt), a, 7e6, 1, DEST);
        assertEq(router.sweep(address(usdt), DEST), 1);
        assertEq(usdt.balanceOf(a), 0);
        assertBacked(address(usdt), 7e6);

        vm.prank(user);
        usdt.transfer(a, 3e6);
        assertEq(router.sweep(address(usdt), DEST), 2);
        assertBacked(address(usdt), 10e6);
    }

    function testSweepEighteenDecimals() public {
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usd18.transfer(a, 25e18);
        vm.expectEmit(true, true, true, true, address(vault));
        emit Vault.Deposited(1, address(usd18), address(router), 2475e16, 25e16, DEST);
        vm.expectEmit(true, true, false, true, address(router));
        emit DepositRouter.Swept(address(usd18), a, 25e18, 1, DEST);
        assertEq(router.sweep(address(usd18), DEST), 1);
        assertEq(usd18.balanceOf(a), 0);
        assertBacked(address(usd18), 25e18);
    }

    function testTwoTokensAtOneAddress() public {
        address a = router.depositAddress(DEST);
        vm.startPrank(user);
        usdc.transfer(a, 4e6);
        usdt.transfer(a, 6e6);
        vm.stopPrank();
        assertEq(router.sweep(address(usdt), DEST), 1);
        assertEq(usdc.balanceOf(a), 4e6); // only the token named is swept
        assertEq(router.sweep(address(usdc), DEST), 2);
        assertBacked(address(usdc), 4e6);
        assertBacked(address(usdt), 6e6);
    }

    function testSweepBelowMinimumRevertsAndFundsStay() public {
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usdc.transfer(a, 5e5); // half the 1 USDC minimum
        vm.expectRevert(Vault.AmountTooSmall.selector);
        router.sweep(address(usdc), DEST);
        assertEq(usdc.balanceOf(a), 5e5);
        assertEq(a.code.length, 0); // the deployment was reverted too
        assertEq(vault.depositCount(), 0);

        // A second transfer lifts the balance above the minimum; both are swept.
        vm.prank(user);
        usdc.transfer(a, 5e5);
        assertEq(router.sweep(address(usdc), DEST), 1);
        assertEq(usdc.balanceOf(a), 0);
        assertBacked(address(usdc), 1e6);

        // Below the minimum with the forwarder already deployed: same outcome.
        vm.prank(user);
        usdc.transfer(a, 1);
        vm.expectRevert(Vault.AmountTooSmall.selector);
        router.sweep(address(usdc), DEST);
        assertEq(usdc.balanceOf(a), 1);
    }

    function testDisallowedTokenReverts() public {
        address a = router.depositAddress(DEST);
        vault.setToken(address(usd18), false, 0);
        vm.prank(user);
        usd18.transfer(a, 5e18);
        vm.expectRevert(Vault.TokenNotAllowed.selector);
        router.sweep(address(usd18), DEST);
        assertEq(usd18.balanceOf(a), 5e18);

        // Once allowed again the same funds can be swept.
        vault.setToken(address(usd18), true, 1e18);
        assertEq(router.sweep(address(usd18), DEST), 1);
        assertEq(usd18.balanceOf(a), 0);
    }

    function testPausedVaultReverts() public {
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usdc.transfer(a, 2e6);
        vm.prank(vm.addr(0xB0B)); // any guardian can pause
        vault.pause();
        vm.expectRevert(Vault.IsPaused.selector);
        router.sweep(address(usdc), DEST);
        assertEq(usdc.balanceOf(a), 2e6);
        vault.proposeUnpause();
        vm.warp(vm.getBlockTimestamp() + 1 days);
        vault.executeUnpause();
        router.sweep(address(usdc), DEST);
        assertEq(usdc.balanceOf(a), 0);
    }

    function testSweepIntoFinalizedVaultFollowsTimelockedChanges() public {
        vault.finalizeSetup();
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usd18.transfer(a, 5e18);

        // Disallowing usd18 takes the delay; until then sweeps still work.
        vault.proposeToken(address(usd18), false, 0);
        assertEq(router.sweep(address(usd18), DEST), 1);
        vm.warp(vm.getBlockTimestamp() + 1 days);
        vault.executeToken(address(usd18), false, 0);
        vm.prank(user);
        usd18.transfer(a, 5e18);
        vm.expectRevert(Vault.TokenNotAllowed.selector);
        router.sweep(address(usd18), DEST);

        // Allowed again with a higher minimum through the timelock: the
        // waiting funds are swept once they reach it.
        vault.proposeToken(address(usd18), true, 6e18);
        vm.warp(vm.getBlockTimestamp() + 1 days);
        vault.executeToken(address(usd18), true, 6e18);
        vm.expectRevert(Vault.AmountTooSmall.selector);
        router.sweep(address(usd18), DEST);
        vm.prank(user);
        usd18.transfer(a, 1e18);
        assertEq(router.sweep(address(usd18), DEST), 2);
        assertBacked(address(usd18), 11e18);
    }

    function testNothingToSweepReverts() public {
        // Forwarder not deployed yet.
        vm.expectRevert(DepositRouter.NothingToSweep.selector);
        router.sweep(address(usdc), DEST);
        assertEq(router.depositAddress(DEST).code.length, 0);

        // Forwarder deployed and emptied.
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usdc.transfer(a, 2e6);
        router.sweep(address(usdc), DEST);
        vm.expectRevert(DepositRouter.NothingToSweep.selector);
        router.sweep(address(usdc), DEST);
        vm.expectRevert(DepositRouter.NothingToSweep.selector);
        router.sweep(address(usdt), DEST);
    }

    function testFuzzAnyoneCanSweep(address caller, uint256 amount) public {
        amount = bound(amount, 1e6, 1000e6);
        address a = router.depositAddress(DEST);
        vm.assume(caller != a && caller != address(vault));
        vm.prank(user);
        usdc.transfer(a, amount);
        uint256 before = usdc.balanceOf(caller);
        vm.prank(caller);
        assertEq(router.sweep(address(usdc), DEST), 1);
        assertEq(usdc.balanceOf(a), 0);
        assertEq(usdc.balanceOf(caller), before); // the caller neither gains nor pays tokens
        assertBacked(address(usdc), amount);
    }

    function testOnlyRouterCanPull() public {
        address a = router.depositAddress(DEST);
        vm.prank(user);
        usdc.transfer(a, 3e6);
        vm.prank(user);
        usdt.transfer(a, 1e6);
        router.sweep(address(usdt), DEST); // deploys the forwarder

        vm.prank(stranger);
        vm.expectRevert(Forwarder.NotRouter.selector);
        Forwarder(a).pull(address(usdc));
        vm.prank(address(vault));
        vm.expectRevert(Forwarder.NotRouter.selector);
        Forwarder(a).pull(address(usdc));
        vm.expectRevert(Forwarder.NotRouter.selector);
        Forwarder(a).pull(address(usdc));
        assertEq(usdc.balanceOf(a), 3e6);

        // The router itself pulls the whole balance (0 when there is none).
        vm.prank(address(router));
        assertEq(Forwarder(a).pull(address(usdc)), 3e6);
        assertEq(usdc.balanceOf(address(router)), 3e6);
        vm.prank(address(router));
        assertEq(Forwarder(a).pull(address(usdc)), 0);
    }

    function testResetsLeftoverAllowanceForUsdtStyleApprove() public {
        StrictApproveToken strict = new StrictApproveToken();
        vault.setToken(address(strict), true, 1e6);
        strict.mint(user, 100e6);
        // A leftover allowance, which `deposit` never leaves behind on its own.
        vm.prank(address(router));
        strict.approve(address(vault), 5);
        address a = router.depositAddress(DEST);
        vm.prank(user);
        strict.transfer(a, 3e6);
        assertEq(router.sweep(address(strict), DEST), 1);
        assertEq(strict.allowance(address(router), address(vault)), 0);
        assertEq(vault.obligations(address(strict)), 297e4);
    }

    function testTokenReturningFalseReverts() public {
        FalseTransferToken bad = new FalseTransferToken();
        vault.setToken(address(bad), true, 0);
        vm.expectRevert(Forwarder.TransferFailed.selector);
        router.sweep(address(bad), DEST);
        vm.expectRevert(); // not a token at all: balanceOf has nothing to decode
        router.sweep(stranger, DEST);
    }
}
