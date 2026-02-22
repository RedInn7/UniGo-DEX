// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

/// @title IUniGoFactory
/// @notice UniGo 工厂合约接口
/// @dev 工厂合约负责：
///      1. 创建新的交易对池（使用 CREATE2 确保地址可预测）
///      2. 管理已创建的池子
///      3. 维护费率到 tickSpacing 的映射
interface IUniGoFactory {
    /// @notice 新池创建事件
    /// @param token0 地址较小的 token
    /// @param token1 地址较大的 token
    /// @param fee 费率
    /// @param tickSpacing tick 间距
    /// @param pool 新创建的 Pool 地址
    event PoolCreated(
        address indexed token0,
        address indexed token1,
        uint24 indexed fee,
        int24 tickSpacing,
        address pool
    );

    /// @notice 创建新的交易对池
    /// @param tokenA 第一个 token 地址
    /// @param tokenB 第二个 token 地址
    /// @param fee 费率（500=0.05%, 3000=0.3%, 10000=1%）
    /// @return pool 新创建的 Pool 地址
    function createPool(
        address tokenA,
        address tokenB,
        uint24 fee
    ) external returns (address pool);

    /// @notice 查询已创建的池地址
    /// @param tokenA 第一个 token 地址
    /// @param tokenB 第二个 token 地址
    /// @param fee 费率
    /// @return pool Pool 合约地址（未创建则返回 address(0)）
    function getPool(
        address tokenA,
        address tokenB,
        uint24 fee
    ) external view returns (address pool);
}
