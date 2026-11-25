// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import {Math as OZMath} from "@openzeppelin/contracts/utils/math/Math.sol";

/// @title BitMath 的开源库版本
/// @notice 接口和 lib/BitMath.sol 完全相同，实现转给 OpenZeppelin 的 Math.log2
/// @dev 用 MATH_IMPL=oss 编译时顶替 lib/BitMath.sol，见 hardhat.config.js
library BitMath {
    /// @notice 最高位 1 的下标，x 必须大于 0
    function mostSignificantBit(uint256 x) internal pure returns (uint8) {
        require(x > 0);
        return uint8(OZMath.log2(x));
    }

    /// @notice 最低位 1 的下标，x 必须大于 0
    function leastSignificantBit(uint256 x) internal pure returns (uint8) {
        require(x > 0);
        // x & -x 只留下最低位的 1，它的 log2 就是下标
        unchecked {
            return uint8(OZMath.log2(x & (0 - x)));
        }
    }
}
