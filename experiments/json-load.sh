#!/usr/bin/env bash
# Experiment, not a product test: how much memory does loading a wide JSON
# table into ClickHouse take the way the deploy agent does it today, and
# with bounded batches and merges held back? Runs on a throwaway CI machine.
#   sudo experiments/json-load.sh ./plugin ROWS_IN_MILLIONS CAP_MIB
set -euo pipefail
plugin=$(readlink -f "$1"); millions=${2:-12}
[[ $EUID -eq 0 && "${CI:-}" == true ]] || { echo 'CI only, as root' >&2; exit 2; }
state=$(mktemp -d); arch=$(dpkg --print-architecture)
flags=$(grep -m1 '^flags' /proc/cpuinfo | cut -d: -f2 | xargs -n1 | jq -R . | jq -sc .)
memory=$(( $(awk '/^MemTotal:/{print $2}' /proc/meminfo) * 1024 ))
jq -nc --arg state "$state" --arg arch "$arch" --argjson flags "$flags" --argjson memory "$memory" \
  '{contract:1,hook:"provision",state_dir:$state,input:{app:"shop",options:{series:"26.3"}},server:{arch:$arch,cpu_flags:$flags,memory_bytes:$memory}}' | (cd "$state" && "$plugin" provision) | jq -c '{ok, messages}'
