// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Test.sol";
import "../Vault.sol";
import "../MockUSDC.sol";

/// Review F (second pass, 2026-10-28): the guardian veto and the release
/// signature encoding. Fixed the same day: a veto is bound to the proposal
/// instance it names (its eta and nonce), and the proposal that follows a
/// veto of the same change is an owner override that waits OVERRIDE_DELAY
/// and cannot be vetoed, so guardians delay the owner but never freeze the
/// vault.
contract ReviewFVetoTest is Test {
    MockUSDC usdc;
    Vault vault;
    uint256 constant G1 = 0xA11CE;
    uint256 constant G2 = 0xB0B;
    uint256 constant G3 = 0xC0FFEE;
    uint256 constant NEW1 = 0x1111;
    uint256 constant NEW2 = 0x2222;
    uint256 constant NEW3 = 0x3333;
    address user = address(0xBEEF);
    uint256 constant DELAY = 2 days;
    uint256 constant N = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141;

    function setUp() public {
        vm.warp(1_800_000_000);
        address[] memory gs = new address[](3);
        gs[0] = vm.addr(G1);
        gs[1] = vm.addr(G2);
        gs[2] = vm.addr(G3);
        usdc = new MockUSDC();
        vault = new Vault(gs, 2, DELAY, 1);
        vault.setToken(address(usdc), true, 1e6);
        vault.setLimits(address(usdc), 10e6, 20e6);
        vault.finalizeSetup();
        usdc.mint(user, 1000e6);
        vm.startPrank(user);
        usdc.approve(address(vault), 1000e6);
        vault.deposit(address(usdc), 1000e6, "targ1qexample");
        vm.stopPrank();
    }

    function _sigs(bytes32 d, uint256 a, uint256 b) internal pure returns (bytes[] memory sigs) {
        (uint256 lo, uint256 hi) = vm.addr(a) < vm.addr(b) ? (a, b) : (b, a);
        sigs = new bytes[](2);
        (uint8 v1, bytes32 r1, bytes32 s1) = vm.sign(lo, d);
        (uint8 v2, bytes32 r2, bytes32 s2) = vm.sign(hi, d);
        sigs[0] = abi.encodePacked(r1, s1, v1);
        sigs[1] = abi.encodePacked(r2, s2, v2);
    }

    function _fresh() internal pure returns (address[] memory fresh) {
        fresh = new address[](3);
        fresh[0] = vm.addr(NEW1);
        fresh[1] = vm.addr(NEW2);
        fresh[2] = vm.addr(NEW3);
    }

    /// The veto digest names the proposal instance (its eta and nonce), so
    /// signatures copied from an earlier veto's calldata never veto a later
    /// proposal of the same change. It used to name the id alone, and
    /// executeUnpause takes no arguments: one vetoed unpause made every
    /// later unpause vetoable by anyone replaying the signatures.
    function test_vetoDoesNotReplayOnReproposal() public {
        vm.prank(vm.addr(G1));
        vault.pause();
        bytes32 id = vault.proposeUnpause();
        assertEq(vault.proposalNonce(id), 1);
        bytes[] memory sigs = _sigs(vault.vetoDigest(id), G1, G2);
        vault.veto(id, sigs); // the guardians think it is too early
        assertTrue(vault.vetoed(id));

        // Weeks later: the owner's next unpause is an override, which no
        // veto drops, replayed or fresh.
        vm.warp(block.timestamp + 30 days);
        bytes32 again = vault.proposeUnpause();
        assertEq(again, id, "same change, same id");
        assertTrue(vault.overrideOf(id));
        assertFalse(vault.vetoed(id));
        assertEq(vault.proposalEta(id), block.timestamp + vault.OVERRIDE_DELAY());
        vm.prank(address(0xBAD));
        vm.expectRevert(Vault.NotVetoable.selector);
        vault.veto(again, sigs);
        bytes[] memory fresh = _sigs(vault.vetoDigest(id), G1, G2);
        vm.expectRevert(Vault.NotVetoable.selector);
        vault.veto(again, fresh);

        // The owner drops the override and proposes again: a normal
        // proposal, vetoable, but not by the old signatures (another eta and
        // nonce), nor by signatures made for the override instance.
        vault.cancel(id);
        assertFalse(vault.overrideOf(id));
        bytes32 third = vault.proposeUnpause();
        assertFalse(vault.overrideOf(third));
        assertEq(vault.proposalEta(third), block.timestamp + DELAY);
        vm.prank(address(0xBAD));
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.veto(third, sigs);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.veto(third, fresh);

        // Cancelled and proposed again in the same block (the same eta):
        // the nonce still tells the instances apart.
        bytes[] memory forThird = _sigs(vault.vetoDigest(third), G1, G2);
        vault.cancel(third);
        bytes32 fourth = vault.proposeUnpause();
        assertEq(vault.proposalEta(fourth), block.timestamp + DELAY);
        assertEq(vault.proposalNonce(fourth), 4);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.veto(fourth, forThird);

        // Nobody vetoes it: the vault runs again after the delay.
        vm.warp(block.timestamp + DELAY);
        vault.executeUnpause();
        assertFalse(vault.paused());
    }

    /// A threshold of compromised guardians can already release up to the
    /// limits; the owner's way out is pause, rotate the guardians, unpause.
    /// Before the fix the same guardians vetoed the rotation and every
    /// unpause, round after round, and the backing stayed frozen for good.
    /// Now the second proposal of the rotation is an override: it cannot be
    /// vetoed and executes after OVERRIDE_DELAY.
    function test_compromisedGuardianMajorityCannotFreezeVault() public {
        vm.prank(vm.addr(G3)); // the honest guardian sees thefts and pauses
        vault.pause();
        address[] memory fresh = _fresh();
        bytes32 g = vault.proposeGuardians(fresh, 2);
        vault.veto(g, _sigs(vault.vetoDigest(g), G1, G2)); // the compromised two
        bytes32 u = vault.proposeUnpause();
        vault.veto(u, _sigs(vault.vetoDigest(u), G1, G2));
        vm.warp(block.timestamp + DELAY);
        vm.expectRevert(Vault.NotProposed.selector);
        vault.executeGuardians(fresh, 2);

        // The owner insists: an override, which the two cannot veto.
        uint256 start = block.timestamp;
        assertEq(vault.proposeGuardians(fresh, 2), g);
        assertTrue(vault.overrideOf(g));
        assertEq(vault.proposalEta(g), start + 2 * vault.MAX_DELAY());
        bytes[] memory again = _sigs(vault.vetoDigest(g), G1, G2);
        vm.expectRevert(Vault.NotVetoable.selector);
        vault.veto(g, again);
        bytes[] memory all3 = new bytes[](3);
        {
            bytes[] memory two = _sigs(vault.vetoDigest(g), G1, G2);
            (uint8 v, bytes32 r, bytes32 s) = vm.sign(G3, vault.vetoDigest(g));
            bytes memory third = abi.encodePacked(r, s, v);
            // ordered by signer address
            address a1 = vm.addr(G1) < vm.addr(G2) ? vm.addr(G1) : vm.addr(G2);
            address a2 = vm.addr(G1) < vm.addr(G2) ? vm.addr(G2) : vm.addr(G1);
            address a3 = vm.addr(G3);
            if (a3 < a1) {
                all3[0] = third;
                all3[1] = two[0];
                all3[2] = two[1];
            } else if (a3 < a2) {
                all3[0] = two[0];
                all3[1] = third;
                all3[2] = two[1];
            } else {
                all3[0] = two[0];
                all3[1] = two[1];
                all3[2] = third;
            }
        }
        vm.expectRevert(Vault.NotVetoable.selector);
        vault.veto(g, all3); // not even every guardian

        // A pause by the compromised guardians does not touch it; an
        // override unpause made now would be dropped by their next pause,
        // which is why the rotation comes first.
        vm.prank(vm.addr(G1));
        vault.pause();
        assertEq(vault.proposalEta(g), start + 2 * vault.MAX_DELAY());

        // Not before the override delay, at once after it.
        vm.warp(start + 2 * vault.MAX_DELAY() - 1);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeGuardians(fresh, 2);
        vm.warp(start + 2 * vault.MAX_DELAY());
        vault.executeGuardians(fresh, 2);
        assertFalse(vault.overrideOf(g));
        assertFalse(vault.isGuardian(vm.addr(G1)));
        assertTrue(vault.isGuardian(vm.addr(NEW1)));
        vm.prank(vm.addr(G1));
        vm.expectRevert(Vault.NotOwnerOrGuardian.selector);
        vault.pause();

        // The unpause was vetoed too: its next proposal is an override as
        // well, and executes after OVERRIDE_DELAY whatever anyone signs.
        bytes32 u2 = vault.proposeUnpause();
        assertEq(u2, u);
        assertTrue(vault.overrideOf(u));
        bytes[] memory byNew = _sigs(vault.vetoDigest(u), NEW1, NEW2);
        vm.expectRevert(Vault.NotVetoable.selector);
        vault.veto(u, byNew);
        vm.warp(block.timestamp + vault.OVERRIDE_DELAY());
        vault.executeUnpause();
        assertFalse(vault.paused());
        assertEq(vault.obligations(address(usdc)), 1000e6, "the backing, intact");

        // Releases work again, with the new guardians' signatures.
        bytes32 burn = keccak256("burn");
        address to = address(0xCAFE);
        vault.release(
            address(usdc), to, 5e6, burn, _sigs(vault.releaseDigest(address(usdc), to, 5e6, burn), NEW1, NEW3)
        );
        assertTrue(vault.released(burn));
    }

    /// Ordinary proposals keep `delay` and stay vetoable: before any veto,
    /// after an override was executed, and after an override expired.
    function test_overrideOnlyAfterVeto() public {
        bytes32 f = vault.proposeFees(10, 10);
        assertFalse(vault.overrideOf(f));
        assertEq(vault.proposalEta(f), block.timestamp + DELAY);
        vault.veto(f, _sigs(vault.vetoDigest(f), G2, G3));
        assertTrue(vault.vetoed(f));

        // The override, executed after OVERRIDE_DELAY.
        vault.proposeFees(10, 10);
        assertTrue(vault.overrideOf(f));
        vm.warp(block.timestamp + DELAY);
        vm.expectRevert(Vault.TooEarly.selector);
        vault.executeFees(10, 10);
        vm.warp(block.timestamp + vault.OVERRIDE_DELAY() - DELAY);
        vault.executeFees(10, 10);
        assertEq(vault.depositFeeBps(), 10);
        assertFalse(vault.overrideOf(f));

        // The same change proposed later: normal again, and vetoable.
        bytes32 f2 = vault.proposeFees(10, 10);
        assertEq(f2, f);
        assertFalse(vault.overrideOf(f));
        assertEq(vault.proposalEta(f), block.timestamp + DELAY);
        vault.veto(f, _sigs(vault.vetoDigest(f), G1, G3));

        // An override left to expire: proposed again, it is normal.
        vault.proposeFees(10, 10);
        assertTrue(vault.overrideOf(f));
        vm.warp(block.timestamp + vault.OVERRIDE_DELAY() + vault.GRACE_PERIOD() + 1);
        vm.expectRevert(Vault.ProposalExpired.selector);
        vault.executeFees(10, 10);
        vault.proposeFees(10, 10);
        assertFalse(vault.overrideOf(f));
        assertEq(vault.proposalEta(f), block.timestamp + DELAY);
        vault.veto(f, _sigs(vault.vetoDigest(f), G1, G2));
        assertEq(vault.proposalEta(f), 0);

        // Other changes are not affected by a veto of this one.
        bytes32 l = vault.proposeLimits(address(usdc), 1e6, 2e6);
        assertFalse(vault.overrideOf(l));
        assertEq(vault.proposalEta(l), block.timestamp + DELAY);
    }

    /// The vault accepts only v of 27 or 28 (0 and 1 taken as 27 and 28)
    /// and a low s. A guardian's own signature over the right digest,
    /// re-encoded with v + 4 (the "compressed key" recovery code) or with
    /// s' = N - s and v flipped, makes release revert; the guardians count
    /// neither toward the threshold (TestReviewF_NonCanonicalReleaseSignature).
    function test_nonCanonicalReleaseSignaturesRevert() public {
        address to = address(0xCAFE);
        bytes32 burn = keccak256("burn");
        bytes32 d = vault.releaseDigest(address(usdc), to, 5e6, burn);
        (uint256 lo, uint256 hi) = vm.addr(G1) < vm.addr(G2) ? (G1, G2) : (G2, G1);
        (uint8 v1, bytes32 r1, bytes32 s1) = vm.sign(lo, d);
        (uint8 v2, bytes32 r2, bytes32 s2) = vm.sign(hi, d);
        bytes[] memory sigs = new bytes[](2);
        sigs[0] = abi.encodePacked(r1, s1, v1);

        sigs[1] = abi.encodePacked(r2, s2, v2 + 4);
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), to, 5e6, burn, sigs);

        sigs[1] = abi.encodePacked(r2, bytes32(N - uint256(s2)), v2 == 27 ? uint8(28) : uint8(27));
        vm.expectRevert(Vault.BadSignatures.selector);
        vault.release(address(usdc), to, 5e6, burn, sigs);

        // v as 0 or 1 is taken as 27 or 28.
        sigs[1] = abi.encodePacked(r2, s2, v2 - 27);
        vault.release(address(usdc), to, 5e6, burn, sigs);
        assertTrue(vault.released(burn));
    }
}
