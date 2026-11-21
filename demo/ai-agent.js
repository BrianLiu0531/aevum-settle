#!/usr/bin/env node
// An AI agent that shops with its owner's money, safely: Claude plans and calls the wallet tools,
// the owner's on-chain mandate bounds what it can spend, and refusals come back as codes it can
// reason about (over the daily cap -> buy something cheaper, or stop and say so).
//
//   node ai-agent.js                       the default task
//   node ai-agent.js "Get me a market quote and tell me the spread"
//   AGENT_CONFIRM_ABOVE=10 node ai-agent.js   ask a human before any purchase above 10 tUSD8
//
// Needs Claude API credentials (ANTHROPIC_API_KEY, or `ant auth login`), plus the wallet
// settings the CLI uses (../.env or ENV_FILE).
import Anthropic from "@anthropic-ai/sdk";
import { betaTool } from "@anthropic-ai/sdk/helpers/beta/json-schema";
import { createInterface } from "node:readline/promises";
import { need, opt } from "./env.js";
import { TOOL_DEFS, walletTools } from "./sdk/tools.js";

const dim = (s) => (process.stdout.isTTY ? `\x1b[2m${s}\x1b[0m` : s);
const bold = (s) => (process.stdout.isTTY ? `\x1b[1m${s}\x1b[0m` : s);

const wallet = walletTools({
  rpcUrl: opt("RPC_URL", "https://rpc-test.qday.info"),
  chainId: opt("CHAIN_ID", "44005"),
  agentKey: need("AGENT_PRIVATE_KEY"),
  owner: need("OWNER_ADDRESS"),
  router: need("ROUTER_ADDRESS"),
  merchantUrl: opt("MERCHANT_URL", "http://localhost:8787"),
  maxPrice: opt("AGENT_MAX_PRICE", undefined),
  waitSeconds: Number(opt("AGENT_WAIT_SECONDS", "120")),
  log: (m) => console.log(dim(`    · ${m}`)),
});

const confirmAbove = process.env.AGENT_CONFIRM_ABOVE ? Number(process.env.AGENT_CONFIRM_ABOVE) : null;

/** Human-in-the-loop gate for large purchases: the tool answers "declined" instead of paying. */
async function approved(product) {
  if (confirmAbove == null) return true;
  const { products } = await wallet.list_products();
  const price = Number(products.find((p) => p.product === product)?.price ?? Infinity);
  if (price <= confirmAbove) return true;
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  const answer = await rl.question(bold(`  ? The agent wants to buy "${product}" for ${price} tUSD8. Allow? [y/N] `));
  rl.close();
  return answer.trim().toLowerCase() === "y";
}

const tools = Object.entries(TOOL_DEFS).map(([name, def]) =>
  betaTool({
    name,
    description: def.description,
    inputSchema: def.schema,
    run: async (input) => {
      console.log(`  ${bold("→ " + name)} ${dim(JSON.stringify(input))}`);
      let result;
      try {
        if (name === "buy" && !(await approved(input.product))) {
          result = { status: "refused", code: "DECLINED_BY_HUMAN", reason: "the owner declined this purchase" };
        } else {
          result = await wallet[name](input);
        }
      } catch (e) {
        result = { status: "error", reason: e.message };
      }
      const summary = result.status ? `${result.status}${result.code ? " " + result.code : ""}` : "ok";
      console.log(`  ${bold("← " + summary)}`);
      return JSON.stringify(result);
    },
  }),
);

const SYSTEM = `You are a purchasing agent with a small tUSD8 budget on the QDay Aevum testnet. You pay a merchant's paid API on behalf of your owner.

How money works here:
- You never hold the owner's funds. Each purchase is paid from the owner's wallet under a mandate the owner set on chain: a per-payment cap, a daily cap, an expiry and a merchant allowlist. The contract enforces it; you cannot exceed it.
- Check wallet_status and list_products before buying, and plan within what is left today.
- A purchase that comes back "refused" moved no money. Read its code and adapt (choose something cheaper, or explain what the owner would need to change). Do not retry the same purchase unchanged.
- "pending" means paid but not yet final enough for the merchant to deliver. Larger amounts need more finality (minutes to ~90 minutes on QDay). Use check_purchase to collect it, or report it as pending.
- Spend only what the task needs, and never buy the same product twice for one task.

When you finish, report briefly: what you bought, what each cost, what you learned from the data, and anything you could not get and why. Include transaction hashes from the receipts.`;

const task =
  process.argv.slice(2).join(" ").trim() ||
  "I'm deciding whether to integrate with post-quantum DeFi protocols. Get the current tUSD8 market quote and the post-quantum readiness report, and if today's budget allows, the full historical dataset too. Don't buy the enterprise licence.";

const client = new Anthropic();
console.log(bold("Task: ") + task + "\n");

const runner = client.beta.messages.toolRunner({
  model: "claude-opus-5-5",
  max_tokens: 16000,
  output_config: { effort: "medium" },
  // On a safety decline, the API re-runs the request on a fallback model it picks by category
  betas: ["server-side-fallback-2026-07-01"],
  fallbacks: "default",
  system: SYSTEM,
  tools,
  messages: [{ role: "user", content: task }],
  max_iterations: 20,
});

let final;
for await (const message of runner) {
  for (const block of message.content) {
    if (block.type === "text" && block.text.trim() && message.stop_reason === "tool_use") console.log(dim(block.text.trim()));
  }
  final = message;
}

if (final?.stop_reason === "refusal") {
  console.log("\nThe model declined this task.");
  process.exitCode = 1;
} else {
  const text = final?.content.filter((b) => b.type === "text").map((b) => b.text).join("\n").trim();
  console.log("\n" + bold("Agent report") + "\n" + (text || "(no report)"));
}
