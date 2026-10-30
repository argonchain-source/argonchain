// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Test.sol";
import "../Vault.sol";
import "../MockUSDC.sol";
import "../MockUSDT.sol";
import "../MockUSD18.sol";

/// A token whose transfers report failure with `false` instead of reverting.
contract FalseToken {
    function transfer(address, uint256) external pure returns (bool) {
        return false;
    }

    function transferFrom(address, address, uint256) external pure returns (bool) {
        return false;
    }

    function balanceOf(address) external pure returns (uint256) {
        return 0;
    }
}

contract VaultTest is Test {
    MockUSDC usdc;
    MockUSDT usdt;
    MockUSD18 usd18;
    Vault vault;
    uint256 constant G1 = 0xA11CE;
    uint256 constant G2 = 0xB0B;
    uint256 constant G3 = 0xC0FFEE;
    address g1;
    address g2;
    address g3;
    address user = address(0xBEEF);
    address stranger = address(0xD00D);
    address treasury = address(0x7EA5);
    string constant DEST = "targ1qexampleaddress";
    uint256 constant DELAY = 2 days;
    uint256 constant START = 1_800_000_000; // a realistic block timestamp

    function setUp() public {
        vm.warp(START);
        g1 = vm.addr(G1);
        g2 = vm.addr(G2);
        g3 = vm.addr(G3);
        address[] memory gs = new address[](3);
        gs[0] = g1;
        gs[1] = g2;
        gs[2] = g3;
        usdc = new MockUSDC();
        usdt = new MockUSDT();
        usd18 = new MockUSD18();
        vault = new Vault(gs, 2, DELAY, 1);
        vault.setToken(address(usdc), true, 1e6);
        vault.setToken(address(usdt), true, 2e6);
        usdc.mint(user, 100e6);
        usdt.mint(user, 100e6);
        usd18.mint(user, 100e18);
    }

    // ---- helpers ----

    function sign(uint256 key, bytes32 digest) internal pure returns (bytes memory) {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(key, digest);
        return abi.encodePacked(r, s, v);
    }

    function sorted(uint256 ka, uint256 kb, bytes32 digest) internal pure returns (bytes[] memory sigs) {
        sigs = new bytes[](2);
        if (vm.addr(ka) < vm.addr(kb)) {
            sigs[0] = sign(ka, digest);
            sigs[1] = sign(kb, digest);
        } else {
            sigs[0] = sign(kb, digest);
            sigs[1] = sign(ka, digest);
        }
    }

    function depositUsdc(uint256 amount) internal returns (uint256 id) {
        vm.startPrank(user);
        usdc.approve(address(vault), amount);
        id = vault.deposit(address(usdc), amount, DEST);
        vm.stopPrank();
    }

    function depositUsdt(uint256 amount) internal returns (uint256 id) {
        vm.startPrank(user);
        usdt.approve(address(vault), amount);
        id = vault.deposit(address(usdt), amount, DEST);
        vm.stopPrank();
    }

    function depositUsd18(uint256 amount) internal returns (uint256 id) {
        vm.startPrank(user);
        usd18.approve(address(vault), amount);
        id = vault.deposit(address(usd18), amount, DEST);
        vm.stopPrank();
    }

    /// g1 and g2's signatures for a release of `gross` of `token` to `stranger`.
    function sigsFor(address token, uint256 gross, bytes32 burn) internal view returns (bytes[] memory) {
        return sorted(G1, G2, vault.releaseDigest(token, stranger, gross, burn));
    }

    /// A release of `gross` of `token` to `stranger` signed by g1 and g2.
    function releaseTo(address token, uint256 gross, bytes32 burn) internal {
        vault.release(token, stranger, gross, burn, sigsFor(token, gross, burn));
    }

    /// Like `releaseTo`, expecting the release to revert with `err`.
    function releaseReverts(address token, uint256 gross, bytes32 burn, bytes4 err) internal {
        bytes[] memory sigs = sigsFor(token, gross, burn);
        vm.expectRevert(err);
        vault.release(token, stranger, gross, burn, sigs);
    }

    function pair(address a, address b) internal pure returns (address[] memory list) {
        list = new address[](2);
        list[0] = a;
        list[1] = b;
    }

    /// Waits out the timelock delay.
    function waitDelay() internal {
        vm.warp(vm.getBlockTimestamp() + DELAY);
    }

    // ---- construction and allowlist ----

    function testConstructorSetsOwnerAndGuardians() public view {
        assertEq(vault.owner(), address(this));
        assertEq(vault.pendingOwner(), address(0));
        assertEq(vault.guardianCount(), 3);
        assertEq(vault.threshold(), 2);
        assertEq(vault.guardians(0), g1);
        assertEq(vault.guardians(2), g3);
        assertTrue(vault.isGuardian(g2));
        assertFalse(vault.isGuardian(stranger));
        assertEq(vault.maxRelease(address(usdc)), 0);
        assertEq(vault.periodLimit(address(usdc)), 0);
        assertEq(vault.periodAvailable(address(usdc)), type(uint256).max);
        assertEq(vault.depositCount(), 0);
        assertEq(vault.firstDepositId(), 1);
        assertEq(vault.MAX_FIRST_DEPOSIT_ID(), 2 ** 62);
        assertFalse(vault.paused());
        assertFalse(vault.finalized());
        assertEq(vault.delay(), DELAY);
        assertEq(vault.depositFeeBps(), 0);
        assertEq(vault.releaseFeeBps(), 0);
        assertEq(vault.MAX_FEE_BPS(), 1000);
        assertEq(vault.MAX_RELEASE_FEE_STEP_BPS(), 100);
        assertEq(vault.PERIOD(), 1 days);
        assertEq(vault.MIN_DELAY(), 1 days);
        assertEq(vault.MAX_DELAY(), 30 days);
        assertEq(vault.GRACE_PERIOD(), 14 days);
        assertEq(vault.obligations(address(usdc)), 0);
        assertEq(vault.surplus(address(usdc)), 0);
    }

    function testConstructorRejectsBadDelayAndGuardians() public {
        address[] memory gs = pair(g1, g2);
        vm.expectRevert(Vault.BadDelay.selector);
        new Vault(gs, 2, 0, 1);
        vm.expectRevert(Vault.BadDelay.selector);
        new Vault(gs, 2, 1 days - 1, 1);
        vm.expectRevert(Vault.BadDelay.selector);
        new Vault(gs, 2, 30 days + 1, 1);
        vm.expectRevert(Vault.BadGuardians.selector);
        new Vault(gs, 3, 1 days, 1);
        vm.expectRevert(Vault.BadGuardians.selector);
        new Vault(pair(g1, g1), 1, 1 days, 1);

        // Exactly the bounds pass.
        vm.expectEmit(false, false, false, true);
        emit Vault.DelaySet(1 days);
        Vault v = new Vault(gs, 2, 1 days, 1);
        assertEq(v.delay(), 1 days);
        assertEq(new Vault(gs, 1, 30 days, 1).delay(), 30 days);
    }

    // A replacement vault numbers its deposits from its firstDepositId, so a
    // deposit stays named by its chain and id alone across vaults.
    function testFirstDepositId() public {
        address[] memory gs = pair(g1, g2);
        vm.expectRevert(Vault.BadFirstDepositId.selector);
        new Vault(gs, 2, 1 days, 0);
        vm.expectRevert(Vault.BadFirstDepositId.selector);
        new Vault(gs, 2, 1 days, 2 ** 62 + 1);
        assertEq(new Vault(gs, 2, 1 days, 2 ** 62).depositCount(), 2 ** 62 - 1);

        Vault v = new Vault(gs, 2, 1 days, 1001);
        assertEq(v.firstDepositId(), 1001);
        assertEq(v.depositCount(), 1000);
        v.setToken(address(usdc), true, 1e6);
        vm.startPrank(user);
        usdc.approve(address(v), 3e6);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1001, address(usdc), user, 1e6, 0, DEST);
        assertEq(v.deposit(address(usdc), 1e6, DEST), 1001);
        assertEq(v.deposit(address(usdc), 2e6, DEST), 1002);
        vm.stopPrank();
        assertEq(v.depositCount(), 1002);
        assertEq(v.firstDepositId(), 1001);
        assertEq(v.obligations(address(usdc)), 3e6);
    }

    function testAllowlistAndMinDepositPerToken() public {
        address[] memory list = vault.tokens();
        assertEq(list.length, 2);
        assertEq(list[0], address(usdc));
        assertEq(list[1], address(usdt));
        assertTrue(vault.isAllowed(address(usdc)));
        assertTrue(vault.isAllowed(address(usdt)));
        assertFalse(vault.isAllowed(address(usd18)));
        assertEq(vault.minDeposit(address(usdc)), 1e6);
        assertEq(vault.minDeposit(address(usdt)), 2e6);
        assertEq(vault.minDeposit(address(usd18)), 0);

        vm.expectEmit(true, false, false, true);
        emit Vault.TokenSet(address(usd18), true, 1e18);
        vault.setToken(address(usd18), true, 1e18);
        list = vault.tokens();
        assertEq(list.length, 3);
        assertEq(list[2], address(usd18));
        assertEq(vault.minDeposit(address(usd18)), 1e18);

        // Disallowing drops the token from tokens() but keeps its slot.
        vm.expectEmit(true, false, false, true);
        emit Vault.TokenSet(address(usdt), false, 0);
        vault.setToken(address(usdt), false, 0);
        assertFalse(vault.isAllowed(address(usdt)));
        list = vault.tokens();
        assertEq(list.length, 2);
        assertEq(list[0], address(usdc));
        assertEq(list[1], address(usd18));

        // Allowing it again keeps the original insertion order, no duplicate.
        vault.setToken(address(usdt), true, 5e6);
        list = vault.tokens();
        assertEq(list.length, 3);
        assertEq(list[0], address(usdc));
        assertEq(list[1], address(usdt));
        assertEq(list[2], address(usd18));
        assertEq(vault.minDeposit(address(usdt)), 5e6);

        // Changing the minimum alone keeps the list as it is.
        vault.setToken(address(usdc), true, 3e6);
        assertEq(vault.tokens().length, 3);
        assertEq(vault.minDeposit(address(usdc)), 3e6);
    }

    function testSetTokenRejectsBadTokenAndStranger() public {
        vm.expectRevert(Vault.BadToken.selector);
        vault.setToken(address(0), true, 1);
        vm.expectRevert(Vault.BadToken.selector);
        vault.setToken(stranger, true, 1); // an account without code
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.setToken(address(usd18), true, 1);

        // The timelocked path checks the token when proposing.
        vm.expectRevert(Vault.BadToken.selector);
        vault.proposeToken(address(0), true, 1);
        vm.expectRevert(Vault.BadToken.selector);
        vault.proposeToken(stranger, true, 1);
    }

    // ---- deposits ----

    function testDepositIdsAndEventsForTwoTokens() public {
        vm.startPrank(user);
        usdc.approve(address(vault), 5e6);
        usdt.approve(address(vault), 3e6);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1, address(usdc), user, 5e6, 0, DEST);
        uint256 id1 = vault.deposit(address(usdc), 5e6, DEST);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(2, address(usdt), user, 3e6, 0, DEST);
        uint256 id2 = vault.deposit(address(usdt), 3e6, DEST);
        vm.stopPrank();
        assertEq(id1, 1);
        assertEq(id2, 2);
        assertEq(vault.depositCount(), 2);
        assertEq(vault.reserve(address(usdc)), 5e6);
        assertEq(vault.reserve(address(usdt)), 3e6);
        assertEq(vault.obligations(address(usdc)), 5e6);
        assertEq(vault.obligations(address(usdt)), 3e6);
        assertEq(usdc.balanceOf(user), 95e6);
        assertEq(usdt.balanceOf(user), 97e6);
        assertEq(depositUsdc(1e6), 3);
    }

    function testDepositDisallowedTokenReverts() public {
        vm.startPrank(user);
        usd18.approve(address(vault), 5e18);
        vm.expectRevert(Vault.TokenNotAllowed.selector);
        vault.deposit(address(usd18), 5e18, DEST);
        vm.stopPrank();

        vault.setToken(address(usdt), false, 0);
        vm.startPrank(user);
        usdt.approve(address(vault), 5e6);
        vm.expectRevert(Vault.TokenNotAllowed.selector);
        vault.deposit(address(usdt), 5e6, DEST);
        vm.stopPrank();

        vm.prank(user);
        vm.expectRevert(Vault.TokenNotAllowed.selector);
        vault.deposit(stranger, 5e6, DEST);
    }

    function testDepositBelowMinimumPerToken() public {
        vm.startPrank(user);
        usdc.approve(address(vault), 10e6);
        usdt.approve(address(vault), 10e6);
        vm.expectRevert(Vault.AmountTooSmall.selector);
        vault.deposit(address(usdc), 5e5, DEST);
        vm.expectRevert(Vault.AmountTooSmall.selector);
        vault.deposit(address(usdt), 15e5, DEST); // USDT minimum is 2 tokens
        vault.deposit(address(usdc), 1e6, DEST); // exactly the minimum passes
        vault.deposit(address(usdt), 2e6, DEST);
        vm.stopPrank();
        vault.setToken(address(usdc), true, 0);
        vm.prank(user);
        vm.expectRevert(Vault.AmountTooSmall.selector);
        vault.deposit(address(usdc), 0, DEST); // zero is never a deposit

        // The minimum is checked on the gross amount: 1 USDC passes with a
        // 1 percent fee even though only 0.99 is credited.
        vault.setToken(address(usdc), true, 1e6);
        vault.setFees(100, 0);
        vm.startPrank(user);
        usdc.approve(address(vault), 1e6);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(3, address(usdc), user, 99e4, 1e4, DEST);
        vault.deposit(address(usdc), 1e6, DEST);
        vm.stopPrank();
    }

    function testDepositBadDestination() public {
        vm.startPrank(user);
        usdc.approve(address(vault), 10e6);
        vm.expectRevert(Vault.BadDestination.selector);
        vault.deposit(address(usdc), 1e6, "");
        bytes memory long = new bytes(129);
        vm.expectRevert(Vault.BadDestination.selector);
        vault.deposit(address(usdc), 1e6, string(long));
        vault.deposit(address(usdc), 1e6, string(new bytes(128)));
        vault.deposit(address(usdc), 1e6, "x");
        vm.stopPrank();
    }

    function testDepositTransferFailureReverts() public {
        // No allowance: the token reverts, the vault reports TransferFailed.
        vm.prank(user);
        vm.expectRevert(Vault.TransferFailed.selector);
        vault.deposit(address(usdc), 1e6, DEST);
        // A token answering `false` is treated the same way.
        FalseToken bad = new FalseToken();
        vault.setToken(address(bad), true, 0);
        vm.prank(user);
        vm.expectRevert(Vault.TransferFailed.selector);
        vault.deposit(address(bad), 1e6, DEST);
        // A failed deposit leaves no obligation behind.
        assertEq(vault.obligations(address(usdc)), 0);
        assertEq(vault.obligations(address(bad)), 0);
        assertEq(vault.depositCount(), 0);
    }

    // ---- fees ----

    function testSetFeesBoundsAndOnlyOwner() public {
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.setFees(100, 100);
        vm.expectRevert(Vault.BadFee.selector);
        vault.setFees(1001, 0);
        vm.expectRevert(Vault.BadFee.selector);
        vault.setFees(0, 1001);
        vm.expectRevert(Vault.BadFee.selector);
        vault.setFees(type(uint16).max, type(uint16).max);
        assertEq(vault.depositFeeBps(), 0);
        assertEq(vault.releaseFeeBps(), 0);

        // The deposit fee may jump to the bound at once; the release fee
        // climbs to it in steps of at most 100 bps.
        vm.expectEmit(false, false, false, true);
        emit Vault.FeesSet(1000, 100);
        vault.setFees(1000, 100);
        for (uint16 r = 200; r <= 1000; r += 100) {
            vault.setFees(1000, r);
        }
        assertEq(vault.depositFeeBps(), 1000);
        assertEq(vault.releaseFeeBps(), 1000); // exactly the bound passes
        vm.expectRevert(Vault.BadFee.selector);
        vault.setFees(1000, 1001);

        vm.expectEmit(false, false, false, true);
        emit Vault.FeesSet(100, 25);
        vault.setFees(100, 25);
        assertEq(vault.depositFeeBps(), 100);
        assertEq(vault.releaseFeeBps(), 25);

        vault.setFees(0, 0);
        assertEq(vault.depositFeeBps(), 0);
        assertEq(vault.releaseFeeBps(), 0);
    }

    function testReleaseFeeStepCap() public {
        // Up by at most 100 bps per change, from whatever the fee is now.
        vm.expectRevert(Vault.FeeStepTooLarge.selector);
        vault.setFees(0, 101);
        vault.setFees(0, 100);
        vm.expectRevert(Vault.FeeStepTooLarge.selector);
        vault.setFees(0, 201);
        vault.setFees(0, 200);
        assertEq(vault.releaseFeeBps(), 200);
        // Down by any amount, and back up from there in steps again.
        vault.setFees(0, 0);
        vm.expectRevert(Vault.FeeStepTooLarge.selector);
        vault.setFees(0, 200);
        // The deposit fee is not stepped (it is known before depositing).
        vault.setFees(1000, 0);
        assertEq(vault.depositFeeBps(), 1000);

        // The same cap holds for proposals, when proposing and when executing.
        vault.finalizeSetup();
        vm.expectRevert(Vault.FeeStepTooLarge.selector);
        vault.proposeFees(0, 101);
        vault.proposeFees(0, 100);
        waitDelay();
        vault.executeFees(0, 100);
        assertEq(vault.releaseFeeBps(), 100);
    }

    function testDepositFeeMath() public {
        vault.setFees(100, 100);
        vm.startPrank(user);
        usdc.approve(address(vault), 100e6);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1, address(usdc), user, 99e6, 1e6, DEST);
        uint256 id = vault.deposit(address(usdc), 100e6, DEST);
        vm.stopPrank();
        assertEq(id, 1);
        assertEq(usdc.balanceOf(user), 0); // the gross amount was pulled
        assertEq(vault.reserve(address(usdc)), 100e6); // balance 100
        assertEq(vault.obligations(address(usdc)), 99e6); // backs 99 uARG
        assertEq(vault.surplus(address(usdc)), 1e6); // the fee

        // Obligations and surplus are per token.
        assertEq(vault.obligations(address(usdt)), 0);
        assertEq(vault.surplus(address(usdt)), 0);
        depositUsdt(10e6);
        assertEq(vault.obligations(address(usdt)), 99e5);
        assertEq(vault.surplus(address(usdt)), 1e5);
        assertEq(vault.obligations(address(usdc)), 99e6);
        assertEq(vault.surplus(address(usdc)), 1e6);
    }

    function testDepositZeroFee() public {
        assertEq(vault.depositFeeBps(), 0);
        vm.startPrank(user);
        usdc.approve(address(vault), 100e6);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1, address(usdc), user, 100e6, 0, DEST);
        vault.deposit(address(usdc), 100e6, DEST);
        vm.stopPrank();
        assertEq(vault.reserve(address(usdc)), 100e6);
        assertEq(vault.obligations(address(usdc)), 100e6);
        assertEq(vault.surplus(address(usdc)), 0);

        // Setting a fee and clearing it again leaves later deposits whole.
        vault.setFees(100, 100);
        vault.setFees(0, 0);
        vm.startPrank(user);
        usdt.approve(address(vault), 10e6);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(2, address(usdt), user, 10e6, 0, DEST);
        vault.deposit(address(usdt), 10e6, DEST);
        vm.stopPrank();
        assertEq(vault.obligations(address(usdt)), 10e6);
        assertEq(vault.surplus(address(usdt)), 0);
    }

    function testReleaseFeeMath() public {
        vault.setFees(100, 100);
        depositUsdc(100e6); // obligations 99, surplus 1
        bytes32 burn = keccak256("burn-fee");
        bytes32 d = vault.releaseDigest(address(usdc), stranger, 50e6, burn); // guardians sign the gross
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usdc), stranger, 50e6, 5e5);
        vault.release(address(usdc), stranger, 50e6, burn, sorted(G1, G3, d));
        assertEq(usdc.balanceOf(stranger), 495e5); // 49.5 paid out
        assertEq(vault.obligations(address(usdc)), 49e6); // reduced by the gross 50
        assertEq(vault.reserve(address(usdc)), 505e5); // 100 - 49.5
        assertEq(vault.surplus(address(usdc)), 15e5); // 1 + 0.5
        assertTrue(vault.released(burn));

        // A signature over the net amount does not authorise a release of the gross.
        bytes32 burn2 = keccak256("burn-fee-2");
        bytes32 net = vault.releaseDigest(address(usdc), stranger, 495e5, burn2);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 50e6, burn2, sorted(G1, G3, net));
    }

    function testReleaseZeroFee() public {
        vault.setFees(100, 0);
        depositUsdc(100e6); // obligations 99, surplus 1
        bytes32 burn = keccak256("burn-nofee");
        bytes32 d = vault.releaseDigest(address(usdc), stranger, 50e6, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usdc), stranger, 50e6, 0);
        vault.release(address(usdc), stranger, 50e6, burn, sorted(G1, G2, d));
        assertEq(usdc.balanceOf(stranger), 50e6);
        assertEq(vault.obligations(address(usdc)), 49e6);
        assertEq(vault.surplus(address(usdc)), 1e6);
    }

    function testFeeRoundingFavoursTheUser() public {
        vault.setFees(100, 100);
        vault.setToken(address(usdc), true, 0);
        vm.startPrank(user);
        usdc.approve(address(vault), 100e6);
        // 1 unit: 1 * 100 / 10000 floors to 0, so the whole unit is credited.
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1, address(usdc), user, 1, 0, DEST);
        vault.deposit(address(usdc), 1, DEST);
        // 99 units: fee 0.99 floors to 0.
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(2, address(usdc), user, 99, 0, DEST);
        vault.deposit(address(usdc), 99, DEST);
        // 100 units: fee exactly 1.
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(3, address(usdc), user, 99, 1, DEST);
        vault.deposit(address(usdc), 100, DEST);
        // 199 units: fee 1.99 floors to 1.
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(4, address(usdc), user, 198, 1, DEST);
        vault.deposit(address(usdc), 199, DEST);
        vm.stopPrank();
        assertEq(vault.reserve(address(usdc)), 399);
        assertEq(vault.obligations(address(usdc)), 397);
        assertEq(vault.surplus(address(usdc)), 2);

        // Releases floor the same way: 1 unit pays 1, 150 pays 149.
        bytes32 burn = keccak256("burn-round-1");
        bytes32 d = vault.releaseDigest(address(usdc), stranger, 1, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usdc), stranger, 1, 0);
        vault.release(address(usdc), stranger, 1, burn, sorted(G1, G2, d));
        assertEq(usdc.balanceOf(stranger), 1);
        burn = keccak256("burn-round-150");
        d = vault.releaseDigest(address(usdc), stranger, 150, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usdc), stranger, 150, 1);
        vault.release(address(usdc), stranger, 150, burn, sorted(G1, G2, d));
        assertEq(usdc.balanceOf(stranger), 150);
        assertEq(vault.obligations(address(usdc)), 246); // 397 - 1 - 150
        assertEq(vault.reserve(address(usdc)), 249); // 399 - 1 - 149
        assertEq(vault.surplus(address(usdc)), 3);

        // The smallest non-zero fee, 1 bps, takes nothing below 10000 units.
        vault.setFees(1, 1);
        vm.startPrank(user);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(5, address(usdc), user, 9999, 0, DEST);
        vault.deposit(address(usdc), 9999, DEST);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(6, address(usdc), user, 9999, 1, DEST);
        vault.deposit(address(usdc), 10000, DEST);
        vm.stopPrank();
    }

    function testMaxReleaseAppliesToGross() public {
        vault.setFees(0, 100);
        depositUsdc(10e6);
        vault.setLimits(address(usdc), 3e6, 0);
        bytes32 burn = keccak256("burn-gross-cap");
        // Gross 3,000,001 would pay only 2,970,001 net, but the cap is on the gross.
        releaseReverts(address(usdc), 3e6 + 1, burn, Vault.LimitExceeded.selector);
        releaseTo(address(usdc), 3e6, burn);
        assertEq(usdc.balanceOf(stranger), 297e4);
        assertEq(vault.obligations(address(usdc)), 7e6);
    }

    // ---- surplus ----

    function testWithdrawSurplusExactlyAndAbove() public {
        vault.setFees(100, 0);
        depositUsdc(100e6); // balance 100, obligations 99, surplus 1
        assertEq(vault.surplus(address(usdc)), 1e6);

        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.withdrawSurplus(address(usdc), stranger, 1e6);
        vm.expectRevert(Vault.NoSurplus.selector);
        vault.withdrawSurplus(address(usdc), treasury, 1e6 + 1);
        vm.expectRevert(Vault.NoSurplus.selector);
        vault.withdrawSurplus(address(usdc), treasury, 100e6);
        vm.expectRevert(Vault.BadDestination.selector);
        vault.withdrawSurplus(address(usdc), address(0), 1e6);
        assertEq(vault.reserve(address(usdc)), 100e6);

        // Part of it, then exactly the rest.
        vm.expectEmit(true, true, false, true);
        emit Vault.SurplusWithdrawn(address(usdc), treasury, 4e5);
        vault.withdrawSurplus(address(usdc), treasury, 4e5);
        assertEq(usdc.balanceOf(treasury), 4e5);
        assertEq(vault.surplus(address(usdc)), 6e5);
        vm.expectEmit(true, true, false, true);
        emit Vault.SurplusWithdrawn(address(usdc), treasury, 6e5);
        vault.withdrawSurplus(address(usdc), treasury, 6e5);
        assertEq(usdc.balanceOf(treasury), 1e6);
        assertEq(vault.surplus(address(usdc)), 0);
        assertEq(vault.reserve(address(usdc)), 99e6); // exactly the backing is left
        assertEq(vault.obligations(address(usdc)), 99e6);

        // Nothing more can be taken, not even one unit.
        vm.expectRevert(Vault.NoSurplus.selector);
        vault.withdrawSurplus(address(usdc), treasury, 1);

        // A token with no balance at all has no surplus.
        vm.expectRevert(Vault.NoSurplus.selector);
        vault.withdrawSurplus(address(usdt), treasury, 1);
    }

    function testWithdrawSurplusIsImmediateAfterFinalize() public {
        vault.setFees(100, 0);
        depositUsdc(100e6);
        vault.finalizeSetup();
        vault.withdrawSurplus(address(usdc), treasury, 1e6); // no proposal needed
        assertEq(usdc.balanceOf(treasury), 1e6);
        vm.expectRevert(Vault.NoSurplus.selector);
        vault.withdrawSurplus(address(usdc), treasury, 1);
    }

    function testSurplusNeverIncludesObligationsAfterReleases() public {
        vault.setFees(100, 100);
        depositUsdc(100e6); // balance 100, obligations 99, surplus 1

        releaseTo(address(usdc), 30e6, keccak256("burn-s1")); // pays 29.7
        assertEq(vault.obligations(address(usdc)), 69e6);
        assertEq(vault.reserve(address(usdc)), 703e5);
        assertEq(vault.surplus(address(usdc)), 13e5);
        assertEq(vault.reserve(address(usdc)) - vault.obligations(address(usdc)), vault.surplus(address(usdc)));

        releaseTo(address(usdc), 20e6, keccak256("burn-s2")); // pays 19.8
        assertEq(vault.obligations(address(usdc)), 49e6);
        assertEq(vault.reserve(address(usdc)), 505e5);
        assertEq(vault.surplus(address(usdc)), 15e5);

        // Sweep the surplus: only the backing remains and it still pays every
        // burn it backs.
        vault.withdrawSurplus(address(usdc), treasury, 15e5);
        assertEq(usdc.balanceOf(treasury), 15e5);
        assertEq(vault.reserve(address(usdc)), 49e6);
        assertEq(vault.surplus(address(usdc)), 0);
        vm.expectRevert(Vault.NoSurplus.selector);
        vault.withdrawSurplus(address(usdc), treasury, 1);

        releaseTo(address(usdc), 49e6, keccak256("burn-s3")); // pays 48.51
        assertEq(usdc.balanceOf(stranger), 297e5 + 198e5 + 4851e4);
        assertEq(vault.obligations(address(usdc)), 0);
        assertEq(vault.reserve(address(usdc)), 49e4); // the last fee
        assertEq(vault.surplus(address(usdc)), 49e4);
    }

    function testObligationsFloorAtZero() public {
        vault.setFees(100, 0);
        depositUsdc(100e6); // balance 100, obligations 99, surplus 1
        // Releasing the gross 100 (more than the 99 obligation) is paid in full
        // from the balance; obligations stop at zero instead of underflowing.
        releaseTo(address(usdc), 100e6, keccak256("burn-floor"));
        assertEq(usdc.balanceOf(stranger), 100e6);
        assertEq(vault.obligations(address(usdc)), 0);
        assertEq(vault.reserve(address(usdc)), 0);
        assertEq(vault.surplus(address(usdc)), 0);
    }

    function testWithdrawSurplusOnDisallowedToken() public {
        vault.setFees(100, 100);
        depositUsdt(10e6); // balance 10, obligations 9.9, surplus 0.1
        vault.setToken(address(usdt), false, 0);
        assertFalse(vault.isAllowed(address(usdt)));

        // Releases of the disallowed token are refused, its surplus is not.
        bytes32 burn = keccak256("burn-disallowed");
        bytes32 d = vault.releaseDigest(address(usdt), stranger, 1e6, burn);
        vm.expectRevert(Vault.TokenNotAllowed.selector);
        vault.release(address(usdt), stranger, 1e6, burn, sorted(G1, G2, d));
        assertEq(vault.surplus(address(usdt)), 1e5);
        vm.expectRevert(Vault.NoSurplus.selector);
        vault.withdrawSurplus(address(usdt), treasury, 1e5 + 1);
        vm.expectEmit(true, true, false, true);
        emit Vault.SurplusWithdrawn(address(usdt), treasury, 1e5);
        vault.withdrawSurplus(address(usdt), treasury, 1e5);
        assertEq(usdt.balanceOf(treasury), 1e5);
        assertEq(vault.reserve(address(usdt)), 99e5);
        assertEq(vault.obligations(address(usdt)), 99e5);
        assertEq(vault.surplus(address(usdt)), 0);

        // A token that was never allowed but was sent to the vault directly is
        // all surplus (no uARG was issued for it).
        vm.prank(user);
        usd18.transfer(address(vault), 5e18);
        assertEq(vault.obligations(address(usd18)), 0);
        assertEq(vault.surplus(address(usd18)), 5e18);
        vault.withdrawSurplus(address(usd18), treasury, 5e18);
        assertEq(usd18.balanceOf(treasury), 5e18);
        assertEq(vault.surplus(address(usd18)), 0);
    }

    function testNonStandardUsdtRoundTripWithFees() public {
        vault.setFees(100, 100);
        vm.startPrank(user);
        usdt.approve(address(vault), 10e6); // approve returns nothing
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1, address(usdt), user, 99e5, 1e5, DEST);
        vault.deposit(address(usdt), 10e6, DEST); // transferFrom returns nothing
        vm.stopPrank();
        assertEq(vault.reserve(address(usdt)), 10e6);
        assertEq(vault.obligations(address(usdt)), 99e5);
        assertEq(vault.surplus(address(usdt)), 1e5);

        bytes32 burn = keccak256("burn-usdt-fee");
        bytes32 d = vault.releaseDigest(address(usdt), stranger, 5e6, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usdt), stranger, 5e6, 5e4);
        vault.release(address(usdt), stranger, 5e6, burn, sorted(G2, G3, d)); // transfer returns nothing
        assertEq(usdt.balanceOf(stranger), 495e4);
        assertEq(vault.obligations(address(usdt)), 49e5);
        assertEq(vault.reserve(address(usdt)), 505e4);
        assertEq(vault.surplus(address(usdt)), 15e4);

        vault.withdrawSurplus(address(usdt), treasury, 15e4); // transfer returns nothing
        assertEq(usdt.balanceOf(treasury), 15e4);
        assertEq(vault.surplus(address(usdt)), 0);
        assertEq(vault.reserve(address(usdt)), 49e5);
    }

    // ---- releases ----

    function testReleaseUsdcWithTwoOfThree() public {
        depositUsdc(10e6);
        bytes32 burn = keccak256("burn-1");
        bytes32 digest = vault.releaseDigest(address(usdc), stranger, 4e6, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usdc), stranger, 4e6, 0);
        vault.release(address(usdc), stranger, 4e6, burn, sorted(G1, G3, digest));
        assertEq(usdc.balanceOf(stranger), 4e6);
        assertEq(vault.reserve(address(usdc)), 6e6);
        assertEq(vault.obligations(address(usdc)), 6e6);
        assertEq(vault.surplus(address(usdc)), 0);
        assertTrue(vault.released(burn));
        assertFalse(vault.released(keccak256("burn-other")));
    }

    function testReleaseNonStandardUsdt() public {
        depositUsdt(10e6); // transferFrom without a return value
        bytes32 burn = keccak256("burn-usdt");
        bytes32 digest = vault.releaseDigest(address(usdt), stranger, 7e6, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usdt), stranger, 7e6, 0);
        vault.release(address(usdt), stranger, 7e6, burn, sorted(G2, G3, digest));
        assertEq(usdt.balanceOf(stranger), 7e6);
        assertEq(vault.reserve(address(usdt)), 3e6);
        assertTrue(vault.released(burn));
    }

    function testReleaseAllThreeSignatures() public {
        depositUsdc(10e6);
        bytes32 burn = keccak256("burn-3of3");
        bytes32 digest = vault.releaseDigest(address(usdc), stranger, 1e6, burn);
        uint256[3] memory keys = [G1, G2, G3];
        // insertion sort by signer address
        for (uint256 i = 1; i < 3; i++) {
            for (uint256 j = i; j > 0 && vm.addr(keys[j - 1]) > vm.addr(keys[j]); j--) {
                (keys[j - 1], keys[j]) = (keys[j], keys[j - 1]);
            }
        }
        bytes[] memory sigs = new bytes[](3);
        for (uint256 i = 0; i < 3; i++) {
            sigs[i] = sign(keys[i], digest);
        }
        vault.release(address(usdc), stranger, 1e6, burn, sigs);
        assertEq(usdc.balanceOf(stranger), 1e6);
    }

    function testReleaseReplayByBurnIdRejected() public {
        depositUsdc(10e6);
        depositUsdt(10e6);
        bytes32 burn = keccak256("burn-2");
        bytes32 digest = vault.releaseDigest(address(usdc), stranger, 1e6, burn);
        bytes[] memory sigs = sorted(G1, G2, digest);
        vault.release(address(usdc), stranger, 1e6, burn, sigs);
        vm.expectRevert(Vault.AlreadyReleased.selector);
        vault.release(address(usdc), stranger, 1e6, burn, sigs);
        // The same burn cannot be paid again in another token either.
        bytes32 other = vault.releaseDigest(address(usdt), stranger, 1e6, burn);
        vm.expectRevert(Vault.AlreadyReleased.selector);
        vault.release(address(usdt), stranger, 1e6, burn, sorted(G1, G2, other));
    }

    function testReleaseBadSignatures() public {
        depositUsdc(10e6);
        bytes32 burn = keccak256("burn-3");
        bytes32 digest = vault.releaseDigest(address(usdc), stranger, 1e6, burn);

        // below threshold
        bytes[] memory one = new bytes[](1);
        one[0] = sign(G1, digest);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, one);

        // wrong order
        bytes[] memory good = sorted(G1, G2, digest);
        bytes[] memory reversed = new bytes[](2);
        reversed[0] = good[1];
        reversed[1] = good[0];
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, reversed);

        // the same guardian twice
        bytes[] memory twice = new bytes[](2);
        twice[0] = good[0];
        twice[1] = good[0];
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, twice);

        // unknown signer
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, sorted(G1, 0xDEAD, digest));

        // malformed signature
        bytes[] memory short = sorted(G1, G2, digest);
        short[0] = bytes.concat(bytes32(0), bytes32(0));
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, short);

        // a signature over a different amount, token or recipient does not authorise this release
        bytes32 otherAmount = vault.releaseDigest(address(usdc), stranger, 2e6, burn);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, sorted(G1, G2, otherAmount));
        bytes32 otherToken = vault.releaseDigest(address(usdt), stranger, 1e6, burn);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, sorted(G1, G2, otherToken));
        bytes32 otherTo = vault.releaseDigest(address(usdc), user, 1e6, burn);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, sorted(G1, G2, otherTo));

        // the good signatures still work afterwards
        vault.release(address(usdc), stranger, 1e6, burn, good);
        assertEq(usdc.balanceOf(stranger), 1e6);
    }

    function testReleaseChecksTokenRecipientAmountAndReserve() public {
        depositUsdc(10e6);
        bytes32 burn = keccak256("burn-4");

        bytes32 d = vault.releaseDigest(address(usd18), stranger, 1e18, burn);
        vm.expectRevert(Vault.TokenNotAllowed.selector);
        vault.release(address(usd18), stranger, 1e18, burn, sorted(G1, G2, d));

        d = vault.releaseDigest(address(usdc), address(0), 1e6, burn);
        vm.expectRevert(Vault.BadDestination.selector);
        vault.release(address(usdc), address(0), 1e6, burn, sorted(G1, G2, d));

        d = vault.releaseDigest(address(usdc), stranger, 0, burn);
        vm.expectRevert(Vault.AmountTooSmall.selector);
        vault.release(address(usdc), stranger, 0, burn, sorted(G1, G2, d));

        // more than the reserve: the token reverts, the vault reports TransferFailed
        d = vault.releaseDigest(address(usdc), stranger, 11e6, burn);
        vm.expectRevert(Vault.TransferFailed.selector);
        vault.release(address(usdc), stranger, 11e6, burn, sorted(G1, G2, d));
        assertFalse(vault.released(burn));
        assertEq(vault.obligations(address(usdc)), 10e6); // the failed release changed nothing
        assertEq(vault.periodReleased(address(usdc)), 0); // nor the release window
    }

    function testReleaseDigestMatchesSpec() public view {
        bytes32 burn = keccak256("burn-digest");
        bytes32 inner = keccak256(abi.encode(block.chainid, address(vault), address(usdc), stranger, 5e6, burn));
        bytes32 expected = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", inner));
        assertEq(vault.releaseDigest(address(usdc), stranger, 5e6, burn), expected);
        assertTrue(vault.releaseDigest(address(usdt), stranger, 5e6, burn) != expected);
    }

    // ---- pause ----

    function testPauseStopsDepositsAndReleases() public {
        depositUsdc(10e6);
        vm.expectEmit(false, false, false, true);
        emit Vault.Paused(true);
        vault.pause();
        assertTrue(vault.paused());
        vm.startPrank(user);
        usdc.approve(address(vault), 5e6);
        vm.expectRevert(Vault.IsPaused.selector);
        vault.deposit(address(usdc), 5e6, DEST);
        vm.stopPrank();
        bytes32 burn = keccak256("burn-paused");
        bytes[] memory sigs = sigsFor(address(usdc), 1e6, burn);
        vm.expectRevert(Vault.IsPaused.selector);
        vault.release(address(usdc), stranger, 1e6, burn, sigs);

        // Lifting the pause takes a proposal and the delay, even during setup.
        vault.proposeUnpause();
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeUnpause();
        waitDelay();
        vm.expectEmit(false, false, false, true);
        emit Vault.Paused(false);
        vault.executeUnpause();
        assertFalse(vault.paused());
        vault.release(address(usdc), stranger, 1e6, burn, sigs);
        assertEq(depositUsdc(5e6), 2);
    }

    function testGuardianCanPauseButNotUnpause() public {
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwnerOrGuardian.selector);
        vault.pause();
        assertFalse(vault.paused());

        // Any one guardian stops the vault at once.
        vm.prank(g3);
        vm.expectEmit(false, false, false, true);
        emit Vault.Paused(true);
        vault.pause();
        assertTrue(vault.paused());
        vm.prank(g1);
        vault.pause(); // pausing again is harmless
        assertTrue(vault.paused());

        // A guardian can neither propose nor execute an unpause.
        vm.startPrank(g3);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.proposeUnpause();
        vm.expectRevert(Vault.NotOwner.selector);
        vault.executeUnpause();
        vm.stopPrank();

        vault.proposeUnpause();
        waitDelay();
        vm.prank(g3);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.executeUnpause();
        vault.executeUnpause();
        assertFalse(vault.paused());
    }

    function testPauseCancelsPendingUnpause() public {
        vm.expectRevert(Vault.NotPaused.selector);
        vault.proposeUnpause();

        vault.pause();
        bytes memory data = abi.encodeCall(vault.executeUnpause, ());
        bytes32 id = keccak256(data);
        vm.expectEmit(true, false, false, true);
        emit Vault.Proposed(id, data, vm.getBlockTimestamp() + DELAY);
        assertEq(vault.proposeUnpause(), id);
        vm.expectRevert(Vault.AlreadyProposed.selector);
        vault.proposeUnpause();

        // A guardian pausing again drops the pending unpause...
        vm.warp(vm.getBlockTimestamp() + DELAY - 1);
        vm.prank(g2);
        vm.expectEmit(true, false, false, true);
        emit Vault.Cancelled(id);
        vault.pause();
        assertEq(vault.proposalEta(id), 0);
        waitDelay();
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeUnpause();
        assertTrue(vault.paused());

        // ...so lifting the pause waits the full delay again.
        vault.proposeUnpause();
        vm.warp(vm.getBlockTimestamp() + DELAY - 1);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeUnpause();
        vm.warp(vm.getBlockTimestamp() + 1);
        vault.executeUnpause();
        assertFalse(vault.paused());
    }

    // ---- release limits ----

    function testMaxReleasePerToken() public {
        depositUsdc(10e6);
        vault.setToken(address(usd18), true, 1e18);
        depositUsd18(10e18);
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.setLimits(address(usdc), 3e6, 0);
        vm.expectRevert(Vault.BadToken.selector);
        vault.setLimits(address(0), 3e6, 0);

        // One cap per token, in that token's units.
        vm.expectEmit(true, false, false, true);
        emit Vault.LimitsSet(address(usdc), 3e6, 0);
        vault.setLimits(address(usdc), 3e6, 0);
        vault.setLimits(address(usd18), 3e18, 0);
        assertEq(vault.maxRelease(address(usdc)), 3e6);
        assertEq(vault.maxRelease(address(usd18)), 3e18);
        assertEq(vault.maxRelease(address(usdt)), 0);

        releaseReverts(address(usdc), 3e6 + 1, keccak256("cap-1"), Vault.LimitExceeded.selector);
        releaseTo(address(usdc), 3e6, keccak256("cap-1"));
        // 3e18 is far above the six-decimal cap and still fine for usd18.
        releaseReverts(address(usd18), 3e18 + 1, keccak256("cap-2"), Vault.LimitExceeded.selector);
        releaseTo(address(usd18), 3e18, keccak256("cap-2"));
        assertEq(usd18.balanceOf(stranger), 3e18);

        // 0 removes the cap.
        vault.setLimits(address(usdc), 0, 0);
        releaseTo(address(usdc), 7e6, keccak256("cap-3"));
        assertEq(usdc.balanceOf(stranger), 10e6);
        assertEq(vault.reserve(address(usdc)), 0);
    }

    function testPeriodLimitWindowRollover() public {
        depositUsdc(50e6);
        vm.expectEmit(true, false, false, true);
        emit Vault.LimitsSet(address(usdc), 0, 5e6);
        vault.setLimits(address(usdc), 0, 5e6);
        assertEq(vault.periodLimit(address(usdc)), 5e6);
        assertEq(vault.periodAvailable(address(usdc)), 5e6);

        uint256 t0 = vm.getBlockTimestamp();
        releaseTo(address(usdc), 3e6, keccak256("p1"));
        assertEq(vault.periodStart(address(usdc)), t0);
        assertEq(vault.periodReleased(address(usdc)), 3e6);
        assertEq(vault.periodAvailable(address(usdc)), 2e6);
        releaseTo(address(usdc), 2e6, keccak256("p2")); // exactly the limit
        assertEq(vault.periodAvailable(address(usdc)), 0);
        releaseReverts(address(usdc), 1, keccak256("p3"), Vault.LimitExceeded.selector);

        // Still the same window one second before it ends.
        vm.warp(t0 + 1 days - 1);
        assertEq(vault.periodAvailable(address(usdc)), 0);
        releaseReverts(address(usdc), 1, keccak256("p3"), Vault.LimitExceeded.selector);

        // A new window opens with the first release after the old one ended.
        vm.warp(t0 + 1 days);
        assertEq(vault.periodAvailable(address(usdc)), 5e6);
        releaseTo(address(usdc), 4e6, keccak256("p3"));
        assertEq(vault.periodStart(address(usdc)), t0 + 1 days);
        assertEq(vault.periodReleased(address(usdc)), 4e6);
        releaseReverts(address(usdc), 1e6 + 1, keccak256("p4"), Vault.LimitExceeded.selector);
        releaseTo(address(usdc), 1e6, keccak256("p4"));

        // Windows are not on a fixed grid: the next starts at its first release.
        uint256 t1 = t0 + 3 days + 123;
        vm.warp(t1);
        releaseTo(address(usdc), 5e6, keccak256("p5"));
        assertEq(vault.periodStart(address(usdc)), t1);
        assertEq(vault.periodReleased(address(usdc)), 5e6);
        assertEq(usdc.balanceOf(stranger), 15e6);

        // Both limits apply together: the per-release cap below the window.
        vm.warp(t1 + 1 days);
        vault.setLimits(address(usdc), 2e6, 5e6);
        releaseReverts(address(usdc), 3e6, keccak256("p6"), Vault.LimitExceeded.selector);
        releaseTo(address(usdc), 2e6, keccak256("p6"));
        assertEq(vault.periodAvailable(address(usdc)), 3e6);
    }

    function testPeriodLimitPerTokenAndCountedWithoutLimit() public {
        depositUsdc(20e6);
        depositUsdt(20e6);

        // Without a limit the window is still counted...
        releaseTo(address(usdc), 4e6, keccak256("w1"));
        assertEq(vault.periodReleased(address(usdc)), 4e6);
        assertEq(vault.periodAvailable(address(usdc)), type(uint256).max);

        // ...so a limit set mid-window sees what already went out in it.
        vault.setLimits(address(usdc), 0, 5e6);
        assertEq(vault.periodAvailable(address(usdc)), 1e6);
        releaseReverts(address(usdc), 2e6, keccak256("w2"), Vault.LimitExceeded.selector);

        // A limit lowered below what was released leaves nothing, not an underflow.
        vault.setLimits(address(usdc), 0, 3e6);
        assertEq(vault.periodAvailable(address(usdc)), 0);

        // Each token has its own limit and window.
        vault.setLimits(address(usdt), 0, 10e6);
        releaseTo(address(usdt), 10e6, keccak256("w3"));
        assertEq(vault.periodReleased(address(usdt)), 10e6);
        assertEq(vault.periodReleased(address(usdc)), 4e6);
        releaseReverts(address(usdt), 1, keccak256("w4"), Vault.LimitExceeded.selector);
    }

    // ---- 18 decimals ----

    function testEighteenDecimalRoundTrip() public {
        vault.setToken(address(usd18), true, 1e18);
        vm.startPrank(user);
        usd18.approve(address(vault), 10e18);
        vm.expectRevert(Vault.AmountTooSmall.selector);
        vault.deposit(address(usd18), 5e17, DEST);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1, address(usd18), user, 5e18, 0, DEST);
        uint256 id = vault.deposit(address(usd18), 5e18, DEST);
        vm.stopPrank();
        assertEq(id, 1);
        assertEq(vault.reserve(address(usd18)), 5e18);

        bytes32 burn = keccak256("burn-18");
        bytes32 d = vault.releaseDigest(address(usd18), stranger, 2e18, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usd18), stranger, 2e18, 0);
        vault.release(address(usd18), stranger, 2e18, burn, sorted(G1, G3, d));
        assertEq(usd18.balanceOf(stranger), 2e18);
        assertEq(vault.reserve(address(usd18)), 3e18);
        assertEq(usd18.balanceOf(user), 95e18);
    }

    function testEighteenDecimalWithFees() public {
        vault.setToken(address(usd18), true, 1e18);
        vault.setFees(100, 100);
        vm.startPrank(user);
        usd18.approve(address(vault), 10e18);
        vm.expectEmit(true, true, true, true);
        emit Vault.Deposited(1, address(usd18), user, 99e17, 1e17, DEST);
        vault.deposit(address(usd18), 10e18, DEST);
        vm.stopPrank();
        assertEq(vault.reserve(address(usd18)), 10e18);
        assertEq(vault.obligations(address(usd18)), 99e17);
        assertEq(vault.surplus(address(usd18)), 1e17);

        bytes32 burn = keccak256("burn-18-fee");
        bytes32 d = vault.releaseDigest(address(usd18), stranger, 2e18, burn);
        vm.expectEmit(true, true, true, true);
        emit Vault.Released(burn, address(usd18), stranger, 2e18, 2e16);
        vault.release(address(usd18), stranger, 2e18, burn, sorted(G1, G3, d));
        assertEq(usd18.balanceOf(stranger), 198e16);
        assertEq(vault.obligations(address(usd18)), 79e17);
        assertEq(vault.reserve(address(usd18)), 802e16);
        assertEq(vault.surplus(address(usd18)), 12e16);

        vault.withdrawSurplus(address(usd18), treasury, 12e16);
        assertEq(usd18.balanceOf(treasury), 12e16);
        assertEq(vault.surplus(address(usd18)), 0);
        assertEq(vault.reserve(address(usd18)), 79e17);
    }

    // ---- setup ----

    function testFinalizeSetupIsOneWay() public {
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.finalizeSetup();
        vm.prank(g1);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.finalizeSetup();

        // During setup tokens, fees and limits change at once.
        vault.setToken(address(usd18), true, 1e18);
        vault.setFees(100, 100);
        vault.setLimits(address(usdc), 5e6, 50e6);
        assertTrue(vault.isAllowed(address(usd18)));

        vm.expectEmit(false, false, false, true);
        emit Vault.SetupFinalized();
        vault.finalizeSetup();
        assertTrue(vault.finalized());

        // Afterwards never again, not even by a new owner.
        vm.expectRevert(Vault.AlreadyFinalized.selector);
        vault.finalizeSetup();
        vm.expectRevert(Vault.AlreadyFinalized.selector);
        vault.setToken(address(usd18), false, 0);
        vm.expectRevert(Vault.AlreadyFinalized.selector);
        vault.setFees(0, 0);
        vm.expectRevert(Vault.AlreadyFinalized.selector);
        vault.setLimits(address(usdc), 0, 0);
        vault.transferOwnership(stranger);
        vm.prank(stranger);
        vault.acceptOwnership();
        vm.startPrank(stranger);
        vm.expectRevert(Vault.AlreadyFinalized.selector);
        vault.setFees(0, 0);
        vm.expectRevert(Vault.AlreadyFinalized.selector);
        vault.finalizeSetup();
        vm.stopPrank();
        assertTrue(vault.finalized());
        assertTrue(vault.isAllowed(address(usd18)));
        assertEq(vault.depositFeeBps(), 100);
        assertEq(vault.maxRelease(address(usdc)), 5e6);
        assertEq(vault.periodLimit(address(usdc)), 50e6);
    }

    function testOwnerCannotInstantlySwapGuardiansAfterFinalize() public {
        depositUsdc(10e6);
        vault.finalizeSetup();
        uint256 attackerKey = 0xBAD;
        address attacker = vm.addr(attackerKey);
        address[] memory evil = new address[](1);
        evil[0] = attacker;

        // The old instant setters are gone.
        bool ok;
        (ok,) = address(vault).call(abi.encodeWithSignature("setGuardians(address[],uint256)", evil, 1));
        assertFalse(ok);
        (ok,) = address(vault).call(abi.encodeWithSignature("setMaxRelease(uint256)", 0));
        assertFalse(ok);
        (ok,) = address(vault).call(abi.encodeWithSignature("setPaused(bool)", false));
        assertFalse(ok);

        // All the owner can do is propose, in public, and wait.
        uint256 eta = vm.getBlockTimestamp() + DELAY;
        vault.proposeGuardians(evil, 1);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeGuardians(evil, 1);
        vm.warp(eta - 1);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeGuardians(evil, 1);
        assertFalse(vault.isGuardian(attacker));
        assertEq(vault.threshold(), 2);

        // Meanwhile the attacker's lone signature releases nothing...
        bytes32 burn = keccak256("theft");
        bytes[] memory sigs = new bytes[](1);
        sigs[0] = sign(attackerKey, vault.releaseDigest(address(usdc), attacker, 10e6, burn));
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), attacker, 10e6, burn, sigs);

        // ...and any guardian who sees the proposal can stop the vault; the
        // owner can then lift the pause only after another full delay.
        vm.prank(g2);
        vault.pause();
        vm.warp(eta);
        vault.executeGuardians(evil, 1);
        vm.expectRevert(Vault.IsPaused.selector);
        vault.release(address(usdc), attacker, 10e6, burn, sigs);
        vault.proposeUnpause();
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeUnpause();
        assertEq(usdc.balanceOf(attacker), 0);
    }

    // ---- timelock ----

    function testTimelockGuardians() public {
        vault.finalizeSetup();
        uint256 newKey = 0xFACE;
        address[] memory two = pair(g1, vm.addr(newKey));
        bytes memory data = abi.encodeCall(vault.executeGuardians, (two, 2));
        bytes32 id = keccak256(data);
        uint256 eta = vm.getBlockTimestamp() + DELAY;
        vm.expectEmit(true, false, false, true);
        emit Vault.Proposed(id, data, eta);
        assertEq(vault.proposeGuardians(two, 2), id);
        assertEq(vault.proposalEta(id), eta);

        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeGuardians(two, 2);
        vm.warp(eta - 1);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeGuardians(two, 2);
        // While pending, the current set still signs.
        depositUsdc(10e6);
        releaseTo(address(usdc), 1e6, keccak256("before"));

        vm.warp(eta);
        vm.expectEmit(true, false, false, true);
        emit Vault.Executed(id);
        vm.expectEmit(false, false, false, true);
        emit Vault.GuardiansSet(two, 2);
        vault.executeGuardians(two, 2);
        assertEq(vault.proposalEta(id), 0);
        assertEq(vault.guardianCount(), 2);
        assertTrue(vault.isGuardian(g1));
        assertTrue(vault.isGuardian(vm.addr(newKey)));
        assertFalse(vault.isGuardian(g2));
        assertFalse(vault.isGuardian(g3));
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeGuardians(two, 2); // once only

        bytes32 burn = keccak256("after");
        bytes32 d = vault.releaseDigest(address(usdc), stranger, 1e6, burn);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), stranger, 1e6, burn, sorted(G2, G3, d));
        vault.release(address(usdc), stranger, 1e6, burn, sorted(G1, newKey, d));
        assertEq(usdc.balanceOf(stranger), 2e6);

        // A former guardian can no longer pause.
        vm.prank(g2);
        vm.expectRevert(Vault.NotOwnerOrGuardian.selector);
        vault.pause();
    }

    function testProposeGuardiansValidation() public {
        address[] memory two = pair(g1, g2);
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.proposeGuardians(two, 1);
        vm.expectRevert(Vault.BadGuardians.selector);
        vault.proposeGuardians(two, 3);
        vm.expectRevert(Vault.BadGuardians.selector);
        vault.proposeGuardians(two, 0);
        vm.expectRevert(Vault.BadGuardians.selector);
        vault.proposeGuardians(new address[](0), 1);
        vm.expectRevert(Vault.BadGuardians.selector);
        vault.proposeGuardians(pair(g1, g1), 1);
        vm.expectRevert(Vault.BadGuardians.selector);
        vault.proposeGuardians(pair(g1, address(0)), 1);
        address[] memory three = new address[](3);
        three[0] = g1;
        three[1] = g2;
        three[2] = g1; // a duplicate that is not adjacent
        vm.expectRevert(Vault.BadGuardians.selector);
        vault.proposeGuardians(three, 2);
    }

    function testTimelockToken() public {
        vault.finalizeSetup();
        bytes memory data = abi.encodeCall(vault.executeToken, (address(usd18), true, 1e18));
        vm.expectEmit(true, false, false, true);
        emit Vault.Proposed(keccak256(data), data, vm.getBlockTimestamp() + DELAY);
        bytes32 id = vault.proposeToken(address(usd18), true, 1e18);
        assertEq(id, keccak256(data));
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeToken(address(usd18), true, 1e18);
        assertFalse(vault.isAllowed(address(usd18)));

        waitDelay();
        vm.expectEmit(true, false, false, true);
        emit Vault.TokenSet(address(usd18), true, 1e18);
        vault.executeToken(address(usd18), true, 1e18);
        assertTrue(vault.isAllowed(address(usd18)));
        assertEq(vault.minDeposit(address(usd18)), 1e18);
        assertEq(vault.tokens().length, 3);

        // Disallowing goes through the timelock too.
        vault.proposeToken(address(usdt), false, 0);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeToken(address(usdt), false, 0);
        assertTrue(vault.isAllowed(address(usdt)));
        waitDelay();
        vault.executeToken(address(usdt), false, 0);
        assertFalse(vault.isAllowed(address(usdt)));
        assertEq(vault.tokens().length, 2);
    }

    function testTimelockFees() public {
        vault.finalizeSetup();
        vm.expectRevert(Vault.BadFee.selector);
        vault.proposeFees(1001, 0);
        vm.expectRevert(Vault.FeeStepTooLarge.selector);
        vault.proposeFees(0, 101);

        bytes32 id = vault.proposeFees(1000, 100);
        assertEq(id, keccak256(abi.encodeCall(vault.executeFees, (1000, 100))));
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeFees(1000, 100);
        assertEq(vault.depositFeeBps(), 0);
        waitDelay();
        vm.expectEmit(true, false, false, true);
        emit Vault.Executed(id);
        vm.expectEmit(false, false, false, true);
        emit Vault.FeesSet(1000, 100);
        vault.executeFees(1000, 100);
        assertEq(vault.depositFeeBps(), 1000);
        assertEq(vault.releaseFeeBps(), 100);

        // The step is checked again when executing, against the fee then in force.
        vault.proposeFees(1000, 200); // 100 -> 200 is one step
        vault.proposeFees(0, 0);
        waitDelay();
        vault.executeFees(0, 0);
        vm.expectRevert(Vault.FeeStepTooLarge.selector);
        vault.executeFees(1000, 200); // 0 -> 200 is two
        assertEq(vault.releaseFeeBps(), 0);
    }

    function testTimelockLimits() public {
        vault.finalizeSetup();
        depositUsdc(10e6);
        vm.expectRevert(Vault.BadToken.selector);
        vault.proposeLimits(address(0), 1, 1);

        bytes32 id = vault.proposeLimits(address(usdc), 2e6, 3e6);
        assertEq(id, keccak256(abi.encodeCall(vault.executeLimits, (address(usdc), 2e6, 3e6))));
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeLimits(address(usdc), 2e6, 3e6);
        assertEq(vault.maxRelease(address(usdc)), 0);
        waitDelay();
        vm.expectEmit(true, false, false, true);
        emit Vault.LimitsSet(address(usdc), 2e6, 3e6);
        vault.executeLimits(address(usdc), 2e6, 3e6);
        assertEq(vault.maxRelease(address(usdc)), 2e6);
        assertEq(vault.periodLimit(address(usdc)), 3e6);
        releaseReverts(address(usdc), 2e6 + 1, keccak256("l1"), Vault.LimitExceeded.selector);
        releaseTo(address(usdc), 2e6, keccak256("l1"));
        releaseReverts(address(usdc), 2e6, keccak256("l2"), Vault.LimitExceeded.selector);
    }

    function testTimelockDelay() public {
        vm.expectRevert(Vault.BadDelay.selector);
        vault.proposeDelay(1 days - 1);
        vm.expectRevert(Vault.BadDelay.selector);
        vault.proposeDelay(30 days + 1);

        // Raising the delay waits for the current one; a proposal already
        // pending keeps its eta.
        uint256 t0 = vm.getBlockTimestamp();
        bytes32 id = vault.proposeDelay(7 days);
        assertEq(id, keccak256(abi.encodeCall(vault.executeDelay, (7 days))));
        vault.proposeFees(100, 100);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeDelay(7 days);
        vm.warp(t0 + DELAY);
        vm.expectEmit(false, false, false, true);
        emit Vault.DelaySet(7 days);
        vault.executeDelay(7 days);
        assertEq(vault.delay(), 7 days);
        vault.executeFees(100, 100);

        // Lowering it waits for the (now longer) current delay.
        uint256 t1 = vm.getBlockTimestamp();
        id = vault.proposeDelay(1 days);
        assertEq(vault.proposalEta(id), t1 + 7 days);
        vm.warp(t1 + 7 days - 1);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeDelay(1 days);
        vm.warp(t1 + 7 days);
        vault.executeDelay(1 days);
        assertEq(vault.delay(), 1 days);

        // New proposals use the new delay.
        id = vault.proposeDelay(30 days);
        assertEq(vault.proposalEta(id), vm.getBlockTimestamp() + 1 days);
    }

    function testCancelEachKind() public {
        vault.pause();
        bytes32[6] memory ids = [
            vault.proposeGuardians(pair(g1, g2), 2),
            vault.proposeToken(address(usd18), true, 1e18),
            vault.proposeFees(100, 100),
            vault.proposeLimits(address(usdc), 1e6, 2e6),
            vault.proposeDelay(3 days),
            vault.proposeUnpause()
        ];
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.cancel(ids[0]);
        for (uint256 i = 0; i < ids.length; i++) {
            vm.expectEmit(true, false, false, true);
            emit Vault.Cancelled(ids[i]);
            vault.cancel(ids[i]);
            assertEq(vault.proposalEta(ids[i]), 0);
            vm.expectRevert(Vault.NotProposed.selector);
            vault.cancel(ids[i]);
        }
        vm.expectRevert(Vault.NotProposed.selector);
        vault.cancel(keccak256("nothing"));

        waitDelay();
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeGuardians(pair(g1, g2), 2);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeToken(address(usd18), true, 1e18);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeFees(100, 100);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeLimits(address(usdc), 1e6, 2e6);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeDelay(3 days);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeUnpause();
        assertEq(vault.guardianCount(), 3);
        assertFalse(vault.isAllowed(address(usd18)));
        assertEq(vault.depositFeeBps(), 0);
        assertEq(vault.maxRelease(address(usdc)), 0);
        assertEq(vault.delay(), DELAY);
        assertTrue(vault.paused());

        // A cancelled change can be proposed again, with a fresh delay.
        bytes32 id = vault.proposeFees(100, 100);
        assertEq(id, ids[2]);
        assertEq(vault.proposalEta(id), vm.getBlockTimestamp() + DELAY);
    }

    function testTimelockOnlyOwner() public {
        vault.pause();
        address[2] memory callers = [stranger, g1]; // a guardian has no admin rights
        for (uint256 i = 0; i < callers.length; i++) {
            vm.startPrank(callers[i]);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.proposeGuardians(pair(g1, g2), 2);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.executeGuardians(pair(g1, g2), 2);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.proposeToken(address(usd18), true, 1);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.executeToken(address(usd18), true, 1);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.proposeFees(1, 1);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.executeFees(1, 1);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.proposeLimits(address(usdc), 1, 1);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.executeLimits(address(usdc), 1, 1);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.proposeDelay(3 days);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.executeDelay(3 days);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.proposeUnpause();
            vm.expectRevert(Vault.NotOwner.selector);
            vault.executeUnpause();
            vm.expectRevert(Vault.NotOwner.selector);
            vault.cancel(bytes32(0));
            vm.expectRevert(Vault.NotOwner.selector);
            vault.setLimits(address(usdc), 1, 1);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.withdrawSurplus(address(usdc), callers[i], 0);
            vm.expectRevert(Vault.NotOwner.selector);
            vault.transferOwnership(callers[i]);
            vm.stopPrank();
        }

        // Once proposed by the owner, the change is still the owner's to execute.
        vault.proposeFees(1, 1);
        waitDelay();
        vm.prank(g1);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.executeFees(1, 1);
        vault.executeFees(1, 1);
        assertEq(vault.depositFeeBps(), 1);
    }

    function testProposalExpiresAndCanBeProposedAgain() public {
        uint256 eta = vm.getBlockTimestamp() + DELAY;
        bytes32 id = vault.proposeFees(100, 100);
        vm.expectRevert(Vault.AlreadyProposed.selector);
        vault.proposeFees(100, 100);
        vm.warp(eta + 1 days); // past the eta but not expired: still pending
        vm.expectRevert(Vault.AlreadyProposed.selector);
        vault.proposeFees(100, 100);

        vm.warp(eta + 14 days + 1);
        vm.expectRevert(Vault.ProposalExpired.selector);
        vault.executeFees(100, 100);
        assertEq(vault.depositFeeBps(), 0);

        // Proposing it again starts a fresh delay; the last second of the
        // grace period still counts.
        uint256 eta2 = vm.getBlockTimestamp() + DELAY;
        assertEq(vault.proposeFees(100, 100), id);
        assertEq(vault.proposalEta(id), eta2);
        vm.warp(eta2 + 14 days);
        vault.executeFees(100, 100);
        assertEq(vault.depositFeeBps(), 100);

        // An expired proposal can also be cancelled.
        id = vault.proposeFees(0, 0);
        vm.warp(vm.getBlockTimestamp() + DELAY + 15 days);
        vault.cancel(id);
        assertEq(vault.proposalEta(id), 0);
    }

    function testExecuteNeedsTheProposedArguments() public {
        vault.proposeFees(100, 100);
        vault.proposeToken(address(usd18), true, 1e18);
        vault.proposeLimits(address(usdc), 1e6, 2e6);
        waitDelay();
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeFees(100, 99);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeToken(address(usd18), true, 1e17);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeToken(address(usd18), false, 1e18);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeToken(address(usdt), true, 1e18);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeLimits(address(usdt), 1e6, 2e6);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeLimits(address(usdc), 2e6, 1e6);

        // Different kinds of change never share an id, even with equal arguments.
        assertTrue(
            keccak256(abi.encodeCall(vault.executeToken, (address(usdc), true, 1)))
                != keccak256(abi.encodeCall(vault.executeLimits, (address(usdc), 1, 1)))
        );
    }

    function testFuzzExecuteTiming(uint256 wait) public {
        wait = bound(wait, 0, DELAY + 14 days + 10 days);
        uint256 t0 = vm.getBlockTimestamp();
        vault.proposeFees(100, 100);
        vm.warp(t0 + wait);
        bool early = wait < DELAY;
        bool late = wait > DELAY + 14 days;
        if (early) vm.expectRevert(Vault.TooEarly.selector);
        else if (late) vm.expectRevert(Vault.ProposalExpired.selector);
        vault.executeFees(100, 100);
        assertEq(vault.depositFeeBps(), early || late ? 0 : 100);
    }

    // ---- ownership ----

    function testOwnershipTwoStep() public {
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.transferOwnership(stranger);

        vm.expectEmit(true, false, false, true);
        emit Vault.OwnershipTransferStarted(stranger);
        vault.transferOwnership(stranger);
        assertEq(vault.owner(), address(this));
        assertEq(vault.pendingOwner(), stranger);
        vault.setLimits(address(usdc), 1e6, 0); // the current owner keeps control until acceptance

        // address(0) withdraws the offer; a new one can follow.
        vault.transferOwnership(address(0));
        vm.prank(stranger);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.acceptOwnership();
        vault.transferOwnership(stranger);

        vm.prank(user);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.acceptOwnership();

        vm.prank(stranger);
        vm.expectEmit(true, false, false, true);
        emit Vault.OwnershipTransferred(stranger);
        vault.acceptOwnership();
        assertEq(vault.owner(), stranger);
        assertEq(vault.pendingOwner(), address(0));

        // The old owner has lost every right, pausing included.
        vm.expectRevert(Vault.NotOwnerOrGuardian.selector);
        vault.pause();
        vm.expectRevert(Vault.NotOwner.selector);
        vault.setToken(address(usd18), true, 1e18);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.setFees(100, 100);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.proposeFees(100, 100);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.finalizeSetup();
        vm.expectRevert(Vault.NotOwner.selector);
        vault.withdrawSurplus(address(usdc), treasury, 0);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.transferOwnership(address(this));

        // The new owner has them all.
        vm.startPrank(stranger);
        vault.setToken(address(usd18), true, 1e18);
        vault.setFees(100, 100);
        vault.finalizeSetup();
        vault.pause();
        vault.proposeUnpause();
        vm.stopPrank();
        assertTrue(vault.isAllowed(address(usd18)));
        assertEq(vault.depositFeeBps(), 100);
        assertTrue(vault.finalized());
        assertTrue(vault.paused());
    }

    function testNewOwnerInheritsPendingProposals() public {
        bytes32 id = vault.proposeFees(100, 100);
        bytes32 other = vault.proposeLimits(address(usdc), 1e6, 0);
        vault.transferOwnership(stranger);
        vm.prank(stranger);
        vault.acceptOwnership();
        waitDelay();

        // The old owner can neither execute nor cancel what it proposed.
        vm.expectRevert(Vault.NotOwner.selector);
        vault.executeFees(100, 100);
        vm.expectRevert(Vault.NotOwner.selector);
        vault.cancel(id);

        // The new owner decides: execute one, cancel the other.
        vm.startPrank(stranger);
        vault.executeFees(100, 100);
        vault.cancel(other);
        vm.stopPrank();
        assertEq(vault.depositFeeBps(), 100);
        assertEq(vault.proposalEta(other), 0);
        assertEq(vault.maxRelease(address(usdc)), 0);
    }
}
