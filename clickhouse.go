package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/agim/lidza-deploy/pluginapi"
)

const (
	serverBinary = "/usr/bin/clickhouse-server"
	unit         = "clickhouse-server.service"
	dataDir      = "/var/lib/clickhouse"
	// avx2Series is the first release series whose published x86-64 build
	// needs AVX2 (with BMI and FMA). ClickHouse's own installer says its
	// default build targets x86-64-v3 and falls back for older processors.
	avx2Series = "26.6"
)

var (
	seriesPattern  = regexp.MustCompile(`^\d{1,3}\.\d{1,2}$`)
	versionPattern = regexp.MustCompile(`\b(\d{1,3}\.\d{1,2})\.\d+\.\d+\b`)
	packages       = []string{"clickhouse-server", "clickhouse-client", "clickhouse-common-static"}
)

// host is what the plugin needs from the server it runs on.
type host interface {
	// Exists reports whether a path exists.
	Exists(path string) bool
	// Run runs a command and returns its combined output.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// FreeBytes is the free space of the file system holding path, or of its
	// nearest existing parent.
	FreeBytes(path string) (uint64, error)
	// DirEmpty reports whether a directory is absent or has no entries.
	DirEmpty(path string) bool
}

// options are an application's settings for its ClickHouse.
type options struct {
	// Series is the release series the application wants, such as "26.8":
	// usually the series of the server it moves from.
	Series string `json:"series"`
}

func compareSeries(a, b string) int {
	parse := func(s string) (int, int) {
		major, minor, _ := strings.Cut(s, ".")
		x, _ := strconv.Atoi(major)
		y, _ := strconv.Atoi(minor)
		return x, y
	}
	ax, ay := parse(a)
	bx, by := parse(b)
	switch {
	case ax != bx:
		return ax - bx
	default:
		return ay - by
	}
}

func gib(bytes uint64) string { return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30)) }

// brokenPackages lists ClickHouse packages dpkg reports as unpacked or half
// configured: what an installation that did not complete leaves behind.
func brokenPackages(ctx context.Context, h host) []string {
	if !h.Exists("/usr/bin/dpkg-query") {
		return nil
	}
	out, err := h.Run(ctx, "/usr/bin/dpkg-query", "-W", "-f", `${db:Status-Abbrev} ${Package}\n`, "clickhouse-*")
	if err != nil {
		return nil
	}
	broken := []string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && len(fields[0]) >= 2 && fields[0][0] == 'i' && strings.ContainsRune("UFH", rune(fields[0][1])) {
			broken = append(broken, fields[1])
		}
	}
	return broken
}

func unitState(ctx context.Context, h host) string {
	out, _ := h.Run(ctx, "/usr/bin/systemctl", "show", "-p", "ActiveState", "--value", unit)
	return strings.TrimSpace(out)
}

func installedSeries(ctx context.Context, h host) (version, series string) {
	out, err := h.Run(ctx, serverBinary, "--version")
	if err != nil {
		return "", ""
	}
	if match := versionPattern.FindStringSubmatch(out); match != nil {
		return strings.TrimSpace(match[0]), match[1]
	}
	return "", ""
}

