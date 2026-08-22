# Build context can be just web/ (unlike agent/ingest/api, this doesn't
# need /proto):
#   docker build -f web/Dockerfile -t cairnobs-web web/

FROM node:22-alpine AS builder
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
# All three VITE_* vars are baked in at build time — this is a
# prerendered static site, not a server. Override with --build-arg for
# non-default deployments. Each one needs its own ARG: an undeclared
# --build-arg from docker-compose.yml's build.args is silently dropped
# by Docker, not an error -- confirmed the hard way when
# VITE_ALERTING_API_BASE_URL/VITE_ENTERPRISE_AUTH_BASE_URL were being
# passed in but only VITE_API_BASE_URL was ever declared here, so the
# other two never reached `npm run build` even though docker-compose.yml
# looked correct. VITE_ENTERPRISE_AUTH_BASE_URL has no default (unlike
# the other two) -- web/src/lib/api.ts treats it as intentionally
# undefined when unset, meaning "enterprise-auth isn't deployed", not
# "use some fallback host."
ARG VITE_API_BASE_URL=http://localhost:8080
ARG VITE_ALERTING_API_BASE_URL=http://localhost:8081
ARG VITE_ENTERPRISE_AUTH_BASE_URL
# Off by default -- local dev's plain WithCORS (wildcard
# Access-Control-Allow-Origin) can't be combined with a credentialed
# fetch at all, so api.ts must not send cookies unless the deployment
# actually turned local auth on server-side too (LOCAL_AUTH_ENABLED).
# See api.ts's requestFrom/alertingRequest doc comment.
ARG VITE_LOCAL_AUTH_ENABLED=false
# Both unset by default: the login page only prefills when it has both,
# so every deployment that doesn't opt in gets an ordinary empty form.
# See web/src/lib/api.ts's demoUsername on why a public demo can bake a
# password in and nothing else should.
ARG VITE_DEMO_USERNAME
ARG VITE_DEMO_PASSWORD
ENV VITE_API_BASE_URL=${VITE_API_BASE_URL}
ENV VITE_ALERTING_API_BASE_URL=${VITE_ALERTING_API_BASE_URL}
ENV VITE_ENTERPRISE_AUTH_BASE_URL=${VITE_ENTERPRISE_AUTH_BASE_URL}
ENV VITE_LOCAL_AUTH_ENABLED=${VITE_LOCAL_AUTH_ENABLED}
ENV VITE_DEMO_USERNAME=${VITE_DEMO_USERNAME}
ENV VITE_DEMO_PASSWORD=${VITE_DEMO_PASSWORD}
RUN npm run build

# Not distroless: serving a static SPA needs *some* HTTP server, and
# nginx:alpine is the boring, well-understood choice for that job — a
# custom static-file-serving binary would be more engineering than a
# Phase 0 placeholder page warrants. See /web/README.md.
FROM nginx:alpine
COPY --from=builder /src/build /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 3000
