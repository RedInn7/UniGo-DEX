// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title IUniGoPool
/// @notice UniGo 流动性池的对外接口
/// @dev 定义了 Pool 合约的核心功能：
///      - 初始化池子
///      - 添加/移除流动性（mint/burn）
///      - 代币交换（swap）
///      - 提取手续费（collect）
///      - 闪电贷（flash）
interface IUniGoPool {
    /// @notice 添加流动性事件
    /// @param sender 调用 mint 的地址（通常是 Manager 合约）
    /// @param owner 流动性拥有者
    /// @param tickLower position 下界 tick
    /// @param tickUpper position 上界 tick
    /// @param amount 添加的流动性数量
    /// @param amount0 存入的 token0 数量
    /// @param amount1 存入的 token1 数量
    event Mint(
        address sender,
        address indexed owner,
        int24 indexed tickLower,
        int24 indexed tickUpper,
        uint128 amount,
        uint256 amount0,
        uint256 amount1
    );

    /// @notice 移除流动性事件
    event Burn(
        address indexed owner,
        int24 indexed tickLower,
        int24 indexed tickUpper,
        uint128 amount,
        uint256 amount0,
        uint256 amount1
    );

    /// @notice 代币交换事件
    /// @param sender 调用 swap 的地址
    /// @param recipient 接收输出 token 的地址
    /// @param amount0 token0 的变化量（负=Pool 转出）
    /// @param amount1 token1 的变化量
    /// @param sqrtPriceX96 swap 后的价格
    /// @param liquidity swap 后的流动性
    /// @param tick swap 后的 tick
    event Swap(
        address indexed sender,
        address indexed recipient,
        int256 amount0,
        int256 amount1,
        uint160 sqrtPriceX96,
        uint128 liquidity,
        int24 tick
    );

    /// @notice 提取手续费事件
    event Collect(
        address indexed owner,
        address recipient,
        int24 indexed tickLower,
        int24 indexed tickUpper,
        uint128 amount0,
        uint128 amount1
    );

    /// @notice 闪电贷事件
    /// @param sender 调用 flash 的地址
    /// @param recipient 接收借出 token 的地址
    /// @param amount0 借出的 token0 数量
    /// @param amount1 借出的 token1 数量
    /// @param paid0 实际支付的 token0 手续费
    /// @param paid1 实际支付的 token1 手续费
    event Flash(
        address indexed sender,
        address indexed recipient,
        uint256 amount0,
        uint256 amount1,
        uint256 paid0,
        uint256 paid1
    );

    /// @notice 池子初始化事件
    /// @param sqrtPriceX96 初始价格（Q64.96 格式）
    /// @param tick 初始价格对应的 tick
    event Initialize(uint160 sqrtPriceX96, int24 tick);

    /// @notice 池子的 token0 地址（地址较小的 token）
    function token0() external view returns (address);

    /// @notice 池子的 token1 地址（地址较大的 token）
    function token1() external view returns (address);

    /// @notice 手续费率（以百万分之一为单位）
    function fee() external view returns (uint24);

    /// @notice 初始化池子
    /// @param sqrtPriceX96 初始价格（Q64.96 格式）
    function initialize(uint160 sqrtPriceX96) external;

    /// @notice 添加流动性
    /// @param owner 流动性拥有者
    /// @param tickLower 下界 tick
    /// @param tickUpper 上界 tick
    /// @param amount 流动性数量
    /// @param data 回调数据
    /// @return amount0 实际存入的 token0 数量
    /// @return amount1 实际存入的 token1 数量
    function mint(
        address owner,
        int24 tickLower,
        int24 tickUpper,
        uint128 amount,
        bytes calldata data
    ) external returns (uint256 amount0, uint256 amount1);

    /// @notice 移除流动性
    /// @param tickLower 下界 tick
    /// @param tickUpper 上界 tick
    /// @param amount 要移除的流动性数量
    /// @return amount0 可提取的 token0 数量
    /// @return amount1 可提取的 token1 数量
    function burn(
        int24 tickLower,
        int24 tickUpper,
        uint128 amount
    ) external returns (uint256 amount0, uint256 amount1);

    /// @notice 代币交换
    /// @param recipient 接收者地址
    /// @param zeroForOne 交易方向（true: token0->token1）
    /// @param amountSpecified 交易数量（正=exactInput, 负=exactOutput）
    /// @param sqrtPriceLimitX96 价格限制（滑点保护）
    /// @param data 回调数据
    /// @return amount0 token0 的变化量
    /// @return amount1 token1 的变化量
    function swap(
        address recipient,
        bool zeroForOne,
        int256 amountSpecified,
        uint160 sqrtPriceLimitX96,
        bytes calldata data
    ) external returns (int256 amount0, int256 amount1);

    /// @notice 提取手续费
    /// @param recipient 接收者地址
    /// @param tickLower 下界 tick
    /// @param tickUpper 上界 tick
    /// @param amount0Requested 请求提取的 token0 数量
    /// @param amount1Requested 请求提取的 token1 数量
    /// @return amount0 实际提取的 token0 数量
    /// @return amount1 实际提取的 token1 数量
    function collect(
        address recipient,
        int24 tickLower,
        int24 tickUpper,
        uint128 amount0Requested,
        uint128 amount1Requested
    ) external returns (uint128 amount0, uint128 amount1);

    /// @notice 闪电贷：同一笔交易内借出并归还 token
    /// @dev 调用者必须实现 uniGoFlashCallback，并在回调内归还本金 + 手续费
    /// @param recipient 接收借出 token 的地址
    /// @param amount0 借出的 token0 数量
    /// @param amount1 借出的 token1 数量
    /// @param data 回调数据
    function flash(
        address recipient,
        uint256 amount0,
        uint256 amount1,
        bytes calldata data
    ) external;
}
