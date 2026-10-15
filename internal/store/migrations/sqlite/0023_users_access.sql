-- 0023: more than one dashboard user. Beknown staff sign in through Clerk
-- (Google); the local password account stays as the break-glass owner.
--
-- role:   admin sees and manages everything; member only manages their own
--         agents, and those agents only reach the servers granted below.
-- status: blocked users can't sign in and their agents stop authenticating.
ALTER TABLE users ADD COLUMN email TEXT;
ALTER TABLE users ADD COLUMN display_name TEXT;
ALTER TABLE users ADD COLUMN avatar_url TEXT;
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member'));
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'blocked'));
ALTER TABLE users ADD COLUMN blocked_reason TEXT;
ALTER TABLE users ADD COLUMN clerk_user_id TEXT;
ALTER TABLE users ADD COLUMN last_seen_at INTEGER;

-- Every user that exists before this migration is a local password owner.
UPDATE users SET role = 'admin';

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_clerk_user_id ON users(clerk_user_id) WHERE clerk_user_id IS NOT NULL;

-- Which tool groups a member may use: an upstream server name, or a built-in
-- data group (memory, lake, events, notes, skills). Admins need no rows.
-- The meta-tools and the inbox/session tools are always available.
CREATE TABLE IF NOT EXISTS user_server_access (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    server     TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    created_by TEXT,
    PRIMARY KEY (user_id, server)
);
