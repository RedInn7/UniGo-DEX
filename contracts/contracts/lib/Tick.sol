// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./LiquidityMath.sol";
import "./TickMath.sol";

/// @title Tick
/// @notice Tick 状态管理库
/// @dev 每个已初始化的 tick 存储以下关键信息：
///      - liquidityGross: 该 tick 上的总流动性引用计数
///      - liquidityNet: 穿过该 tick 时的净流动性变化
///      - feeGrowthOutside: tick 外侧累计的手续费，用于计算任意区间内的手续费
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
        /// @dev tick “外侧”累计的每单位流动性手续费（Q128.128）
        ///      外侧 = 相对当前价格的另一侧；每次价格穿过该 tick 时翻转
        uint256 feeGrowthOutside0X128;
        uint256 feeGrowthOutside1X128;
        /// @dev 标记此 tick 是否已初始化（有 position 引用）
        bool initialized;
    }

    /// @notice 由 tickSpacing 推导单个 tick 允许承载的最大流动性
    /// @dev 把 uint128 的容量平均分给所有可用的 tick，
    ///      保证即使每个 tick 都被用满，全局活跃流动性也不会溢出 uint128，
    ///      同时让 liquidityNet（int128）不可能溢出
    /// @param tickSpacing tick 间距
    /// @return 单个 tick 的 liquidityGross 上限
    function tickSpacingToMaxLiquidityPerTick(int24 tickSpacing) internal pure returns (uint128) {
        int24 minTick = (TickMath.MIN_TICK / tickSpacing) * tickSpacing;
        int24 maxTick = (TickMath.MAX_TICK / tickSpacing) * tickSpacing;
        uint24 numTicks = uint24((maxTick - minTick) / tickSpacing) + 1;
        return type(uint128).max / numTicks;
    }

    /// @notice 更新 tick 的流动性信息
    /// @dev 当用户添加或移除流动性时调用
    /// @param self tick 状态映射
    /// @param tick 要更新的 tick
    /// @param tickCurrent 当前价格所在 tick
    /// @param liquidityDelta 流动性变化量（正=添加，负=移除）
    /// @param feeGrowthGlobal0X128 token0 的全局累计手续费
    /// @param feeGrowthGlobal1X128 token1 的全局累计手续费
    /// @param upper 是否为 position 的上界 tick
    /// @param maxLiquidity 单个 tick 允许承载的最大流动性
    /// @return flipped tick 的初始化状态是否发生了翻转
    ///         （从无引用变为有引用，或从有引用变为无引用）
    function update(
        mapping(int24 => Tick.Info) storage self,
        int24 tick,
        int24 tickCurrent,
        int128 liquidityDelta,
        uint256 feeGrowthGlobal0X128,
        uint256 feeGrowthGlobal1X128,
        bool upper,
        uint128 maxLiquidity
    ) internal returns (bool flipped) {
        Tick.Info storage info = self[tick];

        uint128 liquidityGrossBefore = info.liquidityGross;
        uint128 liquidityGrossAfter = LiquidityMath.addDelta(liquidityGrossBefore, liquidityDelta);

        // 超过上限会让 liquidityNet 和全局活跃流动性失真，直接拒绝
        require(liquidityGrossAfter <= maxLiquidity, "LO");

        // 判断初始化状态是否翻转
        flipped = (liquidityGrossAfter == 0) != (liquidityGrossBefore == 0);

        if (liquidityGrossBefore == 0) {
            // 约定：tick 初始化之前的手续费全部发生在 tick 下方
            if (tick <= tickCurrent) {
                info.feeGrowthOutside0X128 = feeGrowthGlobal0X128;
                info.feeGrowthOutside1X128 = feeGrowthGlobal1X128;
            }
            info.initialized = true;
        }

        info.liquidityGross = liquidityGrossAfter;

        // 上界 tick 的 liquidityNet 方向相反
        // 这确保从左向右穿过时：进入区间加流动性，离开区间减流动性
        // 直接用 int128 做受检运算，溢出时 revert 而不是静默截断
        info.liquidityNet = upper
            ? info.liquidityNet - liquidityDelta
            : info.liquidityNet + liquidityDelta;

        // 如果流动性清零，重置初始化标记
        if (liquidityGrossAfter == 0) {
            info.initialized = false;
        }
    }

    /// @notice 清除 tick 的全部状态（流动性归零后调用）
    function clear(mapping(int24 => Tick.Info) storage self, int24 tick) internal {
        delete self[tick];
    }

    /// @notice 计算 [tickLower, tickUpper) 区间内累计的每单位流动性手续费
    /// @dev 区间内 = 全局 - 下界以下 - 上界以上；减法依赖溢出回绕，必须 unchecked
    function getFeeGrowthInside(
        mapping(int24 => Tick.Info) storage self,
        int24 tickLower,
        int24 tickUpper,
        int24 tickCurrent,
        uint256 feeGrowthGlobal0X128,
        uint256 feeGrowthGlobal1X128
    ) internal view returns (uint256 feeGrowthInside0X128, uint256 feeGrowthInside1X128) {
        Tick.Info storage lower = self[tickLower];
        Tick.Info storage upper = self[tickUpper];

        unchecked {
            // 下界以下的手续费
            uint256 feeGrowthBelow0X128;
            uint256 feeGrowthBelow1X128;
            if (tickCurrent >= tickLower) {
                feeGrowthBelow0X128 = lower.feeGrowthOutside0X128;
                feeGrowthBelow1X128 = lower.feeGrowthOutside1X128;
            } else {
                feeGrowthBelow0X128 = feeGrowthGlobal0X128 - lower.feeGrowthOutside0X128;
                feeGrowthBelow1X128 = feeGrowthGlobal1X128 - lower.feeGrowthOutside1X128;
            }

            // 上界以上的手续费
            uint256 feeGrowthAbove0X128;
            uint256 feeGrowthAbove1X128;
            if (tickCurrent < tickUpper) {
                feeGrowthAbove0X128 = upper.feeGrowthOutside0X128;
                feeGrowthAbove1X128 = upper.feeGrowthOutside1X128;
            } else {
                feeGrowthAbove0X128 = feeGrowthGlobal0X128 - upper.feeGrowthOutside0X128;
                feeGrowthAbove1X128 = feeGrowthGlobal1X128 - upper.feeGrowthOutside1X128;
            }

            feeGrowthInside0X128 = feeGrowthGlobal0X128 - feeGrowthBelow0X128 - feeGrowthAbove0X128;
            feeGrowthInside1X128 = feeGrowthGlobal1X128 - feeGrowthBelow1X128 - feeGrowthAbove1X128;
        }
    }

    /// @notice 当价格穿过 tick 时调用
    /// @dev 翻转 tick 外侧的手续费累计值，并返回需要应用到当前流动性的净变化量
    /// @param self tick 状态映射
    /// @param tick 被穿过的 tick
    /// @param feeGrowthGlobal0X128 token0 的全局累计手续费
    /// @param feeGrowthGlobal1X128 token1 的全局累计手续费
    /// @return liquidityNet 流动性净变化量
    function cross(
        mapping(int24 => Tick.Info) storage self,
        int24 tick,
        uint256 feeGrowthGlobal0X128,
        uint256 feeGrowthGlobal1X128
    ) internal returns (int128 liquidityNet) {
        Tick.Info storage info = self[tick];
        unchecked {
            info.feeGrowthOutside0X128 = feeGrowthGlobal0X128 - info.feeGrowthOutside0X128;
            info.feeGrowthOutside1X128 = feeGrowthGlobal1X128 - info.feeGrowthOutside1X128;
        }
        liquidityNet = info.liquidityNet;
    }
}
