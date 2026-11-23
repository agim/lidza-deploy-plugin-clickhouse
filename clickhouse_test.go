package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/pluginapi"
)

// fakeHost is a server with switches for what is installed and running.
type fakeHost struct {
	installed bool
	state     string // ActiveState of the unit
	version   string
	dpkg      string
	free      uint64
	data      bool // ClickHouse data exists
	startErr  bool
	ran       []string
}

func (f *fakeHost) Exists(path string) bool {
	return path == "/usr/bin/dpkg-query" || path == serverBinary && f.installed
}

func (f *fakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.ran = append(f.ran, line)
	switch {
	case name == "/usr/bin/dpkg-query":
		return f.dpkg, nil
	case name == "/usr/bin/systemctl" && args[0] == "show":
		return f.state + "\n", nil
	case name == serverBinary:
		return "ClickHouse server version " + f.version + " (official build).\n", nil
	case name == "/usr/bin/systemctl" && args[0] == "start":
		if f.startErr {
			return "Job for clickhouse-server.service failed", errors.New("exit status 1")
		}
		f.state = "active"
	case name == "/usr/bin/apt-get":
		f.dpkg, f.installed = "", false
	case name == "/usr/bin/journalctl":
		return "Code: 76. Cannot open file /var/lib/clickhouse/status\n", nil
	}
	return "", nil
}

func (f *fakeHost) FreeBytes(string) (uint64, error) { return f.free, nil }
func (f *fakeHost) DirEmpty(string) bool             { return !f.data }

func (f *fakeHost) changed() bool {
	for _, line := range f.ran {
		if strings.Contains(line, "apt-get") || strings.Contains(line, "systemctl start") || strings.Contains(line, "reset-failed") {
			return true
		}
	}
	return false
}

func find(t *testing.T, checks []pluginapi.Check, id string) pluginapi.Check {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in %+v", id, checks)
	return pluginapi.Check{}
}

func request(flags []string, input any) pluginapi.Request {
	raw, _ := json.Marshal(input)
	return pluginapi.Request{Contract: pluginapi.Contract, Server: pluginapi.Server{Arch: "amd64", CPUFlags: flags}, Input: raw}
}

var (
	modern = []string{"sse4_2", "avx", "avx2", "bmi2", "fma"}
	old    = []string{"sse4_2", "popcnt", "avx", "f16c"}
)

func TestCheckKnowsWhichReleasesAProcessorCanRun(t *testing.T) {
	ctx := context.Background()
	app := func(series string) map[string]any {
		return map[string]any{"app": "shop", "options": map[string]string{"series": series}}
	}
	// A current processor runs everything.
	answer, err := check(ctx, &fakeHost{}, request(modern, app("26.8")))
	if c := find(t, answer.Checks, "processor"); err != nil || !answer.OK || !c.OK || c.Caution {
		t.Fatalf("modern processor: %v %+v", err, c)
	}
	// Without AVX2 a release from 26.6 on cannot run: the answer says what happens instead.
	answer, _ = check(ctx, &fakeHost{}, request(old, app("26.8")))
	if c := find(t, answer.Checks, "processor"); !answer.OK || !c.OK || !c.Caution || !strings.Contains(c.Detail, "installed instead of 26.8") || !strings.Contains(c.Detail, "table by table") {
		t.Fatalf("old processor, new series: %+v", c)
	}
	// An older series, or no application in mind, is no risk.
	answer, _ = check(ctx, &fakeHost{}, request(old, app("26.3")))
	if c := find(t, answer.Checks, "processor"); !c.OK || c.Caution {
		t.Fatalf("old processor, old series: %+v", c)
	}
	answer, _ = check(ctx, &fakeHost{}, request(old, nil))
	if c := find(t, answer.Checks, "processor"); !c.OK || c.Caution || !strings.Contains(c.Detail, "before 26.6") {
		t.Fatalf("old processor, server check: %+v", c)
	}
	// A release that already runs there is proof enough.
	answer, _ = check(ctx, &fakeHost{installed: true, state: "active", version: "26.3.69.4"}, request(old, app("26.8")))
	if c := find(t, answer.Checks, "processor"); !c.OK || c.Caution {
		t.Fatalf("old processor with a running server: %+v", c)
	}
	if c := find(t, answer.Checks, "server"); !c.OK || !strings.Contains(c.Detail, "26.3.69.4 is running") {
		t.Fatalf("running server: %+v", c)
	}
	if _, err := check(ctx, &fakeHost{}, request(modern, app("latest"))); err == nil {
		t.Fatal("an invalid series was accepted")
	}
	if compareSeries("26.10", "26.6") <= 0 || compareSeries("25.8", "26.3") >= 0 || compareSeries("26.6", "26.6") != 0 {
		t.Fatal("series are compared as numbers")
	}
}

