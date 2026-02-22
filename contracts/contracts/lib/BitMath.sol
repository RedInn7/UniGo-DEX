// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title BitMath
/// @notice 位运算工具库
/// @dev 提供查找最高有效位（MSB）和最低有效位（LSB）的功能
///      这些函数在 TickBitmap 中用于高效查找已初始化的 tick
library BitMath {
    /// @notice 查找最高有效位（Most Significant Bit）
    /// @dev 使用二分查找法，时间复杂度 O(log(256)) = O(8)
    /// @param x 输入值，必须大于 0
    /// @return r 最高有效位的位置（0-255）
    function mostSignificantBit(uint256 x) internal pure returns (uint8 r) {
        require(x > 0);

        // 二分查找：依次检查高 128、64、32、16、8、4、2、1 位
        if (x >= 0x100000000000000000000000000000000) {
            x >>= 128;
            r += 128;
        }
        if (x >= 0x10000000000000000) {
            x >>= 64;
            r += 64;
        }
        if (x >= 0x100000000) {
            x >>= 32;
            r += 32;
        }
        if (x >= 0x10000) {
            x >>= 16;
            r += 16;
        }
        if (x >= 0x100) {
            x >>= 8;
            r += 8;
        }
        if (x >= 0x10) {
            x >>= 4;
            r += 4;
        }
        if (x >= 0x4) {
            x >>= 2;
            r += 2;
        }
        if (x >= 0x2) {
            r += 1;
        }
    }

    /// @notice 查找最低有效位（Least Significant Bit）
    /// @dev 使用二分查找法
    /// @param x 输入值，必须大于 0
    /// @return r 最低有效位的位置（0-255）
    function leastSignificantBit(uint256 x) internal pure returns (uint8 r) {
        require(x > 0);

        r = 255;
        // 二分查找：依次检查低 128、64、32、16、8、4、2、1 位
        if (x & type(uint128).max > 0) {
            r -= 128;
        } else {
            x >>= 128;
        }
        if (x & type(uint64).max > 0) {
            r -= 64;
        } else {
            x >>= 64;
        }
        if (x & type(uint32).max > 0) {
            r -= 32;
        } else {
            x >>= 32;
        }
        if (x & type(uint16).max > 0) {
            r -= 16;
        } else {
            x >>= 16;
        }
        if (x & type(uint8).max > 0) {
            r -= 8;
        } else {
            x >>= 8;
        }
        if (x & 0xf > 0) {
            r -= 4;
        } else {
            x >>= 4;
        }
        if (x & 0x3 > 0) {
            r -= 2;
        } else {
            x >>= 2;
        }
        if (x & 0x1 > 0) {
            r -= 1;
        }
    }
}
