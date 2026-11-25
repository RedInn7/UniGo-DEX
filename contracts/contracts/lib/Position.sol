// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./Math.sol";
import "./LiquidityMath.sol";

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
        /// @dev 上次结算时区间内的每单位流动性累计手续费（Q128.128）
        uint256 feeGrowthInside0LastX128;
        uint256 feeGrowthInside1LastX128;
        /// @dev 可领取的 token 数量（手续费 + burn 后待提取的本金）
        uint128 tokensOwed0;
        uint128 tokensOwed1;
    }

    /// @dev 2^128，手续费累计值的定点缩放因子
    uint256 internal constant Q128 = 0x100000000000000000000000000000000;

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

    /// @notice 结算手续费并更新 position 的流动性
    /// @dev 当用户 mint 或 burn 时调用；liquidityDelta 为 0 时只结算手续费
    /// @param self 要更新的 position
    /// @param liquidityDelta 流动性变化量（正=添加，负=移除）
    /// @param feeGrowthInside0X128 区间内 token0 的每单位流动性累计手续费
    /// @param feeGrowthInside1X128 区间内 token1 的每单位流动性累计手续费
    function update(
        Position.Info storage self,
        int128 liquidityDelta,
        uint256 feeGrowthInside0X128,
        uint256 feeGrowthInside1X128
    ) internal {
        uint128 liquidityBefore = self.liquidity;
        // 没有流动性的 position 不允许空结算
        if (liquidityDelta == 0) require(liquidityBefore > 0, "NP");

        // 自上次结算以来，按旧流动性应得的手续费（减法依赖溢出回绕）
        uint128 tokensOwed0;
        uint128 tokensOwed1;
        unchecked {
            tokensOwed0 = uint128(Math.mulDiv(
                feeGrowthInside0X128 - self.feeGrowthInside0LastX128, liquidityBefore, Q128
            ));
            tokensOwed1 = uint128(Math.mulDiv(
                feeGrowthInside1X128 - self.feeGrowthInside1LastX128, liquidityBefore, Q128
            ));
        }

        if (liquidityDelta != 0) self.liquidity = LiquidityMath.addDelta(liquidityBefore, liquidityDelta);
        self.feeGrowthInside0LastX128 = feeGrowthInside0X128;
        self.feeGrowthInside1LastX128 = feeGrowthInside1X128;
        if (tokensOwed0 > 0 || tokensOwed1 > 0) {
            self.tokensOwed0 += tokensOwed0;
            self.tokensOwed1 += tokensOwed1;
        }
    }
}
