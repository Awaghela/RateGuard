FROM caddy:2-alpine
COPY railway/Caddyfile /etc/caddy/Caddyfile
COPY deploy/dashboard /srv/dashboard
