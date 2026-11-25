// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import {Math as OZMath} from "@openzeppelin/contracts/utils/math/Math.sol";

/// @title Math 的开源库版本
/// @notice 接口和 lib/Math.sol 完全相同，实现全部转给 OpenZeppelin 的 Math
/// @dev 用 MATH_IMPL=oss 编译时，Hardhat 会用这个文件顶替 lib/Math.sol（见 hardhat.config.js），
///      其余合约一行不用改。默认编译不会用到它。
library Math {
    /// @notice floor(a × b / denominator)，中间结果按 512 位算
    function mulDiv(uint256 a, uint256 b, uint256 denominator) internal pure returns (uint256) {
        return OZMath.mulDiv(a, b, denominator);
    }

    /// @notice ceil(a × b / denominator)
    function mulDivRoundingUp(uint256 a, uint256 b, uint256 denominator) internal pure returns (uint256) {
        return OZMath.mulDiv(a, b, denominator, OZMath.Rounding.Ceil);
    }

    /// @notice ceil(a / b)
    function divRoundingUp(uint256 a, uint256 b) internal pure returns (uint256) {
        return OZMath.ceilDiv(a, b);
    }
}