admin=$(jq -r .admin_password "$state/server.json")
q() {
  local out code
  out=$(curl -sS -w '\n%{http_code}' -H 'X-ClickHouse-User: default' -H "X-ClickHouse-Key: $admin" --data-binary "$1" 'http://127.0.0.1:8123/')
  code=${out##*$'\n'}; out=${out%$'\n'*}
  if [[ $code != 200 ]]; then echo "ClickHouse refused (HTTP $code): ${out:0:400}" >&2; return 1; fi
  printf '%s\n' "$out"
}
echo "server: $(q 'SELECT version()'), host memory $((memory >> 20)) MiB, $(nproc) cores"

table() { echo "CREATE TABLE $1.events (id UUID, app LowCardinality(String), env LowCardinality(String), stream LowCardinality(String), level LowCardinality(String), ts DateTime64(3, 'UTC'), received_at DateTime64(3, 'UTC'), message String, trace String, attrs JSON(max_dynamic_paths=256), retain_days UInt16, INDEX message_tokens message TYPE tokenbf_v1(8192, 3, 0) GRANULARITY 4) ENGINE = MergeTree PARTITION BY toYYYYMM(ts) ORDER BY (app, stream, ts) SETTINGS index_granularity = 8192, max_bytes_to_merge_at_max_space_in_pool = 33554432, non_replicated_deduplication_window = 100000"; }
# Generating needs room: the cap under test comes afterwards.
cat > /etc/clickhouse-server/config.d/zz-experiment.xml <<XML
<clickhouse><max_server_memory_usage>$(( 12288 << 20 ))</max_server_memory_usage><max_server_memory_usage_to_ram_ratio>1</max_server_memory_usage_to_ram_ratio></clickhouse>
XML
systemctl daemon-reload; mkdir -p /etc/systemd/system/clickhouse-server.service.d
printf '[Service]\nMemoryMax=infinity\n' > /etc/systemd/system/clickhouse-server.service.d/zz-experiment.conf
systemctl daemon-reload; systemctl restart clickhouse-server
for _ in $(seq 1 60); do q 'SELECT 1' >/dev/null 2>&1 && break; sleep 1; done
q 'CREATE DATABASE src'; q "$(table src)"; q 'ALTER TABLE src.events MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 161061273600'
echo "== generating $millions million rows with about 300 distinct JSON keys"
for i in $(seq 1 "$millions"); do
  q "INSERT INTO src.events SELECT generateUUIDv4(), concat('app', toString(number % 6)), 'production', concat('stream', toString(number % 12)), 'info', now64(3) - toIntervalSecond(number % 2000000), now64(3), concat('GET /path/', toString(number % 5000), ' ', repeat('m', 40 + number % 60)), if(number % 50 = 0, repeat('at frame() ', 30), ''), CAST(toJSONString(map(concat('k', toString(number % 300)), toString(number), concat('k', toString((number * 7) % 300)), toString(number % 97), concat('k', toString((number * 13) % 300)), 'value', 'path', concat('/p/', toString(number % 1000)), 'status', toString(200 + number % 5), 'ms', toString(number % 900))) AS JSON(max_dynamic_paths=256)), 30 FROM numbers($(( (i-1) * 1000000 )), 1000000) SETTINGS max_insert_block_size = 100000, max_threads = 2, type_json_skip_duplicated_paths = 1"
done
q 'OPTIMIZE TABLE src.events FINAL' || true
echo "source table: $(q "SELECT concat(toString(sum(rows)), ' rows, ', formatReadableSize(sum(data_uncompressed_bytes)), ' uncompressed, ', toString(count()), ' parts') FROM system.parts WHERE database = 'src' AND active")"
echo "JSON paths: $(q "SELECT length(JSONDynamicPaths(attrs)) FROM src.events LIMIT 1") dynamic in the first row's part"
echo '== exporting the way the agent does (SELECT * FORMAT Native)'
curl -fsS -H 'X-ClickHouse-User: default' -H "X-ClickHouse-Key: $admin" --data-binary 'SELECT * FROM src.events FORMAT Native' 'http://127.0.0.1:8123/' -o /mnt/events.native
ls -la /mnt/events.native | awk '{print "export file: " int($5/1048576) " MiB"}'

caps=${3:-3072}
restart_with() { # restart_with CAP_MIB [MERGE_POOL]
  local pool=''
  [[ -n "${2:-}" ]] && pool="<background_pool_size>$2</background_pool_size><background_merges_mutations_concurrency_ratio>1</background_merges_mutations_concurrency_ratio>"
  cat > /etc/clickhouse-server/config.d/zz-experiment.xml <<XML
<clickhouse><max_server_memory_usage>$(( $1 << 20 ))</max_server_memory_usage><max_server_memory_usage_to_ram_ratio>1</max_server_memory_usage_to_ram_ratio>$pool</clickhouse>
XML
  systemctl restart clickhouse-server
  for _ in $(seq 1 90); do q 'SELECT 1' >/dev/null 2>&1 && return 0; sleep 1; done
  echo 'the server did not come back' >&2; return 1
}
# load NAME CAP INSERT_SETTINGS HOLD_MERGES: a fresh server, one load, then a full merge.
load() {
  local name=$1 cap=$2 settings=$3 hold=$4 pool=${5:-} peak status took started
  restart_with "$cap" "$pool"
  q "CREATE DATABASE $name"; q "$(table "$name")"
  [[ $hold == yes ]] && q "SYSTEM STOP MERGES $name.events"
  ( while :; do q "SELECT value FROM system.metrics WHERE metric = 'MemoryTracking'" 2>/dev/null || echo 0; sleep 0.5; done ) > "/tmp/mem.$name" &
  local sampler=$!
  started=$SECONDS
  status=$(curl -sS -o "/tmp/out.$name" -w '%{http_code}' -H 'X-ClickHouse-User: default' -H "X-ClickHouse-Key: $admin" -X POST -T /mnt/events.native "http://127.0.0.1:8123/?wait_end_of_query=1&query=$(jq -rn --arg q "INSERT INTO $name.events SETTINGS $settings FORMAT Native" '$q|@uri')" || true)
  took=$(( SECONDS - started ))
  kill "$sampler" 2>/dev/null || true; wait "$sampler" 2>/dev/null || true
  peak=$(sort -n "/tmp/mem.$name" | tail -1)
  [[ $hold == yes ]] && q "SYSTEM START MERGES $name.events"
  echo "RESULT cap ${cap} MiB, $name: HTTP $status in ${took}s, peak $(( peak >> 20 )) MiB, rows $(q "SELECT count() FROM $name.events") of $rows, parts $(q "SELECT count() FROM system.parts WHERE database = '$name' AND active")"
  if [[ $status != 200 ]]; then echo "  error: $(head -c 220 "/tmp/out.$name" | tr '\n' ' ')"; q "DROP DATABASE $name SYNC"; return 0; fi
  # Ordinary background merging for a while, as after a real load.
  ( while :; do q "SELECT value FROM system.metrics WHERE metric = 'MemoryTracking'" 2>/dev/null || echo 0; sleep 0.5; done ) > "/tmp/merge.$name" &
  sampler=$!
  sleep 75
  kill "$sampler" 2>/dev/null || true; wait "$sampler" 2>/dev/null || true
  q 'SYSTEM FLUSH LOGS' >/dev/null 2>&1 || true
  echo "  75 s of normal merging: peak $(( $(sort -n "/tmp/merge.$name" | tail -1) >> 20 )) MiB, $(q "SELECT concat(toString(countIf(event_type = 'MergeParts' AND error = 0)), ' merges done, ', toString(countIf(event_type = 'MergeParts' AND error != 0)), ' failed') FROM system.part_log WHERE database = '$name'" 2>/dev/null || echo 'part log unavailable'), parts now $(q "SELECT count() FROM system.parts WHERE database = '$name' AND active"), rows $(q "SELECT count() FROM $name.events")"
  q "DROP DATABASE $name SYNC"
}
rows=$(q 'SELECT count() FROM src.events')
for cap in $caps; do
  echo "== cap $cap MiB"
  load "today_$cap" "$cap" 'optimize_on_insert = 0' no
  load "small_$cap" "$cap" 'optimize_on_insert = 0, max_insert_threads = 1, max_insert_block_size = 65536, min_insert_block_size_rows = 65536, min_insert_block_size_bytes = 67108864' yes
  load "medium_$cap" "$cap" 'optimize_on_insert = 0, max_insert_threads = 1, max_insert_block_size = 250000, min_insert_block_size_rows = 250000, min_insert_block_size_bytes = 134217728' yes
  load "medium_pool2_$cap" "$cap" 'optimize_on_insert = 0, max_insert_threads = 1, max_insert_block_size = 250000, min_insert_block_size_rows = 250000, min_insert_block_size_bytes = 134217728' yes 2
done
