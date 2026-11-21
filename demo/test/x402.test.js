import { test } from "node:test";
import assert from "node:assert/strict";
import { Wallet } from "ethers";
import { decodeHeader, encodeHeader, invoiceFromRequirements, signExact, verifyExact } from "../sdk/index.js";

const merchant = Wallet.createRandom().address;
const requirements = {
  scheme: "exact",
  network: "eip155:44005",
  amount: "50000",
  asset: "0x456569838f1d43009A91ebA4263147Afd7233483",
  payTo: merchant,
  maxTimeoutSeconds: 600,
  extra: { name: "tUSD8", version: "2", invoice: "INV-1" },
};

test("headers are base64 JSON", () => {
  const h = encodeHeader({ x402Version: 2, accepts: [] });
  assert.match(h, /^eyJ/);
  assert.deepEqual(decodeHeader(h), { x402Version: 2, accepts: [] });
  assert.equal(decodeHeader("not base64 json"), undefined);
});

test("an exact payload signed by the payer verifies, and binds payee, amount and time", async () => {
  const payer = Wallet.createRandom();
  const payload = await signExact(payer, requirements, { url: "http://x/api/quote" });
  assert.equal(payload.x402Version, 2);
  assert.deepEqual(payload.accepted, requirements);
  const auth = verifyExact(payload, requirements);
  assert.equal(auth.from, payer.address);
  assert.ok(auth.v === 27 || auth.v === 28);

  const tamper = (f) => {
    const p = structuredClone(payload);
    f(p.payload.authorization);
    return p;
  };
  assert.throws(() => verifyExact(tamper((a) => (a.to = Wallet.createRandom().address)), requirements), (e) => e.reason.includes("recipient"));
  assert.throws(() => verifyExact(tamper((a) => (a.value = "1")), requirements), (e) => e.reason.includes("value"));
  assert.throws(() => verifyExact(tamper((a) => (a.value = "60000")), requirements), (e) => e.reason.includes("signature"));
  assert.throws(() => verifyExact(tamper((a) => (a.from = Wallet.createRandom().address)), requirements), (e) => e.reason.includes("signature"));
  assert.throws(() => verifyExact(payload, requirements, { now: Math.floor(Date.now() / 1000) + 3600 }), (e) => e.reason.includes("valid_before"));
  assert.throws(() => verifyExact({ payload: {} }, requirements), (e) => e.reason === "invalid_payload");
});

test("a signature for another token or chain does not verify", async () => {
  const payer = Wallet.createRandom();
  const other = { ...requirements, network: "eip155:1" };
  const payload = await signExact(payer, other);
  assert.throws(() => verifyExact(payload, requirements), (e) => e.reason.includes("signature"));
});

test("invoiceFromRequirements maps an aevum-mandate offer to the agent's invoice", () => {
  const inv = invoiceFromRequirements({
    scheme: "aevum-mandate", network: "eip155:44005", amount: "20000000", asset: requirements.asset, payTo: merchant, maxTimeoutSeconds: 900,
    extra: { invoice: "INV-2", invoiceId: "0x" + "11".repeat(32), router: Wallet.createRandom().address, requiredAssurance: "virtual" },
  });
  assert.equal(inv.id, "INV-2");
  assert.equal(inv.chainId, 44005);
  assert.equal(inv.amount.display, "20.0");
  assert.equal(inv.requiredAssurance, "virtual");
});
