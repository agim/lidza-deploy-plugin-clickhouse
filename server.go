package main

// The ClickHouse server of a host: one server for every application on it,
// installed from ClickHouse's own package repository, bound to loopback,
// with a memory limit and a random administrator password.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/pluginapi"
)

const (
	configFile      = "/etc/clickhouse-server/config.d/lidza.xml"
	usersFile       = "/etc/clickhouse-server/users.d/lidza.xml"
	dropInFile      = "/etc/systemd/system/clickhouse-server.service.d/lidza.conf"
	keyringFile     = "/etc/apt/keyrings/clickhouse.asc"
	sourceListFile  = "/etc/apt/sources.list.d/clickhouse.list"
	preferencesFile = "/etc/apt/preferences.d/lidza-clickhouse"
	backupDir       = "/var/lib/clickhouse/lidza-backups"
	backupSubdir    = "plugin"
	keyURL          = "https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key"
	keyFingerprint  = "3A9EA1193A97B548BE1457D48919F6BD2B48D754" // ClickHouse Inc. Repositories Key
	defaultHTTPPort = 8123
	defaultTCPPort  = 9000
	// ClickHouse falls back to these ranges when something else holds a
	// default port (an application listening on 9000 is common).
	fallbackHTTPPort = 18123
	fallbackTCPPort  = 19000
	fallbackSpan     = 200
	memoryRatio      = 0.25
	maxExecution     = 300 // seconds per query
	stateFile        = "server.json"
)

var fullVersionPattern = regexp.MustCompile(`^\d{1,3}\.\d{1,2}(\.\d+){0,2}$`)

// serverState is what the plugin keeps about the host's server, in its
// state directory, readable by root only.
type serverState struct {
	HTTPPort  int    `json:"http_port"`
	TCPPort   int    `json:"tcp_port"`
	Admin     string `json:"admin_password"` // of the default user
	Series    string `json:"series,omitempty"`
	Version   string `json:"version,omitempty"`
	Installed bool   `json:"installed"`
	// Databases are the applications' databases on this server.
	Databases []string `json:"databases,omitempty"`
}

func loadState(dir string) (serverState, bool, error) {
	var state serverState
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, false, errors.New("the plugin's record of the ClickHouse server cannot be read")
	}
	return state, state.Admin != "", nil
}

func saveState(dir string, state serverState) error {
	raw, _ := json.MarshalIndent(state, "", "  ")
	tmp := filepath.Join(dir, "."+stateFile+".new")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFile))
}

func randomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// memory is the server's own cap, its unit's hard limit and each
// application's per-query limit, in MiB.
func memory(server pluginapi.Server) (serverMiB, unitMiB, queryMiB int) {
	total := int(server.MemoryBytes >> 20)
	if total <= 0 {
		total = 4096
	}
	serverMiB = max(int(float64(total)*memoryRatio), 512)
	unitMiB = serverMiB + max(serverMiB/4, 256)
	queryMiB = max(serverMiB/2, 256)
	return serverMiB, unitMiB, queryMiB
}

// systemLogs bounds ClickHouse's own log tables: a few days of what explains
// a problem afterwards, and none of the profiling logs, which on a small
// server outgrow the applications' data.
func systemLogs() string {
	var b strings.Builder
	for _, table := range []struct {
		name string
		days int
	}{{"query_log", 3}, {"part_log", 3}, {"text_log", 3}, {"error_log", 14}} {
		extra := ""
		if table.name == "text_log" {
			extra = "<level>warning</level>"
		}
		fmt.Fprintf(&b, "    <%s>%s<ttl>event_date + INTERVAL %d DAY DELETE</ttl></%s>\n", table.name, extra, table.days, table.name)
	}
	for _, name := range []string{"query_thread_log", "query_views_log", "query_metric_log", "metric_log", "asynchronous_metric_log", "processors_profile_log", "latency_log", "trace_log"} {
		fmt.Fprintf(&b, "    <%s remove=\"1\"/>\n", name)
	}
	return b.String()
}

