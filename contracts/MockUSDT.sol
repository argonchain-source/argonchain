// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title MockUSDT: a six-decimal ERC-20 anyone can mint, non-standard like
///        mainnet Tether: `transfer`, `transferFrom` and `approve` return
///        nothing, so a caller that expects a boolean reverts on decoding. It
///        exists to prove the vault copes with that. Fine for local chains and
///        public testnets, where the bridge page offers `mint` as a faucet
///        (daemon flag -token-mint). It has no value and must never be
///        deployed to mainnet or stand behind anything real.
contract MockUSDT {
    string public constant name = "Mock Tether USD";
    string public constant symbol = "USDT";
    uint8 public constant decimals = 6;
    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    function mint(address to, uint256 value) external {
        totalSupply += value;
        balanceOf[to] += value;
        emit Transfer(address(0), to, value);
    }

    function transfer(address to, uint256 value) external {
        _transfer(msg.sender, to, value);
    }

    function approve(address spender, uint256 value) external {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
    }

    function transferFrom(address from, address to, uint256 value) external {
        uint256 a = allowance[from][msg.sender];
        require(a >= value, "allowance");
        if (a != type(uint256).max) allowance[from][msg.sender] = a - value;
        _transfer(from, to, value);
    }

    function _transfer(address from, address to, uint256 value) internal {
        require(to != address(0), "zero address");
        uint256 b = balanceOf[from];
        require(b >= value, "balance");
        unchecked {
            balanceOf[from] = b - value;
        }
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}
