require("@nomicfoundation/hardhat-toolbox");

/** @type import('hardhat/config').HardhatUserConfig */
module.exports = {
  solidity: {
    version: "0.8.20",
    settings: {
      optimizer: {
        enabled: true,
        runs: 200,
      },
      viaIR: true,
    },
  },
  // gas 报告默认关闭，REPORT_GAS=1 npx hardhat test 时开启
  gasReporter: {
    enabled: !!process.env.REPORT_GAS,
  },
  networks: {
    hardhat: {
      // 本地 Hardhat 网络配置
      allowUnlimitedContractSize: true,
    },
    localhost: {
      url: "http://127.0.0.1:8545",
    },
  },
};
