package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/pluginapi"
)

// fakeServer is a machine whose packages, service and SQL are pretended:
// installing creates the server binary, BACKUP writes a directory on the
// backups disk and RESTORE reads one.
type fakeServer struct {
	t                                 *testing.T
	m                                 *machine
	ran, sqls                         []string
	madison                           string
	taken                             map[int]bool
	databases                         map[string]string // name -> content marker
	installErr, backupErr, restoreErr string
	fingerprint                       string
	operations                        map[string]string
}

var (
	backupTo    = regexp.MustCompile("^BACKUP DATABASE `([A-Za-z0-9_]+)` TO Disk\\('backups', '(plugin/[a-z0-9-]+)/'\\)(?: SETTINGS base_backup = Disk\\('backups', 'plugin/[a-z0-9-]+/'\\))? ASYNC$")
	restoreFrom = regexp.MustCompile("^RESTORE DATABASE `([A-Za-z0-9_]+)`(?: AS `([A-Za-z0-9_]+)`)? FROM Disk\\('backups', '(plugin/[a-z0-9-]+)/'\\)(?: SETTINGS base_backup = Disk\\('backups', 'plugin/[a-z0-9-]+/'\\))? ASYNC$")
	quoted      = regexp.MustCompile("[`']([A-Za-z0-9_]+)[`']")
)

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, taken: map[int]bool{}, databases: map[string]string{}, operations: map[string]string{}, fingerprint: keyFingerprint,
		madison: "clickhouse-server | 26.8.4.12 | https://packages.clickhouse.com/deb stable/main amd64 Packages\nclickhouse-server | 26.3.9.8 | x\nclickhouse-server | 25.8.1.1 | x\n"}
	root := t.TempDir()
	f.m = &machine{root: root, portTaken: func(port int) bool { return f.taken[port] }, account: func() (int, int, error) { return os.Getuid(), os.Getgid(), nil },
		download: func(context.Context, string) ([]byte, error) {
			return []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----"), nil
		}}
	f.m.run = func(_ context.Context, name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		f.ran = append(f.ran, line)
		switch {
		case name == "/usr/bin/gpg":
			return "pub:-:4096:1:8919F6BD2B48D754:1:::-:::scESC::::::23::0:\nfpr:::::::::" + f.fingerprint + ":\n", nil
		case name == "/usr/bin/dpkg":
			return "amd64\n", nil
		case name == "/usr/bin/apt-cache":
			return f.madison, nil
		case name == "/usr/bin/apt-get" && strings.Contains(line, "install") && strings.Contains(line, "clickhouse-server"):
			os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755)
			os.WriteFile(filepath.Join(root, serverBinary), nil, 0o755)
			if f.installErr != "" {
				return f.installErr, errors.New("exit status 100")
			}
		case name == "/usr/bin/apt-get" && strings.Contains(line, "purge"):
			os.Remove(filepath.Join(root, serverBinary))
		}
		return "", nil
	}
	f.m.sql = func(_ context.Context, port int, user, password, statement string) (string, error) {
		f.sqls = append(f.sqls, user+": "+statement)
		switch {
		case statement == "SELECT version()":
			return "26.3.9.8", nil
		case statement == "SELECT currentUser(), currentDatabase()":
			return user + "\t" + user, nil
		case strings.HasPrefix(statement, "CREATE DATABASE"):
			if name := quoted.FindStringSubmatch(statement)[1]; f.databases[name] == "" {
				f.databases[name] = "empty"
			}
		case strings.HasPrefix(statement, "DROP DATABASE"):
			delete(f.databases, quoted.FindStringSubmatch(statement)[1])
		case strings.HasPrefix(statement, "RENAME DATABASE"):
			names := quoted.FindAllStringSubmatch(statement, -1)
			f.databases[names[1][1]] = f.databases[names[0][1]]
			delete(f.databases, names[0][1])
		case strings.HasPrefix(statement, "SELECT count() FROM system.databases"):
			if _, ok := f.databases[quoted.FindStringSubmatch(statement)[1]]; ok {
				return "1", nil
			}
			return "0", nil
		case strings.HasPrefix(statement, "SELECT count() FROM system.tables"):
			return "3", nil
		case backupTo.MatchString(statement):
			match := backupTo.FindStringSubmatch(statement)
			dir := filepath.Join(root, backupDir, match[2])
			os.MkdirAll(filepath.Join(dir, "data"), 0o750)
			os.WriteFile(filepath.Join(dir, ".backup"), []byte("<meta/>"), 0o640)
			os.WriteFile(filepath.Join(dir, "data", "rows.bin"), []byte(f.databases[match[1]]), 0o640)
			f.operations["op-b"] = "BACKUP_CREATED\t"
			if f.backupErr != "" {
				f.operations["op-b"] = "BACKUP_FAILED\t" + f.backupErr
			}
			return "op-b\tCREATING_BACKUP", nil
		case restoreFrom.MatchString(statement):
			match := restoreFrom.FindStringSubmatch(statement)
			target := match[2]
			if target == "" {
				target = match[1]
			}
			if f.restoreErr != "" {
				f.databases[target] = "partial"
				f.operations["op-r"] = "RESTORE_FAILED\t" + f.restoreErr
				return "op-r\tRESTORING", nil
			}
			rows, err := os.ReadFile(filepath.Join(root, backupDir, match[3], "data", "rows.bin"))
			if err != nil {
				return "", errors.New("ClickHouse answered 500: backup not found")
			}
			f.databases[target] = string(rows)
			f.operations["op-r"] = "RESTORED\t"
			return "op-r\tRESTORING", nil
		case strings.HasPrefix(statement, "SELECT status"):
			return f.operations[strings.TrimSuffix(strings.SplitN(statement, "id = '", 2)[1], "'")], nil
		}
		return "", nil
	}
	return f
}

