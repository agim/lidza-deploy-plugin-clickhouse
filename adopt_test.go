package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/pluginapi"
)

// Taking over a server the built-in support runs changes nothing an
// application could notice: no package, no configuration file, no restart,
// no statement against its user.
func TestAHandOverTakesARunningServerWithoutTouchingIt(t *testing.T) {
	ctx := context.Background()
	f := newFakeServer(t)
	os.MkdirAll(filepath.Join(f.m.root, "usr/bin"), 0o755)
	os.WriteFile(filepath.Join(f.m.root, serverBinary), nil, 0o755)
	os.MkdirAll(filepath.Join(f.m.root, filepath.Dir(configFile)), 0o755)
	os.WriteFile(filepath.Join(f.m.root, configFile), []byte("<clickhouse><!-- written by the built-in support --></clickhouse>"), 0o640)
	f.databases["app_kiosk"] = "rows"
	inner := f.m.sql
	f.m.sql = func(ctx context.Context, port int, user, password, statement string) (string, error) {
		if user == "default" && password != "handed-admin" {
			return "", errors.New("ClickHouse answered 516: Authentication failed")
		}
		return inner(ctx, port, user, password, statement)
	}
	state := t.TempDir()
	handed := pluginapi.AppInput{App: "kiosk", Record: json.RawMessage(`{"database":"app_kiosk","http_port":18123,"query_memory_mib":768,"adopt":{"tcp_port":19000}}`),
		Secrets: map[string]string{"password": "the-apps-password", "admin_password": "handed-admin"}}

	// A request for this plugin's service is refused while the built-in support runs the server.
	answer, err := check(ctx, realHostFor(f), f.request(t, state, "check", pluginapi.AppInput{App: "kiosk"}, ""))
	if err != nil || answer.OK || find(t, answer.Checks, "ownership").OK {
		t.Fatalf("check before the hand-over: %v %+v", err, answer)
	}
	// A wrong administrator password changes nothing.
	wrong := handed
	wrong.Secrets = map[string]string{"password": "the-apps-password", "admin_password": "not-it"}
	if _, err := provision(ctx, f.m, f.request(t, state, "provision", wrong, "")); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("wrong administrator password: %v", err)
	}
	if _, known, _ := loadState(state); known {
		t.Fatal("a refused hand-over was recorded")
	}

	f.ran, f.sqls = nil, nil
	answer, err = provision(ctx, f.m, f.request(t, state, "provision", handed, ""))
	if err != nil || !answer.OK {
		t.Fatalf("hand-over: %v %+v", err, answer)
	}
	var out struct {
		Env     map[string]string `json:"env"`
		Record  map[string]any    `json:"record"`
		Secrets map[string]string `json:"secrets"`
	}
	json.Unmarshal(answer.Output, &out)
	if out.Env["CLICKHOUSE_URL"] != "http://127.0.0.1:18123" || out.Env["CLICKHOUSE_PASSWORD"] != "the-apps-password" || out.Env["CLICKHOUSE_USER"] != "app_kiosk" {
		t.Fatalf("the application's connection changed: %+v", out.Env)
	}
	if _, kept := out.Record["adopt"]; kept || out.Record["query_memory_mib"] != float64(768) || len(out.Secrets) != 1 || out.Secrets["password"] != "the-apps-password" {
		t.Fatalf("what the agent keeps: %+v %+v", out.Record, out.Secrets)
	}
	saved, known, _ := loadState(state)
	if !known || !saved.Adopted || saved.Admin != "handed-admin" || saved.HTTPPort != 18123 || saved.TCPPort != 19000 || saved.Version != "26.3.9.8" || len(saved.Databases) != 1 {
		t.Fatalf("state: %+v", saved)
	}
	if len(f.ran) != 0 {
		t.Fatalf("commands ran during the hand-over: %v", f.ran)
	}
	for _, statement := range f.sqls {
		for _, change := range []string{"CREATE ", "ALTER ", "REVOKE ", "GRANT ", "DROP "} {
			if strings.Contains(statement, change) {
				t.Fatalf("the hand-over changed something: %s", statement)
			}
		}
	}
	if config, _ := os.ReadFile(filepath.Join(f.m.root, configFile)); !strings.Contains(string(config), "written by the built-in support") || f.m.exists(usersFile) || f.m.exists(dropInFile) {
		t.Fatal("the hand-over wrote configuration files")
	}
	if info, err := os.Stat(filepath.Join(f.m.root, backupDir, backupSubdir)); err != nil || !info.IsDir() {
		t.Fatal("the plugin's directory on the backups disk is missing")
	}
	// From here on it is this plugin's server: a second application gets a
	// database without a reinstall, a restart or a rewritten configuration.
	f.ran = nil
	if _, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "blog"}, "")); err != nil {
		t.Fatalf("a new application after the hand-over: %v", err)
	}
	if f.ranLike("apt-get") || f.ranLike("restart") || f.ranLike("daemon-reload") {
		t.Fatalf("the adopted server was reinstalled or restarted: %v", f.ran)
	}
	if config, _ := os.ReadFile(filepath.Join(f.m.root, configFile)); !strings.Contains(string(config), "written by the built-in support") {
		t.Fatal("the adopted server's configuration was rewritten")
	}
	answer, err = check(ctx, realHostFor(f), f.request(t, state, "check", pluginapi.AppInput{App: "blog"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range answer.Checks {
		if c.ID == "ownership" {
			t.Fatalf("the ownership problem stayed after the hand-over: %+v", c)
		}
	}
}

// realHostFor is the read-only host view of a fake server.
func realHostFor(f *fakeServer) host {
	return &fakeHost{installed: f.m.exists(serverBinary), state: "active", version: "26.3.9.8", free: 100 << 30}
}
