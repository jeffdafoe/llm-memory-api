#!/usr/bin/env bash
# Weekly check for version pins that Dependabot does not watch (LLM-923).
#
# Dependabot plus dependabot-auto-merge.yml keeps npm, Go modules and Actions
# current for patch and minor updates. This script covers the rest:
#   - exact pins nothing else bumps: the Go toolchain
#   - major lines that only end by choice: PostgreSQL and Node.js, flagged
#     near end of life. The server's own weekly check
#     (node/api/scripts/staleness-check.js) warns on the installed versions,
#     90 days out; this one reads the repo pins, a year out.
#   - Dependabot PRs that wait for a person (majors are not auto-merged)
#   - Go dependencies with a newer major module path, which Dependabot never
#     offers because a new major is a new import path (pgx/v5 -> pgx/v6)
#
# Usage: stale-pins.sh [report|issue]
#   report  print the report (and write it to the step summary in CI). Default.
#   issue   also open, update or close the one open issue labelled stale-pins.
#
# A stale pin is a finding, not a failure: the run stays green and the issue
# carries the list. The run fails only when a check cannot complete (a source
# is down, a pin cannot be read), and then it leaves the issue alone, so an
# outage can never close an issue whose pins are still stale.
#
# Needs bash, curl, jq, GNU date/sed/sort and an authenticated gh (GH_TOKEN).

set -euo pipefail
# An unexpected exit under set -e would end the run with no report and no
# reason. Say so on the way out. REPORT_PRINTED is set once the report is out.
REPORT_PRINTED=""
trap 'rc=$?; if [ "$rc" -ne 0 ] && [ -z "$REPORT_PRINTED" ]; then echo "stale-pins.sh: stopped with exit $rc before the report was printed" >&2; fi' EXIT

MODE="${1:-report}"
case "$MODE" in
    report|issue) ;;
    *) echo "usage: $0 [report|issue]" >&2; exit 2 ;;
esac

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
REPO="${GITHUB_REPOSITORY:-jeffdafoe/llm-memory-api}"
LABEL="stale-pins"
ISSUE_TITLE="Stale version pins"
DEPENDABOT_MAX_AGE_DAYS=14
EOL_WARN_MONTHS=12

TODAY="$(date -u +%F)"
EOL_WARN_DATE="$(date -u -d "+${EOL_WARN_MONTHS} months" +%F)"

findings=()
errors=()

finding() {
    findings+=("$1")
}

error() {
    errors+=("$1")
}

fetch() {
    curl -fsSL --retry 2 --max-time 30 "$1"
}

# True when version $1 sorts before version $2.
version_lt() {
    [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n 1)" = "$1" ]
}

# Value of a top-level key in an Ansible vars file, quotes stripped.
yaml_var() {
    sed -nE "s/^$2:[[:space:]]*\"?([^\"#[:space:]]*)\"?.*/\1/p" "$ROOT/$1" | head -n 1
}

# compare_pins NAME LATEST "where=version"...
# Flags each pin older than LATEST (skipped when LATEST is empty) and flags the
# set when the pins disagree, since every copy has to change together.
compare_pins() {
    local name="$1" latest="$2"
    shift 2
    local pin where version seen=""
    for pin in "$@"; do
        where="${pin%%=*}"
        version="${pin#*=}"
        if [ -z "$version" ]; then
            error "$name: could not read the version from \`$where\`."
            continue
        fi
        if [ -n "$latest" ] && version_lt "$version" "$latest"; then
            finding "**$name**: \`$where\` pins $version. The latest stable release is $latest."
        fi
        seen+="$version"$'\n'
    done
    local distinct
    distinct="$(printf '%s' "$seen" | sort -u)"
    if [ "$(printf '%s\n' "$distinct" | grep -c .)" -gt 1 ]; then
        finding "**$name**: the pins disagree ($(printf '%s\n' "$distinct" | paste -sd, - | sed 's/,/, /g')). Change every copy together."
    fi
}

check_go_toolchain() {
    local latest
    if ! latest="$(fetch 'https://go.dev/dl/?mode=json' | jq -r '[.[] | select(.stable)][0].version // empty | ltrimstr("go")')" || [ -z "$latest" ]; then
        error "Go: could not read the latest stable release from go.dev."
        return
    fi
    compare_pins "Go toolchain" "$latest" \
        "go/server/go.mod=$(sed -n 's/^toolchain go//p' "$ROOT/go/server/go.mod")" \
        "go/client/go.mod=$(sed -n 's/^toolchain go//p' "$ROOT/go/client/go.mod")"
}

