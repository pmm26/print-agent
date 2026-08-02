#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 /absolute/path/to/print-agent-data" >&2
  exit 2
fi

target=$1
if [[ $target != /* || $target == / || $target == /home || $target == /Users ]]; then
  echo "refusing unsafe data-directory path: $target" >&2
  exit 2
fi
if [[ ! -d $target ]]; then
  echo "data directory does not exist: $target" >&2
  exit 2
fi
if [[ ! -f $target/print-agent.db ]]; then
  echo "refusing path without print-agent.db: $target" >&2
  exit 2
fi

backup="${target}.pre-release-backup.$(date -u +%Y%m%dT%H%M%SZ)"
if [[ -e $backup ]]; then
  echo "backup path already exists: $backup" >&2
  exit 2
fi

mv -- "$target" "$backup"
echo "Moved the pre-release data directory to: $backup"
echo "Restart print-agent to create a clean database, agent ID, and credentials."
