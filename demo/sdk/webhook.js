// Receiving side of the settler's webhooks. Mirrors settler/internal/outbox/webhook.go:
//
//   X-Settle-Timestamp: unix seconds
//   X-Settle-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))
import { createHmac, timingSafeEqual } from "node:crypto";

export function sign(secret, timestamp, body) {
  return "sha256=" + createHmac("sha256", secret).update(`${timestamp}.`).update(body).digest("hex");
}

export class WebhookError extends Error {}

/**
 * Verifies a webhook and returns its parsed events. `body` must be the raw bytes as received:
 * re-serialised JSON will not match the signature.
 */
export function verifyWebhook({ secret, timestamp, signature, body, now = Date.now(), maxSkewSeconds = 300 }) {
  const ts = Number.parseInt(timestamp ?? "", 10);
  if (!Number.isFinite(ts)) throw new WebhookError("bad timestamp");
  if (Math.abs(now / 1000 - ts) > maxSkewSeconds) throw new WebhookError("timestamp outside the allowed window");
  const want = Buffer.from(sign(secret, String(timestamp), body));
  const got = Buffer.from(String(signature ?? ""));
  if (got.length !== want.length || !timingSafeEqual(got, want)) throw new WebhookError("signature mismatch");
  const parsed = JSON.parse(Buffer.isBuffer(body) ? body.toString("utf8") : body);
  if (!Array.isArray(parsed.events)) throw new WebhookError("body has no events array");
  return parsed.events;
}