// check looks at the server with or without an application in mind. With
// one, the input says which series it wants and how much data will arrive.
func check(ctx context.Context, h host, req pluginapi.Request) (pluginapi.Answer, error) {
	var in pluginapi.AppInput
	if err := req.Decode(&in); err != nil {
		return pluginapi.Answer{}, err
	}
	var want options
	if len(in.Options) > 0 {
		if err := json.Unmarshal(in.Options, &want); err != nil || want.Series != "" && !seriesPattern.MatchString(want.Series) {
			return pluginapi.Answer{}, fmt.Errorf("the series option must be a release series such as 26.8")
		}
	}
	checks := []pluginapi.Check{}
	installed := h.Exists(serverBinary)
	broken := brokenPackages(ctx, h)
	state := ""
	if installed {
		state = unitState(ctx, h)
	}

	// The processor decides which releases can run at all.
	oldProcessor := req.Server.Arch == "amd64" && len(req.Server.CPUFlags) > 0 && !req.Server.HasCPUFlag("avx2")
	switch {
	case !oldProcessor:
		checks = append(checks, pluginapi.Check{ID: "processor", Title: "Processor", OK: true, Detail: "This processor runs every published ClickHouse release."})
	case installed && state == "active":
		checks = append(checks, pluginapi.Check{ID: "processor", Title: "Processor", OK: true, Detail: "This processor has no AVX2; the installed release runs on it. Releases from " + avx2Series + " on would not."})
	case want.Series != "" && compareSeries(want.Series, avx2Series) >= 0:
		checks = append(checks, pluginapi.Check{ID: "processor", Title: "Processor", OK: true, Caution: true,
			Detail: "This processor has no AVX2, which ClickHouse releases from " + avx2Series + " on need. The newest long-term release before that is installed instead of " + want.Series + ": the data is copied table by table into the older release, and anything that only " + want.Series + " has will not work."})
	default:
		checks = append(checks, pluginapi.Check{ID: "processor", Title: "Processor", OK: true, Detail: "This processor has no AVX2: ClickHouse releases before " + avx2Series + " run on it, later ones do not."})
	}

	// Packages of an installation that never completed block every later one.
	if len(broken) > 0 {
		empty := h.DirEmpty(dataDir + "/data")
		detail := "ClickHouse packages are only half installed (" + strings.Join(broken, ", ") + "); nothing can be installed or updated on this server until that is resolved."
		if !empty {
			detail += " ClickHouse data exists on this server, so they are not removed automatically."
		}
		checks = append(checks, pluginapi.Check{ID: "packages", Title: "ClickHouse packages", Detail: detail, Repairable: empty})
	}

	// The server itself.
	switch {
	case !installed:
		checks = append(checks, pluginapi.Check{ID: "server", Title: "ClickHouse server", OK: true, Detail: "ClickHouse is not installed on this server yet."})
	case state == "active":
		version, _ := installedSeries(ctx, h)
		checks = append(checks, pluginapi.Check{ID: "server", Title: "ClickHouse server", OK: true, Detail: strings.TrimSpace("The ClickHouse server " + version + " is running.")})
	case state == "failed":
		checks = append(checks, pluginapi.Check{ID: "server", Title: "ClickHouse server", Detail: "The ClickHouse server is down: its service failed.", Repairable: len(broken) == 0})
	default:
		checks = append(checks, pluginapi.Check{ID: "server", Title: "ClickHouse server", OK: true, Caution: true, Detail: "The ClickHouse server is installed but stopped (its service is " + state + " and did not fail, so it was stopped on purpose)."})
	}

	// A server the built-in support runs is not this plugin's to give
	// databases on, until it is handed over.
	if in.App != "" && installed && req.StateDir != "" {
		if _, known, err := loadState(req.StateDir); err == nil && !known {
			checks = append(checks, pluginapi.Check{ID: "ownership", Title: "ClickHouse ownership", Detail: "The ClickHouse on this server is run by Līdza Deploy's built-in support, not by this plugin. Hand it over first, in the server's Plugins tab."})
		}
	}

	// Room for the data, where ClickHouse keeps it. A backup is unpacked
	// next to the tables it restores.
	if incoming := in.Sizes["clickhouse"]; incoming > 0 {
		needed := uint64(float64(incoming)*2.1) + 1<<30
		free, err := h.FreeBytes(dataDir)
		switch {
		case err != nil:
			checks = append(checks, pluginapi.Check{ID: "disk", Title: "ClickHouse disk", Detail: "The free space under " + dataDir + " could not be measured."})
		case free < needed:
			checks = append(checks, pluginapi.Check{ID: "disk", Title: "ClickHouse disk", Detail: fmt.Sprintf("%s has %s free; the data needs about %s there (%s on the source, with room to load it).", dataDir, gib(free), gib(needed), gib(uint64(incoming)))})
		default:
			checks = append(checks, pluginapi.Check{ID: "disk", Title: "ClickHouse disk", OK: true, Detail: fmt.Sprintf("%s has %s free; about %s is needed.", dataDir, gib(free), gib(needed))})
		}
	}

	ok := true
	for _, c := range checks {
		ok = ok && c.OK
	}
	return pluginapi.Answer{OK: ok, Checks: checks}, nil
}

// status reports the server for the control panel's health view.
func status(ctx context.Context, h host) (pluginapi.Answer, error) {
	type report struct {
		Installed bool   `json:"installed"`
		Version   string `json:"version,omitempty"`
		Series    string `json:"series,omitempty"`
		State     string `json:"state,omitempty"`
		FreeBytes uint64 `json:"free_bytes,omitempty"`
	}
	out := report{Installed: h.Exists(serverBinary)}
	if !out.Installed {
		return pluginapi.Answer{OK: true, Messages: []string{"ClickHouse is not installed on this server."}}.WithOutput(out)
	}
	out.Version, out.Series = installedSeries(ctx, h)
	out.State = unitState(ctx, h)
	out.FreeBytes, _ = h.FreeBytes(dataDir)
	answer := pluginapi.Answer{OK: out.State == "active"}
	if !answer.OK {
		answer.Messages = []string{"The ClickHouse server is not running (its service is " + out.State + ")."}
	}
	return answer.WithOutput(out)
}

// repair fixes what check marks repairable: packages of an installation that
// never completed are removed when no ClickHouse data exists, and a server
// whose service failed is started.
func repair(ctx context.Context, h host) (pluginapi.Answer, error) {
	done := []string{}
	if broken := brokenPackages(ctx, h); len(broken) > 0 {
		if !h.DirEmpty(dataDir + "/data") {
			return pluginapi.Answer{Messages: []string{"ClickHouse packages are half installed and ClickHouse data exists on this server; resolve it by hand (dpkg --configure -a) rather than removing them."}}, nil
		}
		args := append([]string{"-o", "DPkg::Lock::Timeout=300", "purge", "-y", "-q"}, packages...)
		if out, err := h.Run(ctx, "/usr/bin/apt-get", args...); err != nil {
			return pluginapi.Answer{Messages: []string{"The half-installed ClickHouse packages could not be removed: " + lastLine(out)}}, nil
		}
		done = append(done, "removed the packages of a ClickHouse installation that did not complete ("+strings.Join(broken, ", ")+")")
	}
	if h.Exists(serverBinary) && unitState(ctx, h) == "failed" {
		_, _ = h.Run(ctx, "/usr/bin/systemctl", "reset-failed", unit)
		if out, err := h.Run(ctx, "/usr/bin/systemctl", "start", unit); err != nil {
			tail, _ := h.Run(ctx, "/usr/bin/journalctl", "-u", unit, "-n", "5", "--no-pager", "-o", "cat")
			return pluginapi.Answer{Messages: append(done, "The ClickHouse server did not start: "+lastLine(out+"\n"+tail))}, nil
		}
		done = append(done, "started the ClickHouse server")
	}
	if len(done) == 0 {
		done = []string{"nothing needed repair"}
	}
	return pluginapi.Answer{OK: true, Messages: done}, nil
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