func TestCheckMeasuresTheDiskAndNamesWhatIsBroken(t *testing.T) {
	ctx := context.Background()
	sized := map[string]any{"app": "shop", "sizes": map[string]int64{"clickhouse": 5 << 30}}
	h := &fakeHost{free: 4 << 30}
	answer, _ := check(ctx, h, request(modern, sized))
	if c := find(t, answer.Checks, "disk"); answer.OK || c.OK || !strings.Contains(c.Detail, "/var/lib/clickhouse has 4.0 GiB free; the data needs about 11.5 GiB there") {
		t.Fatalf("small disk: %+v", c)
	}
	h.free = 64 << 30
	if answer, _ = check(ctx, h, request(modern, sized)); !answer.OK || !find(t, answer.Checks, "disk").OK {
		t.Fatalf("large disk: %+v", answer.Checks)
	}
	// A server whose service failed can be repaired; one stopped on purpose is left alone.
	answer, _ = check(ctx, &fakeHost{installed: true, state: "failed"}, request(modern, nil))
	if c := find(t, answer.Checks, "server"); answer.OK || c.OK || !c.Repairable {
		t.Fatalf("failed server: %+v", c)
	}
	answer, _ = check(ctx, &fakeHost{installed: true, state: "inactive"}, request(modern, nil))
	if c := find(t, answer.Checks, "server"); !c.OK || !c.Caution || c.Repairable || !strings.Contains(c.Detail, "stopped on purpose") {
		t.Fatalf("stopped server: %+v", c)
	}
	// Half-installed packages are repairable only while no data exists.
	half := "iF clickhouse-server\nii clickhouse-client\niU clickhouse-common-static\n"
	answer, _ = check(ctx, &fakeHost{dpkg: half}, request(modern, nil))
	if c := find(t, answer.Checks, "packages"); c.OK || !c.Repairable || !strings.Contains(c.Detail, "clickhouse-server, clickhouse-common-static") {
		t.Fatalf("half-installed packages: %+v", c)
	}
	h = &fakeHost{dpkg: half, data: true}
	answer, _ = check(ctx, h, request(modern, nil))
	if c := find(t, answer.Checks, "packages"); c.OK || c.Repairable || !strings.Contains(c.Detail, "not removed automatically") {
		t.Fatalf("half-installed packages with data: %+v", c)
	}
	// Checking never changes the server.
	if h.changed() {
		t.Fatalf("check changed the server: %v", h.ran)
	}
}

func TestRepairStartsAFailedServerAndRemovesAnUnfinishedInstallation(t *testing.T) {
	ctx := context.Background()
	h := &fakeHost{installed: true, state: "failed"}
	answer, err := repair(ctx, h)
	if err != nil || !answer.OK || h.state != "active" || strings.Join(answer.Messages, "|") != "started the ClickHouse server" {
		t.Fatalf("failed server: %v %+v", err, answer)
	}
	h = &fakeHost{installed: true, state: "failed", startErr: true}
	if answer, _ = repair(ctx, h); answer.OK || !strings.Contains(strings.Join(answer.Messages, " "), "did not start: Code: 76") {
		t.Fatalf("server that does not start: %+v", answer)
	}
	half := "iF clickhouse-server\n"
	h = &fakeHost{installed: true, state: "inactive", dpkg: half}
	if answer, _ = repair(ctx, h); !answer.OK || !strings.Contains(answer.Messages[0], "removed the packages") || h.installed {
		t.Fatalf("unfinished installation: %+v", answer)
	}
	// With data on the server nothing is removed.
	h = &fakeHost{installed: true, state: "inactive", dpkg: half, data: true}
	if answer, _ = repair(ctx, h); answer.OK || h.changed() {
		t.Fatalf("unfinished installation with data: %+v (ran %v)", answer, h.ran)
	}
	// A server stopped on purpose is not started, and a healthy one is left alone.
	h = &fakeHost{installed: true, state: "inactive"}
	if answer, _ = repair(ctx, h); !answer.OK || h.changed() || answer.Messages[0] != "nothing needed repair" {
		t.Fatalf("stopped server: %+v (ran %v)", answer, h.ran)
	}
}

func TestStatusReportsTheServer(t *testing.T) {
	answer, err := status(context.Background(), &fakeHost{installed: true, state: "active", version: "26.3.69.4", free: 9 << 30})
	if err != nil || !answer.OK || !strings.Contains(string(answer.Output), `"version":"26.3.69.4"`) || !strings.Contains(string(answer.Output), `"series":"26.3"`) {
		t.Fatalf("running: %v %+v %s", err, answer, answer.Output)
	}
	if answer, _ = status(context.Background(), &fakeHost{installed: true, state: "failed"}); answer.OK || !strings.Contains(answer.Messages[0], "not running") {
		t.Fatalf("failed: %+v", answer)
	}
	if answer, _ = status(context.Background(), &fakeHost{}); !answer.OK || !strings.Contains(string(answer.Output), `"installed":false`) {
		t.Fatalf("not installed: %+v %s", answer, answer.Output)
	}
}
