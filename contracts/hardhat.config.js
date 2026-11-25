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
      // 默认连本机 8545；端口被占用时可以用 RPC_URL 指向别的本地节点
      url: process.env.RPC_URL || "http://127.0.0.1:8545",
    },
  },
};
