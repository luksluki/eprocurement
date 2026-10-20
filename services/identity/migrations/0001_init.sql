-- 0001_init: identity schema — users, roles, refresh tokens.
-- Owned by the identity service (ADR-0001); no other service writes here.

CREATE TABLE identity.users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL,
    email         text NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    -- argon2id hash; mandatory from the first user write (login lands in a later issue)
    password_hash text NOT NULL,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Email is unique per tenant, case-insensitive: two tenants may share an address.
CREATE UNIQUE INDEX users_tenant_email_uidx ON identity.users (tenant_id, lower(email));
CREATE INDEX users_tenant_idx ON identity.users (tenant_id);

CREATE TABLE identity.roles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code        text NOT NULL UNIQUE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Fixed RBAC catalog from the PRD (FR-01, §7).
INSERT INTO identity.roles (code, name) VALUES
    ('admin',    'Administrator'),
    ('requester','Requester'),
    ('approver', 'Approver'),
    ('buyer',    'Buyer'),
    ('finance',  'Finance'),
    ('viewer',   'Viewer')
ON CONFLICT (code) DO NOTHING;

CREATE TABLE identity.user_roles (
    user_id    uuid NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,
    role_id    uuid NOT NULL REFERENCES identity.roles (id) ON DELETE CASCADE,
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role_id)
);

CREATE INDEX user_roles_role_idx ON identity.user_roles (role_id);

CREATE TABLE identity.refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,
    -- sha256 of the opaque token; the raw value only exists client-side
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    -- rotation support: set when this token was exchanged for a successor
    rotated_to uuid REFERENCES identity.refresh_tokens (id),
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

CREATE INDEX refresh_tokens_user_idx ON identity.refresh_tokens (user_id);
CREATE INDEX refresh_tokens_expires_idx ON identity.refresh_tokens (expires_at);