func (f *fakeServer) request(t *testing.T, state, hook string, input pluginapi.AppInput, work string) pluginapi.Request {
	raw, _ := json.Marshal(input)
	return pluginapi.Request{Contract: 1, Hook: hook, StateDir: state, WorkDir: work, Input: raw,
		Server: pluginapi.Server{Arch: "amd64", CPUFlags: []string{"sse4_2", "avx2"}, MemoryBytes: 8 << 30}}
}

func (f *fakeServer) ranLike(part string) bool {
	for _, line := range f.ran {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

func init() {
	readyLimit, readyPause, pollPause = 2*time.Second, time.Millisecond, time.Millisecond
}

func TestProvisionInstallsTheServerOnceAndGivesEachApplicationItsOwnDatabase(t *testing.T) {
	f := newFakeServer(t)
	state := t.TempDir()
	ctx := context.Background()
	f.taken[9000] = true // an application already listens on ClickHouse's default native port
	answer, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "shop-api", Options: json.RawMessage(`{"series":"26.3"}`)}, ""))
	if err != nil || !answer.OK {
		t.Fatalf("provision: %v %+v", err, answer)
	}
	var out struct {
		Env     map[string]string `json:"env"`
		Record  serviceRecord     `json:"record"`
		Secrets map[string]string `json:"secrets"`
	}
	json.Unmarshal(answer.Output, &out)
	password := out.Secrets["password"]
	if out.Env["CLICKHOUSE_URL"] != "http://127.0.0.1:8123" || out.Env["CLICKHOUSE_DATABASE"] != "app_shop_api" || out.Env["CLICKHOUSE_USER"] != "app_shop_api" || len(password) != 64 || out.Env["CLICKHOUSE_PASSWORD"] != password {
		t.Fatalf("environment: %+v", out.Env)
	}
	if out.Record.Database != "app_shop_api" || out.Record.HTTPPort != 8123 || out.Record.QueryMemoryMiB != 1024 {
		t.Fatalf("record: %+v", out.Record)
	}
	saved, known, _ := loadState(state)
	if !known || saved.TCPPort != 19000 || saved.Version != "26.3.9.8" || saved.Series != "26.3" || !saved.Installed || len(saved.Databases) != 1 {
		t.Fatalf("state: %+v", saved)
	}
	if info, _ := os.Stat(filepath.Join(state, stateFile)); info.Mode().Perm() != 0o600 {
		t.Fatalf("the state file is readable by others: %v", info.Mode())
	}
	config, _ := os.ReadFile(filepath.Join(f.m.root, configFile))
	users, _ := os.ReadFile(filepath.Join(f.m.root, usersFile))
	pin, _ := os.ReadFile(filepath.Join(f.m.root, preferencesFile))
	source, _ := os.ReadFile(filepath.Join(f.m.root, sourceListFile))
	if !strings.Contains(string(config), "<tcp_port>19000</tcp_port>") || !strings.Contains(string(config), "<listen_host>127.0.0.1</listen_host>") || !strings.Contains(string(config), `<trace_log remove="1"/>`) ||
		!strings.Contains(string(config), "<max_server_memory_usage>2147483648</max_server_memory_usage>") {
		t.Fatalf("config:\n%s", config)
	}
	if strings.Contains(string(users), saved.Admin) || !strings.Contains(string(users), "password_sha256_hex") || !strings.Contains(string(pin), "Pin: version 26.3.*") || !strings.Contains(string(source), "signed-by="+keyringFile) {
		t.Fatalf("users, pin or source list are wrong:\n%s\n%s\n%s", users, pin, source)
	}
	if !f.ranLike("systemctl enable "+unit) || !f.ranLike("systemctl restart "+unit) {
		t.Fatalf("the service was not enabled and started: %v", f.ran)
	}
	// The password travels as a salted hash, never in the clear.
	for _, statement := range f.sqls {
		if strings.Contains(statement, password) {
			t.Fatalf("the password is in a statement: %s", statement)
		}
	}
	if !strings.Contains(strings.Join(f.sqls, "\n"), "GRANT "+grants+" ON `app_shop_api`.* TO `app_shop_api`") {
		t.Fatalf("grants: %v", f.sqls)
	}

	// A second application reuses the server; a retry keeps the first one's password.
	f.ran = nil
	if _, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "blog"}, "")); err != nil {
		t.Fatal(err)
	}
	if f.ranLike("apt-get") || f.ranLike("restart") {
		t.Fatalf("the server was installed or restarted again: %v", f.ran)
	}
	record, _ := json.Marshal(out.Record)
	again, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "shop-api", Record: record, Secrets: out.Secrets}, ""))
	if err != nil || !strings.Contains(string(again.Output), password) {
		t.Fatalf("retry: %v %s", err, again.Output)
	}

	// Releasing one keeps the server; releasing the last stops it.
	f.ran = nil
	if answer, err := release(ctx, f.m, f.request(t, state, "release", pluginapi.AppInput{App: "blog"}, "")); err != nil || !answer.OK || f.ranLike("disable") {
		t.Fatalf("release: %v %+v %v", err, answer, f.ran)
	}
	if _, kept := f.databases["app_shop_api"]; !kept || len(f.databases) != 1 {
		t.Fatalf("databases after one release: %v", f.databases)
	}
	if answer, err := release(ctx, f.m, f.request(t, state, "release", pluginapi.AppInput{App: "shop-api"}, "")); err != nil || !answer.OK || !f.ranLike("systemctl disable --now "+unit) {
		t.Fatalf("last release: %v %+v %v", err, answer, f.ran)
	}
}

