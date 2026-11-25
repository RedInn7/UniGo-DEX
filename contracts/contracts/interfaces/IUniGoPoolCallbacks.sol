// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title IUniGoPoolCallbacks
/// @notice Pool 合约的回调接口
/// @dev Pool 使用回调模式来接收 token：
///      1. Pool 先更新内部状态（记录流动性/执行 swap 计算）
///      2. 然后回调调用者，要求其转入相应的 token
///      3. Pool 在回调后验证余额是否增加
///
///      这种模式的优势：
///      - Pool 不需要 approve/transferFrom，节省 gas
///      - 支持闪电贷等高级模式
///      - 调用者可以灵活决定资金来源
interface IUniGoPoolCallbacks {
    /// @notice mint（添加流动性）的回调
    /// @dev 调用者必须在此回调中向 Pool 转入 amount0 的 token0 和 amount1 的 token1
    /// @param amount0Owed 需要转入的 token0 数量
    /// @param amount1Owed 需要转入的 token1 数量
    /// @param data 调用者传递的额外数据（通常包含调用者地址等信息）
    function uniGoMintCallback(
        uint256 amount0Owed,
        uint256 amount1Owed,
        bytes calldata data
    ) external;

    /// @notice swap（代币交换）的回调
    /// @dev 调用者必须在此回调中向 Pool 转入所需的输入 token
    ///      amount0Delta 和 amount1Delta 中，正数表示需要转入，负数表示 Pool 会转出
    /// @param amount0Delta token0 的变化量（正=转入 Pool，负=Pool 转出）
    /// @param amount1Delta token1 的变化量
    /// @param data 调用者传递的额外数据
    function uniGoSwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata data
    ) external;

    /// @notice flash（闪电贷）的回调
    /// @dev 调用者必须在此回调中向 Pool 归还借出的本金，并支付手续费
    /// @param fee0 需要支付的 token0 手续费
    /// @param fee1 需要支付的 token1 手续费
    /// @param data 调用者传递的额外数据
    function uniGoFlashCallback(
        uint256 fee0,
        uint256 fee1,
        bytes calldata data
    ) external;
}
