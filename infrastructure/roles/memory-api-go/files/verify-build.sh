#!/bin/bash
# Checks that a downloaded build file was built by this repository's GitHub
# Actions workflow (LLM-976). CI attests every published build file
# (actions/attest, SLSA build provenance, signed through Sigstore); this
# fetches the attestations for the file's SHA-256 from GitHub's public API (no
# token needed; CI sends one for the rate limit, see below), and verifies them
# offline with cosign.
#
# Usage: verify-build.sh <file> <signer> <commit>
#   <signer> is the workflow the signing certificate must name, for example
#   https://github.com/jeffdafoe/llm-memory-api/.github/workflows/ci.yml@refs/heads/main
#   <commit> is the full SHA of the commit being deployed. The certificate's
#   workflow run must have built that commit, from a push: every per-commit
#   build has the same signer, so the signer alone would accept a genuine
#   build of another commit.
#
# Exits 0 when one attestation of the file verifies with that signer and
# commit, and 1 otherwise. VERIFY_REPO (default jeffdafoe/llm-memory-api) and COSIGN
# (default cosign) can be set for tests. Runs in the deploy
# (roles/memory-api-go) and in CI (the verify-build job in ci.yml).
set -euo pipefail

if [ "$#" -ne 3 ]; then
    echo "usage: verify-build.sh <file> <signer> <commit>" >&2
    exit 1
fi
file=$1
signer=$2
commit=$3
if ! [[ "$commit" =~ ^[0-9a-f]{40}$ ]]; then
    echo "verify-build: '$commit' is not a full commit SHA" >&2
    exit 1
fi
repo=${VERIFY_REPO:-jeffdafoe/llm-memory-api}
cosign=${COSIGN:-cosign}

digest=$(sha256sum "$file" | cut -d' ' -f1)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# GH_TOKEN, when set, goes with the request. CI sets it (the job's token,
# with attestations: read): a hosted runner shares its IP with other jobs,
# and the anonymous limit of 60 requests an hour per IP ran out there. The
# deploy sets none and stays anonymous. The token only lifts the limit;
# trust still comes from cosign's check of the signature below.
auth=()
if [ -n "${GH_TOKEN:-}" ]; then
    auth=(-H "Authorization: Bearer $GH_TOKEN")
fi
# A 404 means GitHub holds no attestation for this digest: the file is not
# one CI built, or was changed after it. Any other failure (a 403 from the
# rate limit, a 5xx, no network) prints the status and the rate-limit
# headers, so it is not taken for a missing attestation.
status=$(curl -sS --max-time 30 \
        -H 'Accept: application/vnd.github+json' \
        ${auth[@]+"${auth[@]}"} \
        -D "$work/headers" -o "$work/attestations.json" -w '%{http_code}' \
        "https://api.github.com/repos/$repo/attestations/sha256:$digest") || status=000
if [ "$status" = 404 ]; then
    echo "verify-build: no attestation found for $file (sha256:$digest)" >&2
    exit 1
fi
if [ "$status" != 200 ]; then
    echo "verify-build: attestation lookup for $file (sha256:$digest) failed: HTTP $status" >&2
    grep -iE '^x-ratelimit-(limit|remaining|reset|resource):' "$work/headers" >&2 2>/dev/null || true
    head -c 500 "$work/attestations.json" >&2 2>/dev/null || true
    echo >&2
    exit 1
fi

# Each attestation's bundle goes to its own file for cosign.
python3 - "$work" <<'PYTHON'
import json
import sys

work = sys.argv[1]
with open(work + "/attestations.json", encoding="utf-8") as f:
    attestations = json.load(f).get("attestations") or []
for i, attestation in enumerate(attestations):
    with open("%s/bundle-%d.json" % (work, i), "w", encoding="utf-8") as f:
        json.dump(attestation["bundle"], f)
PYTHON

for bundle in "$work"/bundle-*.json; do
    [ -e "$bundle" ] || break
    if "$cosign" verify-blob-attestation \
            --bundle "$bundle" \
            --certificate-identity "$signer" \
            --certificate-oidc-issuer https://token.actions.githubusercontent.com \
            --certificate-github-workflow-sha "$commit" \
            --certificate-github-workflow-trigger push \
            --type slsaprovenance1 \
            "$file" >"$work/cosign.log" 2>&1; then
        echo "verify-build: $file (sha256:$digest) verified, signed by $signer for commit $commit"
        exit 0
    fi
done

echo "verify-build: no attestation of $file (sha256:$digest) verifies with signer $signer for commit $commit" >&2
cat "$work/cosign.log" >&2 2>/dev/null || true
exit 1
