#!/usr/bin/env bash
set -euo pipefail
uid="$(id -u)"
status=0
for path in "$@"; do
    [[ -e "$path" || -L "$path" ]] || continue
    if [[ "$uid" -ne 0 ]]; then
        if ! other_owner="$(find "$path" -xdev ! -uid "$uid" -print -quit 2>/dev/null)"; then
            echo "Leaving $path: ownership could not be checked" >&2
            continue
        fi
        if [[ -n "$other_owner" ]]; then
            echo "Leaving $path: clean artifacts as their owner" >&2
            continue
        fi
    fi
    if ! find "$path" -xdev -delete; then
        echo "Could not completely remove $path; continuing with remaining paths" >&2
        status=1
    fi
done
exit "$status"
