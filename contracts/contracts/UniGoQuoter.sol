// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./interfaces/IUniGoPool.sol";
import "./interfaces/IUniGoPoolCallbacks.sol";

/// @title UniGoQuoter
/// @notice 报价合约，模拟 swap 返回预期输出而不实际执行
/// @dev 报价的实现原理：
///      1. 调用 Pool.swap() 执行真实计算
///      2. 在 swap 回调中 revert，携带计算结果
///      3. 捕获 revert 数据，解码出报价结果
///
///      这种方式的优点：
///      - 复用了 Pool 的完整 swap 逻辑（包括跨 tick 计算）
///      - 保证报价结果与实际交易完全一致
///      - 不消耗 gas（view 函数 + revert）
///
///      缺点：
///      - 需要使用 try/catch 模式
///      - revert 消息编码/解码有额外开销
contract UniGoQuoter is IUniGoPoolCallbacks {
    /// @notice 自定义 revert 错误，用于携带报价结果
    error QuoteResult(int256 amount0Delta, int256 amount1Delta);

    /// @notice 报价参数
    struct QuoteParams {
        address pool;              // Pool 合约地址
        bool zeroForOne;           // 交易方向
        int256 amountSpecified;    // 交易数量
        uint160 sqrtPriceLimitX96; // 价格限制
    }

    /// @notice 获取 swap 报价
    /// @dev 通过 try/catch 捕获 swap 的 revert 结果
    /// @param params 报价参数
    /// @return amount0Delta 预期的 token0 变化量
    /// @return amount1Delta 预期的 token1 变化量
    function quote(QuoteParams memory params)
        external
        returns (int256 amount0Delta, int256 amount1Delta)
    {
        try IUniGoPool(params.pool).swap(
            address(this),   // recipient（不会实际接收）
            params.zeroForOne,
            params.amountSpecified,
            params.sqrtPriceLimitX96,
            bytes("")
        ) {} catch (bytes memory reason) {
            // 解码 revert 数据获取报价结果
            return _parseQuoteResult(reason);
        }
    }

    /// @dev mint 回调 — Quoter 不需要实现，直接 revert
    function uniGoMintCallback(uint256, uint256, bytes calldata) external pure override {
        revert("NOT_SUPPORTED");
    }

    /// @dev swap 回调 — 不转入 token，而是 revert 携带计算结果
    function uniGoSwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata
    ) external pure override {
        // 使用自定义错误 revert，携带报价结果
        revert QuoteResult(amount0Delta, amount1Delta);
    }

    /// @notice 解析 revert 数据中的报价结果
    function _parseQuoteResult(bytes memory reason)
        internal
        pure
        returns (int256 amount0Delta, int256 amount1Delta)
    {
        // QuoteResult 错误的选择器
        if (reason.length < 68) revert("UNEXPECTED_ERROR");

        bytes4 selector;
        assembly {
            selector := mload(add(reason, 32))
        }

        if (selector == QuoteResult.selector) {
            assembly {
                amount0Delta := mload(add(reason, 36))
                amount1Delta := mload(add(reason, 68))
            }
        } else {
            // 如果不是 QuoteResult 错误，可能是其他 revert
            // 将原始 revert 数据转发
            assembly {
                revert(add(reason, 32), mload(reason))
            }
        }
    }
}
