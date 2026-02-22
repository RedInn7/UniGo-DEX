// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title FixedPoint96
/// @notice Q64.96 定点数格式的常量定义
/// @dev 在 Uniswap V3 中，价格使用 Q64.96 格式表示（即 sqrtPriceX96）
///      Q64.96 意味着：64 位整数部分 + 96 位小数部分
///      值 = 实际值 * 2^96
library FixedPoint96 {
    /// @dev 小数部分的位数
    uint8 internal constant RESOLUTION = 96;

    /// @dev 2^96，用于定点数计算中的缩放因子
    uint256 internal constant Q96 = 0x1000000000000000000000000; // 2^96
}
