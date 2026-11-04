-- 0013_outbox_actor_ip: the client IP of the acting user, recorded on every
-- outbox row (issue #44, PRD §14.7 "actor + trace" audit trail). The value
-- flows gateway (fiber c.IP() behind the trusted-proxy check) -> gRPC
-- metadata x-actor-ip -> request context -> outbox rows -> the §14.5
-- envelope (the documented envelope delta, docs/schema/procurement.md) ->
-- the Mongo events/audit_log projections. It is a display/attribution
-- field, not an authentication claim: actor_id (the JWT subject) stays the
-- authoritative attribution, which is why a spoofable IP is acceptable to
-- store verbatim (see docs/schema/documents.md).
--
-- NOT NULL DEFAULT '' keeps existing rows readable (they pre-date the
-- column) and mirrors trace_id's shape: empty means "unknown", never NULL.

ALTER TABLE procurement.outbox
    ADD COLUMN actor_ip text NOT NULL DEFAULT '';
