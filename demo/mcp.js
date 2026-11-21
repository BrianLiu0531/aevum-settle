#!/usr/bin/env node
// MCP server: gives any MCP client (Claude Code, Claude Desktop, ...) this agent's wallet as
// tools: wallet_status, list_products, buy, check_purchase. Spending is bounded by the owner's
// on-chain mandate, whatever the model asks for.
//
//   claude mcp add aevum-wallet -- node /path/to/aevum-settle/demo/mcp.js
//
// Settings come from ../.env (or ENV_FILE): RPC_URL, CHAIN_ID, ROUTER_ADDRESS, OWNER_ADDRESS,
// AGENT_PRIVATE_KEY, MERCHANT_URL, and optionally AGENT_MAX_PRICE.
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { need, opt } from "./env.js";
import { TOOL_DEFS, walletTools } from "./sdk/tools.js";

const tools = walletTools({
  rpcUrl: opt("RPC_URL", "https://rpc-test.qday.info"),
  chainId: opt("CHAIN_ID", "44005"),
  agentKey: need("AGENT_PRIVATE_KEY"),
  owner: need("OWNER_ADDRESS"),
  router: need("ROUTER_ADDRESS"),
  merchantUrl: opt("MERCHANT_URL", "http://localhost:8787"),
  maxPrice: opt("AGENT_MAX_PRICE", undefined),
  waitSeconds: Number(opt("AGENT_WAIT_SECONDS", "120")),
  log: (m) => console.error(`[aevum-wallet] ${m}`), // stdout carries the protocol
});

const shapes = {
  wallet_status: {},
  list_products: {},
  buy: {
    product: z.string().describe(TOOL_DEFS.buy.schema.properties.product.description),
    max_price: z.number().positive().optional().describe(TOOL_DEFS.buy.schema.properties.max_price.description),
  },
  check_purchase: {
    product: z.string(),
    invoice: z.string().describe(TOOL_DEFS.check_purchase.schema.properties.invoice.description),
  },
};

const server = new McpServer({ name: "aevum-wallet", version: "1.0.0" });
for (const [name, def] of Object.entries(TOOL_DEFS)) {
  server.registerTool(
    name,
    { description: def.description, inputSchema: shapes[name], annotations: { readOnlyHint: name === "wallet_status" || name === "list_products" } },
    async (args) => {
      try {
        return { content: [{ type: "text", text: JSON.stringify(await tools[name](args ?? {}), null, 2) }] };
      } catch (e) {
        return { isError: true, content: [{ type: "text", text: e.message }] };
      }
    },
  );
}
await server.connect(new StdioServerTransport());
