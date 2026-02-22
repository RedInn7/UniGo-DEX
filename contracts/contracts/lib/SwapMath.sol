// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./Math.sol";
import "./SqrtPriceMath.sol";

/// @title SwapMath
/// @notice 单步 Swap 计算逻辑
/// @dev 在一个 tick 区间内（流动性不变的区域），计算：
///      1. 能达到的新价格（可能受限于目标价格或输入数量）
///      2. 实际消耗的输入数量
///      3. 实际输出的数量
///      4. 手续费
///
///      swap 循环中，每一步都调用此函数，直到完成全部交换
library SwapMath {
    /// @notice 计算单步 swap 的结果
    /// @param sqrtRatioCurrentX96 当前价格
    /// @param sqrtRatioTargetX96 目标价格（下一个已初始化的 tick 对应的价格）
    /// @param liquidity 当前区间的流动性
    /// @param amountRemaining 剩余待交换的数量（正=exactInput, 负=exactOutput）
    /// @param feePips 手续费率（以百万分之一为单位，如 3000 = 0.3%）
    /// @return sqrtRatioNextX96 这一步结束后的价格
    /// @return amountIn 本步消耗的输入 token 数量
    /// @return amountOut 本步输出的 token 数量
    /// @return feeAmount 本步收取的手续费
    function computeSwapStep(
        uint160 sqrtRatioCurrentX96,
        uint160 sqrtRatioTargetX96,
        uint128 liquidity,
        int256 amountRemaining,
        uint24 feePips
    )
        internal
        pure
        returns (
            uint160 sqrtRatioNextX96,
            uint256 amountIn,
            uint256 amountOut,
            uint256 feeAmount
        )
    {
        // 判断交易方向：价格下降 = token0 -> token1
        bool zeroForOne = sqrtRatioCurrentX96 >= sqrtRatioTargetX96;
        // 判断交易模式：正数 = exactInput
        bool exactIn = amountRemaining >= 0;

        if (exactIn) {
            // === exactInput 模式 ===
            // 先扣除手续费，计算实际可用于交换的输入数量
            uint256 amountRemainingLessFee = Math.mulDiv(
                uint256(amountRemaining),
                1e6 - feePips,
                1e6
            );

            // 计算到达目标价格需要的输入数量
            amountIn = zeroForOne
                ? SqrtPriceMath.getAmount0Delta(sqrtRatioTargetX96, sqrtRatioCurrentX96, liquidity, true)
                : SqrtPriceMath.getAmount1Delta(sqrtRatioCurrentX96, sqrtRatioTargetX96, liquidity, true);

            // 判断是否能到达目标价格
            if (amountRemainingLessFee >= amountIn) {
                // 输入足够，到达目标价格
                sqrtRatioNextX96 = sqrtRatioTargetX96;
            } else {
                // 输入不足，计算能到达的价格
                sqrtRatioNextX96 = SqrtPriceMath.getNextSqrtPriceFromInput(
                    sqrtRatioCurrentX96,
                    liquidity,
                    amountRemainingLessFee,
                    zeroForOne
                );
            }
        } else {
            // === exactOutput 模式 ===
            // 计算到达目标价格的输出数量
            amountOut = zeroForOne
                ? SqrtPriceMath.getAmount1Delta(sqrtRatioTargetX96, sqrtRatioCurrentX96, liquidity, false)
                : SqrtPriceMath.getAmount0Delta(sqrtRatioCurrentX96, sqrtRatioTargetX96, liquidity, false);

            if (uint256(-amountRemaining) >= amountOut) {
                sqrtRatioNextX96 = sqrtRatioTargetX96;
            } else {
                sqrtRatioNextX96 = SqrtPriceMath.getNextSqrtPriceFromOutput(
                    sqrtRatioCurrentX96,
                    liquidity,
                    uint256(-amountRemaining),
                    zeroForOne
                );
            }
        }

        // 是否到达了目标价格
        bool max = sqrtRatioTargetX96 == sqrtRatioNextX96;

        // 计算实际的输入和输出数量
        if (zeroForOne) {
            amountIn = max && exactIn
                ? amountIn
                : SqrtPriceMath.getAmount0Delta(sqrtRatioNextX96, sqrtRatioCurrentX96, liquidity, true);
            amountOut = max && !exactIn
                ? amountOut
                : SqrtPriceMath.getAmount1Delta(sqrtRatioNextX96, sqrtRatioCurrentX96, liquidity, false);
        } else {
            amountIn = max && exactIn
                ? amountIn
                : SqrtPriceMath.getAmount1Delta(sqrtRatioCurrentX96, sqrtRatioNextX96, liquidity, true);
            amountOut = max && !exactIn
                ? amountOut
                : SqrtPriceMath.getAmount0Delta(sqrtRatioCurrentX96, sqrtRatioNextX96, liquidity, false);
        }

        // 确保输出不超过剩余需求（exactOutput 模式下可能因取整产生微小偏差）
        if (!exactIn && amountOut > uint256(-amountRemaining)) {
            amountOut = uint256(-amountRemaining);
        }

        // 计算手续费
        if (exactIn && sqrtRatioNextX96 != sqrtRatioTargetX96) {
            // 未到达目标价格，剩余部分全部作为手续费
            feeAmount = uint256(amountRemaining) - amountIn;
        } else {
            // 根据费率计算手续费
            feeAmount = Math.mulDivRoundingUp(amountIn, feePips, 1e6 - feePips);
        }
    }
}
