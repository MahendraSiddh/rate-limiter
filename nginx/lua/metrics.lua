-- ratelimiter/metrics.lua
-- Prometheus metrics endpoint for OpenResty
--
-- Exposes counters: requests_total, requests_blocked, latency histogram, etc.

local _M = {}

function _M.collect()
    ngx.header.content_type = "text/plain; charset=utf-8"
    -- TODO: integrate lua-resty-prometheus and expose counters
    ngx.say("# HELP ratelimiter_requests_total Total requests processed")
    ngx.say("# TYPE ratelimiter_requests_total counter")
    ngx.say("ratelimiter_requests_total 0")
end

_M.collect()

return _M
