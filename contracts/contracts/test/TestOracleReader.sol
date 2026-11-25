// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "../interfaces/IUniGoPool.sol";

/// @title TestOracleReader
/// @notice 仅用于测试的预言机读取合约
/// @dev 以交易的方式读取 Pool 的观测并存下结果，这样测试可以精确控制查询所在区块的时间戳
contract TestOracleReader {
    /// @notice 最近一次查询得到的时间加权平均 tick
    int24 public averageTick;
    /// @notice 最近一次查询窗口内的每单位流动性秒数增量
    uint160 public secondsPerLiquidityDeltaX128;

    /// @notice 查询过去 secondsAgo 秒的时间加权平均 tick
    function consult(address pool, uint32 secondsAgo) external returns (int24) {
        uint32[] memory secondsAgos = new uint32[](2);
        secondsAgos[0] = secondsAgo;
        secondsAgos[1] = 0;

        (int56[] memory tickCumulatives, uint160[] memory secondsPerLiquidityCumulativeX128s) =
            IUniGoPool(pool).observe(secondsAgos);

        int56 delta = tickCumulatives[1] - tickCumulatives[0];
        int56 window = int56(uint56(secondsAgo));
        averageTick = int24(delta / window);
        // 负数除法向负无穷取整
        if (delta < 0 && (delta % window != 0)) averageTick--;

        unchecked {
            secondsPerLiquidityDeltaX128 =
                secondsPerLiquidityCumulativeX128s[1] - secondsPerLiquidityCumulativeX128s[0];
        }
        return averageTick;
    }
}