// renderConfig is config.d/lidza.xml. The server's cap is written in bytes:
// ClickHouse applies max_server_memory_usage_to_ram_ratio to its cgroup
// limit, so a ratio of 1 keeps the byte cap from being cut down.
func renderConfig(httpPort, tcpPort, serverMiB int, backups string) string {
	dir := strings.TrimRight(backups, "/") + "/"
	return fmt.Sprintf(`<?xml version="1.0"?>
<!-- Managed by the ClickHouse plugin of Līdza Deploy. Changes are overwritten. -->
<clickhouse>
    <listen_host remove="1"/>
    <listen_host>127.0.0.1</listen_host>
    <listen_host>::1</listen_host>
    <http_port>%d</http_port>
    <tcp_port>%d</tcp_port>
    <mysql_port remove="1"/>
    <postgresql_port remove="1"/>
    <interserver_http_port remove="1"/>
    <max_server_memory_usage>%d</max_server_memory_usage>
    <max_server_memory_usage_to_ram_ratio>1</max_server_memory_usage_to_ram_ratio>
    <cache_size_to_ram_max_ratio>%s</cache_size_to_ram_max_ratio>
    <mark_cache_size>268435456</mark_cache_size>
    <send_crash_reports><enabled>false</enabled></send_crash_reports>
    <logger>
        <level>warning</level>
        <size>100M</size>
        <count>5</count>
    </logger>
%s    <storage_configuration>
        <disks>
            <backups>
                <type>local</type>
                <path>%s</path>
            </backups>
        </disks>
    </storage_configuration>
    <backups>
        <allowed_disk>backups</allowed_disk>
        <allowed_path>%s</allowed_path>
        <remove_backup_files_after_failure>true</remove_backup_files_after_failure>
    </backups>
</clickhouse>
`, httpPort, tcpPort, int64(serverMiB)<<20, strconv.FormatFloat(memoryRatio/2, 'f', -1, 64), systemLogs(), dir, dir)
}

// renderUsers is users.d/lidza.xml: the default user keeps loopback access
// only, with the plugin's random password (as a SHA-256).
func renderUsers(admin string) string {
	sum := sha256.Sum256([]byte(admin))
	return fmt.Sprintf(`<?xml version="1.0"?>
<!-- Managed by the ClickHouse plugin of Līdza Deploy. Changes are overwritten. -->
<clickhouse>
    <users>
        <default>
            <password remove="1"/>
            <password_sha256_hex>%s</password_sha256_hex>
            <networks replace="replace">
                <ip>127.0.0.1</ip>
                <ip>::1</ip>
            </networks>
            <access_management>1</access_management>
        </default>
    </users>
</clickhouse>
`, hex.EncodeToString(sum[:]))
}

// renderDropIn caps the service's memory above ClickHouse's own limit and
// hardens the packaged unit.
func renderDropIn(unitMiB int) string {
	return fmt.Sprintf(`# Managed by the ClickHouse plugin of Līdza Deploy.
[Service]
MemoryMax=%dM
MemorySwapMax=0
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
IPAddressDeny=any
IPAddressAllow=localhost
`, unitMiB)
}

func renderPreferences(series string) string {
	return fmt.Sprintf(`# Managed by the ClickHouse plugin of Līdza Deploy: keep ClickHouse on the %s release series.
Package: clickhouse-server clickhouse-client clickhouse-common-static
Pin: version %s.*
Pin-Priority: 1001
`, series, series)
}