# file_pins NAME FILE KEY FILTER VALUE_RE GROUP
# Sets FILE_PINS to one "FILE:LINE=major" pin for each active (not commented)
# YAML line "KEY: value" in FILE whose value matches the ERE FILTER (empty
# FILTER takes every KEY line). Every such value must match VALUE_RE, and
# capture group GROUP is the major. A value that does not match (an expression,
# a new spelling) is an error, so one job cannot drop out of the check while the
# others keep it green. No KEY line at all is an error too: the pin moved or was
# renamed. It fills a global instead of printing, because error() must run in
# this shell, not in a command substitution.
FILE_PINS=()
file_pins() {
    local name="$1" file="$2" key="$3" filter="$4" value_re="$5" group="$6"
    local lines rc=0 entry number value found=0
    FILE_PINS=()
    if [ ! -r "$ROOT/$file" ]; then
        error "$name: cannot read \`$file\`."
        return
    fi
    # grep exits 1 for no match and 2 for a read error.
    lines="$(grep -nE "^[[:space:]]*(-[[:space:]]+)?${key}:" "$ROOT/$file")" || rc=$?
    if [ "$rc" -gt 1 ]; then
        error "$name: cannot read \`$file\`."
        return
    fi
    while IFS= read -r entry; do
        if [ -z "$entry" ]; then
            continue
        fi
        number="${entry%%:*}"
        value="${entry#*:}"
        value="${value#*"${key}":}"
        value="${value%%#*}"
        value="${value#"${value%%[![:space:]]*}"}"
        value="${value%"${value##*[![:space:]]}"}"
        if [[ "$value" =~ ^\'(.*)\'$ ]] || [[ "$value" =~ ^\"(.*)\"$ ]]; then
            value="${BASH_REMATCH[1]}"
        fi
        if [ -n "$filter" ] && ! [[ "$value" =~ $filter ]]; then
            continue
        fi
        found=1
        if [[ "$value" =~ $value_re ]]; then
            FILE_PINS+=("$file:$number=${BASH_REMATCH[$group]}")
        else
            error "$name: cannot read the version in \`$file\` line $number (\`$value\`)."
        fi
    done <<< "$lines"
    if [ "$found" -eq 0 ]; then
        error "$name: found no version in \`$file\`."
    fi
}

# check_eol PRODUCT NAME "where=major"...
# endoflife.date returns eol as a date, or false while no date is set.
check_eol() {
    local product="$1" name="$2"
    shift 2
    compare_pins "$name major" "" "$@"

    local majors major json eol
    # sed, not grep: grep exits 1 when every pin is unreadable, and under
    # pipefail that would end the script before the report is printed.
    majors="$(for pin in "$@"; do printf '%s\n' "${pin#*=}"; done | sed '/^$/d' | sort -u)"
    for major in $majors; do
        # Anything but a boolean or a real YYYY-MM-DD date is an error. A
        # missing or null eol must not read as "still supported".
        if ! json="$(fetch "https://endoflife.date/api/$product/$major.json")" \
            || ! eol="$(printf '%s' "$json" | jq -er '.eol | if type == "boolean" then tostring elif type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$") then . else error("bad eol") end')" \
            || { [ "$eol" != "true" ] && [ "$eol" != "false" ] && [ "$(date -u -d "$eol" +%F 2>/dev/null)" != "$eol" ]; }; then
            error "$name: could not read the end-of-life date of $major from endoflife.date."
            continue
        fi
        if [ "$eol" = "false" ]; then
            continue
        fi
        if [ "$eol" = "true" ]; then
            finding "**$name $major** is past end of life."
        elif [[ "$eol" < "$TODAY" ]]; then
            finding "**$name $major** reached end of life on $eol."
        elif [[ "$eol" < "$EOL_WARN_DATE" ]]; then
            finding "**$name $major** reaches end of life on $eol, less than $EOL_WARN_MONTHS months from now."
        fi
    done
}

check_postgres() {
    # The CI service containers run pgvector/pgvector:pgNN. Any image that
    # names Postgres must have a readable major.
    file_pins "PostgreSQL" .github/workflows/ci.yml image 'pgvector|postgres' \
        '^(pgvector/pgvector:pg|postgres:)([0-9]+)([.-].*)?$' 2
    check_eol postgresql "PostgreSQL" \
        "infrastructure/group_vars/all.yml postgresql_version=$(yaml_var infrastructure/group_vars/all.yml postgresql_version)" \
        "${FILE_PINS[@]}"
}

check_node() {
    file_pins "Node.js" .github/workflows/ci.yml node-version '' '^([0-9]+)(\.[0-9x]+)*$' 1
    check_eol nodejs "Node.js" \
        "infrastructure/group_vars/all.yml nodejs_version=$(yaml_var infrastructure/group_vars/all.yml nodejs_version)" \
        "${FILE_PINS[@]}"
}

check_dependabot_prs() {
    local cutoff prs limit=200
    cutoff="$(date -u -d "-${DEPENDABOT_MAX_AGE_DAYS} days" +%FT%TZ)"
    # The age filter runs on the server, so the limit applies only to old PRs.
    # dependabot.yml caps open PRs far below it; a full page still means the
    # list may be cut short, and that is an error rather than a clean result.
    if ! prs="$(gh pr list -R "$REPO" --author app/dependabot --state open --limit "$limit" \
        --search "created:<$cutoff" --json number,title,createdAt,url \
        --jq ".[] | \"[#\(.number)](\(.url)) \(.title) (open since \(.createdAt[0:10]))\"")"; then
        error "Dependabot: could not list open pull requests."
        return
    fi
    if [ "$(printf '%s\n' "$prs" | grep -c .)" -ge "$limit" ]; then
        error "Dependabot: $limit or more old PRs are open, so the list may be incomplete."
        return
    fi
    local pr
    while IFS= read -r pr; do
        if [ -n "$pr" ]; then
            finding "**Dependabot PR waiting more than $DEPENDABOT_MAX_AGE_DAYS days**: $pr"
        fi
    done <<< "$prs"
}

# Direct requirements of a go.mod as "path version" lines.
direct_requires() {
    awk '
        /^require \(/ { block = 1; next }
        block && /^\)/ { block = 0; next }
        block && NF >= 2 && !/\/\/ indirect/ { print $1, $2 }
        /^require [^(]/ && !/\/\/ indirect/ { print $2, $3 }
    ' "$1"
}

# Module path of the next major version, or nothing when there is none to look
# for. A v0 module goes to v1 at the same path, and Dependabot offers that one.
next_major_path() {
    local path="$1" version="$2"
    case "$version" in
        v0.*|*+incompatible) return ;;
    esac
    if [[ "$path" =~ ^(gopkg\.in/.+)\.v([0-9]+)$ ]]; then
        echo "${BASH_REMATCH[1]}.v$((BASH_REMATCH[2] + 1))"
    elif [[ "$path" =~ ^(.+)/v([0-9]+)$ ]] && [ "${BASH_REMATCH[2]}" -ge 2 ]; then
        echo "${BASH_REMATCH[1]}/v$((BASH_REMATCH[2] + 1))"
    else
        echo "$path/v2"
    fi
}

# check_go_majors GO_MOD
# Asks the module proxy whether the next major path has a tagged stable release.
# Only N+1 is probed. A module that skips a major (v5 straight to v7) is missed.
check_go_majors() {
    local gomod="$1" path version next escaped code stable
    local body
    body="$(mktemp)"
    while read -r path version; do
        next="$(next_major_path "$path" "$version")"
        if [ -z "$next" ]; then
            continue
        fi
        # The proxy wants upper-case letters escaped as "!" plus lower case.
        escaped="$(printf '%s' "$next" | sed -E 's/([A-Z])/!\L\1/g')"
        # Without -f, curl exits non-zero only when the transfer itself fails.
        # That can happen after a 200 header, and a cut-off list must not pass.
        if ! code="$(curl -sS -o "$body" -w '%{http_code}' --retry 2 --max-time 30 "https://proxy.golang.org/$escaped/@v/list")"; then
            error "Go modules: could not download the version list for \`$next\`."
            continue
        fi
        case "$code" in
            200)
                stable="$(grep -v -- '-' "$body" | sort -V | tail -n 1 || true)"
                if [ -n "$stable" ]; then
                    finding "**Go module major**: \`$gomod\` requires \`$path\` $version. \`$next\` is out ($stable). The upgrade changes the import path, so Dependabot does not offer it."
                fi
                ;;
            404|410)
                ;;
            *)
                error "Go modules: the module proxy answered HTTP ${code:-none} for \`$next\`."
                ;;
        esac
    done < <(direct_requires "$ROOT/$gomod")
    rm -f "$body"
}

