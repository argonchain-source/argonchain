// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Test.sol";
import "../Vault.sol";
import "../MockUSDC.sol";

/// Review B: what the guardians' pause does against a compromised owner key.
/// The README says that during the delay "users can redeem, and any guardian
/// can pause() at once"; this shows that the two do not combine into a
/// defence: a guardian pause neither cancels nor blocks a pending guardian
/// change or limit change, the old guardians lose their pause right the
/// moment the change executes, and the owner then unpauses after one more
/// delay and drains the backing with guardians of its own. While the vault is
/// paused no user can redeem; while it is not, the period limit lets out at
/// most `periodLimit` a day.
contract ReviewBGovernanceTest is Test {
    MockUSDC usdc;
    Vault vault;
    uint256 constant G1 = 0xA11CE;
    uint256 constant G2 = 0xB0B;
    uint256 constant G3 = 0xC0FFEE;
    uint256 constant EVIL = 0xE111;
    address user = address(0xBEEF);
    address thief = address(0xDEAD);
    uint256 constant DELAY = 2 days;

    function setUp() public {
        vm.warp(1_800_000_000);
        address[] memory gs = new address[](3);
        gs[0] = vm.addr(G1);
        gs[1] = vm.addr(G2);
        gs[2] = vm.addr(G3);
        usdc = new MockUSDC();
        vault = new Vault(gs, 2, DELAY, 1);
        vault.setToken(address(usdc), true, 1e6);
        vault.setLimits(address(usdc), 10e6, 20e6); // 10 per release, 20 a day
        vault.finalizeSetup();
        usdc.mint(user, 1000e6);
        vm.startPrank(user);
        usdc.approve(address(vault), 1000e6);
        vault.deposit(address(usdc), 1000e6, "targ1qexample");
        vm.stopPrank();
    }

    function test_guardianPauseDoesNotStopCompromisedOwner() public {
        // The owner key is compromised: guardians and limits are proposed at once.
        address[] memory evil = new address[](1);
        evil[0] = vm.addr(EVIL);
        vault.proposeGuardians(evil, 1);
        vault.proposeLimits(address(usdc), 0, 0);

        // A guardian sees the Proposed events and pauses at once.
        vm.prank(vm.addr(G1));
        vault.pause();
        assertTrue(vault.paused());

        // The delay passes: both changes execute while the vault is paused.
        vm.warp(block.timestamp + DELAY);
        vault.executeGuardians(evil, 1);
        vault.executeLimits(address(usdc), 0, 0);

        // The old guardians can no longer pause (or cancel the unpause).
        vault.proposeUnpause();
        vm.prank(vm.addr(G1));
        vm.expectRevert(Vault.NotOwnerOrGuardian.selector);
        vault.pause();

        vm.warp(block.timestamp + DELAY);
        vault.executeUnpause();

        // One release signed by the owner's own guardian takes the backing.
        uint256 all = vault.obligations(address(usdc));
        bytes32 burn = keccak256("nothing was burned");
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(EVIL, vault.releaseDigest(address(usdc), thief, all, burn));
        bytes[] memory sigs = new bytes[](1);
        sigs[0] = abi.encodePacked(r, s, v);
        vault.release(address(usdc), thief, all, burn, sigs);
        assertEq(vault.obligations(address(usdc)), 0);
        assertGt(usdc.balanceOf(thief), 980e6);
        emit log_named_uint("taken by the owner, 2 x delay after its first transaction (USDC units)", usdc.balanceOf(thief));
    }

    function _vetoSigs(bytes32 id, uint256 a, uint256 b) internal view returns (bytes[] memory sigs) {
        bytes32 d = vault.vetoDigest(id);
        (uint256 lo, uint256 hi) = vm.addr(a) < vm.addr(b) ? (a, b) : (b, a);
        sigs = new bytes[](2);
        (uint8 v1, bytes32 r1, bytes32 s1) = vm.sign(lo, d);
        (uint8 v2, bytes32 r2, bytes32 s2) = vm.sign(hi, d);
        sigs[0] = abi.encodePacked(r1, s1, v1);
        sigs[1] = abi.encodePacked(r2, s2, v2);
    }

    /// Fixed 2026-10-28: a threshold of guardians vetoes what a stolen owner
    /// key proposes, during its delay; one guardian alone cannot.
    function test_guardiansVetoCompromisedOwner() public {
        address[] memory evil = new address[](1);
        evil[0] = vm.addr(EVIL);
        bytes32 g = vault.proposeGuardians(evil, 1);
        bytes32 l = vault.proposeLimits(address(usdc), 0, 0);

        // One guardian alone: refused.
        bytes[] memory one = new bytes[](1);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(G1, vault.vetoDigest(g));
        one[0] = abi.encodePacked(r, s, v);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.veto(g, one);
        // Signatures over another proposal: refused.
        bytes[] memory other = _vetoSigs(l, G1, G2);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.veto(g, other);

        // Two of three, submitted by anyone: both changes are dropped.
        vm.prank(address(0x1234));
        vault.veto(g, _vetoSigs(g, G1, G2));
        vault.veto(l, _vetoSigs(l, G2, G3));
        vm.warp(block.timestamp + DELAY);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeGuardians(evil, 1);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeLimits(address(usdc), 0, 0);
        assertTrue(vault.isGuardian(vm.addr(G1)));
        // A veto of nothing pending is refused.
        bytes[] memory again = _vetoSigs(g, G1, G2);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.veto(g, again);
    }
}
