// Agent side: pay a merchant's HTTP 402 invoice in tUSD8 under the owner's mandate, then wait
// for the merchant to release the resource.
//
// The agent checks every rule the contract enforces before it signs anything, so a refused
// payment costs no gas and comes back as a PaymentRefused with a machine-readable code. The
// contract enforces the same rules again; the local check is a courtesy, not the security.
import { Contract, Interface, ethers } from "ethers";
import { ROUTER_ABI, TOKEN_ABI } from "./abi.js";
import { HEADERS, X402_VERSION, decodeHeader, encodeHeader, signExact } from "./x402.js";

export const DECIMALS = 6;
export const formatAmount = (raw) => ethers.formatUnits(raw, DECIMALS);
export const parseAmount = (s) => ethers.parseUnits(String(s), DECIMALS);
/** Invoice ids: a 32-byte hex value is used as is, anything else is hashed (same as the settler API). */
export const invoiceKey = (id) => (ethers.isHexString(id, 32) ? id.toLowerCase() : ethers.id(id));

export class PaymentRefused extends Error {
  constructor(code, message, details = {}) {
    super(message);
    this.code = code;
    this.details = details;
  }
}

const routerInterface = new Interface(ROUTER_ABI);
const CONTRACT_ERRORS = {
  NoMandate: "NO_MANDATE",
  MandateExpired: "MANDATE_EXPIRED",
  MerchantNotAllowed: "MERCHANT_NOT_ALLOWED",
  OverPerPaymentCap: "OVER_PER_PAYMENT_CAP",
  OverDailyCap: "OVER_DAILY_CAP",
  InvoiceAlreadyPaid: "ALREADY_PAID",
  ZeroAmount: "INVALID_INVOICE",
  ZeroAddress: "INVALID_INVOICE",
};

