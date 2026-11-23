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

# A move in two phases, with a second application standing in for the
# destination: everything first, then only what changed. Each side has its
# own move id here because both share this machine's backups disk.
answer=$(hook provision '{"app":"copy"}')
ok 'provision the destination' "$answer"
copy_user=$(jq -r .output.env.CLICKHOUSE_USER <<<"$answer")
copy_password=$(jq -r .output.env.CLICKHOUSE_PASSWORD <<<"$answer")
copy_record=$(jq -c .output.record <<<"$answer")
src=aaaaaaaa11111111
dst=bbbbbbbb22222222
move_input() { # move_input APP RECORD MOVE_ID PHASE [EXTRA_JSON]
  jq -nc --arg app "$1" --argjson record "$2" --arg id "$3" --arg phase "$4" --argjson extra "${5:-null}" '{app:$app,record:$record,move:({id:$id,phase:$phase} + ($extra // {}))}'
}
bulk=$(mktemp -d)
final=$(mktemp -d)
trap 'rm -rf "$state" "$work" "$bulk" "$final"' EXIT
ok 'export, bulk phase' "$(hook export "$(move_input shop "$record" $src bulk)" "$bulk")"
ok 'import, bulk phase' "$(hook import "$(move_input copy "$copy_record" $dst bulk)" "$bulk")"
[[ "$(query "$copy_user" "$copy_password" 'SELECT count() FROM events')" == 50000 ]] || { echo 'the bulk phase did not copy the rows' >&2; exit 1; }
query "$user" "$password" "INSERT INTO events SELECT number, 'late' FROM numbers(50000, 1000)"
answer=$(hook export "$(move_input shop "$record" $src final)" "$final")
ok 'export, final phase' "$answer"
jq -e '.output.incremental == true' <<<"$answer" >/dev/null || { echo 'the final export is not incremental' >&2; exit 1; }
[[ $(du -sk "$final/backup" | cut -f1) -lt $(du -sk "$bulk/backup" | cut -f1) ]] || { echo 'the increment is as large as the full copy' >&2; exit 1; }
# A real destination does not have the base where the source kept it.
mv "/var/lib/clickhouse/lidza-backups/plugin/move-$src-base" /var/lib/clickhouse/lidza-backups/hidden-source-base
ok 'import, final phase' "$(hook import "$(move_input copy "$copy_record" $dst final '{"incremental":true}')" "$final")"
mv /var/lib/clickhouse/lidza-backups/hidden-source-base "/var/lib/clickhouse/lidza-backups/plugin/move-$src-base"
[[ "$(query "$copy_user" "$copy_password" 'SELECT count(), max(id) FROM events')" == $'51000\t50999' ]] || { echo 'the final phase did not bring the late rows' >&2; exit 1; }
ok 'export, end of the move' "$(hook export "$(move_input shop "$record" $src end)")"
ok 'import, end of the move' "$(hook import "$(move_input copy "$copy_record" $dst end)")"
[[ -z "$(ls -A /var/lib/clickhouse/lidza-backups/plugin)" ]] || { echo 'the move left backups on the ClickHouse disk' >&2; exit 1; }
ok 'release the destination' "$(hook release '{"app":"copy"}')"

# A hand-over: forget that this plugin installed the server, as if Līdza
# Deploy's built-in support had, and take it over while it runs. Nothing may
# restart, no configuration file may change and the application keeps its
# credentials.
handed_state=$(mktemp -d)
started=$(systemctl show clickhouse-server -p ActiveEnterTimestampMonotonic --value)
sums=$(sha256sum /etc/clickhouse-server/config.d/lidza.xml /etc/clickhouse-server/users.d/lidza.xml /etc/systemd/system/clickhouse-server.service.d/lidza.conf)
admin=$(jq -r .admin_password "$state/server.json")
tcp=$(jq -r .tcp_port "$state/server.json")
own_state=$state
state=$handed_state
answer=$(hook check '{"app":"shop"}' || true) # a failing check exits non-zero
jq -e '.ok == false and ([.checks[] | select(.id == "ownership")] | length == 1)' <<<"$answer" >/dev/null || { echo "a server this plugin does not run was offered to an application: $answer" >&2; exit 1; }
handed=$(jq -nc --argjson record "$record" --argjson tcp "$tcp" --arg password "$password" --arg admin "$admin" '{app:"shop",record:($record + {adopt:{tcp_port:$tcp}}),secrets:{password:$password,admin_password:$admin}}')
answer=$(hook provision "$handed")
ok 'hand-over' "$answer"
[[ "$(jq -r .output.env.CLICKHOUSE_PASSWORD <<<"$answer")" == "$password" && "$(jq -r '.output.record | has("adopt")' <<<"$answer")" == false ]] || { echo 'the hand-over changed the application or kept the hand-over data' >&2; exit 1; }
[[ "$(systemctl show clickhouse-server -p ActiveEnterTimestampMonotonic --value)" == "$started" ]] || { echo 'the hand-over restarted ClickHouse' >&2; exit 1; }
[[ "$(sha256sum /etc/clickhouse-server/config.d/lidza.xml /etc/clickhouse-server/users.d/lidza.xml /etc/systemd/system/clickhouse-server.service.d/lidza.conf)" == "$sums" ]] || { echo 'the hand-over changed configuration files' >&2; exit 1; }
[[ "$(jq -r .adopted "$state/server.json")" == true ]] || { echo 'the server is not recorded as handed over' >&2; exit 1; }
[[ "$(query "$user" "$password" 'SELECT count() FROM events')" == 51000 ]] || { echo 'the application lost its data or access in the hand-over' >&2; exit 1; }
# The adopted server backs up and restores like any other.
handed_work=$(mktemp -d)
trap 'rm -rf "$own_state" "$handed_state" "$work" "$bulk" "$final" "$handed_work"' EXIT
ok 'backup after the hand-over' "$(hook backup "$input" "$handed_work")"
ok 'restore after the hand-over' "$(hook restore "$input" "$handed_work")"
[[ "$(systemctl show clickhouse-server -p ActiveEnterTimestampMonotonic --value)" == "$started" ]] || { echo 'ClickHouse restarted after the hand-over' >&2; exit 1; }

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
echo 'The plugin installed, provisioned, backed up, restored, moved and released ClickHouse on this machine.'
