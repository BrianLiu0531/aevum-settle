#!/usr/bin/env node
// aevum-agent: an AI agent's wallet tool for tUSD8 on QDay Aevum, plus the owner commands that
// set its limits.
//
//   node agent.js status                         mandate, balances, allowance
//   node agent.js buy <product|url> [--wait s]   GET a paid resource, paying its 402 invoice
//   node agent.js wait <product|url> <INV-id>    keep waiting for a paid invoice to be released
//   node agent.js scenarios                      the demo: three assurance tiers + every refusal
//   node agent.js pay-exact <product|url>        pay as a person, standard x402 "exact" (EIP-3009, no gas)
//
//   node agent.js owner approve [amount]         let the router spend the owner's tUSD8
//   node agent.js owner grant [--per 150] [--daily 200] [--days 7] [--merchant 0x..]...
//   node agent.js owner revoke
//   node agent.js owner fund-agent [tQDAY]       send the agent gas money (default 0.05)
//
// Settings come from ../.env (or ENV_FILE): RPC_URL, ROUTER_ADDRESS, CHAIN_ID, OWNER_ADDRESS,
// AGENT_PRIVATE_KEY, MERCHANT_URL, and OWNER_PRIVATE_KEY for the owner commands.
import { Contract, JsonRpcProvider, NonceManager, Wallet, ethers } from "ethers";
import { need, opt } from "./env.js";
import { AgentPayer, ExactPayer, PaymentRefused, formatAmount, parseAmount } from "./sdk/agent.js";
import { HEADERS, decodeHeader } from "./sdk/x402.js";
import { ROUTER_ABI, TOKEN_ABI } from "./sdk/abi.js";

const RPC_URL = opt("RPC_URL", "https://rpc-test.qday.info");
const CHAIN_ID = Number(opt("CHAIN_ID", "44005"));
const MERCHANT_URL = opt("MERCHANT_URL", "http://localhost:8787").replace(/\/$/, "");
const EXPLORER = opt("EXPLORER_URL", CHAIN_ID === 44005 ? "https://explorer-test.qday.info" : "");

const c = process.stdout.isTTY && !process.env.NO_COLOR
  ? { b: (s) => `\x1b[1m${s}\x1b[0m`, g: (s) => `\x1b[32m${s}\x1b[0m`, r: (s) => `\x1b[31m${s}\x1b[0m`, y: (s) => `\x1b[33m${s}\x1b[0m`, d: (s) => `\x1b[2m${s}\x1b[0m` }
  : { b: String, g: String, r: String, y: String, d: String };
const txLink = (h) => (EXPLORER ? `${EXPLORER}/tx/${h}` : h);

const provider = new JsonRpcProvider(RPC_URL, CHAIN_ID, { staticNetwork: true, pollingInterval: 500 });

function agentPayer() {
  const wallet = new Wallet(need("AGENT_PRIVATE_KEY"), provider);
  return new AgentPayer({
    wallet,
    owner: need("OWNER_ADDRESS"),
    router: need("ROUTER_ADDRESS"),
    chainId: CHAIN_ID,
    maxPrice: opt("AGENT_MAX_PRICE", undefined),
    log: (m) => console.log(c.d("  · " + m)),
  });
}

const urlFor = (target) => (/^https?:\/\//.test(target) ? target : `${MERCHANT_URL}/api/${target}`);

function flags(args) {
  const out = { _: [], merchant: [] };
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (!a.startsWith("--")) out._.push(a);
    else if (a === "--merchant") out.merchant.push(args[++i]);
    else out[a.slice(2)] = args[++i];
  }
  return out;
}

async function status() {
  const agent = agentPayer();
  const [m, b] = await Promise.all([agent.mandate(), agent.balances()]);
  console.log(c.b("agent ") + agent.wallet.address + c.d(`  (gas ${ethers.formatEther(b.agentGas)})`));
  console.log(c.b("owner ") + agent.owner + c.d(`  (tUSD8 ${formatAmount(b.ownerBalance)}, router allowance ${b.allowance === ethers.MaxUint256 ? "unlimited" : formatAmount(b.allowance)})`));
  if (m.expiresAt === 0) return console.log(c.r("no mandate: run `node agent.js owner grant`"));
  console.log(
    `${c.b("mandate")} ${m.active ? c.g("active") : c.r("expired")}  per payment ${formatAmount(m.perPaymentCap)}  ` +
      `daily ${formatAmount(m.dailyCap)} (left today ${formatAmount(m.remainingToday)})  ` +
      `expires ${new Date(m.expiresAt * 1000).toISOString()}  ${m.merchantLocked ? "allowlisted merchants only" : "any merchant"}`,
  );
}

