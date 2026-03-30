-- ─────────────────────────────────────────────────────────────
--  counter.lua — Atomic per-IP sliding-window counters
--
--  Uses ngx.shared.dict "ip_state" with expiry-based keys:
--    ip:{ip}:1s   (TTL 1 s)
--    ip:{ip}:10s  (TTL 10 s)
--    ip:{ip}:60s  (TTL 60 s)
--
--  All operations are lock-free via the `incr` init/init_ttl
--  variant available since OpenResty 1.15.8.1.
-- ─────────────────────────────────────────────────────────────

local _M = {}

-- Window definitions: suffix → TTL in seconds
local WINDOWS = {
    { suffix = "1s",  ttl = 1  },
    { suffix = "10s", ttl = 10 },
    { suffix = "60s", ttl = 60 },
}

local ip_state = ngx.shared.ip_state

--- Atomically increment counters for all three sliding windows.
-- @param ip  string  The client IP address.
-- @return table  { req_rate_1s, req_rate_10s, req_rate_60s }
function _M.increment(ip)
    local rates = {}

    for i = 1, #WINDOWS do
        local w   = WINDOWS[i]
        local key = "ip:" .. ip .. ":" .. w.suffix

        -- incr(key, value, init, init_ttl)
        -- If key does not exist, it is created with init (0) and TTL init_ttl.
        -- Returns the new value after increment.
        local new_val, err = ip_state:incr(key, 1, 0, w.ttl)

        if not new_val then
            ngx.log(ngx.WARN, "counter incr failed for ", key, ": ", err)
            rates[i] = 0
        else
            rates[i] = new_val
        end
    end

    return {
        req_rate_1s  = rates[1],
        req_rate_10s = rates[2],
        req_rate_60s = rates[3],
    }
end

--- Read current counter values without incrementing.
-- @param ip  string  The client IP address.
-- @return table  { req_rate_1s, req_rate_10s, req_rate_60s }
function _M.get_rates(ip)
    local rates = {}

    for i = 1, #WINDOWS do
        local w   = WINDOWS[i]
        local key = "ip:" .. ip .. ":" .. w.suffix
        local val = ip_state:get(key)
        rates[i]  = val or 0
    end

    return {
        req_rate_1s  = rates[1],
        req_rate_10s = rates[2],
        req_rate_60s = rates[3],
    }
end

return _M
