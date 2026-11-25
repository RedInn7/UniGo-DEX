// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "./interfaces/IUniGoPool.sol";
import "./interfaces/IUniGoPoolCallbacks.sol";

/// @title UniGoManager
/// @notice 用户交互管理器
/// @dev 用户不直接与 Pool 合约交互，而是通过 Manager 合约：
///      1. Manager 封装了 Pool 的回调逻辑，处理 token 转账
///      2. Manager 提供更友好的接口（滑点保护 amountOutMinimum / amount0Min / amount1Min，期限保护 deadline）
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
        uint256 deadline;      // 交易截止时间（Unix 秒），超过则 revert
    }

    /// @notice swap 参数
    struct SwapParams {
        address pool;              // Pool 合约地址
        bool zeroForOne;           // 交易方向
        int256 amountSpecified;    // 交易数量
        uint160 sqrtPriceLimitX96; // 价格限制
        address recipient;         // 接收者地址
        uint256 amountOutMinimum;  // 最少到手的输出 token 数量（滑点保护）
        uint256 deadline;          // 交易截止时间（Unix 秒），超过则 revert
    }

    /// @notice 回调数据结构，用于在回调中传递信息
    struct CallbackData {
        address token0;    // token0 地址
        address token1;    // token1 地址
    }

    /// @dev 当前正在处理的 Pool 和付款者，仅在 mint/swap 调用期间有值
    ///      回调只认这两个值，防止任何人直接调用回调、划走已授权用户的 token
    address private _callbackPool;
    address private _callbackPayer;

    /// @dev 记录本次调用的 Pool 和付款者，调用结束后清空
    modifier callbackContext(address pool) {
        require(_callbackPool == address(0), "LOK");
        _callbackPool = pool;
        _callbackPayer = msg.sender;
        _;
        _callbackPool = address(0);
        _callbackPayer = address(0);
    }

    /// @dev 期限保护：交易在内存池里滞留过久时直接作废，
    ///      避免被延迟到对用户不利的价格再打包
    modifier checkDeadline(uint256 deadline) {
        require(block.timestamp <= deadline, "EXP");
        _;
    }

    /// @notice 添加流动性
    /// @param params mint 参数
    /// @return amount0 实际存入的 token0 数量
    /// @return amount1 实际存入的 token1 数量
    function mint(MintParams calldata params)
        external
        checkDeadline(params.deadline)
        callbackContext(params.pool)
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
                token1: token1_
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
        checkDeadline(params.deadline)
        callbackContext(params.pool)
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
                token1: token1_
            }))
        );

        // 滑点保护：负数表示 Pool 转出，即用户实际到手的数量
        // 精确输出（amountSpecified < 0）时的输入上限由 sqrtPriceLimitX96 约束
        uint256 amountOut = uint256(-(params.zeroForOne ? amount1 : amount0));
        require(amountOut >= params.amountOutMinimum, "SLP");
    }

    // ==================== 回调实现 ====================

    /// @inheritdoc IUniGoPoolCallbacks
    /// @dev Pool 在 mint 计算完成后回调此函数
    ///      Manager 从 payer（用户）处转入所需的 token 到 Pool
    ///      只接受当前 mint/swap 所调用的 Pool 发起的回调
    function uniGoMintCallback(
        uint256 amount0Owed,
        uint256 amount1Owed,
        bytes calldata data
    ) external override {
        require(msg.sender == _callbackPool, "CB");
        CallbackData memory decoded = abi.decode(data, (CallbackData));
        address payer = _callbackPayer;

        if (amount0Owed > 0) {
            IERC20(decoded.token0).transferFrom(payer, msg.sender, amount0Owed);
        }
        if (amount1Owed > 0) {
            IERC20(decoded.token1).transferFrom(payer, msg.sender, amount1Owed);
        }
    }

    /// @dev flash 回调 — Manager 不发起闪电贷，直接 revert
    function uniGoFlashCallback(uint256, uint256, bytes calldata) external pure override {
        revert("NOT_SUPPORTED");
    }

    /// @inheritdoc IUniGoPoolCallbacks
    /// @dev Pool 在 swap 计算完成后回调此函数
    ///      正数表示需要转入的数量
    function uniGoSwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata data
    ) external override {
        require(msg.sender == _callbackPool, "CB");
        CallbackData memory decoded = abi.decode(data, (CallbackData));
        address payer = _callbackPayer;

        if (amount0Delta > 0) {
            IERC20(decoded.token0).transferFrom(payer, msg.sender, uint256(amount0Delta));
        }
        if (amount1Delta > 0) {
            IERC20(decoded.token1).transferFrom(payer, msg.sender, uint256(amount1Delta));
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
