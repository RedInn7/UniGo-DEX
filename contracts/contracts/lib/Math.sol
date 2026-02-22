// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title Math
/// @notice 基础数学工具库
/// @dev 提供 mulDiv（全精度乘除）等核心数学运算
///      mulDiv 是 AMM 中最关键的数学函数，用于避免中间结果溢出
library Math {
    /// @notice 计算 (a * b) / denominator，使用 512 位精度避免中间溢出
    /// @dev 参考 Remco Bloemen 的实现：https://xn--2-umb.com/21/muldiv
    ///      大量使用 unchecked 和 assembly 来进行模运算，这是有意为之的
    /// @param a 乘数
    /// @param b 乘数
    /// @param denominator 除数，必须大于 0
    /// @return result 计算结果，向下取整
    function mulDiv(
        uint256 a,
        uint256 b,
        uint256 denominator
    ) internal pure returns (uint256 result) {
        // 512 位乘法：[prod1, prod0] = a * b
        uint256 prod0; // 低 256 位
        uint256 prod1; // 高 256 位
        assembly {
            let mm := mulmod(a, b, not(0))
            prod0 := mul(a, b)
            prod1 := sub(sub(mm, prod0), lt(mm, prod0))
        }

        // 如果没有溢出（prod1 == 0），直接除法返回
        if (prod1 == 0) {
            require(denominator > 0);
            assembly {
                result := div(prod0, denominator)
            }
            return result;
        }

        // 确保结果不溢出 256 位
        require(denominator > prod1);

        // 计算 remainder = (a * b) % denominator
        uint256 remainder;
        assembly {
            remainder := mulmod(a, b, denominator)
        }
        // 从 512 位结果中减去余数
        assembly {
            prod1 := sub(prod1, gt(remainder, prod0))
            prod0 := sub(prod0, remainder)
        }

        // 以下运算涉及模运算（取反、模逆等），必须使用 unchecked
        unchecked {
            // 计算 denominator 的最大 2 的幂因子（即 denominator 的最低有效位）
            // (0 - denominator) 等价于 ~denominator + 1（二进制补码取反）
            uint256 twos = (0 - denominator) & denominator;
            assembly {
                denominator := div(denominator, twos)
            }
            assembly {
                prod0 := div(prod0, twos)
            }
            assembly {
                twos := add(div(sub(0, twos), twos), 1)
            }
            prod0 |= prod1 * twos;

            // 使用牛顿法计算 denominator 的模逆（mod 2^256）
            // 初始近似值：inv ≡ (3 * denominator) ^ 2 (mod 2^4)
            uint256 inv = (3 * denominator) ^ 2;
            // 每次迭代精度翻倍：4 -> 8 -> 16 -> 32 -> 64 -> 128 -> 256
            inv *= 2 - denominator * inv;
            inv *= 2 - denominator * inv;
            inv *= 2 - denominator * inv;
            inv *= 2 - denominator * inv;
            inv *= 2 - denominator * inv;
            inv *= 2 - denominator * inv;

            result = prod0 * inv;
        }
        return result;
    }

    /// @notice 计算 (a * b) / denominator，向上取整
    /// @param a 乘数
    /// @param b 乘数
    /// @param denominator 除数
    /// @return result 计算结果，向上取整
    function mulDivRoundingUp(
        uint256 a,
        uint256 b,
        uint256 denominator
    ) internal pure returns (uint256 result) {
        result = mulDiv(a, b, denominator);
        unchecked {
            if (mulmod(a, b, denominator) > 0) {
                require(result < type(uint256).max);
                result++;
            }
        }
    }

    /// @notice 计算 uint256 的向上取整除法
    /// @param a 被除数
    /// @param b 除数
    /// @return 向上取整的结果
    function divRoundingUp(uint256 a, uint256 b) internal pure returns (uint256) {
        unchecked {
            return (a + b - 1) / b;
        }
    }
}
