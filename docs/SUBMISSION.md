# DoraHacks submission draft

**Name:** Aevum Settle

**One-liner:** Settlement assurance for AI-agent payments on QDay: agents pay tUSD8 invoices under
owner-set mandates, and merchants are told by signed webhook when each payment is safe to act on,
at the assurance level its amount deserves.

## Problem

An agent paying a merchant is half a payment flow. The merchant still has to decide when to deliver.
On QDay Aevum, a zkEVM validium, a transaction is trusted (sequencer, ~1 s), then virtual (batch on
L1, ~10 min), then verified (validity proof on L1, ~90 min). Treating all payments the same is
either too slow for pay-per-call or too risky for large orders, and most payment demos stop at
"the transaction was sent".

## What it does

- **AgentPayRouter** (Solidity): non-custodial. The owner grants each agent key a mandate:
  per-payment cap, daily cap, expiry, merchant allowlist, instant revoke. Invoices can be paid once.
- **Settler** (Go): follows `InvoicePaid` through all three assurance levels using QDay's `zkevm_*`
  RPC, applies the merchant's per-amount policy (`trusted<=1,virtual<=100,verified`), and sends
  `payment.confirmed`, `payment.verified` and, after a reorg, `payment.reverted`, as HMAC-signed
  webhooks from a transactional outbox. It serves the L1 sequence and verify transactions behind any
  block as evidence, a live dashboard and an owner console.
- **Agent SDK + CLI** (JS): a 402-aware `fetch` that checks the invoice against the mandate,
  simulates, pays, and waits for the merchant; every refusal is a machine-readable code.
- **Demo merchant**: a paid API answering HTTP 402, releasing data only on the settler's word.

## Aevum integration evidence

- Router: `<address>` — https://explorer-test.qday.info/address/<address>
- Deploy tx: https://explorer-test.qday.info/tx/<hash>
- Agent payments (trusted / virtual / verified tiers): <tx links>
- Mandate grant / revoke: <tx links>
- tUSD8: `0x456569838f1d43009A91ebA4263147Afd7233483`, chain ID 44005

## How to run

`scripts/local-demo.sh` runs the whole flow locally with no funds; the README has the testnet
steps (deploy, fund, grant, `docker compose --profile demo up`).

## Judging criteria map

| Criterion | Where |
|---|---|
| UX | owner console, dashboard with tier bars and L1 proof, merchant page, CLI output |
| Demo | `node agent.js scenarios`: 10 scenarios with tx hashes and explorer links |
| Docs | README: diagrams, env vars, API, webhooks, SDK, testnet addresses |
| Innovation / agentic | assurance-by-amount for agent commerce; HTTP 402 flow; refusal codes for agents |
| Platform integration | zkEVM virtual/verified batches and L1 evidence; mandates as session-key-style limits |
| Resilience / tests | reorg-safe indexer, randomised simulation, outbox, CI end to end |
| Real-world utility | pay-per-call APIs and higher-value orders with the same rails |
| Safety | caps, expiry, allowlist, revoke, once-only invoices, signed webhooks, on-chain audit trail |
| Ecosystem / open source | MIT, Docker Compose, reusable SDK and webhook verifier |

## Team

Solo: Brian Liu (GitHub: BrianLiu0531).

## Prior work disclosure

The indexing core is adapted from the author's open-source base-usdc-indexer; everything
QDay-specific was written during the build window (see README, "Prior work").
