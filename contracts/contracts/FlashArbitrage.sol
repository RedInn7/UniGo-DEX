// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "./interfaces/IUniGoFactory.sol";
import "./interfaces/IUniGoPool.sol";
import "./interfaces/IUniGoPoolCallbacks.sol";
import "./lib/TickMath.sol";

/// @title FlashArbitrage
/// @notice 闪电贷套利示例：零本金搬平同一交易对在两个池子之间的价差
/// @dev 同一交易对的不同费率池子各自定价，价格出现偏差时存在套利空间：
///
///      【流程】（以借入 token X 为例，另一种 token 记为 Y）
///      1. 从 lender 池闪电贷借入 X
///      2. 在 X 较贵的池子（sellPool）把 X 全部卖成 Y
///      3. 在 X 较便宜的池子（buyPool）把 Y 全部买回 X
///      4. 向 lender 归还本金 + 闪电贷手续费
///      5. 剩余的 X 就是利润，转给发起人；不够还款或利润不达标时整笔交易回滚
///
///      【为什么需要第三个池子放贷】
///      Pool 在 flash 期间处于重入锁定状态，不能同时在放贷的池子里 swap，
///      所以 lender 必须是买卖两个池子之外的池子（任何持有 X 的池子都可以）
///
///      套利的副作用是两个池子的价格向彼此收敛，这正是多池市场保持价格一致的机制
contract FlashArbitrage is IUniGoPoolCallbacks {
    /// @notice 套利参数
    struct ArbitrageParams {
        address lender;        // 提供闪电贷的池子
        address sellPool;      // 借入 token 较贵的池子：卖出借入的 token
        address buyPool;       // 借入 token 较便宜的池子：买回借入的 token
        address borrowToken;   // 借入的 token
        uint256 amount;        // 借入数量
        uint256 minProfit;     // 最低利润（以借入的 token 计），达不到则 revert
    }

    /// @notice 套利成功事件
    /// @param initiator 发起人（利润接收者）
    /// @param borrowToken 借入并结算利润的 token
    /// @param amount 借入数量
    /// @param profit 利润
    event Arbitrage(address indexed initiator, address indexed borrowToken, uint256 amount, uint256 profit);

    /// @notice 工厂合约地址，用于校验 Pool 真伪
    address public immutable factory;

    /// @dev 当前允许回调本合约的 Pool，套利过程之外为零地址
    address private _expectedCaller;

    /// @param factory_ 工厂合约地址
    constructor(address factory_) {
        factory = factory_;
    }

    /// @notice 发起一次闪电贷套利
    /// @param params 套利参数
    /// @return profit 转给发起人的利润
    function arbitrage(ArbitrageParams calldata params) external returns (uint256 profit) {
        require(_expectedCaller == address(0), "LOK");
        require(params.lender != params.sellPool && params.lender != params.buyPool, "LP");

        (address lenderToken0, address lenderToken1) = _verifyPool(params.lender);
        require(params.borrowToken == lenderToken0 || params.borrowToken == lenderToken1, "BT");

        // 买卖两个池子必须是同一交易对，且包含借入的 token
        (address token0, address token1) = _verifyPool(params.sellPool);
        (address buyToken0, address buyToken1) = _verifyPool(params.buyPool);
        require(token0 == buyToken0 && token1 == buyToken1, "PAIR");
        require(params.borrowToken == token0 || params.borrowToken == token1, "BT");

        bool borrowToken0 = params.borrowToken == lenderToken0;

        _expectedCaller = params.lender;
        IUniGoPool(params.lender).flash(
            address(this),
            borrowToken0 ? params.amount : 0,
            borrowToken0 ? 0 : params.amount,
            abi.encode(params)
        );

        // 回调里已还清贷款，合约里剩下的都是利润
        profit = IERC20(params.borrowToken).balanceOf(address(this));
        IERC20(params.borrowToken).transfer(msg.sender, profit);

        emit Arbitrage(msg.sender, params.borrowToken, params.amount, profit);
    }

    // ==================== 回调实现 ====================

    /// @inheritdoc IUniGoPoolCallbacks
    /// @dev 借到 token 之后在这里完成两次 swap 并还款
    function uniGoFlashCallback(uint256 fee0, uint256 fee1, bytes calldata data) external override {
        require(msg.sender == _expectedCaller, "CB");
        ArbitrageParams memory params = abi.decode(data, (ArbitrageParams));

        // 1. 在较贵的池子把借来的 token 全部卖出
        uint256 otherAmount = _swapAll(params.sellPool, params.borrowToken, params.amount);

        // 2. 在较便宜的池子把换来的 token 全部买回借入的 token
        address otherToken = _otherToken(params.sellPool, params.borrowToken);
        _swapAll(params.buyPool, otherToken, otherAmount);

        // 3. 归还本金 + 手续费；利润必须为正且不低于 minProfit，否则整笔回滚
        uint256 owed = params.amount + (params.borrowToken == IUniGoPool(msg.sender).token0() ? fee0 : fee1);
        uint256 balance = IERC20(params.borrowToken).balanceOf(address(this));
        require(balance > owed && balance - owed >= params.minProfit, "NP");

        IERC20(params.borrowToken).transfer(msg.sender, owed);

        // 套利结束，不再接受任何回调
        _expectedCaller = address(0);
    }

    /// @inheritdoc IUniGoPoolCallbacks
    /// @dev swap 的输入 token 由本合约用手头的余额支付
    function uniGoSwapCallback(int256 amount0Delta, int256 amount1Delta, bytes calldata) external override {
        require(msg.sender == _expectedCaller, "CB");

        if (amount0Delta > 0) {
            IERC20(IUniGoPool(msg.sender).token0()).transfer(msg.sender, uint256(amount0Delta));
        }
        if (amount1Delta > 0) {
            IERC20(IUniGoPool(msg.sender).token1()).transfer(msg.sender, uint256(amount1Delta));
        }
    }

    /// @dev mint 回调 — 本合约不提供流动性，直接 revert
    function uniGoMintCallback(uint256, uint256, bytes calldata) external pure override {
        revert("NOT_SUPPORTED");
    }

    // ==================== 内部函数 ====================

    /// @dev 在指定池子把 amountIn 的 tokenIn 按市价全部卖出，返回换到的数量
    function _swapAll(address pool, address tokenIn, uint256 amountIn) private returns (uint256 amountOut) {
        bool zeroForOne = tokenIn == IUniGoPool(pool).token0();

        _expectedCaller = pool;
        (int256 amount0, int256 amount1) = IUniGoPool(pool).swap(
            address(this),
            zeroForOne,
            int256(amountIn),
            zeroForOne ? TickMath.MIN_SQRT_RATIO + 1 : TickMath.MAX_SQRT_RATIO - 1,
            ""
        );

        amountOut = uint256(-(zeroForOne ? amount1 : amount0));
    }

    /// @dev 返回池子里除 token 之外的另一种 token
    function _otherToken(address pool, address token) private view returns (address) {
        address token0 = IUniGoPool(pool).token0();
        return token == token0 ? IUniGoPool(pool).token1() : token0;
    }

    /// @dev 校验 Pool 确实由 Factory 创建，返回它的两种 token
    function _verifyPool(address pool) private view returns (address token0, address token1) {
        token0 = IUniGoPool(pool).token0();
        token1 = IUniGoPool(pool).token1();
        require(IUniGoFactory(factory).getPool(token0, token1, IUniGoPool(pool).fee()) == pool, "POOL");
    }
}
