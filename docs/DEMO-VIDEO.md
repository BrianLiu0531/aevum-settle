# Demo video script (3–5 minutes)

Record at 1080p with three windows visible: a terminal, the settler dashboard
(`http://localhost:8080`) and the merchant page (`http://localhost:8787`). Use the QDay testnet
stack for the on-chain parts, so every transaction has an explorer link, and the local stack
(`scripts/local-demo.sh --keep`) for the reorg, which cannot be staged on a live chain.

| Time | Show | Say |
|---|---|---|
| 0:00–0:25 | README header | "Agents can already pay. The missing half is the merchant knowing when a payment is safe to act on. On QDay that has three answers: trusted in a second, virtual in about ten minutes, verified in about ninety. Aevum Settle lets the merchant choose by amount." |
| 0:25–1:00 | Owner console `/console`: connect wallet, approve 300, grant 150 / 200 / 7 days / merchant | "The owner gives the agent a mandate. Funds stay in the owner's wallet; the agent's own key only pays gas, and only within these limits." Open the grant transaction in the explorer. |
| 1:00–1:40 | Terminal: `node agent.js scenarios`, step 1 | "The agent asks for a 5-cent quote, gets HTTP 402 with an invoice, checks it against its mandate, pays, and has the data a couple of seconds later." Dashboard: the payment appears as trusted, delivered. Click the tx link. |
| 1:40–2:20 | Steps 2–3; dashboard tier bars | "Twenty tUSD8 needs the batch on L1, so the merchant answers 202 until the settler says virtual." Click **L1 proof** on a verified payment: batch number, L1 sequence tx, L1 verify tx. "That's proof beyond the settler's word." |
| 2:20–3:10 | Steps 4–9 | "Over the per-payment cap: refused before signing, no gas spent. Bypass the agent's checks and the contract refuses anyway. Over the daily cap, a double payment, a merchant off the allowlist, and after the owner revokes: all refused, each with a code an agent can act on." |
| 3:10–3:45 | Local stack: step 10; merchant webhook log | "If a payment the merchant already acted on is reorged away, the settler sends payment.reverted and the merchant withdraws access. Every webhook is HMAC-signed and delivered from a transactional outbox, at least once, deduplicated by event id." |
| 3:45–4:15 | CI run, test list in README | "Sixteen contract tests, a randomised reorg simulation across 100 seeds and five policies, and this whole flow end to end in CI." |
| 4:15–4:30 | Architecture diagram | "Open source, MIT, runs with Docker Compose. Thanks." |

Before recording: run the testnet scenarios once ~2 hours earlier so verified payments exist,
re-grant the mandate to reset the day's spend, and clear the terminal.
