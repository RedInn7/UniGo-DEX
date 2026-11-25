require("@nomicfoundation/hardhat-toolbox");

// 数学库实现一键切换：MATH_IMPL=oss 时，编译器读 lib/Math.sol 和 lib/BitMath.sol
// 实际拿到的是 lib/oss/ 下的同名文件（内部转给 OpenZeppelin），其余合约不用改。
// 默认（native）用自己写的实现。两种模式跑的是同一套测试。
const { subtask } = require("hardhat/config");
const { TASK_COMPILE_SOLIDITY_READ_FILE } = require("hardhat/builtin-tasks/task-names");
const path = require("path");
const mathImpl = process.env.MATH_IMPL || "native";
if (!["native", "oss"].includes(mathImpl)) throw new Error(`MATH_IMPL 只能是 native 或 oss，收到 ${mathImpl}`);
subtask(TASK_COMPILE_SOLIDITY_READ_FILE).setAction(async ({ absolutePath }, hre, runSuper) => {
  const lib = path.join(__dirname, "contracts", "lib");
  const swapped = ["Math.sol", "BitMath.sol"].map((file) => path.join(lib, file));
  if (mathImpl === "oss" && swapped.includes(absolutePath)) {
    absolutePath = path.join(lib, "oss", path.basename(absolutePath));
  }
  return runSuper({ absolutePath });
});

// GAS_PRICE（单位 wei）非空时固定 gas 价，测试币不多时用来压低花费
const gasPrice = process.env.GAS_PRICE ? Number(process.env.GAS_PRICE) : "auto";
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
      gasPrice,
    },
    baseSepolia: {
      url: process.env.BASE_SEPOLIA_RPC_URL || "https://sepolia.base.org",
      accounts,
      gasPrice,
    },
    localhost: {
      // 默认连本机 8545；端口被占用时可以用 RPC_URL 指向别的本地节点
      url: process.env.RPC_URL || "http://127.0.0.1:8545",
    },
  },
};
