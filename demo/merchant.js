#!/usr/bin/env node
// Demo merchant: a paid HTTP API that AI agents (and people) buy from in tUSD8, speaking x402 v2.
//
//   GET /api/<product>                          -> 402 + PAYMENT-REQUIRED (two ways to pay, below)
//   GET /api/<product>  PAYMENT-SIGNATURE: ...  -> 402 + PAYMENT-RESPONSE {settlement_pending} while
//                                                  the payment lacks the assurance its amount needs,
//                                                  200 + PAYMENT-RESPONSE {success} + the resource after
//   POST /webhooks/settle                       <- signed payment.* events from the settler
//   POST /api/invoices/<id>/refund              -> refund an underpaid, late or overpaid invoice
//
// Ways to pay (accepts[]):
//   exact          standard x402: the payer signs an EIP-3009 authorization (tUSD8 supports it);
//                  the merchant settles it through AgentPayRouter.settleAuthorization, paying gas
//   aevum-mandate  an agent pays itself with AgentPayRouter.payAsAgent under its owner's mandate
//
// The merchant does not decide on its own when a payment is safe. Its settler tells it (webhook,
// or the invoice API when polled) that a payment reached the assurance its policy requires for
// that amount, and tells it again if a payment it announced is reorged away.
import { createServer } from "node:http";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { Contract, JsonRpcProvider, NonceManager, Wallet, ethers } from "ethers";
import { need, opt } from "./env.js";
import { decodeRevert, formatAmount, invoiceKey, parseAmount } from "./sdk/agent.js";
import { ROUTER_ABI, TOKEN_ABI } from "./sdk/abi.js";
import { requiredFor } from "./sdk/policy.js";
import { verifyWebhook, WebhookError } from "./sdk/webhook.js";
import { HEADERS, X402_VERSION, caip2, decodeHeader, encodeHeader, verifyExact } from "./sdk/x402.js";
import { MerchantStore } from "./store.js";

const MERCHANT = ethers.getAddress(need("MERCHANT_ADDRESS"));
const ROUTER = ethers.getAddress(need("ROUTER_ADDRESS"));
const CHAIN_ID = Number(opt("CHAIN_ID", "44005"));
const NETWORK = caip2(CHAIN_ID);
const TOKEN = ethers.getAddress(opt("TOKEN_ADDRESS", "0x456569838f1d43009A91ebA4263147Afd7233483"));
const RPC_URL = opt("RPC_URL", "https://rpc-test.qday.info");
const SETTLER_URL = opt("SETTLER_URL", "http://localhost:8080").replace(/\/$/, "");
const SECRET = opt("WEBHOOK_SECRET", "");
const PORT = Number(opt("MERCHANT_PORT", "8787"));
const PUBLIC_URL = opt("MERCHANT_PUBLIC_URL", `http://localhost:${PORT}`).replace(/\/$/, "");
const INVOICE_TTL = Number(opt("INVOICE_TTL_SECONDS", "900"));
const AUTO_REFUND = opt("AUTO_REFUND", "true") === "true";
const ADMIN_TOKEN = opt("MERCHANT_ADMIN_TOKEN", "");
const EXPLORER = opt("EXPLORER_URL", CHAIN_ID === 44005 ? "https://explorer-test.qday.info" : "");

// Prices span the default policy's three tiers (trusted<=1, virtual<=100, verified) and one
// product priced above the demo mandate's per-payment cap.
const CATALOG = {
  quote: { title: "Live tUSD8 market quote", price: opt("PRICE_QUOTE", "0.05"), data: () => ({ pair: "tUSD8/USD", bid: 0.9998, ask: 1.0002, at: new Date().toISOString() }) },
  report: { title: "Post-quantum readiness report", price: opt("PRICE_REPORT", "20"), data: () => ({ title: "PQ readiness of 50 DeFi protocols", findings: 50, url: "https://example.com/report.pdf" }) },
  archive: { title: "Full historical dataset", price: opt("PRICE_ARCHIVE", "120"), data: () => ({ rows: 1_250_000, format: "parquet", url: "https://example.com/archive.parquet" }) },
  bulk: { title: "Enterprise data licence", price: opt("PRICE_BULK", "500"), data: () => ({ licence: "enterprise", seats: 100 }) },
};

const store = new MerchantStore(opt("MERCHANT_DB", fileURLToPath(new URL("./data/merchant.db", import.meta.url))));
let policy = "trusted<=1,virtual<=100,verified";

