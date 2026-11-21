// Deploys AgentPayRouter for tUSD8 on QDay Aevum testnet.
//   npx hardhat run scripts/deploy.js --network qday
const hre = require("hardhat");

const TUSD8 = process.env.TOKEN_ADDRESS || "0x456569838f1d43009A91ebA4263147Afd7233483";

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
  console.log("AgentPayRouter", await router.getAddress(), "block", receipt.blockNumber);
}

main().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