// chooseSeries picks the release series to install from the output of
// `apt-cache madison clickhouse-server`: the requested one, or the newest
// long-term series (x.3 and x.8), or the newest at all. A processor without
// AVX2 only runs series before avx2Series, so a newer request is replaced by
// the newest long-term series that can run.
func chooseSeries(madison, requested string, withoutAVX2 bool) (string, error) {
	available := map[string]bool{}
	for _, line := range strings.Split(madison, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}
		version := strings.TrimSpace(fields[1])
		if !fullVersionPattern.MatchString(version) {
			continue
		}
		parts := strings.SplitN(version, ".", 3)
		if series := parts[0] + "." + parts[1]; !withoutAVX2 || compareSeries(series, avx2Series) < 0 {
			available[series] = true
		}
	}
	if withoutAVX2 {
		if len(available) == 0 {
			return "", errors.New("the ClickHouse repository has no release this server's processor can run: it lacks AVX2, which releases from " + avx2Series + " need")
		}
		if !available[requested] {
			requested = ""
		}
	}
	if requested != "" {
		if !available[requested] {
			return "", fmt.Errorf("ClickHouse %s is not available from the ClickHouse repository", requested)
		}
		return requested, nil
	}
	best, bestLTS := "", ""
	for series := range available {
		if best == "" || compareSeries(series, best) > 0 {
			best = series
		}
		if minor := series[strings.Index(series, ".")+1:]; (minor == "3" || minor == "8") && (bestLTS == "" || compareSeries(series, bestLTS) > 0) {
			bestLTS = series
		}
	}
	if best == "" {
		return "", errors.New("the ClickHouse repository lists no server release")
	}
	if bestLTS != "" {
		return bestLTS, nil
	}
	return best, nil
}

func choosePorts(taken func(int) bool) (int, int, error) {
	pick := func(preferred, fallback, other int) (int, error) {
		if !taken(preferred) && preferred != other {
			return preferred, nil
		}
		for port := fallback; port < fallback+fallbackSpan; port++ {
			if !taken(port) && port != other {
				return port, nil
			}
		}
		return 0, fmt.Errorf("no free loopback port for ClickHouse between %d and %d", fallback, fallback+fallbackSpan-1)
	}
	httpPort, err := pick(defaultHTTPPort, fallbackHTTPPort, 0)
	if err != nil {
		return 0, 0, err
	}
	tcpPort, err := pick(defaultTCPPort, fallbackTCPPort, httpPort)
	return httpPort, tcpPort, err
}

func gpgFingerprints(out string) []string {
	var fingerprints []string
	expect := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ":")
		switch {
		case fields[0] == "pub":
			expect = true
		case fields[0] == "fpr" && expect && len(fields) > 9:
			fingerprints = append(fingerprints, fields[9])
			expect = false
		case fields[0] == "sub":
			expect = false
		}
	}
	return fingerprints
}

func (m *machine) apt(ctx context.Context, args ...string) (string, error) {
	return m.run(ctx, "/usr/bin/apt-get", append([]string{"-o", "DPkg::Lock::Timeout=300"}, args...)...)
}

func (m *machine) aptInstall(ctx context.Context, names ...string) (string, error) {
	return m.apt(ctx, append([]string{"-o", "Dpkg::Options::=--force-confold", "install", "-y", "-q", "--no-install-recommends"}, names...)...)
}

func (m *machine) purge(ctx context.Context) bool {
	_, err := m.apt(ctx, append([]string{"purge", "-y", "-q"}, packages...)...)
	return err == nil
}

// writeFiles writes the managed configuration. Before the packages exist
// the files are root-only, so a server the package might start cannot run
// with a passwordless default user.
func (m *machine) writeFiles(server pluginapi.Server, state serverState, preinstall bool) (bool, error) {
	serverMiB, unitMiB, _ := memory(server)
	mode, uid, gid := os.FileMode(0o600), 0, 0
	if !preinstall {
		var err error
		if uid, gid, err = m.account(); err != nil {
			return false, err
		}
		mode = 0o640
		if err := m.mkdir(backupDir, uid, gid); err != nil {
			return false, err
		}
		if err := m.mkdir(filepath.Join(backupDir, backupSubdir), uid, gid); err != nil {
			return false, err
		}
	}
	changed := false
	for path, data := range map[string]string{configFile: renderConfig(state.HTTPPort, state.TCPPort, serverMiB, backupDir), usersFile: renderUsers(state.Admin)} {
		written, err := m.write(path, []byte(data), mode, 0, gid)
		if err != nil {
			return false, err
		}
		changed = changed || written
	}
	written, err := m.write(dropInFile, []byte(renderDropIn(unitMiB)), 0o644, 0, 0)
	return changed || written, err
}

