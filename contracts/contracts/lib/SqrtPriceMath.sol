// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./Math.sol";
import "./FixedPoint96.sol";

/// @title SqrtPriceMath
/// @notice 基于平方根价格计算 token 数量的数学库
/// @dev 核心公式（来自 x * y = L^2 恒等式的变形）：
///      - token0 数量 = L * (1/sqrtP_lower - 1/sqrtP_upper)
///      - token1 数量 = L * (sqrtP_upper - sqrtP_lower)
///      所有计算使用 Q64.96 定点数格式
library SqrtPriceMath {
    /// @notice 根据 token0 数量变化计算新的 sqrtPriceX96
    /// @dev 公式：sqrtP_new = sqrtP * L / (L + amount * sqrtP)
    /// @param sqrtPriceX96 当前平方根价格
    /// @param liquidity 可用流动性
    /// @param amount token0 数量
    /// @param add 是否增加 token0（true=添加流动性方向）
    /// @return 新的 sqrtPriceX96
    function getNextSqrtPriceFromAmount0RoundingUp(
        uint160 sqrtPriceX96,
        uint128 liquidity,
        uint256 amount,
        bool add
    ) internal pure returns (uint160) {
        if (amount == 0) return sqrtPriceX96;

        uint256 numerator1 = uint256(liquidity) << FixedPoint96.RESOLUTION;

        if (add) {
            // 添加 token0：价格下降
            // 使用 unchecked 允许溢出，通过除法回验检测溢出
            unchecked {
                uint256 product = amount * sqrtPriceX96;
                if (product / amount == sqrtPriceX96) {
                    uint256 denominator = numerator1 + product;
                    if (denominator >= numerator1) {
                        return uint160(Math.mulDivRoundingUp(numerator1, sqrtPriceX96, denominator));
                    }
                }
            }
            // 溢出时的备选计算
            return uint160(Math.divRoundingUp(numerator1, (numerator1 / sqrtPriceX96) + amount));
        } else {
            // 移除 token0：价格上升
            unchecked {
                uint256 product = amount * sqrtPriceX96;
                require(product / amount == sqrtPriceX96 && numerator1 > product);
                uint256 denominator = numerator1 - product;
                return uint160(Math.mulDivRoundingUp(numerator1, sqrtPriceX96, denominator));
            }
        }
    }

    /// @notice 根据 token1 数量变化计算新的 sqrtPriceX96
    /// @dev 公式：sqrtP_new = sqrtP + amount / L（或 sqrtP - amount / L）
    /// @param sqrtPriceX96 当前平方根价格
    /// @param liquidity 可用流动性
    /// @param amount token1 数量
    /// @param add 是否增加 token1
    /// @return 新的 sqrtPriceX96
    function getNextSqrtPriceFromAmount1RoundingDown(
        uint160 sqrtPriceX96,
        uint128 liquidity,
        uint256 amount,
        bool add
    ) internal pure returns (uint160) {
        if (add) {
            // 添加 token1：价格上升
            uint256 quotient = Math.mulDiv(amount, FixedPoint96.Q96, liquidity);
            return uint160(uint256(sqrtPriceX96) + quotient);
        } else {
            // 移除 token1：价格下降
            uint256 quotient = Math.mulDivRoundingUp(amount, FixedPoint96.Q96, liquidity);
            require(sqrtPriceX96 > quotient);
            return uint160(uint256(sqrtPriceX96) - quotient);
        }
    }

    /// @notice 根据输入 token 数量计算新的 sqrtPriceX96
    /// @param sqrtPriceX96 当前价格
    /// @param liquidity 可用流动性
    /// @param amountIn 输入数量
    /// @param zeroForOne 交易方向（true: token0->token1, false: token1->token0）
    /// @return sqrtQX96 新价格
    function getNextSqrtPriceFromInput(
        uint160 sqrtPriceX96,
        uint128 liquidity,
        uint256 amountIn,
        bool zeroForOne
    ) internal pure returns (uint160 sqrtQX96) {
        require(sqrtPriceX96 > 0 && liquidity > 0);

        // token0 输入 -> 价格下降；token1 输入 -> 价格上升
        return zeroForOne
            ? getNextSqrtPriceFromAmount0RoundingUp(sqrtPriceX96, liquidity, amountIn, true)
            : getNextSqrtPriceFromAmount1RoundingDown(sqrtPriceX96, liquidity, amountIn, true);
    }

    /// @notice 根据输出 token 数量计算新的 sqrtPriceX96
    /// @param sqrtPriceX96 当前价格
    /// @param liquidity 可用流动性
    /// @param amountOut 输出数量
    /// @param zeroForOne 交易方向
    /// @return sqrtQX96 新价格
    function getNextSqrtPriceFromOutput(
        uint160 sqrtPriceX96,
        uint128 liquidity,
        uint256 amountOut,
        bool zeroForOne
    ) internal pure returns (uint160 sqrtQX96) {
        require(sqrtPriceX96 > 0 && liquidity > 0);

        return zeroForOne
            ? getNextSqrtPriceFromAmount1RoundingDown(sqrtPriceX96, liquidity, amountOut, false)
            : getNextSqrtPriceFromAmount0RoundingUp(sqrtPriceX96, liquidity, amountOut, false);
    }

    /// @notice 计算在价格区间 [sqrtPriceAX96, sqrtPriceBX96] 内所需的 token0 数量
    /// @dev 公式：amount0 = L * (sqrtPriceB - sqrtPriceA) / (sqrtPriceA * sqrtPriceB)
    /// @param sqrtPriceAX96 区间下界价格
    /// @param sqrtPriceBX96 区间上界价格
    /// @param liquidity 流动性
    /// @param roundUp 是否向上取整
    /// @return amount0 token0 数量
    function getAmount0Delta(
        uint160 sqrtPriceAX96,
        uint160 sqrtPriceBX96,
        uint128 liquidity,
        bool roundUp
    ) internal pure returns (uint256 amount0) {
        // 确保 sqrtPriceA < sqrtPriceB
        if (sqrtPriceAX96 > sqrtPriceBX96)
            (sqrtPriceAX96, sqrtPriceBX96) = (sqrtPriceBX96, sqrtPriceAX96);

        require(sqrtPriceAX96 > 0);

        uint256 numerator1 = uint256(liquidity) << FixedPoint96.RESOLUTION;
        uint256 numerator2 = sqrtPriceBX96 - sqrtPriceAX96;

        if (roundUp) {
            amount0 = Math.divRoundingUp(
                Math.mulDivRoundingUp(numerator1, numerator2, sqrtPriceBX96),
                sqrtPriceAX96
            );
        } else {
            amount0 = Math.mulDiv(numerator1, numerator2, sqrtPriceBX96) / sqrtPriceAX96;
        }
    }

    /// @notice 计算在价格区间内所需的 token1 数量
    /// @dev 公式：amount1 = L * (sqrtPriceB - sqrtPriceA)
    /// @param sqrtPriceAX96 区间下界价格
    /// @param sqrtPriceBX96 区间上界价格
    /// @param liquidity 流动性
    /// @param roundUp 是否向上取整
    /// @return amount1 token1 数量
    function getAmount1Delta(
        uint160 sqrtPriceAX96,
        uint160 sqrtPriceBX96,
        uint128 liquidity,
        bool roundUp
    ) internal pure returns (uint256 amount1) {
        if (sqrtPriceAX96 > sqrtPriceBX96)
            (sqrtPriceAX96, sqrtPriceBX96) = (sqrtPriceBX96, sqrtPriceAX96);

        if (roundUp) {
            amount1 = Math.mulDivRoundingUp(liquidity, sqrtPriceBX96 - sqrtPriceAX96, FixedPoint96.Q96);
        } else {
            amount1 = Math.mulDiv(liquidity, sqrtPriceBX96 - sqrtPriceAX96, FixedPoint96.Q96);
        }
    }

    /// @notice 计算带符号的 token0 数量变化
    /// @dev 正数表示需要存入，负数表示可以提取
    function getAmount0Delta(
        uint160 sqrtPriceAX96,
        uint160 sqrtPriceBX96,
        int128 liquidity
    ) internal pure returns (int256 amount0) {
        if (liquidity < 0) {
            amount0 = -int256(getAmount0Delta(sqrtPriceAX96, sqrtPriceBX96, uint128(-liquidity), false));
        } else {
            amount0 = int256(getAmount0Delta(sqrtPriceAX96, sqrtPriceBX96, uint128(liquidity), true));
        }
    }

    /// @notice 计算带符号的 token1 数量变化
    function getAmount1Delta(
        uint160 sqrtPriceAX96,
        uint160 sqrtPriceBX96,
        int128 liquidity
    ) internal pure returns (int256 amount1) {
        if (liquidity < 0) {
            amount1 = -int256(getAmount1Delta(sqrtPriceAX96, sqrtPriceBX96, uint128(-liquidity), false));
        } else {
            amount1 = int256(getAmount1Delta(sqrtPriceAX96, sqrtPriceBX96, uint128(liquidity), true));
        }
    }
}
