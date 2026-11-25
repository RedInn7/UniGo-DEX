// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import "../interfaces/IUniGoPool.sol";

/// @title TestFlashBorrower
/// @notice 仅用于测试的闪电贷借款合约
/// @dev 通过 mode 模拟三种借款人：正常还款 / 不还款 / 在回调里重入 Pool
contract TestFlashBorrower {
    using SafeERC20 for IERC20;

    enum Mode {
        Repay,      // 归还本金 + 手续费
        NoRepay,    // 什么都不还
        Reenter     // 在回调里再次调用 Pool.flash
    }

    /// @dev 回调数据
    struct FlashData {
        address pool;
        uint256 amount0;
        uint256 amount1;
        Mode mode;
    }

    /// @notice 最近一次回调收到的手续费，供测试断言
    uint256 public lastFee0;
    uint256 public lastFee1;

    /// @notice 发起闪电贷，借出的 token 转到本合约
    function flash(address pool, uint256 amount0, uint256 amount1, Mode mode) external {
        IUniGoPool(pool).flash(
            address(this),
            amount0,
            amount1,
            abi.encode(FlashData({pool: pool, amount0: amount0, amount1: amount1, mode: mode}))
        );
    }

    /// @notice Pool 的闪电贷回调
    function uniGoFlashCallback(uint256 fee0, uint256 fee1, bytes calldata data) external {
        FlashData memory decoded = abi.decode(data, (FlashData));
        require(msg.sender == decoded.pool, "NOT_POOL");

        lastFee0 = fee0;
        lastFee1 = fee1;

        if (decoded.mode == Mode.NoRepay) return;

        if (decoded.mode == Mode.Reenter) {
            IUniGoPool(decoded.pool).flash(address(this), 1, 1, data);
            return;
        }

        // 归还本金 + 手续费（手续费需要本合约事先持有）
        (address token0, address token1) = _poolTokens(decoded.pool);
        if (decoded.amount0 > 0) IERC20(token0).safeTransfer(msg.sender, decoded.amount0 + fee0);
        if (decoded.amount1 > 0) IERC20(token1).safeTransfer(msg.sender, decoded.amount1 + fee1);
    }

    /// @dev 读取 Pool 的 token0 / token1 地址
    function _poolTokens(address pool) private view returns (address token0, address token1) {
        (, bytes memory data0) = pool.staticcall(abi.encodeWithSignature("token0()"));
        (, bytes memory data1) = pool.staticcall(abi.encodeWithSignature("token1()"));
        token0 = abi.decode(data0, (address));
        token1 = abi.decode(data1, (address));
    }
}