// install adds ClickHouse's verified package repository, pins the series
// and installs the server and client. It answers the installed series.
func (m *machine) install(ctx context.Context, req pluginapi.Request, series string, state serverState) (string, error) {
	if !m.exists("/usr/bin/gpg") {
		if out, err := m.aptInstall(ctx, "gnupg", "ca-certificates"); err != nil {
			if _, updateErr := m.apt(ctx, "update", "-q"); updateErr != nil {
				return "", errors.New("gnupg could not be installed: " + lastLine(out))
			}
			if out, err = m.aptInstall(ctx, "gnupg", "ca-certificates"); err != nil {
				return "", errors.New("gnupg could not be installed: " + lastLine(out))
			}
		}
	}
	key, err := m.download(ctx, keyURL)
	if err != nil {
		return "", err
	}
	staged := keyringFile + ".download"
	if _, err := m.write(staged, key, 0o644, 0, 0); err != nil {
		return "", err
	}
	defer os.Remove(m.path(staged))
	out, err := m.run(ctx, "/usr/bin/gpg", "--batch", "--no-default-keyring", "--show-keys", "--with-colons", "--with-fingerprint", m.path(staged))
	if err != nil {
		return "", errors.New("the ClickHouse signing key could not be read with gpg")
	}
	if found := gpgFingerprints(out); len(found) != 1 || !strings.EqualFold(found[0], keyFingerprint) {
		return "", fmt.Errorf("the ClickHouse signing key fingerprint %v does not match the published %s; nothing was installed", found, keyFingerprint)
	}
	arch, err := m.run(ctx, "/usr/bin/dpkg", "--print-architecture")
	arch = strings.TrimSpace(arch)
	if err != nil || arch == "" || strings.ContainsAny(arch, " \t\n/]") {
		return "", errors.New("the package architecture could not be determined")
	}
	if _, err := m.write(keyringFile, key, 0o644, 0, 0); err != nil {
		return "", err
	}
	if _, err := m.write(sourceListFile, []byte("deb [signed-by="+keyringFile+" arch="+arch+"] https://packages.clickhouse.com/deb stable main\n"), 0o644, 0, 0); err != nil {
		return "", err
	}
	if out, err := m.apt(ctx, "update", "-q"); err != nil {
		return "", errors.New("the package index could not be updated: " + lastLine(out))
	}
	madison, err := m.run(ctx, "/usr/bin/apt-cache", "madison", "clickhouse-server")
	if err != nil {
		return "", errors.New("the ClickHouse repository could not be read")
	}
	withoutAVX2 := req.Server.Arch == "amd64" && len(req.Server.CPUFlags) > 0 && !req.Server.HasCPUFlag("avx2")
	if series, err = chooseSeries(madison, series, withoutAVX2); err != nil {
		return "", err
	}
	if _, err := m.write(preferencesFile, []byte(renderPreferences(series)), 0o644, 0, 0); err != nil {
		return "", err
	}
	if _, err := m.writeFiles(req.Server, state, true); err != nil {
		return "", err
	}
	if out, err := m.aptInstall(ctx, "clickhouse-server", "clickhouse-client"); err != nil {
		// Nothing of a failed installation stays behind: a half-configured
		// package stops every later package operation on the server.
		suffix := ""
		if m.purge(ctx) {
			suffix = " The packages were removed again; the server is as it was."
		}
		if lower := strings.ToLower(out); strings.Contains(lower, "exit status 132") || strings.Contains(lower, "illegal instruction") {
			return "", errors.New("ClickHouse " + series + " cannot run on this server's processor: its programs stop with an illegal instruction, so they need processor features this one does not have. Host this application's ClickHouse on a server with a newer processor." + suffix)
		}
		return "", errors.New("ClickHouse " + series + " could not be installed: " + lastLine(out) + "." + suffix)
	}
	return series, nil
}

