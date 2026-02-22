// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./BitMath.sol";

/// @title TickBitmap
/// @notice Tick 位图索引，用于高效查找已初始化的 tick
/// @dev 位图将 tick 空间划分为多个 256 位的 word：
///      - 每个 word 对应 256 个连续的 tick（按 tickSpacing 压缩后）
///      - word 的每一位表示对应 tick 是否已初始化
///
///      查找下一个已初始化 tick 的流程：
///      1. 计算当前 tick 在 word 内的位置
///      2. 在当前 word 中搜索
///      3. 如果未找到，移到下一个 word（由上层循环处理）
///
///      这种设计使得查找操作的 gas 消耗与 tick 密度无关
library TickBitmap {
    /// @notice 将 tick 在位图中的位进行翻转（0->1 或 1->0）
    /// @dev 当 tick 的初始化状态改变时调用
    /// @param self 位图存储
    /// @param tick 要翻转的 tick（必须是 tickSpacing 的整数倍）
    /// @param tickSpacing tick 间距
    function flipTick(
        mapping(int16 => uint256) storage self,
        int24 tick,
        int24 tickSpacing
    ) internal {
        require(tick % tickSpacing == 0); // 确保 tick 对齐
        // 将 tick 压缩为 tickSpacing 的倍数
        (int16 wordPos, uint8 bitPos) = position(tick / tickSpacing);
        // 使用 XOR 翻转对应位
        uint256 mask = 1 << bitPos;
        self[wordPos] ^= mask;
    }

    /// @notice 在当前 word 内查找下一个已初始化的 tick
    /// @dev 用于 swap 过程中查找下一个价格边界
    /// @param self 位图存储
    /// @param tick 当前 tick
    /// @param tickSpacing tick 间距
    /// @param lte 搜索方向：true=向左搜索（价格降低），false=向右搜索（价格升高）
    /// @return next 找到的下一个 tick（如果当前 word 内没有，返回 word 边界）
    /// @return initialized 找到的 tick 是否已初始化
    function nextInitializedTickWithinOneWord(
        mapping(int16 => uint256) storage self,
        int24 tick,
        int24 tickSpacing,
        bool lte
    ) internal view returns (int24 next, bool initialized) {
        int24 compressed = tick / tickSpacing;
        // 负数除法向下取整修正
        if (tick < 0 && tick % tickSpacing != 0) compressed--;

        if (lte) {
            // 向左搜索（价格降低方向）
            (int16 wordPos, uint8 bitPos) = position(compressed);
            // 创建掩码：当前位及右侧所有位
            uint256 mask = (1 << bitPos) - 1 + (1 << bitPos);
            uint256 masked = self[wordPos] & mask;

            initialized = masked != 0;
            // 如果找到了已初始化的 tick，返回最近的一个（MSB）
            // 否则返回当前 word 的左边界
            next = initialized
                ? (compressed - int24(uint24(bitPos - BitMath.mostSignificantBit(masked)))) * tickSpacing
                : (compressed - int24(uint24(bitPos))) * tickSpacing;
        } else {
            // 向右搜索（价格升高方向）
            (int16 wordPos, uint8 bitPos) = position(compressed + 1);
            // 创建掩码：当前位及左侧所有位
            uint256 mask = ~((1 << bitPos) - 1);
            uint256 masked = self[wordPos] & mask;

            initialized = masked != 0;
            // 如果找到了，返回最近的一个（LSB）
            // 否则返回当前 word 的右边界
            next = initialized
                ? (compressed + 1 + int24(uint24(BitMath.leastSignificantBit(masked) - bitPos))) * tickSpacing
                : (compressed + 1 + int24(uint24(type(uint8).max - bitPos))) * tickSpacing;
        }
    }

    /// @notice 计算 tick 在位图中的位置
    /// @dev tick 被分为 wordPos（高位，选择哪个 word）和 bitPos（低位，word 内的位置）
    /// @param tick 压缩后的 tick（已除以 tickSpacing）
    /// @return wordPos word 的索引
    /// @return bitPos word 内的位索引
    function position(int24 tick) private pure returns (int16 wordPos, uint8 bitPos) {
        wordPos = int16(tick >> 8);        // 高位：word 索引
        bitPos = uint8(int8(tick % 256));  // 低位：word 内位置
    }
}
