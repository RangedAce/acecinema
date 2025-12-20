-- Phase I minimal schema (Postgres)
CREATE TABLE IF NOT EXISTS sites (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL UNIQUE,
  wg_endpoint INET NOT NULL,
  priority INT NOT NULL DEFAULT 0,
  weight INT NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS services (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL UNIQUE,
  domains TEXT[] NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS backends (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  service_id UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  site_id UUID NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  host TEXT NOT NULL,       -- IP WG ou routée
  port INT NOT NULL,
  health_path TEXT NOT NULL DEFAULT '/health',
  weight INT NOT NULL DEFAULT 1,
  priority INT NOT NULL DEFAULT 0,
  is_healthy BOOL NOT NULL DEFAULT false,
  last_checked TIMESTAMPTZ,
  meta JSONB DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS health_events (
  id BIGSERIAL PRIMARY KEY,
  backend_id UUID NOT NULL REFERENCES backends(id) ON DELETE CASCADE,
  status TEXT NOT NULL,           -- up/down
  latency_ms INT,
  at TIMESTAMPTZ NOT NULL DEFAULT now(),
  detail TEXT
);

CREATE TABLE IF NOT EXISTS api_tokens (
  token TEXT PRIMARY KEY,
  label TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
