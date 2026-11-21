import { test } from "node:test";
import assert from "node:assert/strict";
import { Interface, ethers } from "ethers";
import { ROUTER_ABI, decodeRevert, invoiceKey, requiredFor, sign, verifyWebhook, PaymentRefused } from "../sdk/index.js";

const secret = "0123456789abcdef";

test("signature matches the Go settler (settler/internal/outbox/vector_test.go)", () => {
  assert.equal(sign(secret, "1800000000", '{"events":[]}'), "sha256=044db41e5630c9e1eda7fa2c624a922420b9a745fc2c2eb0ed58b256224bd484");
});

test("verifyWebhook accepts a signed body and rejects forgeries and replays", () => {
  const body = Buffer.from(JSON.stringify({ events: [{ eventId: "e1", type: "payment.confirmed" }] }));
  const ts = "1800000000";
  const now = 1_800_000_010_000;
  const events = verifyWebhook({ secret, timestamp: ts, signature: sign(secret, ts, body), body, now });
  assert.equal(events[0].eventId, "e1");
  assert.throws(() => verifyWebhook({ secret: "wrong-secret-0000", timestamp: ts, signature: sign(secret, ts, body), body, now }), /signature/);
  assert.throws(() => verifyWebhook({ secret, timestamp: ts, signature: sign(secret, ts, body), body: Buffer.concat([body, Buffer.from(" ")]), now }), /signature/);
  assert.throws(() => verifyWebhook({ secret, timestamp: ts, signature: sign(secret, ts, body), body, now: now + 3_600_000 }), /window/);
  assert.throws(() => verifyWebhook({ secret, timestamp: "x", signature: "", body, now }), /timestamp/);
});

test("requiredFor mirrors the settler policy", () => {
  const p = "trusted<=1,virtual<=100,verified";
  assert.equal(requiredFor(50_000n, p), "trusted");
  assert.equal(requiredFor(1_000_000n, p), "trusted");
  assert.equal(requiredFor(1_000_001n, p), "virtual");
  assert.equal(requiredFor(100_000_000n, p), "virtual");
  assert.equal(requiredFor(120_000_000n, p), "verified");
  assert.equal(requiredFor(5n, "verified"), "verified");
});

test("invoiceKey matches the settler API (keccak of the label, or the raw 32-byte id)", () => {
  assert.equal(invoiceKey("INV-42"), ethers.id("INV-42"));
  const raw = "0x" + "ab".repeat(32);
  assert.equal(invoiceKey(raw), raw);
  assert.equal(invoiceKey(raw.toUpperCase().replace("0X", "0x")), raw);
});

test("decodeRevert maps contract errors to refusal codes", () => {
  const iface = new Interface(ROUTER_ABI);
  const data = iface.encodeErrorResult("OverDailyCap", [120_000_000n, 59_950_000n]);
  const e = decodeRevert({ data });
  assert.ok(e instanceof PaymentRefused);
  assert.equal(e.code, "OVER_DAILY_CAP");
  assert.deepEqual(e.details.args, ["120000000", "59950000"]);
  assert.equal(decodeRevert({ data: iface.encodeErrorResult("NoMandate", []) }).code, "NO_MANDATE");
  assert.equal(decodeRevert({ data: "0x08c379a0" }), undefined);
  assert.equal(decodeRevert(new Error("network down")), undefined);
});