// The merchant's key settles exact payments and sends refunds (both cost it gas). Without it the
// merchant still sells through aevum-mandate, and flags refunds for manual handling.
const provider = new JsonRpcProvider(RPC_URL, CHAIN_ID, { staticNetwork: true, pollingInterval: 500 });
const merchantKey = opt("MERCHANT_PRIVATE_KEY", "");
const signer = merchantKey ? new NonceManager(new Wallet(merchantKey, provider)) : null;
if (signer && (await signer.getAddress()) !== MERCHANT) {
  console.error("MERCHANT_PRIVATE_KEY does not belong to MERCHANT_ADDRESS");
  process.exit(2);
}
const router = new Contract(ROUTER, ROUTER_ABI, signer ?? provider);
const token = new Contract(TOKEN, TOKEN_ABI, signer ?? provider);

function note(line) {
  store.note(line);
  console.log(`[merchant] ${line}`);
}

const txLink = (h) => (EXPLORER && h ? `${EXPLORER}/tx/${h}` : null);

// ------------------------------------------------------------------ invoices and x402 terms

function newInvoice(product) {
  const id = "INV-" + randomBytes(6).toString("hex");
  const raw = parseAmount(CATALOG[product].price);
  const now = Math.floor(Date.now() / 1000);
  return store.save({
    id,
    invoiceId: invoiceKey(id),
    product,
    resource: `/api/${product}`,
    merchant: MERCHANT,
    amount: { raw: raw.toString(), display: formatAmount(raw) },
    token: TOKEN,
    router: ROUTER,
    chainId: CHAIN_ID,
    createdAt: now,
    expiresAt: now + INVOICE_TTL,
    requiredAssurance: requiredFor(raw, policy),
    status: "open", // open -> paid -> confirmed [-> verified]; underpaid | late -> refunded; reverted -> open
    scheme: null,
    assurance: null,
    txHash: null,
    payer: null,
    paid: null,
    refund: null,
    authNonce: null,
    history: [],
  });
}

/** The invoice as the 402 body's legacy `invoice` field (what SDK versions before x402 read). */
const terms = ({ id, invoiceId, resource, merchant, amount, token, router, chainId, expiresAt, requiredAssurance }) => ({
  id, invoiceId, resource, merchant, amount, token, router, chainId, expiresAt, requiredAssurance,
});

function paymentRequired(inv, error = "payment_required") {
  const ttl = Math.max(30, inv.expiresAt - Math.floor(Date.now() / 1000));
  const base = { network: NETWORK, amount: inv.amount.raw, asset: TOKEN, payTo: MERCHANT, maxTimeoutSeconds: ttl };
  const accepts = [
    {
      scheme: "aevum-mandate",
      ...base,
      extra: { paymentFlow: "push", invoice: inv.id, invoiceId: inv.invoiceId, router: ROUTER, requiredAssurance: inv.requiredAssurance, settler: SETTLER_URL },
    },
  ];
  if (signer) accepts.push({ scheme: "exact", ...base, extra: { name: "tUSD8", version: "2", invoice: inv.id, requiredAssurance: inv.requiredAssurance } });
  return {
    x402Version: X402_VERSION,
    error,
    resource: { url: PUBLIC_URL + inv.resource, description: CATALOG[inv.product].title, mimeType: "application/json", serviceName: "Aevum Settle demo" },
    accepts,
  };
}

// ------------------------------------------------------------------ settlement state

/** Applies a settler payment record (webhook event or invoice API answer) to an invoice. */
async function applyPayment(inv, p, kind) {
  if (ethers.getAddress(p.merchant) !== MERCHANT) return;
  const amount = BigInt(typeof p.amount === "object" ? p.amount.raw : p.amount);
  const at = new Date().toISOString();
  if (kind === "payment.reverted") {
    Object.assign(inv, { status: "open", assurance: null, txHash: null, payer: null, paid: null, scheme: null, authNonce: null });
    inv.history.push({ at, event: "reverted", txHash: p.txHash });
    note(`${inv.id}: payment ${p.txHash.slice(0, 10)}… was reorged away; access withdrawn until it is paid again`);
    return store.save(inv);
  }
  Object.assign(inv, { txHash: p.txHash, payer: p.payer, assurance: p.assurance, paid: { raw: amount.toString(), display: formatAmount(amount) } });
  inv.scheme ??= p.agent ? "aevum-mandate" : "direct";
  const final = kind === "payment.confirmed" || kind === "payment.verified";
  if (!final) {
    if (inv.status === "open") inv.status = "paid";
    return store.save(inv);
  }
  inv.history.push({ at, event: kind, assurance: p.assurance });
  if (["refunded", "underpaid", "late"].includes(inv.status)) return store.save(inv);

  const price = BigInt(inv.amount.raw);
  const paidAt = Date.parse(p.blockTime) / 1000;
  if (amount < price) {
    inv.status = "underpaid";
    note(`${inv.id}: underpaid (${formatAmount(amount)} of ${inv.amount.display} tUSD8); not delivering`);
  } else if (paidAt > inv.expiresAt) {
    inv.status = "late";
    note(`${inv.id}: paid after the invoice expired; not delivering`);
  } else {
    inv.status = kind === "payment.verified" || inv.status === "verified" ? "verified" : "confirmed";
    if (amount > price && !inv.refund) {
      store.save(inv);
      note(`${inv.id}: overpaid by ${formatAmount(amount - price)} tUSD8`);
      if (AUTO_REFUND) await refund(inv, amount - price, "overpayment");
    }
    return store.save(inv);
  }
  store.save(inv);
  if (AUTO_REFUND) await refund(inv, amount, inv.status);
  return inv;
}

