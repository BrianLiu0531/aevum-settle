// Smoke test for the MCP server against a running stack (scripts/local-demo.sh --keep):
// a real MCP client spawns mcp.js over stdio, lists its tools and buys through it.
//   ENV_FILE=../.env.local node test/mcp-smoke.mjs
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
import { fileURLToPath } from "node:url";

const transport = new StdioClientTransport({
  command: process.execPath,
  args: [fileURLToPath(new URL("../mcp.js", import.meta.url))],
  env: { ...process.env },
  stderr: "inherit",
});
const client = new Client({ name: "smoke", version: "1.0.0" });
await client.connect(transport);
const call = async (name, args = {}) => {
  const r = await client.callTool({ name, arguments: args });
  const out = JSON.parse(r.content[0].text);
  console.log(`${name}(${JSON.stringify(args)}) ->`, JSON.stringify(out).slice(0, 400));
  return out;
};
const { tools } = await client.listTools();
console.log("tools:", tools.map((t) => t.name).join(", "));
await call("wallet_status");
await call("list_products");
const quote = await call("buy", { product: "quote" });
const capped = await call("buy", { product: "report", max_price: 5 });
const over = await call("buy", { product: "bulk" });
await client.close();
const ok = quote.status === "delivered" && capped.code === "OVER_TASK_BUDGET" && over.code === "OVER_PER_PAYMENT_CAP";
console.log(ok ? "MCP smoke test passed" : "MCP smoke test FAILED");
process.exit(ok ? 0 : 1);
