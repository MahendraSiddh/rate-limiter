# ─────────────────────────────────────────────────────────────
#  Dockerfile — OpenResty (Nginx + LuaJIT) rate-limiter proxy
# ─────────────────────────────────────────────────────────────
FROM openresty/openresty:1.25.3.1-alpine

# Install openssl for self-signed certificate generation
RUN apk add --no-cache openssl

# Generate a self-signed TLS certificate for dev/staging
RUN mkdir -p /etc/nginx/ssl \
    && openssl req -x509 -nodes -days 3650 \
       -subj "/C=US/ST=Dev/L=Local/O=RateLimiter/CN=localhost" \
       -newkey rsa:2048 \
       -keyout /etc/nginx/ssl/server.key \
       -out    /etc/nginx/ssl/server.crt \
    && chmod 600 /etc/nginx/ssl/server.key

# Create the shared socket directory
RUN mkdir -p /tmp

# Create conf.d directory
RUN mkdir -p /etc/nginx/conf.d

# Copy configuration
COPY nginx.conf /usr/local/openresty/nginx/conf/nginx.conf
COPY conf.d/    /etc/nginx/conf.d/

# Copy Lua modules
COPY lua/ /usr/local/openresty/lualib/ratelimiter/

EXPOSE 80 443

# Use OpenResty's default entrypoint (openresty -g 'daemon off;')
CMD ["openresty", "-g", "daemon off;"]
