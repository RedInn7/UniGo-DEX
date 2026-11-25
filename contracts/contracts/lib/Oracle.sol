// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title Oracle
/// @notice 价格预言机观测库（TWAP）
/// @dev Pool 把历史价格记录成一组“观测”，存放在环形数组里：
///      - 每个观测保存某一时刻的累计值：tick * 时间、时间 / 流动性
///      - 任意两个时刻的累计值之差除以时间差，就是这段时间的时间加权平均（TWAP）
///      - 每个区块最多写入一次观测，写满后覆盖最老的一条
///
///      【为什么用累计值】
///      攻击者要操纵 TWAP，必须把价格偏离维持很多个区块，成本远高于操纵单个区块的现价
///
///      【容量】
///      环形数组初始只有 1 个槽位，任何人都可以付 gas 调用 grow 扩容（最多 65535）
///      容量越大，可查询的历史越久
///
///      时间戳用 uint32 存储，所有时间差的运算都依赖溢出回绕，必须放在 unchecked 块里
library Oracle {
    /// @notice 单条观测
    struct Observation {
        /// @dev 观测写入时的区块时间戳
        uint32 blockTimestamp;
        /// @dev tick 的时间累计值：从池子初始化起 tick * 经过的秒数 之和
        int56 tickCumulative;
        /// @dev 每单位流动性秒数的累计值：经过的秒数 / max(1, liquidity) 之和（Q128）
        uint160 secondsPerLiquidityCumulativeX128;
        /// @dev 该槽位是否已写入过观测
        bool initialized;
    }

    /// @notice 在上一条观测的基础上推进到 blockTimestamp，生成新的观测
    /// @dev blockTimestamp 必须不早于 last.blockTimestamp（允许 uint32 回绕）
    /// @param last 上一条观测
    /// @param blockTimestamp 新观测的时间戳
    /// @param tick 这段时间内生效的 tick
    /// @param liquidity 这段时间内生效的活跃流动性
    function transform(
        Observation memory last,
        uint32 blockTimestamp,
        int24 tick,
        uint128 liquidity
    ) private pure returns (Observation memory) {
        unchecked {
            uint32 delta = blockTimestamp - last.blockTimestamp;
            return Observation({
                blockTimestamp: blockTimestamp,
                tickCumulative: last.tickCumulative + int56(tick) * int56(uint56(delta)),
                secondsPerLiquidityCumulativeX128: last.secondsPerLiquidityCumulativeX128 +
                    ((uint160(delta) << 128) / (liquidity > 0 ? liquidity : 1)),
                initialized: true
            });
        }
    }

    /// @notice 初始化环形数组，写入第一条观测
    /// @param self 观测数组
    /// @param time 初始化时的区块时间戳
    /// @return cardinality 当前已使用的容量
    /// @return cardinalityNext 下一次可扩到的容量
    function initialize(
        Observation[65535] storage self,
        uint32 time
    ) internal returns (uint16 cardinality, uint16 cardinalityNext) {
        self[0] = Observation({
            blockTimestamp: time,
            tickCumulative: 0,
            secondsPerLiquidityCumulativeX128: 0,
            initialized: true
        });
        return (1, 1);
    }

    /// @notice 写入一条观测
    /// @dev 每个区块最多写入一次；必须在 tick 或流动性变化之前调用，
    ///      传入的是变化之前的 tick 和流动性（它们在上一条观测到现在这段时间内生效）
    /// @param self 观测数组
    /// @param index 最近一次写入的位置
    /// @param blockTimestamp 当前区块时间戳
    /// @param tick 变化之前的 tick
    /// @param liquidity 变化之前的活跃流动性
    /// @param cardinality 当前已使用的容量
    /// @param cardinalityNext 已预留的容量
    /// @return indexUpdated 新的写入位置
    /// @return cardinalityUpdated 新的已使用容量
    function write(
        Observation[65535] storage self,
        uint16 index,
        uint32 blockTimestamp,
        int24 tick,
        uint128 liquidity,
        uint16 cardinality,
        uint16 cardinalityNext
    ) internal returns (uint16 indexUpdated, uint16 cardinalityUpdated) {
        Observation memory last = self[index];

        // 本区块已经写过，直接返回
        if (last.blockTimestamp == blockTimestamp) return (index, cardinality);

        // 写到数组末尾且有预留容量时，启用新容量
        if (cardinalityNext > cardinality && index == (cardinality - 1)) {
            cardinalityUpdated = cardinalityNext;
        } else {
            cardinalityUpdated = cardinality;
        }

        indexUpdated = (index + 1) % cardinalityUpdated;
        self[indexUpdated] = transform(last, blockTimestamp, tick, liquidity);
    }

    /// @notice 预留更多观测槽位
    /// @dev 提前把槽位写成非零值，把首次写入的 gas（20000）转移给扩容的人，
    ///      之后 swap 写观测时只需付改写的 gas
    /// @param self 观测数组
    /// @param current 当前已预留的容量
    /// @param next 期望的容量
    /// @return 扩容后的预留容量
    function grow(
        Observation[65535] storage self,
        uint16 current,
        uint16 next
    ) internal returns (uint16) {
        require(current > 0, "I");
        // 不缩容
        if (next <= current) return current;
        // 只占位，initialized 保持 false，查询时会跳过
        for (uint16 i = current; i < next; i++) self[i].blockTimestamp = 1;
        return next;
    }

    /// @notice 比较两个 32 位时间戳的先后（a <= b），正确处理回绕
    /// @dev time 是当前时间，a 和 b 都不晚于 time
    function lte(uint32 time, uint32 a, uint32 b) private pure returns (bool) {
        // 都没有回绕，直接比较
        if (a <= time && b <= time) return a <= b;

        unchecked {
            uint256 aAdjusted = a > time ? a : a + 2**32;
            uint256 bAdjusted = b > time ? b : b + 2**32;
            return aAdjusted <= bAdjusted;
        }
    }

    /// @notice 二分查找：找到时间上夹住 target 的两条相邻观测
    /// @dev 环形数组中，最老的观测在 index + 1，最新的在 index
    function binarySearch(
        Observation[65535] storage self,
        uint32 time,
        uint32 target,
        uint16 index,
        uint16 cardinality
    ) private view returns (Observation memory beforeOrAt, Observation memory atOrAfter) {
        uint256 l = (index + 1) % cardinality; // 最老的观测
        uint256 r = l + cardinality - 1;       // 最新的观测
        uint256 i;
        while (true) {
            i = (l + r) / 2;

            beforeOrAt = self[i % cardinality];

            // 落在尚未写入的槽位上，继续向右找
            if (!beforeOrAt.initialized) {
                l = i + 1;
                continue;
            }

            atOrAfter = self[(i + 1) % cardinality];

            bool targetAtOrAfter = lte(time, beforeOrAt.blockTimestamp, target);

            // 找到了夹住 target 的一对
            if (targetAtOrAfter && lte(time, target, atOrAfter.blockTimestamp)) break;

            if (!targetAtOrAfter) r = i - 1;
            else l = i + 1;
        }
    }

    /// @notice 找到夹住 target 的两条观测
    /// @dev target 不能早于最老的观测，否则 revert
    function getSurroundingObservations(
        Observation[65535] storage self,
        uint32 time,
        uint32 target,
        int24 tick,
        uint16 index,
        uint128 liquidity,
        uint16 cardinality
    ) private view returns (Observation memory beforeOrAt, Observation memory atOrAfter) {
        // 先看最新的观测
        beforeOrAt = self[index];

        if (lte(time, beforeOrAt.blockTimestamp, target)) {
            if (beforeOrAt.blockTimestamp == target) {
                // 正好命中最新观测
                return (beforeOrAt, atOrAfter);
            } else {
                // target 比最新观测还新，按当前 tick 和流动性外推
                return (beforeOrAt, transform(beforeOrAt, target, tick, liquidity));
            }
        }

        // 再看最老的观测
        beforeOrAt = self[(index + 1) % cardinality];
        if (!beforeOrAt.initialized) beforeOrAt = self[0];

        // target 早于最老的观测，无法回答
        require(lte(time, beforeOrAt.blockTimestamp, target), "OLD");

        return binarySearch(self, time, target, index, cardinality);
    }

    /// @notice 查询 secondsAgo 秒之前的累计值
    /// @dev secondsAgo 为 0 时返回当前时刻的累计值（按当前 tick 和流动性外推）；
    ///      目标时刻落在两条观测之间时线性插值
    /// @param self 观测数组
    /// @param time 当前区块时间戳
    /// @param secondsAgo 回看的秒数
    /// @param tick 当前 tick
    /// @param index 最近一次写入的位置
    /// @param liquidity 当前活跃流动性
    /// @param cardinality 当前已使用的容量
    /// @return tickCumulative 目标时刻的 tick 累计值
    /// @return secondsPerLiquidityCumulativeX128 目标时刻的每单位流动性秒数累计值
    function observeSingle(
        Observation[65535] storage self,
        uint32 time,
        uint32 secondsAgo,
        int24 tick,
        uint16 index,
        uint128 liquidity,
        uint16 cardinality
    ) internal view returns (int56 tickCumulative, uint160 secondsPerLiquidityCumulativeX128) {
        unchecked {
            if (secondsAgo == 0) {
                Observation memory last = self[index];
                if (last.blockTimestamp != time) last = transform(last, time, tick, liquidity);
                return (last.tickCumulative, last.secondsPerLiquidityCumulativeX128);
            }

            uint32 target = time - secondsAgo;

            (Observation memory beforeOrAt, Observation memory atOrAfter) = getSurroundingObservations(
                self, time, target, tick, index, liquidity, cardinality
            );

            if (target == beforeOrAt.blockTimestamp) {
                // 命中左边界
                return (beforeOrAt.tickCumulative, beforeOrAt.secondsPerLiquidityCumulativeX128);
            } else if (target == atOrAfter.blockTimestamp) {
                // 命中右边界
                return (atOrAfter.tickCumulative, atOrAfter.secondsPerLiquidityCumulativeX128);
            } else {
                // 落在两条观测之间，线性插值
                uint32 observationTimeDelta = atOrAfter.blockTimestamp - beforeOrAt.blockTimestamp;
                uint32 targetDelta = target - beforeOrAt.blockTimestamp;
                return (
                    beforeOrAt.tickCumulative +
                        ((atOrAfter.tickCumulative - beforeOrAt.tickCumulative) / int56(uint56(observationTimeDelta))) *
                        int56(uint56(targetDelta)),
                    beforeOrAt.secondsPerLiquidityCumulativeX128 +
                        uint160(
                            (uint256(
                                atOrAfter.secondsPerLiquidityCumulativeX128 -
                                    beforeOrAt.secondsPerLiquidityCumulativeX128
                            ) * targetDelta) / observationTimeDelta
                        )
                );
            }
        }
    }

    /// @notice 批量查询多个时刻的累计值
    /// @param self 观测数组
    /// @param time 当前区块时间戳
    /// @param secondsAgos 每个元素是回看的秒数
    /// @param tick 当前 tick
    /// @param index 最近一次写入的位置
    /// @param liquidity 当前活跃流动性
    /// @param cardinality 当前已使用的容量
    /// @return tickCumulatives 各时刻的 tick 累计值
    /// @return secondsPerLiquidityCumulativeX128s 各时刻的每单位流动性秒数累计值
    function observe(
        Observation[65535] storage self,
        uint32 time,
        uint32[] memory secondsAgos,
        int24 tick,
        uint16 index,
        uint128 liquidity,
        uint16 cardinality
    ) internal view returns (int56[] memory tickCumulatives, uint160[] memory secondsPerLiquidityCumulativeX128s) {
        require(cardinality > 0, "I");

        tickCumulatives = new int56[](secondsAgos.length);
        secondsPerLiquidityCumulativeX128s = new uint160[](secondsAgos.length);
        for (uint256 i = 0; i < secondsAgos.length; i++) {
            (tickCumulatives[i], secondsPerLiquidityCumulativeX128s[i]) = observeSingle(
                self, time, secondsAgos[i], tick, index, liquidity, cardinality
            );
        }
    }
}
