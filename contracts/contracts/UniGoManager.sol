// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import "./interfaces/IUniGoFactory.sol";
import "./interfaces/IUniGoPool.sol";
import "./interfaces/IUniGoPoolCallbacks.sol";
import "./lib/TickMath.sol";

/// @title UniGoManager
/// @notice 用户交互管理器
/// @dev 用户不直接与 Pool 合约交互，而是通过 Manager 合约：
///      1. Manager 封装了 Pool 的回调逻辑，处理 token 转账
///      2. Manager 提供更友好的接口（滑点保护 amountOutMinimum / amountInMaximum / amount0Min / amount1Min，期限保护 deadline）
///      3. Manager 充当 Pool 回调的接收者
///      4. Manager 支持多跳兑换：按路径依次穿过多个 Pool
///
///      【工作流程】
///      用户 -> Manager.mint() -> Pool.mint() -> Manager.uniGoMintCallback() -> token.safeTransferFrom(用户)
///      用户 -> Manager.swap() -> Pool.swap() -> Manager.uniGoSwapCallback() -> token.safeTransferFrom(用户)
///
///      【路径编码】
///      多跳路径是紧凑编码的字节串：token(20 字节) + fee(3 字节) + token(20 字节) + fee + token ...
///      每个 (token, fee, token) 三元组确定一个 Pool，n 跳的路径长度为 20 + 23 * n 字节
///
///      【Pool 真伪校验】
///      所有入口和回调都通过 Factory 确认 Pool 是真实部署的池子，
///      伪造的 Pool 无法借 Manager 划走用户已授权的 token
///
///      注意：用户需要先 approve Manager 合约才能使用
contract UniGoManager is IUniGoPoolCallbacks {
    using SafeERC20 for IERC20;

    /// @dev 路径中一个地址的字节数
    uint256 private constant ADDR_SIZE = 20;
    /// @dev 路径中一跳（fee + token）的字节数
    uint256 private constant HOP_SIZE = 23;
    /// @dev 单个 Pool 的路径长度（token + fee + token）
    uint256 private constant POOL_PATH_SIZE = 43;

    /// @notice 工厂合约地址，用于校验 Pool 真伪
    address public immutable factory;

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

    /// @notice 单池 swap 参数
    struct SwapParams {
        address pool;              // Pool 合约地址
        bool zeroForOne;           // 交易方向
        int256 amountSpecified;    // 交易数量（正=精确输入，负=精确输出）
        uint160 sqrtPriceLimitX96; // 价格限制
        address recipient;         // 接收者地址
        uint256 amountOutMinimum;  // 最少到手的输出 token 数量（滑点保护）
        uint256 deadline;          // 交易截止时间（Unix 秒），超过则 revert
    }

    /// @notice 多跳精确输入参数
    struct ExactInputParams {
        bytes path;                // 路径：token + fee + token + ...
        address recipient;         // 最终输出 token 的接收者
        uint256 amountIn;          // 输入 token 数量
        uint256 amountOutMinimum;  // 最少到手的最终输出数量（滑点保护）
        uint256 deadline;          // 交易截止时间（Unix 秒），超过则 revert
    }

    /// @notice 单池精确输出参数
    struct ExactOutputSingleParams {
        address tokenIn;           // 输入 token
        address tokenOut;          // 输出 token
        uint24 fee;                // Pool 费率
        address recipient;         // 接收者地址
        uint256 amountOut;         // 要拿到的输出 token 数量
        uint256 amountInMaximum;   // 最多付出的输入 token 数量（滑点保护）
        uint160 sqrtPriceLimitX96; // 价格限制，0 表示不限
        uint256 deadline;          // 交易截止时间（Unix 秒），超过则 revert
    }

    /// @notice 回调数据结构，用于在回调中传递信息
    struct CallbackData {
        address token0;    // token0 地址
        address token1;    // token1 地址
        uint24 fee;        // Pool 费率，与 token0 / token1 一起确定 Pool
        address payer;     // 付款者地址（用户，或多跳中间跳时的 Manager 自己）
    }

    /// @param factory_ 工厂合约地址
    constructor(address factory_) {
        factory = factory_;
    }

    /// @dev 期限保护：交易在内存池里滞留过久时直接作废，
    ///      避免被延迟到对用户不利的价格再打包
    modifier checkDeadline(uint256 deadline) {
        require(block.timestamp <= deadline, "EXP");
        _;
    }

    // ==================== 添加流动性 ====================

    /// @notice 添加流动性
    /// @param params mint 参数
    /// @return amount0 实际存入的 token0 数量
    /// @return amount1 实际存入的 token1 数量
    function mint(MintParams calldata params)
        external
        checkDeadline(params.deadline)
        returns (uint256 amount0, uint256 amount1)
    {
        CallbackData memory data = _verifyPool(params.pool, msg.sender);

        // 调用 Pool.mint，回调中处理 token 转账
        (amount0, amount1) = IUniGoPool(params.pool).mint(
            msg.sender,
            params.tickLower,
            params.tickUpper,
            params.amount,
            abi.encode(data)
        );

        // 滑点保护
        require(amount0 >= params.amount0Min, "S0");
        require(amount1 >= params.amount1Min, "S1");
    }

    // ==================== 代币交换 ====================

    /// @notice 单池代币交换
    /// @param params swap 参数
    /// @return amount0 token0 的变化量
    /// @return amount1 token1 的变化量
    function swap(SwapParams calldata params)
        external
        checkDeadline(params.deadline)
        returns (int256 amount0, int256 amount1)
    {
        CallbackData memory data = _verifyPool(params.pool, msg.sender);

        (amount0, amount1) = IUniGoPool(params.pool).swap(
            params.recipient,
            params.zeroForOne,
            params.amountSpecified,
            params.sqrtPriceLimitX96,
            abi.encode(data)
        );

        // 滑点保护：负数表示 Pool 转出，即用户实际到手的数量
        // 精确输出需要限制输入上限时，使用 exactOutputSingle
        uint256 amountOut = uint256(-(params.zeroForOne ? amount1 : amount0));
        require(amountOut >= params.amountOutMinimum, "SLP");
    }

    /// @notice 多跳精确输入：按路径依次穿过多个 Pool
    /// @dev 第一跳由用户付款；中间跳的输出先转给 Manager，再由 Manager 付给下一个 Pool；
    ///      最后一跳的输出直接转给 recipient。
    ///      中间 token 不能直接转给下一个 Pool：Pool 在回调前后比较余额，提前转入的不算数
    /// @param params 多跳参数
    /// @return amountOut 最终到手的输出 token 数量
    function exactInput(ExactInputParams calldata params)
        external
        checkDeadline(params.deadline)
        returns (uint256 amountOut)
    {
        bytes calldata path = params.path;
        require(
            path.length >= POOL_PATH_SIZE && (path.length - ADDR_SIZE) % HOP_SIZE == 0,
            "PATH"
        );

        address payer = msg.sender;
        // 每一跳的输出作为下一跳的输入
        amountOut = params.amountIn;

        while (true) {
            bool isLastHop = path.length == POOL_PATH_SIZE;

            amountOut = _exactInputHop(
                address(bytes20(path[:ADDR_SIZE])),
                address(bytes20(path[HOP_SIZE:POOL_PATH_SIZE])),
                uint24(bytes3(path[ADDR_SIZE:HOP_SIZE])),
                isLastHop ? params.recipient : address(this),
                amountOut,
                payer
            );

            if (isLastHop) break;

            // 后续各跳由 Manager 用上一跳收到的 token 付款
            payer = address(this);
            path = path[HOP_SIZE:];
        }

        require(amountOut >= params.amountOutMinimum, "SLP");
    }

    /// @notice 单池精确输出：拿到指定数量的输出 token，输入不超过上限
    /// @param params 精确输出参数
    /// @return amountIn 实际付出的输入 token 数量
    function exactOutputSingle(ExactOutputSingleParams calldata params)
        external
        checkDeadline(params.deadline)
        returns (uint256 amountIn)
    {
        require(params.amountOut <= uint256(type(int256).max), "AS");
        bool zeroForOne = params.tokenIn < params.tokenOut;
        (address pool, CallbackData memory data) = _getPool(
            params.tokenIn, params.tokenOut, params.fee, msg.sender
        );

        (int256 amount0, int256 amount1) = IUniGoPool(pool).swap(
            params.recipient,
            zeroForOne,
            -int256(params.amountOut),
            params.sqrtPriceLimitX96 == 0 ? _unlimitedPrice(zeroForOne) : params.sqrtPriceLimitX96,
            abi.encode(data)
        );

        // 流动性不足或撞到价格限制时拿不满，整笔作废
        require(uint256(-(zeroForOne ? amount1 : amount0)) == params.amountOut, "PF");

        amountIn = uint256(zeroForOne ? amount0 : amount1);
        require(amountIn <= params.amountInMaximum, "TMI");
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
        CallbackData memory decoded = _verifyCallback(data);

        if (amount0Owed > 0) _pay(decoded.token0, decoded.payer, msg.sender, amount0Owed);
        if (amount1Owed > 0) _pay(decoded.token1, decoded.payer, msg.sender, amount1Owed);
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
        CallbackData memory decoded = _verifyCallback(data);

        if (amount0Delta > 0) _pay(decoded.token0, decoded.payer, msg.sender, uint256(amount0Delta));
        if (amount1Delta > 0) _pay(decoded.token1, decoded.payer, msg.sender, uint256(amount1Delta));
    }

    // ==================== 内部函数 ====================

    /// @dev 多跳中的一跳：把 amountIn 的 tokenIn 全部换成 tokenOut
    function _exactInputHop(
        address tokenIn,
        address tokenOut,
        uint24 fee,
        address recipient,
        uint256 amountIn,
        address payer
    ) private returns (uint256 amountOut) {
        require(amountIn <= uint256(type(int256).max), "AS");
        bool zeroForOne = tokenIn < tokenOut;
        (address pool, CallbackData memory data) = _getPool(tokenIn, tokenOut, fee, payer);

        (int256 amount0, int256 amount1) = IUniGoPool(pool).swap(
            recipient,
            zeroForOne,
            int256(amountIn),
            _unlimitedPrice(zeroForOne),
            abi.encode(data)
        );

        // 流动性不足时输入吃不完，剩余的 token 会滞留在 Manager，整笔作废
        require(uint256(zeroForOne ? amount0 : amount1) == amountIn, "PF");

        amountOut = uint256(-(zeroForOne ? amount1 : amount0));
    }

    /// @dev 不限价时使用的价格边界
    function _unlimitedPrice(bool zeroForOne) private pure returns (uint160) {
        return zeroForOne ? TickMath.MIN_SQRT_RATIO + 1 : TickMath.MAX_SQRT_RATIO - 1;
    }

    /// @dev 由 (tokenA, tokenB, fee) 向 Factory 查询 Pool，不存在则 revert
    function _getPool(
        address tokenA,
        address tokenB,
        uint24 fee,
        address payer
    ) private view returns (address pool, CallbackData memory data) {
        pool = IUniGoFactory(factory).getPool(tokenA, tokenB, fee);
        require(pool != address(0), "POOL");

        (address token0, address token1) = tokenA < tokenB ? (tokenA, tokenB) : (tokenB, tokenA);
        data = CallbackData({token0: token0, token1: token1, fee: fee, payer: payer});
    }

    /// @dev 校验用户传入的 Pool 地址确实由 Factory 创建，并生成回调数据
    function _verifyPool(address pool, address payer) private view returns (CallbackData memory data) {
        data = CallbackData({
            token0: IUniGoPool(pool).token0(),
            token1: IUniGoPool(pool).token1(),
            fee: IUniGoPool(pool).fee(),
            payer: payer
        });
        require(IUniGoFactory(factory).getPool(data.token0, data.token1, data.fee) == pool, "POOL");
    }

    /// @dev 校验回调确实来自 Factory 创建的 Pool
    ///      Pool 只会回调它的调用者，所以能走到这里的 data 一定是 Manager 自己编码的，payer 可信
    function _verifyCallback(bytes calldata data) private view returns (CallbackData memory decoded) {
        decoded = abi.decode(data, (CallbackData));
        require(
            msg.sender == IUniGoFactory(factory).getPool(decoded.token0, decoded.token1, decoded.fee),
            "CB"
        );
    }

    /// @dev 付款：多跳中间跳由 Manager 用自己收到的 token 付，其余从用户处划转
    function _pay(address token, address payer, address to, uint256 amount) private {
        if (payer == address(this)) {
            IERC20(token).safeTransfer(to, amount);
        } else {
            IERC20(token).safeTransferFrom(payer, to, amount);
        }
    }
}
