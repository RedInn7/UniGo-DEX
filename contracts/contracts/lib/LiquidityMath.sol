// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title LiquidityMath
/// @notice 流动性安全加减运算
/// @dev 流动性使用 uint128 类型存储
///      添加流动性时加，移除流动性时减
///      需要防止溢出和下溢
library LiquidityMath {
    /// @notice 安全地对流动性进行加减运算
    /// @param x 当前流动性值
    /// @param y 变化量（带符号）
    /// @return z 新的流动性值
    function addDelta(uint128 x, int128 y) internal pure returns (uint128 z) {
        if (y < 0) {
            // 移除流动性：确保不会下溢
            require((z = x - uint128(-y)) < x, "LS");
        } else {
            // 添加流动性：确保不会溢出
            require((z = x + uint128(y)) >= x, "LA");
        }
    }
}
