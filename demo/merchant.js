#!/usr/bin/env node
// Demo merchant: a paid HTTP API that AI agents buy from in tUSD8.
//
//   GET /api/<product>                         -> 402 Payment Required + invoice
//   GET /api/<product>  X-Payment-Invoice: id  -> 202 while the payment is not yet safe to act on,
//                                                 200 + the resource once the settler confirms it
//   POST /webhooks/settle                      <- signed payment.* events from the settler
//
// The merchant never reads the chain itself. It trusts its own settler, which tells it (by
// webhook, or by the invoice API when polled) that a payment reached the assurance its policy
// requires for that amount, and tells it again if a payment it announced is reorged away.
import { createServer } from "node:http";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ethers } from "ethers";
import { need, opt } from "./env.js";
import { formatAmount, invoiceKey, parseAmount } from "./sdk/agent.js";
import { requiredFor } from "./sdk/policy.js";
import { verifyWebhook, WebhookError } from "./sdk/webhook.js";

const MERCHANT = ethers.getAddress(need("MERCHANT_ADDRESS"));
const ROUTER = ethers.getAddress(need("ROUTER_ADDRESS"));
const CHAIN_ID = Number(opt("CHAIN_ID", "44005"));
const TOKEN = opt("TOKEN_ADDRESS", "0x456569838f1d43009A91ebA4263147Afd7233483");
const SETTLER_URL = opt("SETTLER_URL", "http://localhost:8080").replace(/\/$/, "");
const SECRET = opt("WEBHOOK_SECRET", "");
const PORT = Number(opt("MERCHANT_PORT", "8787"));
const INVOICE_TTL = Number(opt("INVOICE_TTL_SECONDS", "900"));

// Prices span the default policy's three tiers (trusted<=1, virtual<=100, verified) and one
// product priced above the demo mandate's per-payment cap.
const CATALOG = {
  quote: { title: "Live tUSD8 market quote", price: opt("PRICE_QUOTE", "0.05"), data: () => ({ pair: "tUSD8/USD", bid: 0.9998, ask: 1.0002, at: new Date().toISOString() }) },
  report: { title: "Post-quantum readiness report", price: opt("PRICE_REPORT", "20"), data: () => ({ title: "PQ readiness of 50 DeFi protocols", findings: 50, url: "https://example.com/report.pdf" }) },
  archive: { title: "Full historical dataset", price: opt("PRICE_ARCHIVE", "120"), data: () => ({ rows: 1_250_000, format: "parquet", url: "https://example.com/archive.parquet" }) },
  bulk: { title: "Enterprise data licence", price: opt("PRICE_BULK", "500"), data: () => ({ licence: "enterprise", seats: 100 }) },
};

const invoices = new Map(); // id -> invoice
const byKey = new Map(); // bytes32 invoice id -> id
const seenEvents = new Set();
const log = [];
let policy = "trusted<=1,virtual<=100,verified";

function note(line) {
  const entry = { at: new Date().toISOString(), line };
  log.unshift(entry);
  log.length = Math.min(log.length, 200);
  console.log(`[merchant] ${line}`);
}

function newInvoice(product) {
  const id = "INV-" + randomBytes(6).toString("hex");
  const raw = parseAmount(CATALOG[product].price);
  const inv = {
    id,
    invoiceId: invoiceKey(id),
    product,
    resource: `/api/${product}`,
    merchant: MERCHANT,
    amount: { raw: raw.toString(), display: formatAmount(raw) },
    token: TOKEN,
    router: ROUTER,
    chainId: CHAIN_ID,
    expiresAt: Math.floor(Date.now() / 1000) + INVOICE_TTL,
    requiredAssurance: requiredFor(raw, policy),
    status: "open", // open -> paid (seen, not yet safe) -> confirmed (deliver) [-> verified]; reverted -> open
    assurance: null,
    txHash: null,
    explorerUrl: null,
    history: [],
  };
  invoices.set(id, inv);
  byKey.set(inv.invoiceId, id);
  return inv;
}

const publicInvoice = ({ history, status, assurance, txHash, explorerUrl, ...inv }) => inv;

/** Applies a settler payment record (webhook event or invoice API answer) to an invoice. */
function applyPayment(inv, p, kind) {
  if (ethers.getAddress(p.merchant) !== MERCHANT) return;
  const amount = BigInt(typeof p.amount === "object" ? p.amount.raw : p.amount);
  if (kind === "payment.reverted") {
    inv.status = "open";
    inv.assurance = null;
    inv.history.push({ at: new Date().toISOString(), event: "reverted", txHash: p.txHash });
    note(`${inv.id}: payment ${p.txHash} was reorged away; access withdrawn until it is paid again`);
    return;
  }
  if (amount < BigInt(inv.amount.raw)) {
    inv.status = "underpaid";
    note(`${inv.id}: underpaid (${formatAmount(amount)} of ${inv.amount.display}); not delivering`);
    return;
  }
  const paidAt = Date.parse(p.blockTime) / 1000;
  if (paidAt > inv.expiresAt) {
    inv.status = "late";
    note(`${inv.id}: paid after the invoice expired; refund manually`);
    return;
  }
  inv.txHash = p.txHash;
  inv.explorerUrl = p.explorerUrl ?? inv.explorerUrl;
  inv.assurance = p.assurance;
  if (kind === "payment.verified") {
    inv.status = "verified";
  } else if (kind === "payment.confirmed" && inv.status !== "verified") {
    inv.status = "confirmed";
  } else if (inv.status === "open") {
    inv.status = "paid";
  }
  inv.history.push({ at: new Date().toISOString(), event: kind, assurance: p.assurance });
}

