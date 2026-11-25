// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "../lib/Tick.sol";

/// @title TestTickHarness
/// @notice 仅用于测试：直接调用 Tick 库，可以绕开 Pool 的流动性上限单独验证库本身的行为
contract TestTickHarness {
    using Tick for mapping(int24 => Tick.Info);

    mapping(int24 => Tick.Info) public ticks;

    function update(int24 tick, int128 liquidityDelta, bool upper, uint128 maxLiquidity) external {
        ticks.update(tick, 0, liquidityDelta, 0, 0, upper, maxLiquidity);
    }

    function tickSpacingToMaxLiquidityPerTick(int24 tickSpacing) external pure returns (uint128) {
        return Tick.tickSpacingToMaxLiquidityPerTick(tickSpacing);
    }
}
