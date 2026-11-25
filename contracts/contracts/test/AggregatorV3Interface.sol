// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title Chainlink 价格聚合器接口（AggregatorV3Interface）
/// @notice 与 Chainlink 官方接口的函数签名一致，后端按这份 ABI 读取喂价
interface AggregatorV3Interface {
    function decimals() external view returns (uint8);

    function description() external view returns (string memory);

    function version() external view returns (uint256);

    function latestRoundData()
        external
        view
        returns (
            uint80 roundId,
            int256 answer,
            uint256 startedAt,
            uint256 updatedAt,
            uint80 answeredInRound
        );
}