/** Turns a contract revert into a PaymentRefused, or returns undefined if it is not one. */
export function decodeRevert(err) {
  const data = err?.data ?? err?.info?.error?.data ?? err?.error?.data;
  if (typeof data !== "string" || data.length < 10) return undefined;
  let parsed;
  try {
    parsed = routerInterface.parseError(data);
  } catch {
    return undefined;
  }
  if (!parsed) return undefined;
  const args = parsed.args.map((a) => (typeof a === "bigint" ? a.toString() : a));
  return new PaymentRefused(CONTRACT_ERRORS[parsed.name] ?? "REVERTED", `contract refused: ${parsed.name}(${args.join(", ")})`, {
    error: parsed.name,
    args,
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export class AgentPayer {
  /**
   * @param {object} o
   * @param {ethers.Wallet} o.wallet   the agent's key, connected to a provider (it pays gas only)
   * @param {string} o.owner           the wallet whose tUSD8 the mandate spends
   * @param {string} o.router          AgentPayRouter address the agent trusts
   * @param {bigint|number} o.chainId  chain the agent pays on
   * @param {string} [o.maxPrice]      the agent's own per-purchase ceiling, below the mandate (tUSD8)
   * @param {(msg: string) => void} [o.log]
   */
  constructor({ wallet, owner, router, chainId, maxPrice, log = () => {} }) {
    this.wallet = wallet;
    this.owner = ethers.getAddress(owner);
    this.routerAddress = ethers.getAddress(router);
    this.chainId = BigInt(chainId);
    this.maxPrice = maxPrice != null ? parseAmount(maxPrice) : undefined;
    this.router = new Contract(this.routerAddress, ROUTER_ABI, wallet);
    this.log = log;
  }

  async mandate() {
    const m = await this.router.mandate(this.owner, this.wallet.address);
    return {
      perPaymentCap: m.perPaymentCap,
      dailyCap: m.dailyCap,
      remainingToday: m.remainingToday,
      expiresAt: Number(m.expiresAt),
      active: m.active,
      merchantLocked: m.merchantLocked,
    };
  }

  async balances() {
    const token = new Contract(await this.router.token(), TOKEN_ABI, this.wallet.provider);
    const [owner, allowance, gas] = await Promise.all([
      token.balanceOf(this.owner),
      token.allowance(this.owner, this.routerAddress),
      this.wallet.provider.getBalance(this.wallet.address),
    ]);
    return { ownerBalance: owner, allowance, agentGas: gas };
  }

  /** Checks an invoice against the agent's limits and the mandate. Throws PaymentRefused. */
  async check(invoice) {
    const amount = BigInt(invoice.amount.raw);
    const merchant = ethers.getAddress(invoice.merchant);
    const id = invoiceKey(invoice.invoiceId);
    const refuse = (code, msg, details) => {
      throw new PaymentRefused(code, msg, details);
    };

    if (BigInt(invoice.chainId) !== this.chainId) refuse("WRONG_CHAIN", `invoice is for chain ${invoice.chainId}`);
    if (ethers.getAddress(invoice.router) !== this.routerAddress) refuse("WRONG_ROUTER", `invoice names router ${invoice.router}`);
    if (amount <= 0n) refuse("INVALID_INVOICE", "invoice amount must be positive");
    if (invoice.expiresAt && invoice.expiresAt * 1000 < Date.now()) refuse("INVOICE_EXPIRED", "invoice has expired");
    if (this.maxPrice != null && amount > this.maxPrice) {
      refuse("OVER_AGENT_BUDGET", `${formatAmount(amount)} tUSD8 is above this agent's own limit of ${formatAmount(this.maxPrice)}`);
    }

    const [m, allowed, paid] = await Promise.all([
      this.mandate(),
      this.router.isMerchantAllowed(this.owner, this.wallet.address, merchant),
      this.router.invoicePaid(merchant, id),
    ]);
    if (paid) refuse("ALREADY_PAID", "this invoice is already paid", { invoiceId: id });
    if (m.expiresAt === 0) refuse("NO_MANDATE", "the owner has not granted this agent a mandate (or revoked it)");
    if (!m.active) refuse("MANDATE_EXPIRED", `the mandate expired at ${new Date(m.expiresAt * 1000).toISOString()}`);
    if (!allowed) refuse("MERCHANT_NOT_ALLOWED", `merchant ${merchant} is not on the mandate's allowlist`);
    if (amount > m.perPaymentCap) {
      refuse("OVER_PER_PAYMENT_CAP", `${formatAmount(amount)} tUSD8 is above the per-payment cap of ${formatAmount(m.perPaymentCap)}`);
    }
    if (amount > m.remainingToday) {
      refuse("OVER_DAILY_CAP", `${formatAmount(amount)} tUSD8 is above what is left of today's cap (${formatAmount(m.remainingToday)})`);
    }
    return { amount, merchant, id, mandate: m };
  }

  /**
   * Pays an invoice. With `skipChecks` the local checks are skipped and the transaction is
   * simulated against the contract only: use it to see the contract enforce the mandate itself.
   */
  async pay(invoice, { skipChecks = false } = {}) {
    let amount, merchant, id;
    if (skipChecks) {
      amount = BigInt(invoice.amount.raw);
      merchant = ethers.getAddress(invoice.merchant);
      id = invoiceKey(invoice.invoiceId);
    } else {
      ({ amount, merchant, id } = await this.check(invoice));
    }
    try {
      // Simulate first: a revert surfaces as a decoded error instead of a failed, paid-for tx.
      await this.router.payAsAgent.staticCall(this.owner, id, merchant, amount);
      const tx = await this.router.payAsAgent(this.owner, id, merchant, amount);
      this.log(`sent ${formatAmount(amount)} tUSD8 for ${invoice.id ?? id}: ${tx.hash}`);
      const receipt = await tx.wait();
      if (receipt.status !== 1) throw new PaymentRefused("REVERTED", `transaction ${tx.hash} reverted`);
      return { txHash: tx.hash, blockNumber: receipt.blockNumber, amount, invoiceId: id };
    } catch (err) {
      throw decodeRevert(err) ?? err;
    }
  }

  /**
   * fetch() that pays x402 invoices with the "aevum-mandate" scheme: on a 402 it checks the terms
   * against the mandate, pays once on chain, then retries with PAYMENT-SIGNATURE until the
   * merchant answers 200, or `waitMs` runs out (it then returns the last 402 with
   * settlement_pending, and `resume` picks it up again).
   */
  async fetch(url, { waitMs = 120_000, pollMs = 1_000, skipChecks = false, headers = {} } = {}) {
    const res = await fetch(url, { headers });
    if (res.status !== 402) return { response: res, paid: null };
    const offer = await readOffer(res);
    const accepted = offer.required.accepts.find((a) => a.scheme === "aevum-mandate" && a.network === `eip155:${this.chainId}`);
    if (!accepted) throw new PaymentRefused("NO_ACCEPTABLE_SCHEME", "the merchant does not accept aevum-mandate payments on this chain");
    const invoice = invoiceFromRequirements(accepted);
    this.log(`402: ${invoice.id} asks ${invoice.amount.display} tUSD8 (merchant needs ${invoice.requiredAssurance ?? "?"} assurance)`);
    const paid = await this.pay(invoice, { skipChecks });
    const payload = { x402Version: X402_VERSION, resource: offer.required.resource, accepted, payload: { txHash: paid.txHash, invoiceId: paid.invoiceId } };
    return { response: await retryWithPayment(url, payload, { waitMs, pollMs, headers, log: this.log }), paid, invoice };
  }

  /** Keeps asking the merchant to release a paid invoice. */
  async resume(url, invoice, { waitMs = 120_000, pollMs = 1_000, headers = {} } = {}) {
    const accepted = { scheme: "aevum-mandate", network: `eip155:${this.chainId}`, extra: { invoice: invoice.id } };
    return retryWithPayment(url, { x402Version: X402_VERSION, accepted, payload: {} }, { waitMs, pollMs, headers, log: this.log });
  }
}

/**
 * A standard x402 "exact" client: pays from its own wallet by signing an EIP-3009 authorization.
 * It needs no gas and no allowance (the merchant settles it), but also has no mandate: use it for a
 * person, or an agent that holds its own budget.
 */
export class ExactPayer {
  constructor({ signer, chainId, maxPrice, log = () => {} }) {
    this.signer = signer;
    this.chainId = Number(chainId);
    this.maxPrice = maxPrice != null ? parseAmount(maxPrice) : undefined;
    this.log = log;
  }

  async fetch(url, { waitMs = 120_000, pollMs = 1_000, headers = {} } = {}) {
    const res = await fetch(url, { headers });
    if (res.status !== 402) return { response: res, payload: null };
    const offer = await readOffer(res);
    const accepted = offer.required.accepts.find((a) => a.scheme === "exact" && a.network === `eip155:${this.chainId}`);
    if (!accepted) throw new PaymentRefused("NO_ACCEPTABLE_SCHEME", "the merchant does not accept x402 exact payments on this chain");
    if (this.maxPrice != null && BigInt(accepted.amount) > this.maxPrice) {
      throw new PaymentRefused("OVER_AGENT_BUDGET", `${formatAmount(accepted.amount)} tUSD8 is above this payer's limit of ${formatAmount(this.maxPrice)}`);
    }
    const payload = await signExact(this.signer, accepted, offer.required.resource);
    this.log(`402: signed an EIP-3009 authorization for ${formatAmount(accepted.amount)} tUSD8 to ${accepted.payTo} (no gas)`);
    return { response: await retryWithPayment(url, payload, { waitMs, pollMs, headers, log: this.log }), payload };
  }
}

async function readOffer(res) {
  const body = await res.json().catch(() => ({}));
  const required = decodeHeader(res.headers.get(HEADERS.required)) ?? (body.accepts ? body : undefined);
  if (!required?.accepts?.length) throw new Error(`402 from ${res.url} without x402 payment requirements`);
  return { required, body };
}

export function invoiceFromRequirements(a) {
  return {
    id: a.extra.invoice,
    invoiceId: a.extra.invoiceId,
    merchant: a.payTo,
    amount: { raw: String(a.amount), display: formatAmount(a.amount) },
    token: a.asset,
    router: a.extra.router,
    chainId: Number(a.network.split(":")[1]),
    expiresAt: Math.floor(Date.now() / 1000) + Number(a.maxTimeoutSeconds),
    requiredAssurance: a.extra.requiredAssurance,
  };
}

/** Sends the payment header until the merchant releases the resource or the wait runs out. */
async function retryWithPayment(url, payload, { waitMs, pollMs, headers, log }) {
  const deadline = Date.now() + waitMs;
  const header = encodeHeader(payload);
  let last = "";
  for (;;) {
    const res = await fetch(url, { headers: { ...headers, [HEADERS.signature]: header } });
    const settlement = decodeHeader(res.headers.get(HEADERS.response));
    if (res.status !== 402 || settlement?.errorReason !== "settlement_pending" || Date.now() > deadline) return res;
    const body = await res.clone().json().catch(() => ({}));
    const line = `waiting: payment is ${body.assurance ?? "not indexed yet"}, merchant needs ${body.required ?? "?"}`;
    if (line !== last) log((last = line));
    await sleep(Math.max(pollMs, (Number(res.headers.get("retry-after")) || 0) * 1000));
  }
}
