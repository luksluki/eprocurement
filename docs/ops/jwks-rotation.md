# JWKS rotation runbook

How to rotate the identity service's Ed25519 signing keys without invalidating unexpired access tokens.

## Background

- Access tokens are 15-minute Ed25519 JWTs; the `kid` header names the signing key.
- `identity` serves the active public keys via the `GetJwks` gRPC RPC; the gateway caches them (`JWKS_CACHE_TTL`, default 5 min) and refreshes immediately when it sees a token with an unknown `kid`.
- The keyring has two slots: `IDENTITY_JWT_KEY` (current — signs new tokens) and `IDENTITY_JWT_KEY_PREVIOUS` (kept verifiable during the rotation window). Both are base64-encoded 32-byte seeds.

## Rotate

1. **Generate the new key** (never reuse seeds; never commit them):

   ```bash
   openssl rand -base64 32
   ```

2. **Set both slots** in the environment that runs `identity` (`.env` locally):

   ```text
   IDENTITY_JWT_KEY_PREVIOUS=<the old IDENTITY_JWT_KEY value>
   IDENTITY_JWT_KEY=<the new seed from step 1>
   ```

3. **Restart identity** (compose: `docker compose up -d --build identity` if the image changed, else `docker compose restart identity`). Startup logs list the JWKS key count; `GET http://localhost:8080/.well-known/jwks.json` must now list **two** keys — current first, previous second.

4. **Verify the rotation window**:

   - An access token issued **before** the restart still passes the gateway (its `kid` matches the previous key). The gateway picks the new key set up automatically: unknown `kid` triggers one JWKS refresh, and the TTL refreshes within 5 minutes anyway.
   - A fresh login (or any token issued after the restart) is signed with the new key.

5. **Close the window** after every pre-rotation token has expired — 15 minutes after the restart, plus a safety margin:

   ```text
   #IDENTITY_JWT_KEY_PREVIOUS=...
   ```

   Remove/comment the previous key and restart identity. `/.well-known/jwks.json` serves a single key again; any lingering pre-rotation token now fails with 401, which is correct — it is expired.

## Rules

- The previous key stays configured **only** for the 15-minute overlap; keeping it forever widens the attack surface for no benefit.
- Never configure the same seed in both slots — a previous key identical to the current one is dropped (it would be a no-op rotation serving one key).
- With no `IDENTITY_JWT_KEY` set at all, identity generates an ephemeral key per boot and logs a warning: all tokens die on every restart. Fine for local development, never for a demo.
