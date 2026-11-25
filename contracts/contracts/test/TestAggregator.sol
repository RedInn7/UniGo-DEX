// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "./AggregatorV3Interface.sol";

/// @title 测试用的 Chainlink 模拟聚合器
/// @notice 价格、时间戳和轮次都可以任意设置，用来模拟喂价正常、偏离、过期、轮次未完成等情况
contract TestAggregator is AggregatorV3Interface {
    uint8 public immutable override decimals;
    string public override description;
    uint256 public constant override version = 1;

    uint80 private _roundId;
    int256 private _answer;
    uint256 private _startedAt;
    uint256 private _updatedAt;
    uint80 private _answeredInRound;

    constructor(uint8 decimals_, string memory description_, int256 initialAnswer) {
        decimals = decimals_;
        description = description_;
        setAnswer(initialAnswer);
    }

    /// @notice 上报一个新价格：轮次加一，时间戳取当前区块时间
    function setAnswer(int256 answer) public {
        _roundId += 1;
        _answer = answer;
        _startedAt = block.timestamp;
        _updatedAt = block.timestamp;
        _answeredInRound = _roundId;
    }

    /// @notice 直接指定整轮数据，用来构造过期、轮次未完成（updatedAt = 0）、
    ///         答案来自旧轮次（answeredInRound < roundId）等异常情况
    function setRoundData(
        uint80 roundId,
        int256 answer,
        uint256 startedAt,
        uint256 updatedAt,
        uint80 answeredInRound
    ) external {
        _roundId = roundId;
        _answer = answer;
        _startedAt = startedAt;
        _updatedAt = updatedAt;
        _answeredInRound = answeredInRound;
    }

    function latestRoundData()
        external
        view
        override
        returns (
            uint80 roundId,
            int256 answer,
            uint256 startedAt,
            uint256 updatedAt,
            uint80 answeredInRound
        )
    {
        return (_roundId, _answer, _startedAt, _updatedAt, _answeredInRound);
    }
}
