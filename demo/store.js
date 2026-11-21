// The demo merchant's own records: invoices, webhook events already handled, and an activity log.
// SQLite (node:sqlite, built into Node 22.13+), so a restart keeps every invoice and never handles
// a webhook event twice.
import { DatabaseSync } from "node:sqlite";
import { mkdirSync } from "node:fs";
import { dirname } from "node:path";

export class MerchantStore {
  constructor(file) {
    if (file !== ":memory:") mkdirSync(dirname(file), { recursive: true });
    this.db = new DatabaseSync(file);
    this.db.exec(`
      PRAGMA journal_mode = WAL;
      CREATE TABLE IF NOT EXISTS invoices (
        id TEXT PRIMARY KEY,
        invoice_key TEXT NOT NULL UNIQUE,
        created_at INTEGER NOT NULL,
        data TEXT NOT NULL
      );
      CREATE TABLE IF NOT EXISTS seen_events (event_id TEXT PRIMARY KEY, at INTEGER NOT NULL);
      CREATE TABLE IF NOT EXISTS log (id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, line TEXT NOT NULL);
    `);
    this.q = {
      get: this.db.prepare("SELECT data FROM invoices WHERE id = ?"),
      byKey: this.db.prepare("SELECT data FROM invoices WHERE invoice_key = ?"),
      put: this.db.prepare(
        "INSERT INTO invoices (id, invoice_key, created_at, data) VALUES (?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET data = excluded.data",
      ),
      list: this.db.prepare("SELECT data FROM invoices ORDER BY created_at DESC, rowid DESC LIMIT ?"),
      seen: this.db.prepare("INSERT OR IGNORE INTO seen_events (event_id, at) VALUES (?, ?)"),
      log: this.db.prepare("INSERT INTO log (at, line) VALUES (?, ?)"),
      logs: this.db.prepare("SELECT at, line FROM log ORDER BY id DESC LIMIT ?"),
    };
  }

  get(id) {
    const row = this.q.get.get(String(id));
    return row ? JSON.parse(row.data) : undefined;
  }

  byKey(key) {
    const row = this.q.byKey.get(String(key).toLowerCase());
    return row ? JSON.parse(row.data) : undefined;
  }

  save(inv) {
    this.q.put.run(inv.id, inv.invoiceId.toLowerCase(), inv.createdAt, JSON.stringify(inv));
    return inv;
  }

  list(limit = 100) {
    return this.q.list.all(limit).map((r) => JSON.parse(r.data));
  }

  /** Records an event id; returns false if it was already handled. */
  firstTime(eventId) {
    return this.q.seen.run(eventId, Date.now()).changes === 1;
  }

  note(line) {
    const at = new Date().toISOString();
    this.q.log.run(at, line);
    return { at, line };
  }

  logs(limit = 60) {
    return this.q.logs.all(limit);
  }
}
