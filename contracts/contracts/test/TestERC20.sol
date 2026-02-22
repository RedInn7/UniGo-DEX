// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// @title TestERC20
/// @notice 仅用于测试的 ERC20 代币合约
/// @dev 任何人都可以铸造代币，仅供测试使用
contract TestERC20 is ERC20 {
    constructor(string memory name_, string memory symbol_) ERC20(name_, symbol_) {}

    /// @notice 铸造代币（仅供测试）
    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}
