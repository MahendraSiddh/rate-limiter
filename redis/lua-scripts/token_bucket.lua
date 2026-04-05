-- ─────────────────────────────────────────────────────────────────────────────
-- lua-scripts/token_bucket.lua
--
-- Atomic sliding-window rate limiter backed by a Redis Sorted Set.
--
-- Each element in the sorted set represents one accepted request.
-- The score is the request's Unix timestamp (fractional seconds supported).
-- Expired entries (outside the current window) are pruned on every call.
--
-- KEYS[1]  = bucket key, e.g. "bucket:{192.168.1.1}"
--            Hash tags ({...}) guarantee the key is on a single cluster slot
--            when used with pipeline-friendly EVALSHA routing.
--
-- ARGV[1]  = max_tokens   (integer) – max requests allowed per window
-- ARGV[2]  = window_seconds (integer) – sliding window width in seconds
-- ARGV[3]  = current_timestamp (float / integer, Unix epoch seconds)
--
-- Returns:
--   1  → request ALLOWED   (token consumed, counter incremented)
--   0  → request DENIED    (bucket full, no token consumed)
--
-- Complexity: O(log N + M) where N = window occupancy, M = expired entries
-- ─────────────────────────────────────────────────────────────────────────────

local key            = KEYS[1]
local max_tokens     = tonumber(ARGV[1])
local window_seconds = tonumber(ARGV[2])
local now            = tonumber(ARGV[3])

-- ── 1. Remove all entries outside the current sliding window ─────────────────
--
-- Any entry with score < (now - window_seconds) is older than the window and
-- no longer counts toward the rate limit.

local window_start = now - window_seconds
redis.call("ZREMRANGEBYSCORE", key, "-inf", window_start)

-- ── 2. Count requests still inside the window ────────────────────────────────

local current_count = redis.call("ZCARD", key)

-- ── 3. Decide allow / deny ───────────────────────────────────────────────────

if current_count < max_tokens then
    -- Under the limit — admit the request.
    --
    -- Use a unique member name: "<timestamp>-<random>" to prevent hash
    -- collisions when multiple requests arrive within the same millisecond.
    -- redis.call("TIME") returns {seconds, microseconds}; we combine them
    -- for a monotonically increasing, unique member per call.
    local time_parts   = redis.call("TIME")         -- {sec, usec}
    local unique_ts    = time_parts[1] .. "." .. time_parts[2]
    local member       = unique_ts                  -- score ≈ member for debug

    -- Score = current timestamp so ZRANGEBYSCORE can expire it later.
    redis.call("ZADD", key, now, member)

    -- Refresh TTL so the key self-expires after the window even when idle.
    redis.call("EXPIRE", key, window_seconds)

    return 1   -- ALLOW
else
    -- Bucket full — still refresh TTL to prevent zombie keys on busy IPs.
    redis.call("EXPIRE", key, window_seconds)

    return 0   -- DENY
end
