// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "../lib/TickMath.sol";
import "../lib/BitMath.sol";

/// @title TestMathHarness
/// @notice 仅用于测试：把 TickMath / BitMath 的内部函数暴露出来，便于与已知值对照
contract TestMathHarness {
    function getSqrtRatioAtTick(int24 tick) external pure returns (uint160) {
        return TickMath.getSqrtRatioAtTick(tick);
    }

    function getTickAtSqrtRatio(uint160 sqrtPriceX96) external pure returns (int24) {
        return TickMath.getTickAtSqrtRatio(sqrtPriceX96);
    }

    function mostSignificantBit(uint256 x) external pure returns (uint8) {
        return BitMath.mostSignificantBit(x);
    }

    function leastSignificantBit(uint256 x) external pure returns (uint8) {
        return BitMath.leastSignificantBit(x);
    }
}
