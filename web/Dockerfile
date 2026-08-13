# Build context can be just web/ (unlike agent/ingest/api, this doesn't
# need /proto):
#   docker build -f web/Dockerfile -t sentry-web web/

FROM node:22-alpine AS builder
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
# VITE_API_BASE_URL is baked in at build time — this is a prerendered
# static site, not a server. Override with --build-arg for non-default
# deployments.
ARG VITE_API_BASE_URL=http://localhost:8080
ENV VITE_API_BASE_URL=${VITE_API_BASE_URL}
RUN npm run build

# Not distroless: serving a static SPA needs *some* HTTP server, and
# nginx:alpine is the boring, well-understood choice for that job — a
# custom static-file-serving binary would be more engineering than a
# Phase 0 placeholder page warrants. See /web/README.md.
FROM nginx:alpine
COPY --from=builder /src/build /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 3000