/** Sends tUSD8 back to the payer. Only ever runs on a payment the settler has confirmed. */
async function refund(inv, amount, reason) {
  if (inv.refund) return inv.refund;
  if (!signer) {
    note(`${inv.id}: refund of ${formatAmount(amount)} tUSD8 needed (${reason}); set MERCHANT_PRIVATE_KEY to send it`);
    return null;
  }
  inv.refund = { status: "sending", amount: { raw: amount.toString(), display: formatAmount(amount) }, reason, to: inv.payer };
  store.save(inv);
  try {
    const tx = await token.transfer(inv.payer, amount);
    await tx.wait();
    inv.refund = { ...inv.refund, status: "sent", txHash: tx.hash, explorerUrl: txLink(tx.hash), at: new Date().toISOString() };
    if (reason !== "overpayment") inv.status = "refunded";
    note(`${inv.id}: refunded ${formatAmount(amount)} tUSD8 to ${inv.payer} (${reason}): ${tx.hash}`);
  } catch (e) {
    inv.refund = { ...inv.refund, status: "failed", error: e.shortMessage ?? e.message };
    note(`${inv.id}: refund failed: ${inv.refund.error}`);
  }
  store.save(inv);
  return inv.refund;
}

/** Pull mode: asks the settler about an invoice that no webhook has confirmed yet. */
async function poll(inv) {
  try {
    const res = await fetch(`${SETTLER_URL}/v1/invoices/${MERCHANT}/${inv.invoiceId}`);
    if (!res.ok) return;
    const body = await res.json();
    if (!body.paid) {
      if (inv.status === "paid" && inv.scheme !== "exact") {
        inv.status = "open"; // the payment we saw was reorged away
        store.save(inv);
      }
      return;
    }
    const p = body.payment;
    await applyPayment(inv, p, p.verifiedAt ? "payment.verified" : p.confirmed ? "payment.confirmed" : "seen");
  } catch {
    // settler unreachable: the webhook will still arrive
  }
}

/** Settles an x402 exact payment on chain: AgentPayRouter.settleAuthorization from the merchant. */
async function settleExact(inv, payload) {
  const requirements = paymentRequired(inv).accepts.find((a) => a.scheme === "exact");
  const auth = verifyExact(payload, requirements);
  if (inv.authNonce) {
    if (inv.authNonce !== auth.nonce) throw Object.assign(new Error("invoice is already being settled with another authorization"), { reason: "invalid_payload" });
    return; // a retry of the request we already settled
  }
  inv.authNonce = auth.nonce;
  inv.scheme = "exact";
  inv.payer = ethers.getAddress(auth.from);
  store.save(inv);
  try {
    const tx = await router.settleAuthorization(inv.invoiceId, auth.from, auth.value, auth.validAfter, auth.validBefore, auth.nonce, auth.v, auth.r, auth.s);
    inv.txHash = tx.hash;
    inv.status = "paid";
    store.save(inv);
    note(`${inv.id}: settling x402 exact payment from ${auth.from}: ${tx.hash}`);
    const receipt = await tx.wait();
    if (receipt.status !== 1) throw new Error(`settlement ${tx.hash} reverted`);
  } catch (e) {
    Object.assign(inv, { authNonce: null, scheme: null, payer: null, txHash: null, status: "open" });
    store.save(inv);
    const decoded = decodeRevert(e);
    throw Object.assign(new Error(decoded?.message ?? e.shortMessage ?? e.message), { reason: "unexpected_settle_error" });
  }
}

// ------------------------------------------------------------------ HTTP

