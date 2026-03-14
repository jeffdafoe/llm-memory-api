#!/usr/bin/env bash
# Bundle admin UI source files into a single document for code review.
# Usage: bash scripts/bundle-for-review.sh [output-file]
#
# Output goes to stdout by default, or to the specified file.

set -euo pipefail

ADMIN_DIR="$(cd "$(dirname "$0")/../public/admin" && pwd)"
OUTPUT="${1:-}"

files=(
    index.html
    style.css
    main.js
    core.js
    dashboard.js
    agents.js
    actors-config.js
    notes.js
    mail.js
    chat.js
    discussions.js
    config.js
    apilog.js
    errorlog.js
    events.js
    views/DashboardView.js
    views/AgentsView.js
    views/CommsView.js
    views/ConfigView.js
    views/NotesView.js
    views/ActorDialogs.js
    views/AgentDialog.js
    views/MiscDialogs.js
    views/dashboard.html
    views/agents.html
    views/comms.html
    views/config.html
    views/notes.html
    views/actor-dialogs.html
    views/agent-dialog.html
    views/misc-dialogs.html
)

bundle() {
    echo "# Admin UI — Code Review Bundle"
    echo ""
    echo "Generated: $(date -u '+%Y-%m-%d %H:%M UTC')"
    echo "Files: ${#files[@]}"
    echo ""

    for f in "${files[@]}"; do
        filepath="$ADMIN_DIR/$f"
        if [ ! -f "$filepath" ]; then
            echo "# MISSING: $f"
            echo ""
            continue
        fi
        lines=$(wc -l < "$filepath")
        size=$(wc -c < "$filepath" | tr -d ' ')
        echo "---"
        echo ""
        echo "## $f  ($lines lines, $size bytes)"
        echo ""
        echo '```'"${f##*.}"
        cat "$filepath"
        echo '```'
        echo ""
    done
}

if [ -n "$OUTPUT" ]; then
    bundle > "$OUTPUT"
    echo "Written to $OUTPUT ($(wc -c < "$OUTPUT" | tr -d ' ') bytes)" >&2
else
    bundle
fi
