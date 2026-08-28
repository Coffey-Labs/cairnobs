#!/usr/bin/env bash
# Keeps web/nginx.conf's route allowlists in sync with web/src/routes.
#
# web/nginx.conf answers 404 for any path that isn't a route (see the "404
# enforcement" block there for why). Most routes need no help from nginx:
# adapter-static prerenders them to a flat <route>.html that try_files
# matches on disk. Two kinds don't, and are named explicitly in nginx.conf
# because there is no file for them to match:
#
#   1. Dynamic routes -- src/routes/<seg>/[param] -- listed in the
#      alternation of nginx.conf's dynamic-route `location ~` regex.
#   2. Routes that never opted into prerendering (no `export const
#      prerender = true`) -- each needs its own `location = /<route>`.
#
# Both lists are hand-maintained, and getting them wrong fails *only in
# production*: `vite dev` and `npm run preview` route from the client
# manifest and never consult nginx.conf, so a new dynamic route works
# perfectly in dev and 404s the moment it ships. This script IS the
# enforcement for that. Run in CI on every change; exits non-zero, naming
# the route and the edit to make, on a mismatch.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

ROUTES_DIR="web/src/routes"
NGINX_CONF="web/nginx.conf"
fail=0

# --- what the app actually declares -----------------------------------
declare -a dynamic_segments=() unprerendered_routes=() odd_shaped=()

while IFS= read -r page; do
    dir="$(dirname "$page")"
    route="${dir#"$ROUTES_DIR"}"     # "" for the root route, else /foo/bar
    route="${route:-/}"

    if [[ "$route" == *"["* ]]; then
        # nginx's regex covers exactly /<segment>/<one-param>. A rest
        # param, an optional param, a param at the top level, or a param
        # nested deeper all need a *different* regex, not another entry in
        # the alternation -- so flag rather than silently "fix" them.
        if [[ "$route" =~ ^/([^/[]+)/\[[^./][^]]*\]$ ]]; then
            dynamic_segments+=("${BASH_REMATCH[1]}")
        else
            odd_shaped+=("$route")
        fi
        continue
    fi

    # Non-dynamic: prerendered routes land on disk and need no nginx entry.
    if [[ -f "$dir/+page.ts" ]] && \
       grep -Eq '^[[:space:]]*export[[:space:]]+const[[:space:]]+prerender[[:space:]]*=[[:space:]]*true' "$dir/+page.ts"; then
        continue
    fi
    # "/" is handled by its own `location = /` and is never fallback-served.
    [[ "$route" == "/" ]] && continue
    unprerendered_routes+=("$route")
done < <(find "$ROUTES_DIR" -name '+page.svelte' | sort)

# --- what nginx.conf claims -------------------------------------------
# The alternation out of `location ~ ^/(?:a|b|c)/[^/]+$ {`.
nginx_segments="$(grep -oE 'location[[:space:]]+~[[:space:]]+\^/\(\?:[^)]+\)/\[\^/\]\+\$' "$NGINX_CONF" \
    | sed -E 's#.*\(\?:([^)]+)\).*#\1#' | tr '|' '\n' | sort -u || true)"
# Every `location = /foo` except the root and the fallback shell itself.
nginx_exact="$(grep -oE 'location[[:space:]]+=[[:space:]]+/[A-Za-z0-9._/-]*' "$NGINX_CONF" \
    | sed -E 's#.*=[[:space:]]+##' | grep -vx '/' | sort -u || true)"

sorted() { printf '%s\n' "$@" | grep -v '^$' | sort -u; }
app_segments="$(sorted "${dynamic_segments[@]+"${dynamic_segments[@]}"}")"
app_exact="$(sorted "${unprerendered_routes[@]+"${unprerendered_routes[@]}"}")"

# --- compare -----------------------------------------------------------
if ((${#odd_shaped[@]})); then
    echo "FAIL: route param shape nginx.conf's regex does not cover:"
    printf '  %s\n' "${odd_shaped[@]}"
    echo "  nginx.conf matches only /<segment>/<single param>. Widen the"
    echo "  dynamic-route location regex there, then update this check."
    fail=1
fi

echo "Checking: dynamic routes are in nginx.conf's fallback allowlist..."
if [[ "$app_segments" != "$nginx_segments" ]]; then
    echo "FAIL: $ROUTES_DIR and $NGINX_CONF disagree on dynamic routes."
    comm -23 <(echo "$app_segments") <(echo "$nginx_segments") \
        | sed 's#^#  missing from nginx.conf (would 404 in prod): /#'
    comm -13 <(echo "$app_segments") <(echo "$nginx_segments") \
        | sed 's#^#  stale in nginx.conf (route no longer exists): /#'
    echo "  Fix: edit the alternation in nginx.conf's dynamic-route location."
    fail=1
else
    echo "OK: dynamic routes match (${app_segments//$'\n'/, })"
fi

echo "Checking: non-prerendered routes have their own nginx.conf location..."
if [[ "$app_exact" != "$nginx_exact" ]]; then
    echo "FAIL: $ROUTES_DIR and $NGINX_CONF disagree on non-prerendered routes."
    comm -23 <(echo "$app_exact") <(echo "$nginx_exact") \
        | sed 's#^#  missing from nginx.conf (would 404 in prod): #'
    comm -13 <(echo "$app_exact") <(echo "$nginx_exact") \
        | sed 's#^#  stale in nginx.conf (route is prerendered or gone): #'
    echo "  Fix: add/remove a \`location = <route> { try_files /200.html =404; }\`"
    echo "  in nginx.conf -- or give the route an \`export const prerender = true\`."
    fail=1
else
    echo "OK: non-prerendered routes match (${app_exact//$'\n'/, })"
fi

exit "$fail"
