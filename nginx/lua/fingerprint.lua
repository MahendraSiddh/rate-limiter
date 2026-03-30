-- ─────────────────────────────────────────────────────────────
--  fingerprint.lua — 12-field feature vector + decision engine
--
--  Extracts a per-request fingerprint, serialises it as JSON,
--  sends it to the Go decision engine over a Unix domain socket
--  (/tmp/decision.sock), and acts on the response:
--
--    "allow"    → pass request to upstream
--    "throttle" → 429 + Retry-After header
--    "block"    → 403 Forbidden
--
--  FAIL-OPEN: if the engine is unreachable or times out (50 ms
--  ceiling), the request is allowed through.
-- ─────────────────────────────────────────────────────────────

local cjson   = require "cjson.safe"
local counter = require "counter"

local _M = {}

-- ── Constants ──────────────────────────────────────────────
local SOCKET_PATH     = "unix:/tmp/decision.sock"
local CONNECT_TIMEOUT = 50   -- ms
local SEND_TIMEOUT    = 50   -- ms
local READ_TIMEOUT    = 50   -- ms

-- ── Helpers ────────────────────────────────────────────────

--- Build the 12-field feature vector for the current request.
-- @return table
local function build_vector()
    local headers = ngx.req.get_headers()

    -- 1. client_ip
    local client_ip = ngx.var.remote_addr or "0.0.0.0"

    -- 2. ja3_hash — populated by OpenResty's ssl_client_hello phase
    --    Falls back to "none" for plain-HTTP requests.
    local ja3_hash = ngx.var.http_x_ja3_hash
                  or ngx.var.ssl_ja3_hash
                  or "none"

    -- 3. user_agent_hash (MD5)
    local ua = headers["user-agent"] or ""
    local user_agent_hash = ngx.md5(ua)

    -- 4-6. Sliding-window request rates
    local rates = counter.get_rates(client_ip)

    -- 7. http_method
    local http_method = ngx.var.request_method or "GET"

    -- 8. uri_hash (MD5)
    local uri_hash = ngx.md5(ngx.var.uri or "/")

    -- 9. content_length
    local content_length = tonumber(headers["content-length"]) or 0

    -- 10. header_count
    local header_count = 0
    for _ in pairs(headers) do
        header_count = header_count + 1
    end

    -- 11. is_http2  (boolean → 1 / 0)
    local proto  = ngx.var.server_protocol or ""
    local is_http2 = (proto == "HTTP/2.0" or proto == "HTTP/2") and 1 or 0

    -- 12. unix_timestamp
    local unix_timestamp = ngx.time()

    return {
        client_ip       = client_ip,
        ja3_hash        = ja3_hash,
        user_agent_hash = user_agent_hash,
        req_rate_1s     = rates.req_rate_1s,
        req_rate_10s    = rates.req_rate_10s,
        req_rate_60s    = rates.req_rate_60s,
        http_method     = http_method,
        uri_hash        = uri_hash,
        content_length  = content_length,
        header_count    = header_count,
        is_http2        = is_http2,
        unix_timestamp  = unix_timestamp,
    }
end

--- Send the vector to the decision engine and return the raw
--  response body, or nil + error string on failure.
-- @param payload string  JSON-encoded fingerprint
-- @return string|nil, string|nil
local function query_engine(payload)
    local sock = ngx.socket.tcp()

    sock:settimeouts(CONNECT_TIMEOUT, SEND_TIMEOUT, READ_TIMEOUT)

    local ok, err = sock:connect(SOCKET_PATH)
    if not ok then
        return nil, "connect: " .. (err or "unknown")
    end

    -- Protocol: newline-terminated JSON
    local bytes, send_err = sock:send(payload .. "\n")
    if not bytes then
        sock:close()
        return nil, "send: " .. (send_err or "unknown")
    end

    local line, read_err = sock:receive("*l")
    if not line then
        sock:close()
        return nil, "read: " .. (read_err or "unknown")
    end

    sock:setkeepalive(60000, 32)  -- pool idle 60 s, pool size 32

    return line, nil
end

-- ── Public API ─────────────────────────────────────────────

--- Evaluate the current request against the decision engine.
--  Must be called in the access_by_lua phase.
function _M.evaluate()
    -- 1. Build feature vector
    local vector = build_vector()

    -- 2. Serialise
    local payload, encode_err = cjson.encode(vector)
    if not payload then
        ngx.log(ngx.ERR, "fingerprint: JSON encode failed: ", encode_err)
        return  -- fail-open
    end

    -- 3. Query decision engine
    local resp_body, query_err = query_engine(payload)
    if not resp_body then
        ngx.log(ngx.WARN, "fingerprint: decision engine unreachable: ", query_err,
                " — failing open for ", vector.client_ip)
        return  -- fail-open
    end

    -- 4. Parse response
    local resp, decode_err = cjson.decode(resp_body)
    if not resp then
        ngx.log(ngx.ERR, "fingerprint: JSON decode failed: ", decode_err,
                " — raw: ", resp_body)
        return  -- fail-open
    end

    local action = resp.action

    -- 5. Act on decision
    if action == "block" then
        ngx.log(ngx.WARN, "BLOCKED ", vector.client_ip,
                " ua=", vector.user_agent_hash)
        ngx.exit(ngx.HTTP_FORBIDDEN)        -- 403

    elseif action == "throttle" then
        local retry_after = tonumber(resp.retry_after) or 5
        ngx.header["Retry-After"] = retry_after
        ngx.log(ngx.NOTICE, "THROTTLED ", vector.client_ip,
                " retry_after=", retry_after)
        ngx.exit(429)                        -- 429 Too Many Requests
    end

    -- action == "allow" (or anything unexpected) → pass through
end

return _M
