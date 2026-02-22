// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "./interfaces/IUniGoPool.sol";
import "./interfaces/IUniGoPoolCallbacks.sol";
import "./lib/Tick.sol";
import "./lib/TickBitmap.sol";
import "./lib/Position.sol";
import "./lib/TickMath.sol";
import "./lib/SqrtPriceMath.sol";
import "./lib/SwapMath.sol";
import "./lib/LiquidityMath.sol";
import "./lib/FixedPoint96.sol";
import "./lib/Math.sol";

/// @title UniGoPool
/// @notice 核心流动性池合约
/// @dev 实现集中流动性 AMM 的所有核心逻辑：
///
///      【架构概述】
///      - 每个 Pool 对应一个交易对（token0/token1）和一个费率
///      - 流动性被集中在用户指定的价格区间 [tickLower, tickUpper)
///      - swap 时在 tick 之间逐步计算，穿过 tick 边界时更新活跃流动性
///
///      【关键状态】
///      - slot0: 存储当前价格和 tick（最频繁更新的状态，放在一个 storage slot 中节省 gas）
///      - liquidity: 当前价格点的活跃流动性总量
///      - ticks: 所有已初始化 tick 的状态
///      - positions: 所有 position 的状态
///      - tickBitmap: tick 位图索引，用于高效查找已初始化的 tick
///
///      【回调模式】
///      - Pool 使用回调模式接收 token（而非 approve + transferFrom）
///      - mint/swap 完成计算后，通过回调要求调用者转入 token
///      - 回调后验证余额增加是否符合预期
contract UniGoPool is IUniGoPool {
    using Tick for mapping(int24 => Tick.Info);
    using TickBitmap for mapping(int16 => uint256);
    using Position for mapping(bytes32 => Position.Info);
    using Position for Position.Info;

    // ==================== 不可变状态 ====================

    /// @notice 池子的 token0 地址（地址较小的 token）
    address public immutable token0;
    /// @notice 池子的 token1 地址（地址较大的 token）
    address public immutable token1;
    /// @notice 手续费率（以百万分之一为单位，如 3000 = 0.3%）
    uint24 public immutable fee;
    /// @notice Tick 间距，与费率关联（费率越高间距越大，减少 gas 消耗）
    int24 public immutable tickSpacing;
    /// @notice 创建此池的工厂合约地址
    address public immutable factory;

    // ==================== 核心状态 ====================

    /// @notice Slot0 结构体，存储最频繁更新的状态
    /// @dev 打包在一个 storage slot 中以节省 gas
    struct Slot0 {
        /// @dev 当前平方根价格（Q64.96 格式）
        uint160 sqrtPriceX96;
        /// @dev 当前 tick（由 sqrtPriceX96 计算得出）
        int24 tick;
        /// @dev 池子是否已解锁（重入保护）
        bool unlocked;
    }

    /// @notice 池子的核心状态
    Slot0 public slot0;

    /// @notice 当前价格点的活跃流动性
    /// @dev 只有价格区间包含当前价格的 position 的流动性会被计入
    uint128 public liquidity;

    // ==================== 映射状态 ====================

    /// @notice Tick 状态映射
    mapping(int24 => Tick.Info) public ticks;
    /// @notice Tick 位图索引
    mapping(int16 => uint256) public tickBitmap;
    /// @notice Position 状态映射
    mapping(bytes32 => Position.Info) public positions;

    // ==================== Swap 内部结构体 ====================

    /// @dev swap 循环的全局状态
    struct SwapState {
        int256 amountSpecifiedRemaining;  // 剩余待交换数量
        int256 amountCalculated;          // 已计算的对手方数量
        uint160 sqrtPriceX96;             // 当前价格
        int24 tick;                       // 当前 tick
        uint128 liquidity;                // 当前活跃流动性
    }

    /// @dev swap 单步计算状态
    struct StepState {
        uint160 sqrtPriceStartX96;    // 本步开始价格
        int24 tickNext;               // 下一个已初始化的 tick
        bool initialized;            // tickNext 是否已初始化
        uint160 sqrtPriceNextX96;     // tickNext 对应的价格
        uint256 amountIn;             // 本步输入量
        uint256 amountOut;            // 本步输出量
        uint256 feeAmount;            // 本步手续费
    }

    // ==================== 修饰符 ====================

    /// @dev 重入锁，防止在回调中重入
    modifier lock() {
        require(slot0.unlocked, "LOK");
        slot0.unlocked = false;
        _;
        slot0.unlocked = true;
    }

    // ==================== 构造函数 ====================

    /// @notice 构造函数，由 Factory 合约调用
    constructor(
        address _factory,
        address _token0,
        address _token1,
        uint24 _fee,
        int24 _tickSpacing
    ) {
        factory = _factory;
        token0 = _token0;
        token1 = _token1;
        fee = _fee;
        tickSpacing = _tickSpacing;
    }

    // ==================== 初始化 ====================

    /// @inheritdoc IUniGoPool
    function initialize(uint160 sqrtPriceX96) external override {
        // 只能初始化一次
        require(slot0.sqrtPriceX96 == 0, "AI");

        int24 tick = TickMath.getTickAtSqrtRatio(sqrtPriceX96);

        slot0 = Slot0({
            sqrtPriceX96: sqrtPriceX96,
            tick: tick,
            unlocked: true
        });
    }

    // ==================== 添加流动性 ====================

    /// @inheritdoc IUniGoPool
    /// @dev mint 流程：
    ///      1. 计算在指定 tick 区间内需要的 token0 和 token1 数量
    ///      2. 更新 tick 和 position 状态
    ///      3. 回调调用者，要求转入相应 token
    ///      4. 验证转入数量是否正确
    function mint(
        address owner,
        int24 tickLower,
        int24 tickUpper,
        uint128 amount,
        bytes calldata data
    ) external override lock returns (uint256 amount0, uint256 amount1) {
        require(amount > 0, "AM");
        // 验证 tick 范围有效
        require(tickLower < tickUpper, "TLU");
        require(tickLower >= TickMath.MIN_TICK, "TLM");
        require(tickUpper <= TickMath.MAX_TICK, "TUM");

        // --- 更新 tick 状态 ---
        // 更新下界 tick（upper=false，穿过时流动性增加）
        bool flippedLower = ticks.update(tickLower, int128(amount), false);
        // 更新上界 tick（upper=true，穿过时流动性减少）
        bool flippedUpper = ticks.update(tickUpper, int128(amount), true);

        // 如果 tick 的初始化状态发生翻转，更新位图
        if (flippedLower) {
            tickBitmap.flipTick(tickLower, tickSpacing);
        }
        if (flippedUpper) {
            tickBitmap.flipTick(tickUpper, tickSpacing);
        }

        // --- 更新 position 状态 ---
        Position.Info storage position = positions.get(owner, tickLower, tickUpper);
        position.update(int128(amount));

        // --- 计算需要的 token 数量 ---
        Slot0 memory _slot0 = slot0;

        if (_slot0.tick < tickLower) {
            // 当前价格在区间下方：只需要 token0
            amount0 = uint256(int256(SqrtPriceMath.getAmount0Delta(
                TickMath.getSqrtRatioAtTick(tickLower),
                TickMath.getSqrtRatioAtTick(tickUpper),
                int128(amount)
            )));
        } else if (_slot0.tick < tickUpper) {
            // 当前价格在区间内：需要 token0 和 token1
            amount0 = uint256(int256(SqrtPriceMath.getAmount0Delta(
                _slot0.sqrtPriceX96,
                TickMath.getSqrtRatioAtTick(tickUpper),
                int128(amount)
            )));
            amount1 = uint256(int256(SqrtPriceMath.getAmount1Delta(
                TickMath.getSqrtRatioAtTick(tickLower),
                _slot0.sqrtPriceX96,
                int128(amount)
            )));
            // 更新活跃流动性
            liquidity = LiquidityMath.addDelta(liquidity, int128(amount));
        } else {
            // 当前价格在区间上方：只需要 token1
            amount1 = uint256(int256(SqrtPriceMath.getAmount1Delta(
                TickMath.getSqrtRatioAtTick(tickLower),
                TickMath.getSqrtRatioAtTick(tickUpper),
                int128(amount)
            )));
        }

        // --- 回调接收 token ---
        uint256 balance0Before;
        uint256 balance1Before;
        if (amount0 > 0) balance0Before = balance0();
        if (amount1 > 0) balance1Before = balance1();

        IUniGoPoolCallbacks(msg.sender).uniGoMintCallback(amount0, amount1, data);

        // 验证 token 转入
        if (amount0 > 0) require(balance0Before + amount0 <= balance0(), "M0");
        if (amount1 > 0) require(balance1Before + amount1 <= balance1(), "M1");

        emit Mint(msg.sender, owner, tickLower, tickUpper, amount, amount0, amount1);
    }

    // ==================== 移除流动性 ====================

    /// @inheritdoc IUniGoPool
    /// @dev burn 只更新状态，不转移 token
    ///      用户需要再调用 collect() 来提取 token
    function burn(
        int24 tickLower,
        int24 tickUpper,
        uint128 amount
    ) external override lock returns (uint256 amount0, uint256 amount1) {
        // 更新 position
        Position.Info storage position = positions.get(msg.sender, tickLower, tickUpper);
        require(position.liquidity >= amount, "BL");

        // --- 计算可提取的 token 数量（与 mint 反向计算）---
        Slot0 memory _slot0 = slot0;

        if (_slot0.tick < tickLower) {
            amount0 = uint256(-int256(SqrtPriceMath.getAmount0Delta(
                TickMath.getSqrtRatioAtTick(tickLower),
                TickMath.getSqrtRatioAtTick(tickUpper),
                -int128(amount)
            )));
        } else if (_slot0.tick < tickUpper) {
            amount0 = uint256(-int256(SqrtPriceMath.getAmount0Delta(
                _slot0.sqrtPriceX96,
                TickMath.getSqrtRatioAtTick(tickUpper),
                -int128(amount)
            )));
            amount1 = uint256(-int256(SqrtPriceMath.getAmount1Delta(
                TickMath.getSqrtRatioAtTick(tickLower),
                _slot0.sqrtPriceX96,
                -int128(amount)
            )));
            liquidity = LiquidityMath.addDelta(liquidity, -int128(amount));
        } else {
            amount1 = uint256(-int256(SqrtPriceMath.getAmount1Delta(
                TickMath.getSqrtRatioAtTick(tickLower),
                TickMath.getSqrtRatioAtTick(tickUpper),
                -int128(amount)
            )));
        }

        // --- 更新 tick 状态 ---
        bool flippedLower = ticks.update(tickLower, -int128(amount), false);
        bool flippedUpper = ticks.update(tickUpper, -int128(amount), true);

        if (flippedLower) {
            tickBitmap.flipTick(tickLower, tickSpacing);
        }
        if (flippedUpper) {
            tickBitmap.flipTick(tickUpper, tickSpacing);
        }

        // 更新 position
        position.update(-int128(amount));

        emit Burn(msg.sender, tickLower, tickUpper, amount, amount0, amount1);
    }

    // ==================== 提取手续费 ====================

    /// @inheritdoc IUniGoPool
    /// @dev 简化版 collect，直接提取 burn 后的可用 token
    function collect(
        address recipient,
        int24 tickLower,
        int24 tickUpper,
        uint128 amount0Requested,
        uint128 amount1Requested
    ) external override lock returns (uint128 amount0, uint128 amount1) {
        // 简化实现：直接转出请求的数量
        amount0 = amount0Requested;
        amount1 = amount1Requested;

        if (amount0 > 0) {
            IERC20(token0).transfer(recipient, amount0);
        }
        if (amount1 > 0) {
            IERC20(token1).transfer(recipient, amount1);
        }

        emit Collect(msg.sender, recipient, tickLower, tickUpper, amount0, amount1);
    }

    // ==================== 代币交换 ====================

    /// @inheritdoc IUniGoPool
    /// @dev swap 核心流程：
    ///      1. 确定交易方向和价格限制
    ///      2. 循环计算：
    ///         a. 找到下一个已初始化的 tick（价格边界）
    ///         b. 在当前区间内执行单步 swap（SwapMath.computeSwapStep）
    ///         c. 如果到达了 tick 边界，穿过 tick，更新活跃流动性
    ///         d. 重复直到数量用尽或达到价格限制
    ///      3. 更新全局状态
    ///      4. 回调接收输入 token，转出输出 token
    function swap(
        address recipient,
        bool zeroForOne,
        int256 amountSpecified,
        uint160 sqrtPriceLimitX96,
        bytes calldata data
    ) external override lock returns (int256 amount0, int256 amount1) {
        require(amountSpecified != 0, "AS");

        Slot0 memory _slot0 = slot0;

        // 验证价格限制方向
        require(
            zeroForOne
                ? sqrtPriceLimitX96 < _slot0.sqrtPriceX96 && sqrtPriceLimitX96 > TickMath.MIN_SQRT_RATIO
                : sqrtPriceLimitX96 > _slot0.sqrtPriceX96 && sqrtPriceLimitX96 < TickMath.MAX_SQRT_RATIO,
            "SPL"
        );

        bool exactInput = amountSpecified > 0;

        SwapState memory state = SwapState({
            amountSpecifiedRemaining: amountSpecified,
            amountCalculated: 0,
            sqrtPriceX96: _slot0.sqrtPriceX96,
            tick: _slot0.tick,
            liquidity: liquidity
        });

        // === swap 主循环 ===
        while (state.amountSpecifiedRemaining != 0 && state.sqrtPriceX96 != sqrtPriceLimitX96) {
            StepState memory step;
            step.sqrtPriceStartX96 = state.sqrtPriceX96;

            // 1. 查找下一个已初始化的 tick
            (step.tickNext, step.initialized) = tickBitmap.nextInitializedTickWithinOneWord(
                state.tick,
                tickSpacing,
                zeroForOne
            );

            // 限制 tick 范围
            if (step.tickNext < TickMath.MIN_TICK) {
                step.tickNext = TickMath.MIN_TICK;
            } else if (step.tickNext > TickMath.MAX_TICK) {
                step.tickNext = TickMath.MAX_TICK;
            }

            step.sqrtPriceNextX96 = TickMath.getSqrtRatioAtTick(step.tickNext);

            // 2. 在当前区间内执行单步 swap
            (state.sqrtPriceX96, step.amountIn, step.amountOut, step.feeAmount) = SwapMath.computeSwapStep(
                state.sqrtPriceX96,
                // 价格目标取价格限制和下一个 tick 价格中更保守的一个
                (zeroForOne ? step.sqrtPriceNextX96 < sqrtPriceLimitX96 : step.sqrtPriceNextX96 > sqrtPriceLimitX96)
                    ? sqrtPriceLimitX96
                    : step.sqrtPriceNextX96,
                state.liquidity,
                state.amountSpecifiedRemaining,
                fee
            );

            // 3. 更新剩余数量
            if (exactInput) {
                state.amountSpecifiedRemaining -= int256(step.amountIn + step.feeAmount);
                state.amountCalculated -= int256(step.amountOut);
            } else {
                state.amountSpecifiedRemaining += int256(step.amountOut);
                state.amountCalculated += int256(step.amountIn + step.feeAmount);
            }

            // 4. 如果到达了 tick 边界，穿过 tick
            if (state.sqrtPriceX96 == step.sqrtPriceNextX96) {
                if (step.initialized) {
                    int128 liquidityNet = ticks.cross(step.tickNext);
                    // 如果是反向穿越（zeroForOne），取反
                    if (zeroForOne) liquidityNet = -liquidityNet;
                    state.liquidity = LiquidityMath.addDelta(state.liquidity, liquidityNet);
                }
                // 更新 tick（zeroForOne 方向减 1，因为 tick 对应区间是 [tick, tick+1)）
                state.tick = zeroForOne ? step.tickNext - 1 : step.tickNext;
            } else if (state.sqrtPriceX96 != step.sqrtPriceStartX96) {
                // 价格变化了但未到达 tick 边界，重新计算 tick
                state.tick = TickMath.getTickAtSqrtRatio(state.sqrtPriceX96);
            }
        }

        // === 更新全局状态 ===
        if (state.tick != _slot0.tick) {
            (slot0.sqrtPriceX96, slot0.tick) = (state.sqrtPriceX96, state.tick);
        } else {
            slot0.sqrtPriceX96 = state.sqrtPriceX96;
        }

        if (liquidity != state.liquidity) {
            liquidity = state.liquidity;
        }

        // === 计算最终的 token 数量 ===
        if (exactInput) {
            (amount0, amount1) = zeroForOne
                ? (amountSpecified - state.amountSpecifiedRemaining, state.amountCalculated)
                : (state.amountCalculated, amountSpecified - state.amountSpecifiedRemaining);
        } else {
            (amount0, amount1) = zeroForOne
                ? (state.amountCalculated, amountSpecified - state.amountSpecifiedRemaining)
                : (amountSpecified - state.amountSpecifiedRemaining, state.amountCalculated);
        }

        // === 执行 token 转账 ===
        if (zeroForOne) {
            // token0 -> token1：转出 token1，回调接收 token0
            if (amount1 < 0) {
                IERC20(token1).transfer(recipient, uint256(-amount1));
            }
            uint256 balance0Before = balance0();
            IUniGoPoolCallbacks(msg.sender).uniGoSwapCallback(amount0, amount1, data);
            require(balance0Before + uint256(amount0) <= balance0(), "IIA");
        } else {
            // token1 -> token0：转出 token0，回调接收 token1
            if (amount0 < 0) {
                IERC20(token0).transfer(recipient, uint256(-amount0));
            }
            uint256 balance1Before = balance1();
            IUniGoPoolCallbacks(msg.sender).uniGoSwapCallback(amount0, amount1, data);
            require(balance1Before + uint256(amount1) <= balance1(), "IIA");
        }

        emit Swap(msg.sender, recipient, amount0, amount1, state.sqrtPriceX96, state.liquidity, state.tick);
    }

    // ==================== 内部函数 ====================

    /// @notice 获取池子的 token0 余额
    function balance0() private view returns (uint256) {
        return IERC20(token0).balanceOf(address(this));
    }

    /// @notice 获取池子的 token1 余额
    function balance1() private view returns (uint256) {
        return IERC20(token1).balanceOf(address(this));
    }
}