const CORS = {
  "access-control-allow-origin": "*",
  "access-control-allow-headers": "payment-signature, x-payment-invoice, content-type, authorization",
  "access-control-expose-headers": "payment-required, payment-response, retry-after",
};

function send(res, code, body, headers = {}) {
  res.writeHead(code, { "content-type": "application/json", ...CORS, ...headers });
  res.end(JSON.stringify(body, null, 2));
}

function settlement(inv, extra = {}) {
  return { success: false, transaction: inv.txHash ?? "", network: NETWORK, payer: inv.payer ?? undefined, amount: inv.paid?.raw, ...extra };
}

async function readBody(req, limit = 1 << 20) {
  const chunks = [];
  let size = 0;
  for await (const c of req) {
    size += c.length;
    if (size > limit) throw new WebhookError("body too large");
    chunks.push(c);
  }
  return Buffer.concat(chunks);
}

async function handleWebhook(req, res) {
  if (!SECRET) return send(res, 404, { error: "webhooks are disabled: set WEBHOOK_SECRET" });
  const body = await readBody(req);
  let events;
  try {
    events = verifyWebhook({ secret: SECRET, timestamp: req.headers["x-settle-timestamp"], signature: req.headers["x-settle-signature"], body });
  } catch (e) {
    note(`rejected webhook: ${e.message}`);
    return send(res, 401, { error: e.message });
  }
  for (const ev of events) {
    if (!store.firstTime(ev.eventId)) continue; // delivery is at least once
    const inv = store.byKey(ev.invoiceId);
    if (!inv) {
      note(`${ev.type} for an invoice this merchant did not issue (${String(ev.invoiceId).slice(0, 10)}…), ignored`);
      continue;
    }
    note(`${ev.type} ${inv.id}: ${formatAmount(ev.amount)} tUSD8 at ${ev.assurance} assurance (needs ${ev.required})`);
    await applyPayment(inv, ev, ev.type);
  }
  send(res, 200, { ok: true });
}

function issue(res, product, error) {
  const fresh = newInvoice(product);
  note(`402 ${fresh.id} for ${product}: ${fresh.amount.display} tUSD8, needs ${fresh.requiredAssurance}`);
  const required = paymentRequired(fresh, error);
  return send(res, 402, { ...required, invoice: terms(fresh) }, { [HEADERS.required]: encodeHeader(required) });
}

async function handleProduct(req, res, product) {
  const item = CATALOG[product];
  const x402 = decodeHeader(req.headers[HEADERS.signature]);
  const invoiceRef = x402?.accepted?.extra?.invoice ?? req.headers["x-payment-invoice"];
  const inv = invoiceRef && store.get(invoiceRef);
  if (!inv || inv.product !== product) return issue(res, product, invoiceRef ? "unknown_invoice" : "payment_required");

  if (x402) {
    if (x402.x402Version !== X402_VERSION || x402.accepted?.network !== NETWORK) {
      return send(res, 400, { error: "invalid_x402_version_or_network" });
    }
    if (x402.accepted.scheme === "exact") {
      try {
        await settleExact(inv, x402);
      } catch (e) {
        return send(res, 402, { error: e.reason, message: e.message }, { [HEADERS.response]: encodeHeader(settlement(inv, { errorReason: e.reason, errorMessage: e.message })) });
      }
    } else if (x402.accepted.scheme === "aevum-mandate") {
      if (!inv.txHash && ethers.isHexString(x402.payload?.txHash, 32)) {
        inv.txHash = x402.payload.txHash;
        store.save(inv);
      }
    } else {
      return send(res, 402, { error: "unsupported_scheme" });
    }
  }

  if (inv.status !== "confirmed" && inv.status !== "verified") await poll(inv);
  const fresh = store.get(inv.id);
  const pending = (status) => {
    const body = { status, invoice: fresh.id, assurance: fresh.assurance, required: fresh.requiredAssurance, transaction: fresh.txHash };
    if (!x402) return send(res, 202, body, { "retry-after": "2" });
    return send(res, 402, body, { "retry-after": "2", [HEADERS.response]: encodeHeader(settlement(fresh, { errorReason: "settlement_pending", errorMessage: `payment is ${fresh.assurance ?? "not indexed yet"}; this amount needs ${fresh.requiredAssurance}` })) });
  };
  switch (fresh.status) {
    case "confirmed":
    case "verified": {
      const receipt = { invoice: fresh.id, scheme: fresh.scheme, txHash: fresh.txHash, assurance: fresh.assurance, explorerUrl: txLink(fresh.txHash) };
      return send(res, 200, { product, title: item.title, data: item.data(), receipt }, { [HEADERS.response]: encodeHeader(settlement(fresh, { success: true })) });
    }
    case "paid":
      return pending("pending");
    case "open":
      if (fresh.expiresAt * 1000 < Date.now()) return issue(res, product, "invoice_expired");
      return pending("awaiting_payment");
    default:
      return send(res, 409, { error: fresh.status, invoice: fresh.id, refund: fresh.refund }, { [HEADERS.response]: encodeHeader(settlement(fresh, { errorReason: fresh.status })) });
  }
}

