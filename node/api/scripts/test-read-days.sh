#!/bin/bash
# test-read-days.sh — regression tests for read_days in db-cleanup.sh and
# pg-backup.sh (LLM-1021). Runs in CI (ci.yml, "Script Tests").
#
# read_days reads a retention key from the config table with psql and must
# return the configured whole number of days, or 7 on anything else, and must
# never hold up the job: a stalled psql is stopped after 15 s, and one that
# ignores SIGTERM is killed 2 s later. Each case puts a stub psql first on
# PATH and checks what read_days prints and how long it took.

set -u

here="$(cd "$(dirname "$0")" && pwd)"
fail=0

# Both scripts carry their own copy of read_days; they must stay identical,
# so the cases below cover both.
extract() { sed -n '/^read_days() {/,/^}/p' "$1"; }
cleanup_fn="$(extract "$here/db-cleanup.sh")"
backup_fn="$(extract "$here/pg-backup.sh")"
if [ -z "$cleanup_fn" ]; then
    echo "FAIL: read_days not found in db-cleanup.sh"
    exit 1
fi
if [ "$cleanup_fn" != "$backup_fn" ]; then
    echo "FAIL: read_days differs between db-cleanup.sh and pg-backup.sh"
    exit 1
fi

stubdir="$(mktemp -d)"
trap 'rm -rf "$stubdir"' EXIT

# check NAME WANT MAX_SECONDS STUB_BODY
check() {
    local name="$1" want="$2" max="$3" body="$4"
    printf '#!/bin/bash\n%s\n' "$body" > "$stubdir/psql"
    chmod +x "$stubdir/psql"
    local start end got
    start=$(date +%s)
    got=$(PATH="$stubdir:$PATH" DATABASE_URL=stub bash -c "$cleanup_fn
read_days some_key")
    end=$(date +%s)
    if [ "$got" != "$want" ]; then
        echo "FAIL: $name: got [$got], want [$want]"
        fail=1
    elif [ $((end - start)) -gt "$max" ]; then
        echo "FAIL: $name: took $((end - start)) s, limit $max s"
        fail=1
    else
        echo "ok: $name -> $got"
    fi
}

check "plain value"             30     5  'echo 30'
check "padded with ASCII space" 14     5  'printf " 14 \n"'
check "inner space"             7      5  'echo "1 4"'
check "number but psql failed"  7      5  'echo 30; exit 1'
check "leading zero"            8      5  'echo 08'
check "zero"                    7      5  'echo 0'
check "negative"                7      5  'echo -3'
check "exponent"                7      5  'echo 1e3'
check "trailing text"           7      5  'echo 7days'
check "empty"                   7      5  'true'
check "largest allowed"         999999 5  'echo 999999'
check "seven digits"            7      5  'echo 1234567'
check "non-breaking space"      7      5  "printf '\\xc2\\xa014\\xc2\\xa0\\n'"
# The stalling stubs exec sleep, so the stub is one process, as psql is. A
# bash stub with a child sleep fails on timeout builds that kill only the
# process they started (coreutils on ubuntu-latest sends the final KILL to
# the child alone): the orphaned sleep keeps the $(...) pipe open, and
# read_days waits the full 60 s.
check "stalls, honours TERM"    7      18 'exec sleep 60'
check "stalls, ignores TERM"    7      20 "trap '' TERM; exec sleep 60"

if [ "$fail" -ne 0 ]; then
    exit 1
fi
echo "read_days: all cases pass"
