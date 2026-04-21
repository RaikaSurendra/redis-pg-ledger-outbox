-- Accounts table: authoritative balance
CREATE TABLE IF NOT EXISTS accounts (
  id BIGINT PRIMARY KEY,
  balance BIGINT NOT NULL CHECK (balance >= 0)
);

-- Ledger entries: immutable record of debits/credits
CREATE TABLE IF NOT EXISTS ledger_entries (
  id BIGSERIAL PRIMARY KEY,
  account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  amount BIGINT NOT NULL, -- positive=credit, negative=debit
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_account_id ON ledger_entries(account_id);

-- Idempotency keys per account
CREATE TABLE IF NOT EXISTS idempotency (
  account_id BIGINT NOT NULL,
  key TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, key)
);

-- Outbox for Redis mirror updates
CREATE TABLE IF NOT EXISTS outbox (
  id BIGSERIAL PRIMARY KEY,
  account_id BIGINT NOT NULL,
  delta BIGINT NOT NULL, -- amount applied to Redis mirror (same sign as ledger entry)
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_outbox_unprocessed ON outbox(processed_at) WHERE processed_at IS NULL;
