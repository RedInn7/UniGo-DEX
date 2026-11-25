// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title TestNoReturnERC20
/// @notice 仅用于测试：transfer / transferFrom / approve 都没有返回值的代币（USDT 式）
/// @dev 这类代币不符合 ERC20 标准的返回值约定，按标准接口直接调用会因为解码返回值失败而 revert
contract TestNoReturnERC20 {
    string public name = "No Return Token";
    string public symbol = "NRT";
    uint8 public decimals = 18;
    uint256 public totalSupply;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        totalSupply += amount;
    }

    function approve(address spender, uint256 amount) external {
        allowance[msg.sender][spender] = amount;
    }

    function transfer(address to, uint256 amount) external {
        _transfer(msg.sender, to, amount);
    }

    function transferFrom(address from, address to, uint256 amount) external {
        require(allowance[from][msg.sender] >= amount, "ALLOWANCE");
        if (allowance[from][msg.sender] != type(uint256).max) allowance[from][msg.sender] -= amount;
        _transfer(from, to, amount);
    }

    function _transfer(address from, address to, uint256 amount) private {
        require(balanceOf[from] >= amount, "BALANCE");
        balanceOf[from] -= amount;
        balanceOf[to] += amount;
    }
}
