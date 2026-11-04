#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

docker compose down -v --remove-orphans
docker compose up -d --wait

echo "Local stack reset: volumes dropped and services healthy."
