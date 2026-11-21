// Deploys AgentPayRouter for tUSD8 on QDay Aevum testnet.
//   npx hardhat run scripts/deploy.js --network qday
const fs = require("fs");
const path = require("path");
const hre = require("hardhat");

const TUSD8 = process.env.TOKEN_ADDRESS || "0x456569838f1d43009A91ebA4263147Afd7233483";

function setEnv(env, key, value) {
  const line = new RegExp("^" + key + "=.*$", "m");
  return line.test(env) ? env.replace(line, () => `${key}=${value}`) : `${env.trimEnd()}\n${key}=${value}\n`;
}

async function main() {
  const { chainId } = await hre.ethers.provider.getNetwork();
  if (hre.network.name === "qday" && chainId !== 44005n) throw new Error(`unexpected chain id ${chainId}`);
  const [deployer] = await hre.ethers.getSigners();
  console.log("deployer", deployer.address);
  const router = await hre.ethers.deployContract("AgentPayRouter", [TUSD8]);
  const tx = router.deploymentTransaction();
  console.log("tx", tx.hash);
  await router.waitForDeployment();
  const receipt = await tx.wait();
  const address = await router.getAddress();
  console.log("AgentPayRouter", address, "block", receipt.blockNumber);

  // Record the deployment in ../.env so the settler, merchant and agent pick it up.
  const envFile = path.join(__dirname, "..", "..", ".env");
  if (hre.network.name === "qday" && fs.existsSync(envFile)) {
    let env = fs.readFileSync(envFile, "utf8");
    env = setEnv(env, "ROUTER_ADDRESS", address);
    env = setEnv(env, "START_BLOCK", String(receipt.blockNumber));
    fs.writeFileSync(envFile, env);
    console.log("updated ROUTER_ADDRESS and START_BLOCK in", envFile);
  }
}

main().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
