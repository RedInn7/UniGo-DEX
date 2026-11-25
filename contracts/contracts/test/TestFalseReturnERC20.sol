// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title TestFalseReturnERC20
/// @notice 仅用于测试：转账失败时返回 false 而不是 revert 的代币
/// @dev 余额或授权不足、或者被暂停时，transfer / transferFrom 不动账并返回 false；
///      调用方如果不检查返回值，就会把失败的转账当成成功
contract TestFalseReturnERC20 {
    string public name = "False Return Token";
    string public symbol = "FRT";
    uint8 public decimals = 18;
    uint256 public totalSupply;

    /// @notice 暂停后所有转账都返回 false
    bool public paused;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        totalSupply += amount;
    }

    function setPaused(bool paused_) external {
        paused = paused_;
    }

    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount;
        return true;
    }

    function transfer(address to, uint256 amount) external returns (bool) {
        return _transfer(msg.sender, to, amount);
    }

    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        if (allowance[from][msg.sender] < amount) return false;
        if (!_transfer(from, to, amount)) return false;
        if (allowance[from][msg.sender] != type(uint256).max) allowance[from][msg.sender] -= amount;
        return true;
    }

    function _transfer(address from, address to, uint256 amount) private returns (bool) {
        if (paused || balanceOf[from] < amount) return false;
        balanceOf[from] -= amount;
        balanceOf[to] += amount;
        return true;
    }
}
