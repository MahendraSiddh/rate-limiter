-- ─────────────────────────────────────────────────────────────
--  access.lua — OpenResty access-phase entry point
--
--  Runs on every request. Increments per-IP sliding-window
--  counters, then evaluates the request fingerprint against
--  the Go decision engine.
-- ─────────────────────────────────────────────────────────────

local counter     = require "counter"
local fingerprint = require "fingerprint"

-- 1. Increment per-IP rate counters (1 s / 10 s / 60 s windows)
local ip = ngx.var.remote_addr
counter.increment(ip)

-- 2. Build fingerprint → query decision engine → act on verdict
fingerprint.evaluate()
