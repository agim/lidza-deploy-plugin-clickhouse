#!/usr/bin/env bash
# Takes the built plugin through a real server, as the agent would: install
# ClickHouse, give an application a database, back it up, restore it and
# release it. It changes the machine it runs on (packages, a service), so it
# is for a throwaway machine such as a CI runner:
#   sudo scripts/integration.sh ./plugin
set -euo pipefail
plugin=$(readlink -f "${1:?path of the built plugin required}")
[[ $EUID -eq 0 ]] || { echo 'run as root on a throwaway machine' >&2; exit 2; }
[[ "${CI:-}" == true || "${PLUGIN_INTEGRATION:-}" == yes ]] || { echo 'refusing outside CI; set PLUGIN_INTEGRATION=yes to run it anyway' >&2; exit 2; }
state=$(mktemp -d)
work=$(mktemp -d)
trap 'rm -rf "$state" "$work"' EXIT
arch=$(dpkg --print-architecture)
flags=$(grep -m1 '^flags' /proc/cpuinfo | cut -d: -f2 | xargs -n1 | jq -R . | jq -sc .)
memory=$(( $(awk '/^MemTotal:/{print $2}' /proc/meminfo) * 1024 ))

# hook NAME INPUT_JSON [WORK_DIR] runs one hook and prints its answer.
hook() {
  jq -nc --arg hook "$1" --argjson input "$2" --arg state "$state" --arg work "${3:-}" --arg arch "$arch" --argjson flags "$flags" --argjson memory "$memory" \
    '{contract:1,hook:$hook,state_dir:$state,work_dir:$work,input:$input,server:{arch:$arch,cpu_flags:$flags,memory_bytes:$memory}}' |
    (cd "$state" && "$plugin" "$1")
}
ok() { # ok LABEL ANSWER
  jq -e '.ok == true' <<<"$2" >/dev/null || { echo "FAILED: $1: $2" >&2; journalctl -u clickhouse-server -n 30 --no-pager >&2 || true; exit 1; }
  echo "ok: $1: $(jq -r '(.messages // []) | join(" ")' <<<"$2")"
}
query() { # query USER PASSWORD SQL
  curl -fsS -H "X-ClickHouse-User: $1" -H "X-ClickHouse-Key: $2" --data-binary "$3" "$url/"
}

answer=$(hook check '{"app":"shop","options":{"series":"26.3"},"sizes":{"clickhouse":1048576}}')
ok 'check before anything is installed' "$answer"

answer=$(hook provision '{"app":"shop","options":{"series":"26.3"}}')
ok 'provision' "$answer"
url=$(jq -r .output.env.CLICKHOUSE_URL <<<"$answer")
user=$(jq -r .output.env.CLICKHOUSE_USER <<<"$answer")
password=$(jq -r .output.env.CLICKHOUSE_PASSWORD <<<"$answer")
record=$(jq -c .output.record <<<"$answer")
secrets=$(jq -c .output.secrets <<<"$answer")
[[ "$(query "$user" "$password" 'SELECT version()')" == 26.3.* ]] || { echo 'the requested series was not installed' >&2; exit 1; }
[[ "$(stat -c %a "$state/server.json")" == 600 ]] || { echo 'the plugin state is readable by others' >&2; exit 1; }
ss -ltnH | awk '{print $4}' | grep -E ':(8123|9000)$' | grep -vE '^(127\.0\.0\.1|\[::1\]):' && { echo 'ClickHouse listens beyond loopback' >&2; exit 1; }

# The application works in its own database and nowhere else.
query "$user" "$password" 'CREATE TABLE events (id UInt64, name String) ENGINE = MergeTree ORDER BY id'
query "$user" "$password" "INSERT INTO events SELECT number, toString(number) FROM numbers(50000)"
if query "$user" "$password" 'SELECT count() FROM system.users' 2>/dev/null; then echo 'the application can read the user list' >&2; exit 1; fi
if query "$user" "$password" 'CREATE DATABASE elsewhere' 2>/dev/null; then echo 'the application can create other databases' >&2; exit 1; fi
if curl -fsS --data-binary 'SELECT 1' "$url/" 2>/dev/null; then echo 'the default user has no password' >&2; exit 1; fi

# A second provision is a repair: same password, nothing reinstalled.
answer=$(hook provision "$(jq -nc --argjson record "$record" --argjson secrets "$secrets" '{app:"shop",record:$record,secrets:$secrets}')")
ok 'provision again' "$answer"
[[ "$(jq -r .output.env.CLICKHOUSE_PASSWORD <<<"$answer")" == "$password" ]] || { echo 'the password changed on a retry' >&2; exit 1; }

input=$(jq -nc --argjson record "$record" --argjson secrets "$secrets" '{app:"shop",record:$record,secrets:$secrets}')
answer=$(hook backup "$input" "$work")
ok 'backup' "$answer"
[[ -s "$work/manifest.json" && -d "$work/backup" ]] || { echo 'the backup directory is incomplete' >&2; exit 1; }
[[ -z "$(ls -A /var/lib/clickhouse/lidza-backups/plugin)" ]] || { echo 'the backup stayed on the ClickHouse disk' >&2; exit 1; }

# Writes after the backup are gone after the restore; the backup's rows are back.
query "$user" "$password" 'TRUNCATE TABLE events'
query "$user" "$password" "INSERT INTO events VALUES (1, 'after the backup')"
answer=$(hook restore "$input" "$work")
ok 'restore' "$answer"
[[ "$(query "$user" "$password" 'SELECT count(), max(id) FROM events')" == $'50000\t49999' ]] || { echo 'the restore did not bring the rows back' >&2; exit 1; }
[[ "$(query "$user" "$password" "SELECT count() FROM system.databases WHERE name LIKE '%lidza_previous'" 2>/dev/null || echo 0)" == 0 ]] || { echo 'the aside copy stayed' >&2; exit 1; }
[[ -z "$(ls -A /var/lib/clickhouse/lidza-backups/plugin)" ]] || { echo 'the restore left its files on the ClickHouse disk' >&2; exit 1; }

# Upkeep bounds ClickHouse's own logs, and a second run finds nothing to do.
admin=$(jq -r .admin_password "$state/server.json")
query default "$admin" 'SYSTEM FLUSH LOGS'
ok 'maintain' "$(hook maintain '{}')"
answer=$(hook maintain '{}')
ok 'maintain again' "$answer"
jq -e '.messages | join(" ") | test("within their limits")' <<<"$answer" >/dev/null || { echo 'a second upkeep run still changed tables' >&2; exit 1; }
[[ "$(query default "$admin" "SELECT count() FROM system.tables WHERE database = 'system' AND name = 'trace_log'")" == 0 ]] || { echo 'the profiling log exists although it is switched off' >&2; exit 1; }
ok 'status' "$(hook status '{}')"
ok 'check with the server running' "$(hook check '{}')"

answer=$(hook release '{"app":"shop"}')
ok 'release' "$answer"
[[ "$(systemctl is-active clickhouse-server || true)" != active ]] || { echo 'the unused server keeps running' >&2; exit 1; }
echo 'The plugin installed, provisioned, backed up, restored and released ClickHouse on this machine.'
