// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "./interfaces/IUniGoPool.sol";
import "./interfaces/IUniGoPoolCallbacks.sol";

/// @title UniGoManager
/// @notice 用户交互管理器
/// @dev 用户不直接与 Pool 合约交互，而是通过 Manager 合约：
///      1. Manager 封装了 Pool 的回调逻辑，处理 token 转账
///      2. Manager 提供更友好的接口（如滑点保护）
///      3. Manager 充当 Pool 回调的接收者
///
///      【工作流程】
///      用户 -> Manager.mint() -> Pool.mint() -> Manager.uniGoMintCallback() -> token.transferFrom(用户)
///      用户 -> Manager.swap() -> Pool.swap() -> Manager.uniGoSwapCallback() -> token.transferFrom(用户)
///
///      注意：用户需要先 approve Manager 合约才能使用
contract UniGoManager is IUniGoPoolCallbacks {
    /// @notice mint 参数
    struct MintParams {
        address pool;          // Pool 合约地址
        int24 tickLower;       // 下界 tick
        int24 tickUpper;       // 上界 tick
        uint128 amount;        // 流动性数量
        uint256 amount0Min;    // 最少存入的 token0（滑点保护）
        uint256 amount1Min;    // 最少存入的 token1（滑点保护）
    }

    /// @notice swap 参数
    struct SwapParams {
        address pool;              // Pool 合约地址
        bool zeroForOne;           // 交易方向
        int256 amountSpecified;    // 交易数量
        uint160 sqrtPriceLimitX96; // 价格限制
        address recipient;         // 接收者地址
    }

    /// @notice 回调数据结构，用于在回调中传递信息
    struct CallbackData {
        address token0;    // token0 地址
        address token1;    // token1 地址
        address payer;     // 付款者地址
    }

    /// @notice 添加流动性
    /// @param params mint 参数
    /// @return amount0 实际存入的 token0 数量
    /// @return amount1 实际存入的 token1 数量
    function mint(MintParams calldata params)
        external
        returns (uint256 amount0, uint256 amount1)
    {
        IUniGoPool pool = IUniGoPool(params.pool);

        // 获取 pool 的 token 信息（通过 low-level call 避免接口限制）
        (address token0_, address token1_) = _getPoolTokens(params.pool);

        // 调用 Pool.mint，回调中处理 token 转账
        (amount0, amount1) = pool.mint(
            msg.sender,
            params.tickLower,
            params.tickUpper,
            params.amount,
            abi.encode(CallbackData({
                token0: token0_,
                token1: token1_,
                payer: msg.sender
            }))
        );

        // 滑点保护
        require(amount0 >= params.amount0Min, "S0");
        require(amount1 >= params.amount1Min, "S1");
    }

    /// @notice 代币交换
    /// @param params swap 参数
    /// @return amount0 token0 的变化量
    /// @return amount1 token1 的变化量
    function swap(SwapParams calldata params)
        external
        returns (int256 amount0, int256 amount1)
    {
        (address token0_, address token1_) = _getPoolTokens(params.pool);

        (amount0, amount1) = IUniGoPool(params.pool).swap(
            params.recipient,
            params.zeroForOne,
            params.amountSpecified,
            params.sqrtPriceLimitX96,
            abi.encode(CallbackData({
                token0: token0_,
                token1: token1_,
                payer: msg.sender
            }))
        );
    }

    // ==================== 回调实现 ====================

    /// @inheritdoc IUniGoPoolCallbacks
    /// @dev Pool 在 mint 计算完成后回调此函数
    ///      Manager 从 payer（用户）处转入所需的 token 到 Pool
    function uniGoMintCallback(
        uint256 amount0Owed,
        uint256 amount1Owed,
        bytes calldata data
    ) external override {
        CallbackData memory decoded = abi.decode(data, (CallbackData));

        if (amount0Owed > 0) {
            IERC20(decoded.token0).transferFrom(decoded.payer, msg.sender, amount0Owed);
        }
        if (amount1Owed > 0) {
            IERC20(decoded.token1).transferFrom(decoded.payer, msg.sender, amount1Owed);
        }
    }

    /// @inheritdoc IUniGoPoolCallbacks
    /// @dev Pool 在 swap 计算完成后回调此函数
    ///      正数表示需要转入的数量
    function uniGoSwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata data
    ) external override {
        CallbackData memory decoded = abi.decode(data, (CallbackData));

        if (amount0Delta > 0) {
            IERC20(decoded.token0).transferFrom(decoded.payer, msg.sender, uint256(amount0Delta));
        }
        if (amount1Delta > 0) {
            IERC20(decoded.token1).transferFrom(decoded.payer, msg.sender, uint256(amount1Delta));
        }
    }

    // ==================== 内部函数 ====================

    /// @notice 从 Pool 合约获取 token0 和 token1 地址
    /// @dev 使用 low-level staticcall 读取 Pool 的 immutable 变量
    function _getPoolTokens(address pool) internal view returns (address token0_, address token1_) {
        // solhint-disable-next-line avoid-low-level-calls
        (, bytes memory data0) = pool.staticcall(abi.encodeWithSignature("token0()"));
        (, bytes memory data1) = pool.staticcall(abi.encodeWithSignature("token1()"));
        token0_ = abi.decode(data0, (address));
        token1_ = abi.decode(data1, (address));
    }
}
