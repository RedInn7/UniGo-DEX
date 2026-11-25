// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "../interfaces/IUniGoPoolCallbacks.sol";

/// @title TestFakePool
/// @notice 仅用于测试的伪造 Pool
/// @dev 冒充真实池子的 token0 / token1 / fee，试图借 Manager 的回调划走用户已授权的 token
contract TestFakePool {
    address public immutable token0;
    address public immutable token1;
    uint24 public immutable fee;

    constructor(address token0_, address token1_, uint24 fee_) {
        token0 = token0_;
        token1 = token1_;
        fee = fee_;
    }

    /// @notice 直接调用 Manager 的 swap 回调，付款者填成受害者
    function attack(address manager, address victim, uint256 amount) external {
        IUniGoPoolCallbacks(manager).uniGoSwapCallback(
            int256(amount),
            0,
            abi.encode(token0, token1, fee, victim)
        );
    }

    /// @dev 被 Manager 当作 Pool 调用时，同样在回调里尝试划走调用者的 token
    function swap(address, bool, int256 amountSpecified, uint160, bytes calldata data)
        external
        returns (int256, int256)
    {
        IUniGoPoolCallbacks(msg.sender).uniGoSwapCallback(amountSpecified, 0, data);
        return (amountSpecified, 0);
    }
}
