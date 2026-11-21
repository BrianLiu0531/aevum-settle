require("@nomicfoundation/hardhat-toolbox");
require("dotenv").config({ path: "../.env" });

const key = process.env.DEPLOYER_PRIVATE_KEY;

/** @type import('hardhat/config').HardhatUserConfig */
module.exports = {
  solidity: { version: "0.8.24", settings: { optimizer: { enabled: true, runs: 200 }, evmVersion: "paris" } },
  networks: {
    qday: {
      url: process.env.RPC_URL || "https://rpc-test.qday.info",
      chainId: 44005,
      accounts: key ? [key] : [],
    },
  },
};
