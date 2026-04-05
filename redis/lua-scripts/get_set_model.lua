-- ─────────────────────────────────────────────────────────────────────────────
-- lua-scripts/get_set_model.lua
--
-- Atomic check-and-return for per-client ML model bytes stored in Redis.
--
-- The ML Sidecar serialises its per-client anomaly model (ONNX bytes or a
-- compact feature vector snapshot) into Redis so that:
--   1. On restart, it can reload the latest per-client model without calling
--      the training pipeline.
--   2. Multiple ML Sidecar replicas share a single source of truth.
--
-- KEYS[1]  = model key, e.g. "model:{client_id}"
--            Hash-tagged so all keys for the same client land on one shard.
--
-- ARGV[1]  = (unused by this script; reserved for future caller-supplied TTL
--            override. Set to "0" or "" when not needed.)
--
-- Behaviour:
--   • Key MISSING  → returns nil (false).
--                    The caller must load the baseline model from disk/DB.
--   • Key EXISTS   → returns a two-element array:
--                      [1] = raw bytes (bulk string) of the stored model
--                      [2] = remaining TTL in seconds (integer, -1 = no TTL)
--
-- Why atomic?
--   Without atomicity a concurrent writer could SET the key between our EXISTS
--   check and our GET+TTL fetch, causing the caller to load a stale baseline
--   and immediately overwrite the fresh model.  A single Lua execution is
--   guaranteed to be atomic within Redis.
--
-- Returns:
--   nil / false           → key absent, load baseline
--   {bytes, ttl_seconds}  → key present, use stored model
-- ─────────────────────────────────────────────────────────────────────────────

local key = KEYS[1]

-- ── 1. Check existence ───────────────────────────────────────────────────────

if redis.call("EXISTS", key) == 0 then
    -- Key not found — signal caller to load the baseline model.
    return false
end

-- ── 2. Fetch model bytes and remaining TTL atomically ────────────────────────
--
-- GET returns the raw binary blob (safe for ONNX bytes via RESP3 or base64
-- encoded strings if the caller normalizes encoding at the application layer).
--
-- TTL returns:
--   -2 → key does not exist (shouldn't happen here, but guard anyway)
--   -1 → key exists with no expiry
--   N  → seconds remaining

local model_bytes    = redis.call("GET", key)
local ttl_remaining  = redis.call("TTL", key)

-- ── 3. Guard: if key vanished between EXISTS and GET (very unlikely race) ────

if not model_bytes then
    return false
end

return { model_bytes, ttl_remaining }
