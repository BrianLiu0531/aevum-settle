import { parseAmount } from "./agent.js";

/**
 * The assurance level a settler CONFIRM_POLICY such as "trusted<=1,virtual<=100,verified"
 * requires for an amount in base units. Mirrors settler/internal/store/policy.go.
 */
export function requiredFor(amountRaw, policyString) {
  for (const tier of policyString.split(",")) {
    const [level, limit] = tier.split("<=");
    if (limit === undefined || BigInt(amountRaw) <= parseAmount(limit.trim())) return level.trim();
  }
  return "verified";
}
