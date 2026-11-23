package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/pluginapi"
)

func TestAMoveCopiesEverythingOnceAndThenOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	const move = "0123456789abcdef01234567"
	provisioned := func(f *fakeServer, state, app string) pluginapi.AppInput {
		answer, err := provision(ctx, f.m, f.request(t, state, "provision", pluginapi.AppInput{App: app}, ""))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Record json.RawMessage `json:"record"`
		}
		json.Unmarshal(answer.Output, &out)
		return pluginapi.AppInput{App: app, Record: out.Record}
	}
	in := func(app pluginapi.AppInput, phase string, full, incremental bool) pluginapi.AppInput {
		app.Move = &pluginapi.Move{ID: move, Phase: phase, Full: full, Incremental: incremental}
		return app
	}
	kept := func(f *fakeServer) []string {
		entries, _ := os.ReadDir(filepath.Join(f.m.root, backupDir, backupSubdir))
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	last := func(f *fakeServer, prefix string) string {
		for i := len(f.sqls) - 1; i >= 0; i-- {
			if statement := strings.TrimPrefix(f.sqls[i], "default: "); strings.HasPrefix(statement, prefix) {
				return statement
			}
		}
		return ""
	}
	source, sourceState := newFakeServer(t), t.TempDir()
	shop := provisioned(source, sourceState, "shop")
	source.databases["app_shop"] = "monday"

	// Bulk: a full backup, handed over, and kept here as the base.
	bulk := t.TempDir()
	if answer, err := export(ctx, source.m, source.request(t, sourceState, "export", in(shop, "bulk", false, false), bulk)); err != nil || !answer.OK || len(answer.Output) != 0 {
		t.Fatalf("bulk export: %v %+v", err, answer)
	}
	if rows, _ := os.ReadFile(filepath.Join(bulk, "backup", "data", "rows.bin")); string(rows) != "monday" || strings.Join(kept(source), " ") != "move-"+move+"-base" {
		t.Fatalf("bulk export: %q, kept %v", rows, kept(source))
	}
	if strings.Contains(last(source, "BACKUP"), "base_backup") {
		t.Fatalf("the bulk backup is incremental: %s", last(source, "BACKUP"))
	}
	// Final: only what changed, against the base; nothing new stays behind.
	source.databases["app_shop"] = "tuesday"
	final := t.TempDir()
	answer, err := export(ctx, source.m, source.request(t, sourceState, "export", in(shop, "final", false, false), final))
	if err != nil || !strings.Contains(string(answer.Output), `"incremental":true`) {
		t.Fatalf("final export: %v %+v", err, answer)
	}
	if want := "SETTINGS base_backup = Disk('backups', 'plugin/move-" + move + "-base/')"; !strings.Contains(last(source, "BACKUP"), want) || strings.Join(kept(source), " ") != "move-"+move+"-base" {
		t.Fatalf("final export: %s, kept %v", last(source, "BACKUP"), kept(source))
	}
	var manifest backupManifest
	raw, _ := os.ReadFile(filepath.Join(final, "manifest.json"))
	if json.Unmarshal(raw, &manifest); !manifest.Incremental || manifest.Database != "app_shop" {
		t.Fatalf("manifest: %s", raw)
	}
	// Asked for everything, or without its base, the final export is a full one.
	full := t.TempDir()
	if answer, err := export(ctx, source.m, source.request(t, sourceState, "export", in(shop, "final", true, false), full)); err != nil || strings.Contains(string(answer.Output), `"incremental":true`) || strings.Contains(last(source, "BACKUP"), "base_backup") {
		t.Fatalf("full final export: %v %+v", err, answer)
	}

	// The destination: another server, another application id is not allowed, the same one is.
	target, targetState := newFakeServer(t), t.TempDir()
	arrived := provisioned(target, targetState, "shop")
	// An increment cannot arrive before its base.
	if _, err := importMove(ctx, target.m, target.request(t, targetState, "import", in(arrived, "final", false, true), final)); err == nil || !strings.Contains(err.Error(), "bulk copy this increment builds on") {
		t.Fatalf("increment without a base: %v", err)
	}
	if answer, err := importMove(ctx, target.m, target.request(t, targetState, "import", in(arrived, "bulk", false, false), bulk)); err != nil || !answer.OK {
		t.Fatalf("bulk import: %v %+v", err, answer)
	}
	if target.databases["app_shop"] != "monday" || strings.Join(kept(target), " ") != "move-"+move+"-base" {
		t.Fatalf("after the bulk import: %v, kept %v", target.databases, kept(target))
	}
	if answer, err := importMove(ctx, target.m, target.request(t, targetState, "import", in(arrived, "final", false, true), final)); err != nil || !answer.OK {
		t.Fatalf("final import: %v %+v", err, answer)
	}
	if want := "FROM Disk('backups', 'plugin/move-" + move + "-final/') SETTINGS base_backup = Disk('backups', 'plugin/move-" + move + "-base/')"; !strings.Contains(last(target, "RESTORE"), want) {
		t.Fatalf("final import: %s", last(target, "RESTORE"))
	}
	if target.databases["app_shop"] != "tuesday" || len(target.databases) != 1 || strings.Join(kept(target), " ") != "move-"+move+"-base" {
		t.Fatalf("after the final import: %v, kept %v", target.databases, kept(target))
	}
	// The agent and the export must agree on what the files are.
	if _, err := importMove(ctx, target.m, target.request(t, targetState, "import", in(arrived, "final", false, false), final)); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("an increment imported as a full copy: %v", err)
	}
	// The end of the move removes what both servers kept.
	for _, side := range []struct {
		f     *fakeServer
		state string
		app   pluginapi.AppInput
		hook  func(context.Context, *machine, pluginapi.Request) (pluginapi.Answer, error)
	}{{source, sourceState, shop, export}, {target, targetState, arrived, importMove}} {
		if answer, err := side.hook(ctx, side.f.m, side.f.request(t, side.state, "end", in(side.app, "end", false, false), "")); err != nil || !answer.OK || len(kept(side.f)) != 0 {
			t.Fatalf("end of the move: %v %+v, kept %v", err, answer, kept(side.f))
		}
	}
	// Without a move, or with an invalid one, nothing runs.
	if _, err := export(ctx, source.m, source.request(t, sourceState, "export", shop, t.TempDir())); err == nil {
		t.Fatal("an export without a move was accepted")
	}
	bad := shop
	bad.Move = &pluginapi.Move{ID: "../escape", Phase: "bulk"}
	if _, err := export(ctx, source.m, source.request(t, sourceState, "export", bad, t.TempDir())); err == nil {
		t.Fatal("an invalid move id was accepted")
	}
}
