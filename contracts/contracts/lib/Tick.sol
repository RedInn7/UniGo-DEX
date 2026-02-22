// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./LiquidityMath.sol";

/// @title Tick
/// @notice Tick 状态管理库
/// @dev 每个已初始化的 tick 存储以下关键信息：
///      - liquidityGross: 该 tick 上的总流动性引用计数
///      - liquidityNet: 穿过该 tick 时的净流动性变化
///
///      当价格从左向右穿过某个 tick 时：
///      - 如果是某个 position 的下界，流动性增加（liquidityNet > 0）
///      - 如果是某个 position 的上界，流动性减少（liquidityNet < 0）
library Tick {
    /// @notice 单个 tick 的状态数据
    struct Info {
        /// @dev 引用此 tick 的所有 position 的流动性总和
        ///      用于判断 tick 是否需要从 bitmap 中移除（当 liquidityGross 变为 0 时）
        uint128 liquidityGross;
        /// @dev 当价格从左向右穿过此 tick 时，流动性的净变化量
        ///      正值表示进入区间（tick 是某 position 的下界）
        ///      负值表示离开区间（tick 是某 position 的上界）
        int128 liquidityNet;
        /// @dev 标记此 tick 是否已初始化（有 position 引用）
        bool initialized;
    }

    /// @notice 更新 tick 的流动性信息
    /// @dev 当用户添加或移除流动性时调用
    /// @param self tick 状态映射
    /// @param tick 要更新的 tick
    /// @param liquidityDelta 流动性变化量（正=添加，负=移除）
    /// @param upper 是否为 position 的上界 tick
    /// @return flipped tick 的初始化状态是否发生了翻转
    ///         （从无引用变为有引用，或从有引用变为无引用）
    function update(
        mapping(int24 => Tick.Info) storage self,
        int24 tick,
        int128 liquidityDelta,
        bool upper
    ) internal returns (bool flipped) {
        Tick.Info storage info = self[tick];

        uint128 liquidityGrossBefore = info.liquidityGross;
        uint128 liquidityGrossAfter = LiquidityMath.addDelta(liquidityGrossBefore, liquidityDelta);

        // 判断初始化状态是否翻转
        flipped = (liquidityGrossAfter == 0) != (liquidityGrossBefore == 0);

        if (liquidityGrossBefore == 0) {
            info.initialized = true;
        }

        info.liquidityGross = liquidityGrossAfter;

        // 上界 tick 的 liquidityNet 方向相反
        // 这确保从左向右穿过时：进入区间加流动性，离开区间减流动性
        info.liquidityNet = upper
            ? int128(int256(info.liquidityNet) - liquidityDelta)
            : int128(int256(info.liquidityNet) + liquidityDelta);

        // 如果流动性清零，重置初始化标记
        if (liquidityGrossAfter == 0) {
            info.initialized = false;
        }
    }

    /// @notice 当价格穿过 tick 时调用
    /// @dev 返回需要应用到当前流动性的净变化量
    /// @param self tick 状态映射
    /// @param tick 被穿过的 tick
    /// @return liquidityNet 流动性净变化量
    function cross(
        mapping(int24 => Tick.Info) storage self,
        int24 tick
    ) internal view returns (int128 liquidityNet) {
        Tick.Info storage info = self[tick];
        liquidityNet = info.liquidityNet;
    }
}