// admin runs a statement as the server's administrator.
func (m *machine) admin(ctx context.Context, state serverState, statement string) (string, error) {
	return m.sql(ctx, state.HTTPPort, "default", state.Admin, statement)
}

// waitReady polls until the server answers an authenticated query.
var readyLimit, readyPause = 2 * time.Minute, 500 * time.Millisecond

func (m *machine) waitReady(ctx context.Context, state serverState) (string, error) {
	deadline := time.Now().Add(readyLimit)
	for {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		version, err := m.admin(probe, state, "SELECT version()")
		cancel()
		if err == nil && fullVersionPattern.MatchString(version) {
			return version, nil
		}
		if err == nil {
			err = errors.New("it reported an unexpected version")
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("ClickHouse did not become ready: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(readyPause):
		}
	}
}

// ensureServer installs, configures and starts the host's server when
// needed and answers its record. A server this plugin did not install is
// left alone.
func (m *machine) ensureServer(ctx context.Context, req pluginapi.Request, series string) (serverState, error) {
	state, known, err := loadState(req.StateDir)
	if err != nil {
		return state, err
	}
	installed := m.exists(serverBinary)
	if !known {
		if installed {
			return state, errors.New("ClickHouse is already installed on this server and was not installed by this plugin; it is left unchanged")
		}
		if state.HTTPPort, state.TCPPort, err = choosePorts(m.portTaken); err != nil {
			return state, err
		}
		state.Admin = randomSecret()
		// Recorded before anything is installed: a retry continues with the
		// same administrator password.
		if err := saveState(req.StateDir, state); err != nil {
			return state, err
		}
	}
	if known && state.Version == "" {
		// The server never became ready, so it holds no data: if another
		// process took one of its ports, it moves to free ones.
		if installed {
			_, _ = m.run(ctx, "/usr/bin/systemctl", "stop", unit)
		}
		if m.portTaken(state.HTTPPort) || m.portTaken(state.TCPPort) {
			if state.HTTPPort, state.TCPPort, err = choosePorts(m.portTaken); err != nil {
				return state, err
			}
			if err := saveState(req.StateDir, state); err != nil {
				return state, err
			}
		}
	}
	if installed && !state.Installed {
		// Packages of an installation that never completed: remove them and
		// install afresh.
		if m.purge(ctx) {
			installed = false
		}
	}
	if !installed {
		chosen, err := m.install(ctx, req, series, state)
		if err != nil {
			return state, err
		}
		state.Series, state.Installed = chosen, true
		if err := saveState(req.StateDir, state); err != nil {
			return state, err
		}
	}
	changed, err := m.writeFiles(req.Server, state, false)
	if err != nil {
		return state, err
	}
	if out, err := m.run(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return state, errors.New("systemd could not be reloaded: " + lastLine(out))
	}
	if out, err := m.run(ctx, "/usr/bin/systemctl", "enable", unit); err != nil {
		return state, errors.New("the ClickHouse service could not be enabled: " + lastLine(out))
	}
	action := "start"
	if changed {
		action = "restart"
	}
	if out, err := m.run(ctx, "/usr/bin/systemctl", action, unit); err != nil {
		return state, errors.New("the ClickHouse server did not start: " + lastLine(out))
	}
	version, err := m.waitReady(ctx, state)
	if err != nil {
		return state, err
	}
	parts := strings.SplitN(version, ".", 3)
	state.Version, state.Series, state.Installed = version, parts[0]+"."+parts[1], true
	return state, saveState(req.StateDir, state)
}
