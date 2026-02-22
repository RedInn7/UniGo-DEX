// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title Position
/// @notice Position（头寸）状态管理库
/// @dev Position 代表用户在某个 [tickLower, tickUpper) 区间内提供的流动性
///      每个 position 由 (owner, tickLower, tickUpper) 三元组唯一标识
///      position 的 key 通过 keccak256(abi.encodePacked(owner, tickLower, tickUpper)) 计算
library Position {
    /// @notice Position 的状态数据
    struct Info {
        /// @dev 该 position 提供的流动性数量
        uint128 liquidity;
    }

    /// @notice 获取指定 position 的信息
    /// @param self position 状态映射
    /// @param owner position 拥有者地址
    /// @param tickLower position 的下界 tick
    /// @param tickUpper position 的上界 tick
    /// @return position 对应的 Info 引用
    function get(
        mapping(bytes32 => Position.Info) storage self,
        address owner,
        int24 tickLower,
        int24 tickUpper
    ) internal view returns (Position.Info storage position) {
        position = self[keccak256(abi.encodePacked(owner, tickLower, tickUpper))];
    }

    /// @notice 更新 position 的流动性
    /// @dev 当用户 mint 或 burn 时调用
    /// @param self 要更新的 position
    /// @param liquidityDelta 流动性变化量（正=添加，负=移除）
    function update(
        Position.Info storage self,
        int128 liquidityDelta
    ) internal {
        uint128 liquidityBefore = self.liquidity;
        // Solidity 0.8+ 自带溢出检查
        uint128 liquidityAfter = liquidityDelta < 0
            ? liquidityBefore - uint128(-liquidityDelta)
            : liquidityBefore + uint128(liquidityDelta);
        self.liquidity = liquidityAfter;
    }
}
