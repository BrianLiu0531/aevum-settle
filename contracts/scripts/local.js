// Sets up a complete local demo on a Hardhat node (npx hardhat node):
// deploys a 6-decimal mock tUSD8 and the router, funds the owner, approves the router,
// grants the agent a mandate for the demo merchant, switches to one block per second,
// and writes ../.env.local with every address the settler and the demo need.
//
//   npx hardhat run scripts/local.js --network localhost
const fs = require("fs");
const path = require("path");
const hre = require("hardhat");

const { ethers } = hre;
const units = (v) => ethers.parseUnits(v, 6);

// Keys of Hardhat's public dev accounts (#0 owner, #1 merchant, #2 agent), derived from its
// well-known test mnemonic. Never use them anywhere but a local node.
function devKey(i) {
  const { mnemonic, path: base } = hre.config.networks.hardhat.accounts;
  return ethers.HDNodeWallet.fromPhrase(mnemonic, undefined, `${base}/${i}`).privateKey;
}

async function main() {
  const { chainId } = await ethers.provider.getNetwork();
  if (chainId !== 31337n) throw new Error(`refusing to run on chain ${chainId}: this script is for a local node`);
  const [owner, merchant, agent] = await ethers.getSigners();
  const keys = [0, 1, 2].map(devKey);
  if (new ethers.Wallet(keys[2]).address !== agent.address) throw new Error("dev key derivation does not match the node's accounts");

  const token = await ethers.deployContract("MockToken");
  await token.waitForDeployment();
  const router = await ethers.deployContract("AgentPayRouter", [await token.getAddress()]);
  const receipt = await router.deploymentTransaction().wait();

  await (await token.mint(owner.address, units("1000"))).wait();
  await (await token.approve(await router.getAddress(), ethers.MaxUint256)).wait();
  const week = Math.floor(Date.now() / 1000) + 7 * 24 * 3600;
  await (
    await router.grantMandate(agent.address, units("150"), units("200"), week, [merchant.address])
  ).wait();

  // Blocks every second, so the simulated virtual and verified depths advance on their own.
  await ethers.provider.send("evm_setIntervalMining", [1000]);

  const env = {
    NETWORK: "local",
    RPC_URLS: "http://127.0.0.1:8545",
    RPC_URL: "http://127.0.0.1:8545",
    CHAIN_ID: "31337",
    TOKEN_ADDRESS: await token.getAddress(),
    ROUTER_ADDRESS: await router.getAddress(),
    START_BLOCK: String(receipt.blockNumber),
    OWNER_ADDRESS: owner.address,
    OWNER_PRIVATE_KEY: keys[0],
    MERCHANT_ADDRESS: merchant.address,
    MERCHANT_PRIVATE_KEY: keys[1],
    AGENT_PRIVATE_KEY: keys[2],
    LOCAL_VIRTUAL_DEPTH: "10",
    LOCAL_VERIFIED_DEPTH: "30",
  };
  const out = path.join(__dirname, "..", "..", ".env.local");
  fs.writeFileSync(
    out,
    "# Written by contracts/scripts/local.js for the local demo. Hardhat dev keys only.\n" +
      Object.entries(env)
        .map(([k, v]) => `${k}=${v}`)
        .join("\n") +
      "\n",
  );
  console.log(`token ${env.TOKEN_ADDRESS}\nrouter ${env.ROUTER_ADDRESS} (block ${env.START_BLOCK})`);
  console.log(`owner ${owner.address}\nmerchant ${merchant.address}\nagent ${agent.address}`);
  console.log(`wrote ${out}`);
}

main().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
