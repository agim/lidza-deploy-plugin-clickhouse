package main

import (
	"context"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/pluginapi"
)

func TestMaintainBoundsTheServersOwnLogsAndChangesNothingTwice(t *testing.T) {
	f := newFakeServer(t)
	state := t.TempDir()
	ctx := context.Background()
	if answer, err := maintain(ctx, f.m, f.request(t, state, "maintain", pluginapi.AppInput{}, "")); err != nil || !answer.OK || len(f.sqls) != 0 {
		t.Fatalf("without a server: %v %+v %v", err, answer, f.sqls)
	}
	if _, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "shop"}, "")); err != nil {
		t.Fatal(err)
	}
	tables := "asynchronous_metric_log\t0\tMergeTree ORDER BY event_time\n" +
		"query_log\t900\tMergeTree PARTITION BY toYYYYMM(event_date) ORDER BY event_time\n" +
		"query_log_3\t12\tMergeTree ORDER BY event_time TTL event_date + toIntervalDay(3)\n" +
		"trace_log\t5000\tMergeTree ORDER BY event_time\n" +
		"trace_log_1\t70\tMergeTree ORDER BY event_time\n" +
		"events\t10\tMergeTree ORDER BY id"
	inner, active := f.m.sql, "active"
	f.m.sql = func(ctx context.Context, port int, user, password, statement string) (string, error) {
		switch {
		case strings.HasPrefix(statement, "SELECT name, toString"):
			return tables, nil
		case strings.HasPrefix(statement, "SELECT toUInt64(sum(bytes_on_disk))"):
			return "3221225472", nil
		}
		return inner(ctx, port, user, password, statement)
	}
	run := f.m.run
	f.m.run = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/usr/bin/systemctl" && args[0] == "show" {
			return active + "\n", nil
		}
		return run(ctx, name, args...)
	}
	f.sqls = nil
	answer, err := maintain(ctx, f.m, f.request(t, state, "maintain", pluginapi.AppInput{}, ""))
	if err != nil || !answer.OK || !strings.Contains(string(answer.Output), `"system_log_bytes":3221225472`) {
		t.Fatalf("maintain: %v %+v", err, answer)
	}
	all := strings.Join(f.sqls, "\n")
	for _, want := range []string{"TRUNCATE TABLE IF EXISTS system.`trace_log`", "TRUNCATE TABLE IF EXISTS system.`trace_log_1`",
		"ALTER TABLE system.`query_log` MODIFY TTL event_date + INTERVAL 3 DAY DELETE SETTINGS materialize_ttl_after_modify = 0"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in:\n%s", want, all)
		}
	}
	for _, never := range []string{"`asynchronous_metric_log`", "`query_log_3`", "`events`"} {
		if strings.Contains(all, never) {
			t.Fatalf("%s was changed:\n%s", never, all)
		}
	}
	// A stopped server is left alone.
	active, f.sqls = "inactive", nil
	if answer, err := maintain(ctx, f.m, f.request(t, state, "maintain", pluginapi.AppInput{}, "")); err != nil || !answer.OK || len(f.sqls) != 0 {
		t.Fatalf("stopped server: %v %+v %v", err, answer, f.sqls)
	}
}