async function buy(agent, target, { waitMs, skipChecks } = {}) {
  const url = urlFor(target);
  const started = Date.now();
  const { response, paid, invoice } = await agent.fetch(url, { waitMs, skipChecks });
  const secs = ((Date.now() - started) / 1000).toFixed(1);
  if (paid) console.log(c.d(`  · paid in block ${paid.blockNumber}: ${txLink(paid.txHash)}`));
  if (response.status === 200) {
    const body = await response.json();
    console.log(c.g(`  ✓ 200 after ${secs}s`) + c.d(` (released at ${body.receipt?.assurance ?? "-"} assurance)`));
    return { ok: true, invoice, paid, body };
  }
  if (response.status === 402 && decodeHeader(response.headers.get(HEADERS.response))?.errorReason === "settlement_pending") {
    console.log(c.y(`  … still pending after ${secs}s; resume with: node agent.js wait ${target} ${invoice.id}`));
    return { ok: false, pending: true, invoice, paid };
  }
  console.log(c.r(`  ✗ merchant answered ${response.status}: ${await response.text()}`));
  return { ok: false, invoice, paid };
}

/** Runs fn and reports a PaymentRefused as the expected outcome. */
async function expectRefusal(code, fn) {
  try {
    await fn();
    console.log(c.r(`  ✗ expected ${code}, but the payment went through`));
    return false;
  } catch (e) {
    if (!(e instanceof PaymentRefused)) throw e;
    const ok = e.code === code;
    console.log((ok ? c.g("  ✓ refused ") : c.r("  ✗ refused ")) + `${e.code}: ${e.message}`);
    return ok;
  }
}

async function invoiceFor(product) {
  const res = await fetch(urlFor(product));
  if (res.status !== 402) throw new Error(`expected 402 from ${product}, got ${res.status}`);
  return (await res.json()).invoice;
}

