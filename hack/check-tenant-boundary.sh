#!/usr/bin/env bash
# Enforces the two boundary properties /docs/phase-4-isolation-design.md
# and /docs/phase-4-rbac-design.md describe as "grep-and-review-enforced,
# not compiler-enforced" -- this script IS that enforcement. Run in CI on
# every change; both checks exit non-zero (and print the offending lines)
# on a violation.
#
# 1. No core Go code imports enterprise/ -- core must stay genuinely
#    single-tenant with zero multi-tenant mechanism present. This was
#    originally also a licensing boundary (enterprise/ was
#    commercial-licensed through Phase 5); as of Phase 6 both sides are
#    AGPLv3, so this is now purely architectural -- see
#    /docs/compliance/license-audit-report.md.
# 2. tenant.TrustFromValidatedSession is called, in non-test production
#    code, only from the auth-middleware allowlist below -- everywhere
#    else is either a mistake or a new call site that needs the same
#    scrutiny the original one got.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

fail=0

echo "Checking: no core Go package imports enterprise/..."
# Core = every top-level Go module except enterprise/ and hack/ (hack/
# tooling isn't shipped, and load-test/fixture scripts have no reason to
# import enterprise/ either, but they're not part of the architectural
# boundary claim, so they're excluded rather than asserted about).
core_hits="$(grep -rn '"github.com/cairnobs/cairnobs/enterprise' \
    --include='*.go' \
    agent ingest storage api web cli alerting 2>/dev/null || true)"
if [[ -n "$core_hits" ]]; then
    echo "FAIL: core Go code imports enterprise/ -- this must never happen:"
    echo "$core_hits"
    fail=1
else
    echo "OK: no core package imports enterprise/"
fi

echo "Checking: tenant.TrustFromValidatedSession call sites..."
# Allowlist: files permitted to call the trust constructor in non-test
# code. Update this list deliberately, one line per new legitimate
# caller, as auth middleware (task 5) and tenant provisioning (task 4+)
# land -- an addition here should get the same review a change to
# enterprise/internal/tenant/tenant.go itself would.
allowlist=(
    "enterprise/internal/tenant/tenant.go"   # the definition itself
)

hits="$(grep -rn 'tenant\.TrustFromValidatedSession(' \
    --include='*.go' \
    enterprise 2>/dev/null | grep -v '_test\.go:' || true)"

violations=""
while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    file="${line%%:*}"
    allowed=0
    for a in "${allowlist[@]}"; do
        [[ "$file" == "$a" ]] && allowed=1 && break
    done
    if [[ "$allowed" -eq 0 ]]; then
        violations+="$line"$'\n'
    fi
done <<< "$hits"

if [[ -n "$violations" ]]; then
    echo "FAIL: tenant.TrustFromValidatedSession called outside the allowlist:"
    echo "$violations"
    echo "If this is a legitimate new caller (e.g. new auth middleware), add it to"
    echo "the allowlist in this script deliberately -- don't silence this check."
    fail=1
else
    echo "OK: TrustFromValidatedSession has no unexpected call sites"
fi

if [[ "$fail" -ne 0 ]]; then
    exit 1
fi
echo "check-tenant-boundary: all checks passed"
