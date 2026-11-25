require("@nomicfoundation/hardhat-toolbox");

// 测试网部署账户私钥，只从环境变量读，不写进仓库
const accounts = process.env.DEPLOYER_KEY ? [process.env.DEPLOYER_KEY] : [];

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
      // FORK_URL 非空时分叉对应网络，用来在上链前演练部署
      forking: process.env.FORK_URL ? { url: process.env.FORK_URL } : undefined,
    },
    sepolia: {
      url: process.env.SEPOLIA_RPC_URL || "https://ethereum-sepolia-rpc.publicnode.com",
      accounts,
    },
    baseSepolia: {
      url: process.env.BASE_SEPOLIA_RPC_URL || "https://sepolia.base.org",
      accounts,
    },
    localhost: {
      // 默认连本机 8545；端口被占用时可以用 RPC_URL 指向别的本地节点
      url: process.env.RPC_URL || "http://127.0.0.1:8545",
    },
  },
};
