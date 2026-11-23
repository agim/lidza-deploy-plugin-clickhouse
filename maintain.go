package main

// Upkeep of ClickHouse's own log tables. New tables get their limits from
// the configuration (server.go); this brings existing ones in line, hourly:
// the tables that explain a problem afterwards keep a few days, and the
// profiling and metrics logs, which on a small server outgrow the
// applications' data, are emptied.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/agim/lidza-deploy/pluginapi"
)

// logDays are the system log tables that are kept, and for how many days.
var logDays = map[string]int{"query_log": 3, "part_log": 3, "text_log": 3, "error_log": 14}

// logsOff are the system log tables the server does not write.
var logsOff = []string{"query_thread_log", "query_views_log", "query_metric_log", "metric_log", "asynchronous_metric_log", "processors_profile_log", "latency_log", "trace_log"}

// logTableName matches a system log table and the numbered copies
// ClickHouse keeps after a table's definition changes.
var logTableName = regexp.MustCompile(`^([a-z_]+?)(_[0-9]+)?$`)

// logPolicy answers how a system table is treated: kept for days, emptied
// (off), or not a table this plugin bounds.
func logPolicy(table string) (days int, off, bounded bool) {
	match := logTableName.FindStringSubmatch(table)
	if match == nil {
		return 0, false, false
	}
	for _, name := range []string{table, match[1]} {
		if days, ok := logDays[name]; ok {
			return days, false, true
		}
		for _, candidate := range logsOff {
			if candidate == name {
				return 0, true, true
			}
		}
	}
	return 0, false, false
}

// maintain applies the limits to the existing system log tables. Tables
// that already have them are left alone, so repeated runs change nothing.
func maintain(ctx context.Context, m *machine, req pluginapi.Request) (pluginapi.Answer, error) {
	state, known, err := loadState(req.StateDir)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	if !known || state.Version == "" {
		return pluginapi.Answer{OK: true, Messages: []string{"This plugin runs no ClickHouse server on this host."}}, nil
	}
	if out, _ := m.run(ctx, "/usr/bin/systemctl", "show", "-p", "ActiveState", "--value", unit); strings.TrimSpace(out) != "active" {
		return pluginapi.Answer{OK: true, Messages: []string{"The ClickHouse server is not running; nothing to maintain."}}, nil
	}
	rows, err := m.admin(ctx, state, "SELECT name, toString(toUInt64(ifNull(total_rows, 0))), replaceAll(engine_full, '\\n', ' ') FROM system.tables WHERE database = 'system' AND engine LIKE '%MergeTree' ORDER BY name")
	if err != nil {
		return pluginapi.Answer{}, err
	}
	// A table that cannot be changed (ClickHouse marks a damaged leftover
	// permanently read-only) is named and skipped: it must not stop the
	// upkeep of the others.
	var emptied, bounded, skipped []string
	for _, row := range strings.Split(rows, "\n") {
		fields := strings.SplitN(row, "\t", 3)
		if len(fields) != 3 || !namePattern.MatchString(fields[0]) {
			continue
		}
		name := fields[0]
		days, off, ok := logPolicy(name)
		switch {
		case !ok:
		case off:
			// What a running server still writes until its next start, and
			// what was left from before. An empty table costs nothing.
			if count, _ := strconv.ParseUint(fields[1], 10, 64); count == 0 {
				continue
			}
			if _, err := m.admin(ctx, state, "TRUNCATE TABLE IF EXISTS system."+ident(name)); err != nil {
				if ctx.Err() != nil {
					return pluginapi.Answer{}, ctx.Err()
				}
				skipped = append(skipped, name+" ("+shortReason(err)+")")
				continue
			}
			emptied = append(emptied, name)
		case strings.Contains(fields[2], fmt.Sprintf("TTL event_date + toIntervalDay(%d)", days)):
		default:
			// Not materialised: rewriting gigabytes of old log parts at once
			// would load the server for nothing. ClickHouse drops the expired
			// parts in its regular merges over the following hours.
			if _, err := m.admin(ctx, state, fmt.Sprintf("ALTER TABLE system.%s MODIFY TTL event_date + INTERVAL %d DAY DELETE SETTINGS materialize_ttl_after_modify = 0", ident(name), days)); err != nil {
				if ctx.Err() != nil {
					return pluginapi.Answer{}, ctx.Err()
				}
				skipped = append(skipped, name+" ("+shortReason(err)+")")
				continue
			}
			bounded = append(bounded, name)
		}
	}
	messages := []string{}
	if len(emptied) > 0 {
		messages = append(messages, "Emptied the profiling logs "+strings.Join(emptied, ", ")+".")
	}
	if len(bounded) > 0 {
		messages = append(messages, "Limited how long "+strings.Join(bounded, ", ")+" are kept.")
	}
	if len(skipped) > 0 {
		messages = append(messages, "Could not change "+strings.Join(skipped, ", ")+"; the other tables were handled.")
	}
	size, _ := m.admin(ctx, state, "SELECT toUInt64(sum(bytes_on_disk)) FROM system.parts WHERE database = 'system' AND active AND match(table, '_log(_[0-9]+)?$')")
	bytes, _ := strconv.ParseUint(size, 10, 64)
	if len(messages) == 0 {
		messages = append(messages, "ClickHouse's own logs are within their limits.")
	}
	messages = append(messages, "They use "+gib(bytes)+".")
	return pluginapi.Answer{OK: true, Messages: messages}.WithOutput(map[string]uint64{"system_log_bytes": bytes})
}

// shortReason is the first line of an error, bounded, for a message.
func shortReason(err error) string {
	reason, _, _ := strings.Cut(err.Error(), "\n")
	if len(reason) > 160 {
		reason = reason[:160] + "…"
	}
	return reason
}