func TestProvisionRefusesWhatItMustNotTouchAndLeavesNothingHalfInstalled(t *testing.T) {
	ctx := context.Background()
	// A ClickHouse someone else installed is left alone.
	f := newFakeServer(t)
	os.MkdirAll(filepath.Join(f.m.root, "usr/bin"), 0o755)
	os.WriteFile(filepath.Join(f.m.root, serverBinary), nil, 0o755)
	if _, err := provision(ctx, f.m, f.request(t, t.TempDir(), "provision", pluginapi.AppInput{App: "shop"}, "")); err == nil || !strings.Contains(err.Error(), "was not installed by this plugin") || len(f.ran) != 0 {
		t.Fatalf("foreign server: %v %v", err, f.ran)
	}
	// A key with another fingerprint installs nothing.
	f = newFakeServer(t)
	f.fingerprint = strings.Repeat("A", 40)
	if _, err := provision(ctx, f.m, f.request(t, t.TempDir(), "provision", pluginapi.AppInput{App: "shop"}, "")); err == nil || !strings.Contains(err.Error(), "fingerprint") || f.ranLike("install -y -q --no-install-recommends clickhouse-server") || f.m.exists(sourceListFile) {
		t.Fatalf("wrong key: %v %v", err, f.ran)
	}
	// A release the processor cannot run is removed again and said plainly.
	f = newFakeServer(t)
	f.installErr = "dpkg: error processing package clickhouse-server (--configure): exit status 132"
	state := t.TempDir()
	if _, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "shop"}, "")); err == nil || !strings.Contains(err.Error(), "illegal instruction") || !strings.Contains(err.Error(), "removed again") || f.m.exists(serverBinary) {
		t.Fatalf("unsupported processor: %v", err)
	}
	// The retry continues with the same administrator password.
	first, _, _ := loadState(state)
	f.installErr = ""
	if _, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "shop"}, "")); err != nil {
		t.Fatalf("retry after a failed installation: %v", err)
	}
	if second, _, _ := loadState(state); second.Admin != first.Admin || first.Admin == "" {
		t.Fatal("the administrator password changed between attempts")
	}
	// Without AVX2 the newest long-term release that runs is chosen instead.
	if series, err := chooseSeries(f.madison, "26.8", true); err != nil || series != "26.3" {
		t.Fatalf("series without AVX2: %q %v", series, err)
	}
	if series, err := chooseSeries(f.madison, "", false); err != nil || series != "26.8" {
		t.Fatalf("default series: %q %v", series, err)
	}
	if _, err := chooseSeries(f.madison, "24.3", false); err == nil {
		t.Fatal("a series the repository does not have was accepted")
	}
	for _, bad := range []pluginapi.AppInput{{App: "Bad App"}, {App: "shop", Options: json.RawMessage(`{"series":"latest"}`)}} {
		if _, err := provision(ctx, newFakeServer(t).m, f.request(t, t.TempDir(), "provision", bad, "")); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestBackupHandsTheAgentADirectoryAndRestoreReplacesTheDatabaseSafely(t *testing.T) {
	f := newFakeServer(t)
	state := t.TempDir()
	ctx := context.Background()
	answer, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: "shop"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Record json.RawMessage `json:"record"`
	}
	json.Unmarshal(answer.Output, &out)
	app := pluginapi.AppInput{App: "shop", Record: out.Record}
	f.databases["app_shop"] = "monday"
	work := t.TempDir()
	if answer, err := backup(ctx, f.m, f.request(t, state, "backup", app, work)); err != nil || !answer.OK {
		t.Fatalf("backup: %v %+v", err, answer)
	}
	if rows, _ := os.ReadFile(filepath.Join(work, "backup", "data", "rows.bin")); string(rows) != "monday" {
		t.Fatalf("the backup's files: %q", rows)
	}
	var manifest backupManifest
	raw, _ := os.ReadFile(filepath.Join(work, "manifest.json"))
	if json.Unmarshal(raw, &manifest); manifest.Database != "app_shop" || manifest.Version != "26.3.9.8" || manifest.Format != 1 {
		t.Fatalf("manifest: %s", raw)
	}
	left := func() []os.DirEntry {
		entries, _ := os.ReadDir(filepath.Join(f.m.root, backupDir, backupSubdir))
		return entries
	}
	if len(left()) != 0 {
		t.Fatalf("the backup stayed on ClickHouse's disk: %v", left())
	}
	// A failed BACKUP is the hook's error and leaves nothing behind.
	f.backupErr = "Not enough space"
	if _, err := backup(ctx, f.m, f.request(t, state, "backup", app, t.TempDir())); err == nil || !strings.Contains(err.Error(), "Not enough space") || len(left()) != 0 {
		t.Fatalf("failed backup: %v %v", err, left())
	}
	f.backupErr = ""

	// The data changes; a failed restore leaves it as it was.
	f.databases["app_shop"] = "tuesday"
	f.restoreErr = "Cannot restore table"
	if _, err := restore(ctx, f.m, f.request(t, state, "restore", app, work)); err == nil || !strings.Contains(err.Error(), "as it was") {
		t.Fatalf("failed restore: %v", err)
	}
	if f.databases["app_shop"] != "tuesday" || len(f.databases) != 1 || len(left()) != 0 {
		t.Fatalf("after a failed restore: %v %v", f.databases, left())
	}
	// A good restore replaces it and drops the aside copy.
	f.restoreErr = ""
	if answer, err := restore(ctx, f.m, f.request(t, state, "restore", app, work)); err != nil || !answer.OK || !strings.Contains(answer.Messages[0], "3 tables") {
		t.Fatalf("restore: %v %+v", err, answer)
	}
	if f.databases["app_shop"] != "monday" || len(f.databases) != 1 || len(left()) != 0 {
		t.Fatalf("after the restore: %v %v", f.databases, left())
	}
	// A backup of another application's database is restored under this one's name.
	manifest.Database = "app_other"
	raw, _ = json.Marshal(manifest)
	os.WriteFile(filepath.Join(work, "manifest.json"), raw, 0o600)
	f.sqls = nil
	if _, err := restore(ctx, f.m, f.request(t, state, "restore", app, work)); err != nil || !strings.Contains(strings.Join(f.sqls, "\n"), "RESTORE DATABASE `app_other` AS `app_shop` FROM") {
		t.Fatalf("restore under another name: %v %v", err, f.sqls)
	}
	// A record of another application, or something that is not a backup, is refused.
	if _, err := backup(ctx, f.m, f.request(t, state, "backup", pluginapi.AppInput{App: "blog", Record: out.Record}, t.TempDir())); err == nil {
		t.Fatal("another application's record was accepted")
	}
	if _, err := restore(ctx, f.m, f.request(t, state, "restore", app, t.TempDir())); err == nil || !strings.Contains(err.Error(), "not a backup") {
		t.Fatalf("not a backup: %v", err)
	}
}