async function scenarios(opts) {
  const agent = agentPayer();
  const local = CHAIN_ID === 31337;
  const waitMs = Number(opts.wait ?? (local ? 90 : 900)) * 1000;
  let pass = 0, fail = 0;
  const step = async (title, fn) => {
    console.log("\n" + c.b(title));
    let ok = false;
    try {
      ok = await fn();
    } catch (e) {
      console.log(c.r(`  ✗ ${e instanceof PaymentRefused ? `refused ${e.code}: ${e.message}` : e.shortMessage ?? e.message}`));
    }
    ok ? pass++ : fail++;
  };
  await status();

  let first;
  await step("1. Pay-per-call: 0.05 tUSD8 quote (policy: trusted, ~1 s)", async () => {
    const r = await buy(agent, "quote", { waitMs });
    first = r.invoice;
    return r.ok;
  });
  await step("2. 20 tUSD8 report (policy: virtual, batch must be on L1)", async () => (await buy(agent, "report", { waitMs })).ok);
  await step("3. 120 tUSD8 archive (policy: verified, validity proof on L1)", async () => {
    const r = await buy(agent, "archive", { waitMs });
    return r.ok || r.pending;
  });
  await step("4. 500 tUSD8 licence: over the per-payment cap, refused before signing", async () =>
    expectRefusal("OVER_PER_PAYMENT_CAP", async () => agent.pay(await invoiceFor("bulk"))));
  await step("5. Same, with the agent's checks bypassed: the contract itself refuses", async () =>
    expectRefusal("OVER_PER_PAYMENT_CAP", async () => agent.pay(await invoiceFor("bulk"), { skipChecks: true })));
  await step("6. Another 120 tUSD8 archive: over what is left of the daily cap", async () =>
    expectRefusal("OVER_DAILY_CAP", async () => agent.pay(await invoiceFor("archive"))));
  await step("7. Paying invoice #1 again (a retrying agent): the contract refuses a double payment", async () =>
    expectRefusal("ALREADY_PAID", async () => agent.pay(first, { skipChecks: true })));
  await step("8. An invoice from a merchant outside the allowlist", async () => {
    const forged = { ...(await invoiceFor("quote")), merchant: Wallet.createRandom().address };
    return expectRefusal("MERCHANT_NOT_ALLOWED", async () => agent.pay(forged, { skipChecks: true }));
  });

  if (process.env.OWNER_PRIVATE_KEY) {
    await step("9. The owner revokes the mandate; the agent can no longer pay", async () => {
      const before = await agent.mandate();
      const tx = await ownerRouter().revokeMandate(agent.wallet.address);
      await tx.wait();
      console.log(c.d(`  · revoked: ${txLink(tx.hash)}`));
      const ok = await expectRefusal("NO_MANDATE", async () => agent.pay(await invoiceFor("quote"), { skipChecks: true }));
      const merchant = (await (await fetch(`${MERCHANT_URL}/api/catalog`)).json()).merchant;
      const regrant = await ownerRouter().grantMandate(agent.wallet.address, before.perPaymentCap, before.dailyCap, before.expiresAt, [merchant]);
      await regrant.wait();
      console.log(c.d(`  · mandate granted again for the next run: ${txLink(regrant.hash)}`));
      return ok;
    });
  } else {
    console.log(c.d("\n9. (skipped: set OWNER_PRIVATE_KEY to demo revocation)"));
  }

  if (process.env.OWNER_PRIVATE_KEY) {
    await step("10. Standard x402 \"exact\": the owner pays from their own wallet by signing an EIP-3009 authorization (no gas, no allowance)", async () => {
      const r = await payExact("quote", { waitMs });
      return r.ok && r.body.receipt?.scheme === "exact";
    });
    await step("11. A payment below the price: the merchant does not deliver, and refunds it once the settler confirms it", async () => {
      const inv = await invoiceFor("quote");
      const tx = await ownerRouter().pay(inv.invoiceId, inv.merchant, BigInt(inv.amount.raw) - 10_000n);
      await tx.wait();
      console.log(c.d(`  · paid ${formatAmount(BigInt(inv.amount.raw) - 10_000n)} of ${inv.amount.display} for ${inv.id}: ${txLink(tx.hash)}`));
      const deadline = Date.now() + waitMs;
      while (Date.now() < deadline) {
        const state = await (await fetch(`${MERCHANT_URL}/api/invoices/${inv.id}`)).json();
        if (state.refund?.status === "sent") {
          console.log(c.g(`  ✓ ${state.status}: ${state.refund.amount.display} tUSD8 back to the payer: ${txLink(state.refund.txHash)}`));
          return state.status === "refunded";
        }
        await new Promise((ok) => setTimeout(ok, 1000));
      }
      console.log(c.r("  ✗ no refund"));
      return false;
    });
  }

  if (local) {
    await step("12. Local only: a reorg removes a delivered payment; the merchant is told and withdraws access", async () => {
      const snapshot = await provider.send("evm_snapshot", []);
      const r = await buy(agent, "quote", { waitMs });
      if (!r.ok) return false;
      await provider.send("evm_revert", [snapshot]);
      console.log(c.d("  · chain rewound to before the payment; new blocks replace it"));
      const deadline = Date.now() + 60_000;
      while (Date.now() < deadline) {
        const { invoices } = await (await fetch(`${MERCHANT_URL}/api/invoices`)).json();
        if (invoices.find((i) => i.id === r.invoice.id)?.status === "open") {
          const res = await fetch(urlFor("quote"), { headers: { "X-Payment-Invoice": r.invoice.id } });
          console.log(c.g(`  ✓ payment.reverted delivered; merchant now answers ${res.status} for ${r.invoice.id}`));
          return res.status === 202;
        }
        await new Promise((ok) => setTimeout(ok, 500));
      }
      console.log(c.r("  ✗ merchant was not told within 60 s"));
      return false;
    });
  }

  console.log("\n" + (fail ? c.r(`${pass} passed, ${fail} failed`) : c.g(`all ${pass} scenarios behaved as expected`)));
  if (fail) process.exitCode = 1;
}

