-- Example sessions table for pgstore.Store. Copy it
-- into your migrations and adapt it; it is not meant to be applied as is.
-- Edit the user_id / impersonated_user_id type and foreign keys to match your
-- user table.

CREATE TABLE IF NOT EXISTS http_sessions (
	-- hex SHA-256 of the session id; the raw id only lives in the cookie
	session_id CHAR(64) NOT NULL PRIMARY KEY,
	user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	-- Deleting the impersonated user deletes the whole session, logging the
	-- admin out too, rather than leaving them impersonating a missing user.
	impersonated_user_id UUID NULL REFERENCES users (id) ON DELETE CASCADE,
	created_on TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	expires_on TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_http_sessions_expires_on ON http_sessions (expires_on);

CREATE INDEX IF NOT EXISTS idx_http_sessions_user_id ON http_sessions (user_id);

-- Backs the ON DELETE CASCADE above: without it, deleting any user scans the
-- whole table. Partial, since most sessions aren't impersonating.
CREATE INDEX IF NOT EXISTS idx_http_sessions_impersonated_user_id ON http_sessions (impersonated_user_id)
	WHERE impersonated_user_id IS NOT NULL;

-- One-time codes the portal mints for a service host, and the service
-- sessions they are redeemed for. Both go when their portal session does.
CREATE TABLE IF NOT EXISTS http_service_sessions (
	-- hex SHA-256 of the code until redeemed, then of the service session id
	id CHAR(64) NOT NULL PRIMARY KEY,
	session_id CHAR(64) NOT NULL REFERENCES http_sessions (session_id) ON DELETE CASCADE,
	-- the service host the code was minted for, with its port if any
	host TEXT NOT NULL,
	-- set while the row is an unredeemed code
	redeem_by TIMESTAMPTZ NULL
);

-- Backs the ON DELETE CASCADE above.
CREATE INDEX IF NOT EXISTS idx_http_service_sessions_session_id ON http_service_sessions (session_id);

CREATE INDEX IF NOT EXISTS idx_http_service_sessions_redeem_by ON http_service_sessions (redeem_by)
	WHERE redeem_by IS NOT NULL;
