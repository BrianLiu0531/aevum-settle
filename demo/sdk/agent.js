// Agent side: pay a merchant's HTTP 402 invoice in tUSD8 under the owner's mandate, then wait
// for the merchant to release the resource.
//
// The agent checks every rule the contract enforces before it signs anything, so a refused
// payment costs no gas and comes back as a PaymentRefused with a machine-readable code. The
// contract enforces the same rules again; the local check is a courtesy, not the security.
import { Contract, Interface, ethers } from "ethers";
import { ROUTER_ABI, TOKEN_ABI } from "./abi.js";

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
   * fetch() that pays HTTP 402 invoices. It pays at most once per resource request, then retries
   * with the invoice id until the merchant answers 200 or `waitMs` runs out (then it returns the
   * last 202 so the caller can resume with `resume`).
   */
  async fetch(url, { waitMs = 120_000, pollMs = 1_000, skipChecks = false, headers = {} } = {}) {
    let res = await fetch(url, { headers });
    if (res.status !== 402) return { response: res, paid: null };
    const body = await res.json();
    const invoice = body.invoice;
    if (!invoice) throw new Error(`402 from ${url} without an invoice`);
    this.log(`402: ${invoice.id} asks ${invoice.amount.display} tUSD8 (merchant needs ${invoice.requiredAssurance ?? "?"} assurance)`);
    const paid = await this.pay(invoice, { skipChecks });
    res = await this.resume(url, invoice, { waitMs, pollMs, headers });
    return { response: res, paid, invoice };
  }

  /** Polls the merchant with a paid invoice until it releases the resource. */
  async resume(url, invoice, { waitMs = 120_000, pollMs = 1_000, headers = {} } = {}) {
    const deadline = Date.now() + waitMs;
    let last = "";
    for (;;) {
      const res = await fetch(url, { headers: { ...headers, "X-Payment-Invoice": invoice.id } });
      if (res.status !== 202 || Date.now() > deadline) return res;
      const status = await res.json().catch(() => ({}));
      const line = `waiting: payment is ${status.assurance ?? "not indexed yet"}, merchant needs ${status.required ?? invoice.requiredAssurance}`;
      if (line !== last) this.log((last = line));
      await sleep(Math.max(pollMs, (Number(res.headers.get("retry-after")) || 0) * 1000));
    }
  }
}
