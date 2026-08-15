-- Queries for expected state (the part that MUST be backed up).
-- See docs/design/07-persistence.md section 1.7.
--
-- IMPORTANT: comments in this file must be ASCII only.
-- sqlc strips comments by byte offset; multi-byte characters shift those
-- offsets and silently corrupt the generated SQL (it produced identifiers
-- like "INSEid" and "GetC" before we found this). The Chinese rationale for
-- each query lives in the Go repository layer instead.
-- A unit test enforces the ASCII constraint.

-- -- sites ---------------------------------------------------------------

-- name: CreateSite :one
INSERT INTO sites (name, kind, labels, created_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetSiteByName :one
SELECT * FROM sites WHERE name = ?;

-- name: GetSite :one
SELECT * FROM sites WHERE id = ?;

-- name: ListSites :many
SELECT * FROM sites ORDER BY name;

-- -- nodes ---------------------------------------------------------------

-- name: UpsertNode :one
INSERT INTO nodes (site_id, name, address, labels, roots, volumes, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (site_id, name) DO UPDATE SET
    address = excluded.address,
    labels  = excluded.labels,
    roots   = excluded.roots,
    volumes = excluded.volumes,
    status  = excluded.status
RETURNING *;

-- name: InsertNode :one
-- Plain insert, no ON CONFLICT: fails with SQLITE_CONSTRAINT_UNIQUE when
-- (site_id, name) is already taken. Used by the join claim, which must
-- reject rather than silently overwrite an existing identity.
INSERT INTO nodes (site_id, name, address, labels, roots, volumes, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ClaimReservedNode :one
-- Turns a "node add"-only row (status='reserved', no cert ever issued) into
-- a claimed one, in place. Only matches rows still in 'reserved' status, so
-- it cannot be used to steal an identity that already has a cert.
-- Zero rows matched surfaces to Go as sql.ErrNoRows.
UPDATE nodes SET address = ?, status = ?
WHERE site_id = ? AND name = ? AND status = 'reserved'
RETURNING *;

-- name: GetNodeByName :one
SELECT * FROM nodes WHERE site_id = ? AND name = ?;

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: ListNodes :many
SELECT * FROM nodes WHERE site_id = ? ORDER BY name;

-- name: SetNodeStatus :exec
UPDATE nodes SET status = ? WHERE id = ?;

-- -- components ----------------------------------------------------------

-- name: CreateComponent :one
INSERT INTO components (
    site_id, name, pack_name, pack_version, pack_revision,
    profile, params, drift_policy, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetComponentByName :one
SELECT * FROM components WHERE site_id = ? AND name = ?;

-- name: GetComponent :one
SELECT * FROM components WHERE id = ?;

-- name: ListComponents :many
SELECT * FROM components WHERE site_id = ? ORDER BY name;

-- name: UpdateComponent :one
UPDATE components SET
    pack_version      = ?,
    pack_revision     = ?,
    profile           = ?,
    params            = ?,
    drift_policy      = ?,
    previous_version  = ?,
    previous_revision = ?,
    updated_at        = ?
WHERE id = ?
RETURNING *;

-- name: DeleteComponent :exec
DELETE FROM components WHERE id = ?;

-- -- config_groups -------------------------------------------------------

-- name: UpsertConfigGroup :one
INSERT INTO config_groups (component_id, role, name, members, params, paths)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (component_id, role, name) DO UPDATE SET
    members = excluded.members,
    params  = excluded.params,
    paths   = excluded.paths
RETURNING *;

-- name: ListConfigGroups :many
SELECT * FROM config_groups WHERE component_id = ? ORDER BY role, name;

-- name: GetConfigGroup :one
SELECT * FROM config_groups
WHERE component_id = ? AND role = ? AND name = ?;

-- name: DeleteConfigGroup :exec
DELETE FROM config_groups WHERE component_id = ? AND role = ? AND name = ?;

-- -- role_instances ------------------------------------------------------

-- name: ListRoleInstances :many
SELECT * FROM role_instances WHERE component_id = ? ORDER BY role, ordinal;

-- name: ListRoleInstancesByRole :many
SELECT * FROM role_instances
WHERE component_id = ? AND role = ?
ORDER BY ordinal;

-- name: GetRoleInstance :one
SELECT * FROM role_instances
WHERE component_id = ? AND role = ? AND node_id = ?;

-- name: MaxOrdinal :one
-- Returns -1 when the role has no instances, so the first one gets 0.
-- Do NOT use count(): ordinals may have holes and count() would collide
-- with an already-used number. See ADR-0028.
SELECT COALESCE(MAX(ordinal), -1) AS max_ordinal
FROM role_instances
WHERE component_id = ? AND role = ?;

-- name: CreateRoleInstance :one
INSERT INTO role_instances (component_id, role, node_id, ordinal, config_group_id, created_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: SetRoleInstanceConfigGroup :exec
UPDATE role_instances SET config_group_id = ? WHERE id = ?;

-- name: DeleteRoleInstance :exec
DELETE FROM role_instances WHERE id = ?;

-- -- pack_bindings -------------------------------------------------------

-- name: GetPackBinding :one
SELECT * FROM pack_bindings WHERE component_id = ? AND require_name = ?;

-- name: ListPackBindings :many
SELECT * FROM pack_bindings WHERE component_id = ? ORDER BY require_name;

-- name: CreatePackBinding :one
INSERT INTO pack_bindings (component_id, require_name, bound_component_id, created_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: CountDependents :one
SELECT count(*) AS n FROM pack_bindings WHERE bound_component_id = ?;

-- name: ListDependents :many
-- Names the components that depend on this one, plus which require slot binds
-- them. A bare count cannot answer the only question the user actually has
-- when a removal is refused: which ones, and do I still need them.
SELECT c.name AS component, b.require_name
FROM pack_bindings b
JOIN components c ON c.id = b.component_id
WHERE b.bound_component_id = ?
ORDER BY c.name, b.require_name;

-- -- secrets -------------------------------------------------------------

-- name: GetSecret :one
SELECT * FROM secrets WHERE component_id = ? AND param = ?;

-- name: ListSecrets :many
SELECT * FROM secrets WHERE component_id = ? ORDER BY param;

-- name: PutSecret :one
-- First write stores version=1; rotation bumps it. The version participates
-- in the spec digest, so rotation necessarily produces a new generation.
-- See docs/design/16-secrets.md section 5.
INSERT INTO secrets (component_id, param, version, ciphertext, created_at, updated_at)
VALUES (?, ?, 1, ?, ?, ?)
ON CONFLICT (component_id, param) DO UPDATE SET
    version    = secrets.version + 1,
    ciphertext = excluded.ciphertext,
    updated_at = excluded.updated_at
RETURNING *;

-- -- vault_keys ----------------------------------------------------------

-- name: GetWrappedDEK :one
SELECT * FROM vault_keys WHERE id = 1;

-- name: PutWrappedDEK :exec
INSERT INTO vault_keys (id, wrapped_dek, created_at)
VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET wrapped_dek = excluded.wrapped_dek;

-- name: SetRoleInstanceFacts :exec
UPDATE role_instances SET facts = ? WHERE id = ?;

-- name: SetRoleInstanceRunState :exec
UPDATE role_instances SET run_state = ? WHERE id = ?;

-- name: SetComponentRunState :execrows
UPDATE role_instances SET run_state = ? WHERE component_id = ?;

-- -- join_tokens ---------------------------------------------------------

-- name: CreateJoinToken :one
INSERT INTO join_tokens (
    site_id, token_hash, node_name, expires_at, max_uses, created_at, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetJoinTokenByHash :one
SELECT * FROM join_tokens WHERE token_hash = ?;

-- name: ListJoinTokens :many
SELECT * FROM join_tokens WHERE site_id = ? ORDER BY id DESC;

-- name: UseJoinTokenIfAvailable :execrows
UPDATE join_tokens
SET used = used + 1
WHERE id = ? AND used < max_uses AND revoked_at IS NULL AND expires_at > ?;

-- name: RevokeJoinToken :exec
UPDATE join_tokens SET revoked_at = ? WHERE id = ?;

-- name: DeleteNode :exec
DELETE FROM nodes WHERE id = ?;

-- name: ListRoleInstancesByNode :many
SELECT * FROM role_instances WHERE node_id = ?;

-- name: SetNodeRevoked :exec
UPDATE nodes SET revoked_at = ? WHERE id = ?;

-- name: SetNodeCordoned :exec
UPDATE nodes SET cordoned_at = ? WHERE id = ?;

-- name: SetComponentRollout :exec
UPDATE components
SET rollout_max_unavailable = ?, rollout_canary = ?, updated_at = ?
WHERE id = ?;

-- name: StartComponentRemoval :exec
-- Writes the lifecycle state and the three switches in one statement; see
-- the Go wrapper for why they must not be written separately.
--
-- The state is a bound parameter, not a literal: sqlc 1.31 STRIPS THE QUOTES
-- off a string literal on the right-hand side of an UPDATE ... SET, turning
-- SET state = removing into SET state = removing -- a column reference.
-- It exits 0 while doing this. The failure only shows up at runtime as
-- "no such column: removing".
UPDATE components
SET state = ?,
    removal_keep_config = ?, removal_purge_data = ?, removal_purge_user = ?,
    removing_at = ?, updated_at = ?
WHERE id = ?;

-- -- users --------------------------------------------------------------

-- name: CreateUser :one
INSERT INTO users (name, password_hash, created_at, updated_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetUserByName :one
SELECT * FROM users WHERE name = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY name;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = ?, updated_at = ? WHERE name = ?;

-- name: DeleteUser :exec
DELETE FROM users WHERE name = ?;

-- -- sessions -----------------------------------------------------------

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_name, created_at, expires_at)
VALUES (?, ?, ?, ?);

-- name: GetSession :one
SELECT * FROM sessions WHERE token_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteSessionsOfUser :exec
DELETE FROM sessions WHERE user_name = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;
