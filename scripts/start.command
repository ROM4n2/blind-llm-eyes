#!/usr/bin/env bash
# Double-click launcher for blind-llm-eyes on macOS.
# Finder treats .command files as double-clickable scripts. Place next to
# the blind-llm-eyes binary in the release archive.
# Make executable: chmod +x start.command

set -e
cd "$(dirname "$0")"

echo "=== blind-llm-eyes starting ==="
echo

./blind-llm-eyes start

echo
echo "=== blind-llm-eyes exited (code $?) ==="