let ownerContract;
function ownerRouter() {
  // One signer for every owner transaction, so back-to-back sends get consecutive nonces.
  ownerContract ??= new Contract(need("ROUTER_ADDRESS"), ROUTER_ABI, new NonceManager(new Wallet(need("OWNER_PRIVATE_KEY"), provider)));
  return ownerContract;
}

async function ownerCommand([cmd, ...rest]) {
  const f = flags(rest);
  const router = ownerRouter();
  const agentAddress = f.agent ?? new Wallet(need("AGENT_PRIVATE_KEY")).address;
  let tx;
  if (cmd === "approve") {
    const token = new Contract(await router.token(), TOKEN_ABI, router.runner);
    const amount = f._[0] ? parseAmount(f._[0]) : ethers.MaxUint256;
    tx = await token.approve(await router.getAddress(), amount);
  } else if (cmd === "grant") {
    const merchants = f.merchant.length ? f.merchant : [opt("MERCHANT_ADDRESS", undefined)].filter(Boolean);
    const expires = Math.floor(Date.now() / 1000) + Number(f.days ?? 7) * 86400;
    tx = await router.grantMandate(agentAddress, parseAmount(f.per ?? "150"), parseAmount(f.daily ?? "200"), expires, merchants);
    console.log(`granting ${agentAddress}: per ${f.per ?? "150"}, daily ${f.daily ?? "200"}, ${f.days ?? 7} days, merchants ${merchants.join(", ") || "any"}`);
  } else if (cmd === "fund-agent") {
    tx = await router.runner.sendTransaction({ to: agentAddress, value: ethers.parseEther(f._[0] ?? "0.05") });
  } else if (cmd === "revoke") {
    tx = await router.revokeMandate(agentAddress);
  } else {
    throw new Error("usage: owner approve [amount] | owner fund-agent [tQDAY] | owner grant [--per n] [--daily n] [--days n] [--merchant 0x..] | owner revoke");
  }
  const receipt = await tx.wait();
  console.log(`${cmd}: block ${receipt.blockNumber} ${txLink(tx.hash)}`);
}

async function payExact(target, { waitMs = 300_000 } = {}) {
  const payer = new ExactPayer({ signer: new Wallet(need("OWNER_PRIVATE_KEY"), provider), chainId: CHAIN_ID, log: (m) => console.log(c.d("  · " + m)) });
  const started = Date.now();
  const { response } = await payer.fetch(urlFor(target), { waitMs });
  const secs = ((Date.now() - started) / 1000).toFixed(1);
  const settlement = decodeHeader(response.headers.get(HEADERS.response));
  if (response.status === 200) {
    const body = await response.json();
    console.log(c.g(`  ✓ 200 after ${secs}s`) + c.d(` (scheme ${body.receipt.scheme}, settled by the merchant in ${txLink(settlement?.transaction)})`));
    return { ok: true, body };
  }
  console.log(c.r(`  ✗ ${response.status} ${settlement?.errorReason ?? ""} ${settlement?.errorMessage ?? (await response.text())}`));
  return { ok: false };
}

async function main() {
  const [cmd, ...args] = process.argv.slice(2);
  const f = flags(args);
  switch (cmd) {
    case "status":
      return status();
    case "buy":
      if (!f._[0]) throw new Error("usage: buy <product|url> [--wait seconds]");
      return void (await buy(agentPayer(), f._[0], { waitMs: Number(f.wait ?? 300) * 1000 }));
    case "wait": {
      if (!f._[1]) throw new Error("usage: wait <product|url> <INV-id>");
      const res = await agentPayer().resume(urlFor(f._[0]), { id: f._[1] }, { waitMs: Number(f.wait ?? 3600) * 1000 });
      console.log(res.status, await res.text());
      return;
    }
    case "pay-exact":
      if (!f._[0]) throw new Error("usage: pay-exact <product|url>");
      return void (await payExact(f._[0]));
    case "scenarios":
      return scenarios(f);
    case "owner":
      return ownerCommand(args);
    default:
      console.log("usage: node agent.js status | buy <product|url> | wait <product|url> <INV-id> | scenarios | owner approve|grant|revoke");
      process.exitCode = 2;
  }
}

main().catch((e) => {
  console.error(c.r(e instanceof PaymentRefused ? `refused ${e.code}: ${e.message}` : e.stack ?? String(e)));
  process.exitCode = 1;
});
