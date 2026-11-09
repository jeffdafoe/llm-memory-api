#!/bin/bash
# Checks that a downloaded build file was built by this repository's GitHub
# Actions workflow (LLM-976). CI attests every published build file
# (actions/attest, SLSA build provenance, signed through Sigstore); this
# fetches the attestations for the file's SHA-256 from GitHub's public API, no
# token needed, and verifies them offline with cosign.
#
# Usage: verify-build.sh <file> <signer>
#   <signer> is the workflow the signing certificate must name, for example
#   https://github.com/jeffdafoe/llm-memory-api/.github/workflows/ci.yml@refs/heads/main
#
# Exits 0 when one attestation of the file verifies with that signer, and 1
# otherwise. VERIFY_REPO (default jeffdafoe/llm-memory-api) and COSIGN
# (default cosign) can be set for tests. Runs in the deploy
# (roles/memory-api-go) and in CI (the verify-build job in ci.yml).
set -euo pipefail

if [ "$#" -ne 2 ]; then
    echo "usage: verify-build.sh <file> <signer>" >&2
    exit 1
fi
file=$1
signer=$2
repo=${VERIFY_REPO:-jeffdafoe/llm-memory-api}
cosign=${COSIGN:-cosign}

digest=$(sha256sum "$file" | cut -d' ' -f1)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# A 404 means GitHub holds no attestation for this digest: the file is not
# one CI built, or was changed after it.
if ! curl -fsS --max-time 30 \
        -H 'Accept: application/vnd.github+json' \
        -o "$work/attestations.json" \
        "https://api.github.com/repos/$repo/attestations/sha256:$digest"; then
    echo "verify-build: no attestation found for $file (sha256:$digest)" >&2
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
            --type slsaprovenance1 \
            "$file" >"$work/cosign.log" 2>&1; then
        echo "verify-build: $file (sha256:$digest) verified, signed by $signer"
        exit 0
    fi
done

echo "verify-build: no attestation of $file (sha256:$digest) verifies with signer $signer" >&2
cat "$work/cosign.log" >&2 2>/dev/null || true
exit 1
