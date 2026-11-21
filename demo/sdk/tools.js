// The agent's wallet as tools for an LLM: shared by the MCP server (mcp.js) and the Claude agent
// (ai-agent.js). Each tool takes plain JSON and returns plain JSON.
//
// Safety is layered so that a confused or prompt-injected model cannot overspend:
//   - the tools only buy from the one merchant they are configured with (no arbitrary URLs);
//   - each purchase can carry its own price ceiling, under the agent's AGENT_MAX_PRICE;
//   - the SDK checks the owner's on-chain mandate before signing, and the contract enforces it again.
import { Wallet, JsonRpcProvider } from "ethers";
import { AgentPayer, PaymentRefused, formatAmount, parseAmount } from "./agent.js";
import { HEADERS, decodeHeader } from "./x402.js";

const PRODUCT = /^[a-z]{1,32}$/;

export function walletTools({ rpcUrl, chainId, agentKey, owner, router, merchantUrl, maxPrice, waitSeconds = 120, log = () => {} }) {
  const provider = new JsonRpcProvider(rpcUrl, Number(chainId), { staticNetwork: true, pollingInterval: 500 });
  const payer = new AgentPayer({ wallet: new Wallet(agentKey, provider), owner, router, chainId, maxPrice, log });
  const base = merchantUrl.replace(/\/$/, "");
  const url = (product) => {
    if (!PRODUCT.test(product)) throw new Error("product must be a catalog name such as \"quote\"");
    return `${base}/api/${product}`;
  };

  async function outcome(response, extra = {}) {
    const settlement = decodeHeader(response.headers.get(HEADERS.response));
    if (response.status === 200) {
      const body = await response.json();
      return { status: "delivered", data: body.data, receipt: body.receipt, ...extra };
    }
    if (response.status === 402 && settlement?.errorReason === "settlement_pending") {
      return { status: "pending", reason: settlement.errorMessage, hint: "the payment is made; call check_purchase later to collect it", ...extra };
    }
    return { status: "failed", httpStatus: response.status, reason: settlement?.errorReason ?? (await response.text()).slice(0, 300), ...extra };
  }

  return {
    async wallet_status() {
      const [m, b] = await Promise.all([payer.mandate(), payer.balances()]);
      return {
        agent: payer.wallet.address,
        owner: payer.owner,
        mandate: m.expiresAt === 0
          ? { active: false, note: "no mandate: the owner has not granted one, or revoked it" }
          : {
              active: m.active,
              perPaymentCap: formatAmount(m.perPaymentCap),
              dailyCap: formatAmount(m.dailyCap),
              remainingToday: formatAmount(m.remainingToday),
              expiresAt: new Date(m.expiresAt * 1000).toISOString(),
              merchantAllowlist: m.merchantLocked,
            },
        ownerBalance: formatAmount(b.ownerBalance),
        agentOwnLimitPerPurchase: payer.maxPrice != null ? formatAmount(payer.maxPrice) : null,
        currency: "tUSD8",
      };
    },

    async list_products() {
      const res = await fetch(`${base}/api/catalog`);
      if (!res.ok) throw new Error(`merchant catalog answered ${res.status}`);
      const c = await res.json();
      return {
        merchant: c.merchant,
        note: "requiredAssurance is how final the payment must be before the merchant delivers: trusted ~1s, virtual ~10 min, verified ~90 min on QDay",
        products: c.products.map(({ product, title, price, requiredAssurance }) => ({ product, title, price, requiredAssurance })),
      };
    },

    async buy({ product, max_price }) {
      const target = url(product);
      if (max_price != null) {
        // Look at the price before paying, so the model's own ceiling is honoured too
        const probe = await fetch(target);
        const required = decodeHeader(probe.headers.get(HEADERS.required));
        const offer = required?.accepts?.find((a) => a.scheme === "aevum-mandate");
        if (probe.status === 200) return outcome(probe);
        if (offer && BigInt(offer.amount) > parseAmount(String(max_price))) {
          return { status: "refused", code: "OVER_TASK_BUDGET", reason: `price ${formatAmount(offer.amount)} is above max_price ${max_price}` };
        }
      }
      try {
        const { response, paid, invoice } = await payer.fetch(target, { waitMs: waitSeconds * 1000 });
        return outcome(response, invoice ? { invoice: invoice.id, paid: paid && { amount: formatAmount(paid.amount), txHash: paid.txHash } } : {});
      } catch (e) {
        if (e instanceof PaymentRefused) return { status: "refused", code: e.code, reason: e.message };
        throw e;
      }
    },

    async check_purchase({ product, invoice }) {
      if (!/^INV-[0-9a-f]+$/.test(invoice)) throw new Error("invoice must look like INV-…");
      const response = await payer.resume(url(product), { id: invoice }, { waitMs: Math.min(waitSeconds, 60) * 1000 });
      return outcome(response, { invoice });
    },
  };
}

/** JSON Schemas for the tools, shared by the MCP server and the Claude agent. */
export const TOOL_DEFS = {
  wallet_status: {
    description: "Show this agent's spending mandate (per-payment cap, daily cap and what is left today, expiry), and the owner's tUSD8 balance. Call it before planning purchases.",
    schema: { type: "object", properties: {}, additionalProperties: false },
  },
  list_products: {
    description: "List what the merchant sells, with prices in tUSD8 and how final a payment must be before each is delivered.",
    schema: { type: "object", properties: {}, additionalProperties: false },
  },
  buy: {
    description:
      "Buy one product from the merchant, paying in tUSD8 from the owner's wallet under the mandate. Returns status delivered (with the data and an on-chain receipt), pending (paid, not yet final enough: use check_purchase later), or refused (with a code such as OVER_DAILY_CAP or OVER_PER_PAYMENT_CAP: no money moved). Never retry a refused purchase unchanged.",
    schema: {
      type: "object",
      properties: {
        product: { type: "string", description: "Product name from list_products, e.g. \"quote\"" },
        max_price: { type: "number", description: "Optional: refuse if the price is above this many tUSD8" },
      },
      required: ["product"],
      additionalProperties: false,
    },
  },
  check_purchase: {
    description: "Collect a purchase that buy reported as pending. Does not pay again.",
    schema: {
      type: "object",
      properties: {
        product: { type: "string" },
        invoice: { type: "string", description: "Invoice id from buy, e.g. INV-1a2b3c4d5e6f" },
      },
      required: ["product", "invoice"],
      additionalProperties: false,
    },
  },
};