check_go_toolchain
check_postgres
check_node
check_dependabot_prs
check_go_majors go/server/go.mod
check_go_majors go/client/go.mod

report="$(mktemp)"
{
    echo "## $ISSUE_TITLE"
    echo
    if [ "${#findings[@]}" -eq 0 ]; then
        echo "All pins are current."
    else
        echo "These pins are behind and need a person to update them."
        echo
        printf -- '- %s\n' "${findings[@]}"
    fi
    if [ "${#errors[@]}" -gt 0 ]; then
        echo
        echo "### The check could not complete"
        echo
        printf -- '- %s\n' "${errors[@]}"
    fi
    echo
    echo "---"
    run=""
    if [ -n "${GITHUB_RUN_ID:-}" ]; then
        run=" ([run](${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}))"
    fi
    echo "Checked on $TODAY by \`.github/workflows/stale-pins.yml\`$run. The check is described in LLM-923."
} > "$report"

cat "$report"
REPORT_PRINTED=1
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    cat "$report" >> "$GITHUB_STEP_SUMMARY"
fi

if [ "${#errors[@]}" -gt 0 ]; then
    rm -f "$report"
    exit 1
fi

if [ "$MODE" = "issue" ]; then
    number="$(gh issue list -R "$REPO" --label "$LABEL" --state open --limit 1 --json number --jq '.[0].number // empty')"
    if [ "${#findings[@]}" -gt 0 ]; then
        if [ -n "$number" ]; then
            gh issue edit "$number" -R "$REPO" --body-file "$report"
        else
            gh label create "$LABEL" -R "$REPO" --color FBCA04 \
                --description "Version pins behind the latest release (LLM-923)" --force
            gh issue create -R "$REPO" --title "$ISSUE_TITLE" --label "$LABEL" --body-file "$report"
        fi
    elif [ -n "$number" ]; then
        gh issue close "$number" -R "$REPO" --comment "All pins are current as of $TODAY."
    fi
fi

rm -f "$report"