async function handleRefund(req, res, id) {
  if (!ADMIN_TOKEN || req.headers.authorization !== `Bearer ${ADMIN_TOKEN}`) return send(res, 401, { error: "set MERCHANT_ADMIN_TOKEN and send it as a bearer token" });
  const inv = store.get(id);
  if (!inv) return send(res, 404, { error: "no such invoice" });
  if (!inv.paid || !["underpaid", "late", "confirmed", "verified"].includes(inv.status)) return send(res, 409, { error: `nothing to refund (status ${inv.status})` });
  const excess = BigInt(inv.paid.raw) - BigInt(inv.amount.raw);
  const full = inv.status === "underpaid" || inv.status === "late";
  if (!full && excess <= 0n) return send(res, 409, { error: "invoice was paid exactly; nothing to refund" });
  const r = await refund(inv, full ? BigInt(inv.paid.raw) : excess, full ? inv.status : "overpayment");
  send(res, r?.status === "sent" ? 200 : 502, { invoice: store.get(id) });
}

const page = readFileSync(fileURLToPath(new URL("./public/merchant.html", import.meta.url)));

const server = createServer(async (req, res) => {
  const url = new URL(req.url, "http://localhost");
  try {
    if (req.method === "OPTIONS") return send(res, 204, {});
    if (req.method === "POST" && url.pathname === "/webhooks/settle") return await handleWebhook(req, res);
    const refundMatch = url.pathname.match(/^\/api\/invoices\/(INV-[0-9a-f]+)\/refund$/);
    if (req.method === "POST" && refundMatch) return await handleRefund(req, res, refundMatch[1]);
    if (req.method !== "GET") return send(res, 405, { error: "method not allowed" });
    if (url.pathname === "/") {
      res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
      return res.end(page);
    }
    if (url.pathname === "/healthz") return send(res, 200, { ok: true });
    if (url.pathname === "/api/catalog") {
      return send(res, 200, {
        merchant: MERCHANT,
        policy,
        settler: SETTLER_URL,
        network: NETWORK,
        schemes: signer ? ["aevum-mandate", "exact"] : ["aevum-mandate"],
        products: Object.entries(CATALOG).map(([k, v]) => ({ product: k, title: v.title, price: v.price, path: `/api/${k}`, requiredAssurance: requiredFor(parseAmount(v.price), policy) })),
      });
    }
    if (url.pathname === "/api/invoices") {
      return send(res, 200, { invoices: store.list(100).map(({ authNonce, ...i }) => ({ ...i, explorerUrl: txLink(i.txHash) })), log: store.logs(60) });
    }
    const invMatch = url.pathname.match(/^\/api\/invoices\/(INV-[0-9a-f]+)$/);
    if (invMatch) {
      const inv = store.get(invMatch[1]);
      return inv ? send(res, 200, { ...inv, authNonce: undefined, explorerUrl: txLink(inv.txHash) }) : send(res, 404, { error: "no such invoice" });
    }
    const m = url.pathname.match(/^\/api\/([a-z]+)$/);
    if (m && CATALOG[m[1]]) return await handleProduct(req, res, m[1]);
    send(res, 404, { error: "not found" });
  } catch (e) {
    console.error(e);
    send(res, 500, { error: "internal error" });
  }
});

if (SECRET && SECRET.length < 16) {
  console.error("WEBHOOK_SECRET must be at least 16 characters");
  process.exit(2);
}
try {
  policy = (await (await fetch(`${SETTLER_URL}/v1/status`)).json()).policy;
} catch {
  console.warn(`[merchant] settler at ${SETTLER_URL} not reachable yet; assuming policy ${policy}`);
}
server.listen(PORT, () => {
  note(
    `listening on :${PORT} as ${MERCHANT}; policy ${policy}; schemes ${signer ? "aevum-mandate + exact" : "aevum-mandate"}; ` +
      `webhooks ${SECRET ? "verified with WEBHOOK_SECRET" : "DISABLED (no secret), polling the settler"}; auto refund ${AUTO_REFUND ? "on" : "off"}`,
  );
});
