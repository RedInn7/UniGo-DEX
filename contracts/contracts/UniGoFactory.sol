// SPDX-License-Identifier: GPL-2.0-or-later
pragma solidity ^0.8.20;

import "./UniGoPool.sol";
import "./interfaces/IUniGoFactory.sol";

/// @title UniGoFactory
/// @notice 池工厂合约，负责创建和管理交易对池
/// @dev 工厂合约的核心职责：
///      1. 使用 CREATE2 创建 Pool 合约（地址可预测）
///      2. 维护 (tokenA, tokenB, fee) -> Pool 地址的映射
///      3. 管理费率与 tickSpacing 的对应关系
///
///      【费率与 tickSpacing 的关系】
///      - 低费率（500 = 0.05%）：tickSpacing=10，适合稳定币对
///      - 中费率（3000 = 0.3%）：tickSpacing=60，适合大多数交易对
///      - 高费率（10000 = 1%）：tickSpacing=200，适合波动较大的交易对
///
///      tickSpacing 越大，可选的价格点越少，但 gas 更便宜
///      tickSpacing 越小，价格精度越高，但 swap 时可能需要遍历更多 tick
contract UniGoFactory is IUniGoFactory {
    /// @notice 费率到 tickSpacing 的映射
    mapping(uint24 => int24) public feeAmountTickSpacing;

    /// @notice 已创建的池子映射
    /// @dev pools[token0][token1][fee] = pool address
    ///      token0 < token1（按地址排序）
    mapping(address => mapping(address => mapping(uint24 => address))) public pools;

    constructor() {
        // 初始化支持的费率及对应的 tickSpacing
        feeAmountTickSpacing[500] = 10;     // 0.05% 费率，适合稳定币
        feeAmountTickSpacing[3000] = 60;    // 0.3% 费率，标准费率
        feeAmountTickSpacing[10000] = 200;  // 1% 费率，高波动
    }

    /// @inheritdoc IUniGoFactory
    function createPool(
        address tokenA,
        address tokenB,
        uint24 fee_
    ) external override returns (address pool) {
        require(tokenA != tokenB, "IA");
        // 按地址大小排序，确保每个交易对只有一个 Pool
        (address token0_, address token1_) = tokenA < tokenB ? (tokenA, tokenB) : (tokenB, tokenA);
        require(token0_ != address(0), "ZA");

        int24 tickSpacing_ = feeAmountTickSpacing[fee_];
        require(tickSpacing_ != 0, "FEE");
        require(pools[token0_][token1_][fee_] == address(0), "PE");

        // 使用 CREATE2 部署 Pool 合约
        // salt 由 token0, token1, fee 决定，使得 Pool 地址可预测
        pool = address(
            new UniGoPool{salt: keccak256(abi.encode(token0_, token1_, fee_))}(
                address(this),
                token0_,
                token1_,
                fee_,
                tickSpacing_
            )
        );

        // 双向存储映射（无论传入顺序如何都能查到）
        pools[token0_][token1_][fee_] = pool;
        pools[token1_][token0_][fee_] = pool;

        emit PoolCreated(token0_, token1_, fee_, tickSpacing_, pool);
    }

    /// @inheritdoc IUniGoFactory
    function getPool(
        address tokenA,
        address tokenB,
        uint24 fee_
    ) external view override returns (address pool) {
        pool = pools[tokenA][tokenB][fee_];
    }
}
