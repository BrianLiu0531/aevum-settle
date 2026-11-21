// x402 v2 wire format (https://github.com/x402-foundation/x402, specs/x402-specification-v2.md):
//
//   402 + PAYMENT-REQUIRED: base64(JSON PaymentRequired)      server -> client
//   PAYMENT-SIGNATURE:      base64(JSON PaymentPayload)       client -> server (the retry)
//   PAYMENT-RESPONSE:       base64(JSON SettlementResponse)   server -> client
//
// Two schemes are offered for each invoice:
//   exact          the standard x402 scheme on EVM: the payer signs an EIP-3009
//                  TransferWithAuthorization to the merchant (tUSD8 supports it natively), and the
//                  merchant settles it through AgentPayRouter.settleAuthorization. The payer needs no
//                  gas and no allowance.
//   aevum-mandate  an AI agent pays on-chain itself, from its owner's wallet under a mandate
//                  (AgentPayRouter.payAsAgent), and sends the transaction hash.
import { ethers } from "ethers";

export const X402_VERSION = 2;
export const HEADERS = { required: "payment-required", signature: "payment-signature", response: "payment-response" };

export const encodeHeader = (obj) => Buffer.from(JSON.stringify(obj)).toString("base64");
export function decodeHeader(value) {
  if (!value) return undefined;
  try {
    return JSON.parse(Buffer.from(String(value), "base64").toString("utf8"));
  } catch {
    return undefined;
  }
}

export const caip2 = (chainId) => `eip155:${chainId}`;

export const TRANSFER_WITH_AUTHORIZATION = [
  { name: "from", type: "address" },
  { name: "to", type: "address" },
  { name: "value", type: "uint256" },
  { name: "validAfter", type: "uint256" },
  { name: "validBefore", type: "uint256" },
  { name: "nonce", type: "bytes32" },
];

export function tokenDomain(requirements, chainId) {
  return {
    name: requirements.extra?.name ?? "tUSD8",
    version: requirements.extra?.version ?? "2",
    chainId: Number(chainId),
    verifyingContract: requirements.asset,
  };
}

const chainIdOf = (network) => {
  const m = /^eip155:(\d+)$/.exec(network ?? "");
  if (!m) throw new Error(`unsupported network ${network}`);
  return Number(m[1]);
};

/** Client side of "exact": signs an EIP-3009 authorization for the requirements. */
export async function signExact(signer, requirements, resource) {
  const now = Math.floor(Date.now() / 1000);
  const authorization = {
    from: await signer.getAddress(),
    to: requirements.payTo,
    value: requirements.amount,
    validAfter: "0",
    validBefore: String(now + Math.min(Number(requirements.maxTimeoutSeconds) || 300, 3600)),
    nonce: ethers.hexlify(ethers.randomBytes(32)),
  };
  const signature = await signer.signTypedData(
    tokenDomain(requirements, chainIdOf(requirements.network)),
    { TransferWithAuthorization: TRANSFER_WITH_AUTHORIZATION },
    authorization,
  );
  return { x402Version: X402_VERSION, resource, accepted: requirements, payload: { signature, authorization } };
}

/**
 * Server side of "exact": checks a payload against the requirements the server issued.
 * Returns the authorization with the split signature, or throws with an x402 error reason.
 */
export function verifyExact(payload, requirements, { now = Math.floor(Date.now() / 1000) } = {}) {
  const fail = (reason, msg) => {
    const e = new Error(msg);
    e.reason = reason;
    throw e;
  };
  const a = payload?.payload?.authorization;
  const sig = payload?.payload?.signature;
  if (!a || !sig) fail("invalid_payload", "missing authorization or signature");
  if (ethers.getAddress(a.to) !== ethers.getAddress(requirements.payTo)) fail("invalid_exact_evm_payload_recipient_mismatch", "authorization is not to the merchant");
  if (BigInt(a.value) < BigInt(requirements.amount)) fail("invalid_exact_evm_payload_authorization_value", "authorization value is below the price");
  if (BigInt(a.validBefore) <= BigInt(now + 6)) fail("invalid_exact_evm_payload_authorization_valid_before", "authorization expires too soon");
  if (BigInt(a.validAfter) > BigInt(now)) fail("invalid_exact_evm_payload_authorization_valid_after", "authorization is not valid yet");
  let signer;
  try {
    signer = ethers.verifyTypedData(
      tokenDomain(requirements, chainIdOf(requirements.network)),
      { TransferWithAuthorization: TRANSFER_WITH_AUTHORIZATION },
      a,
      sig,
    );
  } catch {
    fail("invalid_exact_evm_payload_signature", "signature does not parse");
  }
  if (signer !== ethers.getAddress(a.from)) fail("invalid_exact_evm_payload_signature", "signature is not from the payer");
  const { v, r, s } = ethers.Signature.from(sig);
  return { ...a, v, r, s };
}