/** Pull mode: asks the settler about an invoice that no webhook has confirmed yet. */
async function poll(inv) {
  try {
    const res = await fetch(`${SETTLER_URL}/v1/invoices/${MERCHANT}/${inv.invoiceId}`);
    if (!res.ok) return;
    const body = await res.json();
    if (!body.paid) {
      if (inv.status === "paid") inv.status = "open"; // the payment we saw was reorged away
      return;
    }
    const p = body.payment;
    const kind = p.verifiedAt ? "payment.verified" : p.confirmed ? "payment.confirmed" : "seen";
    applyPayment(inv, p, kind);
  } catch {
    // settler unreachable: the webhook will still arrive
  }
}

function send(res, code, body, headers = {}) {
  res.writeHead(code, { "content-type": "application/json", "access-control-allow-origin": "*", ...headers });
  res.end(JSON.stringify(body, null, 2));
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
    events = verifyWebhook({
      secret: SECRET,
      timestamp: req.headers["x-settle-timestamp"],
      signature: req.headers["x-settle-signature"],
      body,
    });
  } catch (e) {
    note(`rejected webhook: ${e.message}`);
    return send(res, 401, { error: e.message });
  }
  for (const ev of events) {
    if (seenEvents.has(ev.eventId)) continue; // delivery is at least once
    seenEvents.add(ev.eventId);
    const inv = invoices.get(byKey.get(String(ev.invoiceId).toLowerCase()));
    if (!inv) {
      note(`${ev.type} for an invoice this merchant did not issue (${ev.invoiceId.slice(0, 10)}…), ignored`);
      continue;
    }
    note(`${ev.type} ${inv.id}: ${formatAmount(ev.amount)} tUSD8 at ${ev.assurance} assurance (needs ${ev.required})`);
    applyPayment(inv, ev, ev.type);
  }
  send(res, 200, { ok: true });
}

async function handleProduct(req, res, product) {
  const item = CATALOG[product];
  const id = req.headers["x-payment-invoice"];
  const inv = id && invoices.get(String(id));
  if (!inv || inv.product !== product) {
    const fresh = newInvoice(product);
    note(`402 ${fresh.id} for ${product}: ${fresh.amount.display} tUSD8, needs ${fresh.requiredAssurance}`);
    return send(res, 402, { error: "payment_required", invoice: publicInvoice(fresh) }, { "x-payment-required": "aevum-settle" });
  }
  if (inv.status !== "confirmed" && inv.status !== "verified") await poll(inv);
  switch (inv.status) {
    case "confirmed":
    case "verified":
      return send(res, 200, { product, title: item.title, data: item.data(), receipt: { invoice: inv.id, txHash: inv.txHash, assurance: inv.assurance, explorerUrl: inv.explorerUrl } });
    case "paid":
      return send(res, 202, { status: "pending", invoice: inv.id, assurance: inv.assurance, required: inv.requiredAssurance }, { "retry-after": "2" });
    case "open":
      if (inv.expiresAt * 1000 < Date.now()) {
        return send(res, 402, { error: "invoice_expired", invoice: publicInvoice(newInvoice(product)) });
      }
      return send(res, 202, { status: "awaiting_payment", invoice: inv.id, assurance: null, required: inv.requiredAssurance }, { "retry-after": "2" });
    default:
      return send(res, 409, { error: inv.status, invoice: inv.id });
  }
}

const page = readFileSync(fileURLToPath(new URL("./public/merchant.html", import.meta.url)));

const server = createServer(async (req, res) => {
  const url = new URL(req.url, "http://localhost");
  try {
    if (req.method === "POST" && url.pathname === "/webhooks/settle") return await handleWebhook(req, res);
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
        products: Object.entries(CATALOG).map(([k, v]) => ({ product: k, title: v.title, price: v.price, path: `/api/${k}`, requiredAssurance: requiredFor(parseAmount(v.price), policy) })),
      });
    }
    if (url.pathname === "/api/invoices") {
      return send(res, 200, { invoices: [...invoices.values()].reverse().slice(0, 100), log: log.slice(0, 60) });
    }
    const m = url.pathname.match(/^\/api\/([a-z]+)$/);
    if (m && CATALOG[m[1]]) return await handleProduct(req, res, m[1]);
    send(res, 404, { error: "not found" });
  } catch (e) {
    console.error(e);
    send(res, 500, { error: "internal error" });
  }
});

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  if (SECRET && SECRET.length < 16) {
    console.error("WEBHOOK_SECRET must be at least 16 characters");
    process.exit(2);
  }
  try {
    const st = await (await fetch(`${SETTLER_URL}/v1/status`)).json();
    policy = st.policy;
  } catch {
    console.warn(`[merchant] settler at ${SETTLER_URL} not reachable yet; assuming policy ${policy}`);
  }
  server.listen(PORT, () => {
    note(`listening on :${PORT} as ${MERCHANT}; policy ${policy}; webhooks ${SECRET ? "verified with WEBHOOK_SECRET" : "DISABLED (no secret), polling the settler"}`);
  });
}
