// Loads settings for the demo scripts: ENV_FILE if set (the local demo uses ../.env.local),
// otherwise ../.env when it exists. Variables already in the environment win.
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const file = process.env.ENV_FILE ? resolve(process.env.ENV_FILE) : resolve(here, "..", ".env");
if (existsSync(file)) process.loadEnvFile(file);

export function need(name) {
  const v = process.env[name]?.trim();
  if (!v) {
    console.error(`${name} is required (set it in .env or the environment)`);
    process.exit(2);
  }
  return v;
}

export const opt = (name, def) => process.env[name]?.trim() || def;
