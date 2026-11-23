package main

// Moving an application's database to another server. The bulk phase copies
// a full BACKUP and keeps it on both servers as the base; the final phase,
// with the application stopped, copies an incremental BACKUP against that
// base, so only the parts written since travel. The destination restores
// the increment on top of its copy of the base. When the move ends, both
// servers drop what they kept.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/agim/lidza-deploy/pluginapi"
)

var moveID = regexp.MustCompile(`^[a-z0-9]{8,40}$`)

// moveBackups names what a move keeps on ClickHouse's backups disk.
func moveBackups(m *machine, id string) (base, final, basePath, finalPath string) {
	base, final = "move-"+id+"-base", "move-"+id+"-final"
	return base, final, m.path(filepath.Join(backupDir, backupSubdir, base)), m.path(filepath.Join(backupDir, backupSubdir, final))
}

func moveInput(req pluginapi.Request) (pluginapi.AppInput, serviceRecord, error) {
	var in pluginapi.AppInput
	if err := req.Decode(&in); err != nil {
		return in, serviceRecord{}, err
	}
	record, err := service(in)
	if err != nil {
		return in, record, err
	}
	if in.Move == nil || !moveID.MatchString(in.Move.ID) {
		return in, record, errors.New("the agent did not say which move this belongs to")
	}
	return in, record, nil
}

func writeManifest(dir string, manifest backupManifest) error {
	raw, _ := json.Marshal(manifest)
	return os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600)
}

// export leaves the database in the agent's directory: all of it in the
// bulk phase, and in the final phase only what changed since, when the base
// of the bulk phase is still on this server.
func export(ctx context.Context, m *machine, req pluginapi.Request) (pluginapi.Answer, error) {
	in, record, err := moveInput(req)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	base, final, basePath, finalPath := moveBackups(m, in.Move.ID)
	if in.Move.Phase == "end" {
		return pluginapi.Answer{OK: true, Messages: []string{"Removed what the move kept on this server."}}, errors.Join(os.RemoveAll(basePath), os.RemoveAll(finalPath))
	}
	if req.WorkDir == "" {
		return pluginapi.Answer{}, errors.New("the agent gave no directory for the export")
	}
	state, known, err := loadState(req.StateDir)
	if err != nil || !known {
		return pluginapi.Answer{}, errors.New("this plugin has no ClickHouse server on this host")
	}
	manifest := backupManifest{Format: 1, Database: record.Database, Version: state.Version, Created: time.Now().UTC().Format(time.RFC3339)}
	target := filepath.Join(req.WorkDir, "backup")
	switch in.Move.Phase {
	case "bulk":
		// A repeated bulk phase starts over: its base replaces the earlier one.
		if err := os.RemoveAll(basePath); err != nil {
			return pluginapi.Answer{}, err
		}
		if err := m.operation(ctx, state, "BACKUP DATABASE "+ident(record.Database)+" TO "+backupRef(base)); err != nil {
			return pluginapi.Answer{}, fmt.Errorf("the backup of %s failed: %w", record.Database, err)
		}
		// The base stays on ClickHouse's disk for the final phase.
		if err := m.copyTree(basePath, target, 0, 0); err != nil {
			return pluginapi.Answer{}, fmt.Errorf("the backup could not be handed to the agent: %w", err)
		}
		return pluginapi.Answer{OK: true, Messages: []string{"Exported the database " + record.Database + "; its backup stays here as the base of the final sync."}}, writeManifest(req.WorkDir, manifest)
	case "final":
		incremental := !in.Move.Full && m.existsPath(filepath.Join(basePath, ".backup"))
		if err := os.RemoveAll(finalPath); err != nil {
			return pluginapi.Answer{}, err
		}
		defer os.RemoveAll(finalPath)
		statement := "BACKUP DATABASE " + ident(record.Database) + " TO " + backupRef(final)
		if incremental {
			statement += " SETTINGS base_backup = " + backupRef(base)
		}
		if err := m.operation(ctx, state, statement); err != nil {
			return pluginapi.Answer{}, fmt.Errorf("the backup of %s failed: %w", record.Database, err)
		}
		if err := m.moveTree(finalPath, target); err != nil {
			return pluginapi.Answer{}, fmt.Errorf("the backup could not be handed to the agent: %w", err)
		}
		manifest.Incremental = incremental
		if err := writeManifest(req.WorkDir, manifest); err != nil {
			return pluginapi.Answer{}, err
		}
		message := "Exported the whole database " + record.Database + "."
		if incremental {
			message = "Exported what changed in " + record.Database + " since the bulk copy."
		}
		return pluginapi.Answer{OK: true, Messages: []string{message}}.WithOutput(pluginapi.Exported{Incremental: incremental})
	}
	return pluginapi.Answer{}, errors.New("unknown phase of a move")
}

// importMove restores what export left: the bulk phase's full backup, which
// stays on this server as the base, and the final phase's increment on top
// of it (or a full backup again).
func importMove(ctx context.Context, m *machine, req pluginapi.Request) (pluginapi.Answer, error) {
	in, record, err := moveInput(req)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	base, final, basePath, finalPath := moveBackups(m, in.Move.ID)
	if in.Move.Phase == "end" {
		return pluginapi.Answer{OK: true, Messages: []string{"Removed what the move kept on this server."}}, errors.Join(os.RemoveAll(basePath), os.RemoveAll(finalPath))
	}
	raw, err := os.ReadFile(filepath.Join(req.WorkDir, "manifest.json"))
	var manifest backupManifest
	if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Format != 1 || !namePattern.MatchString(manifest.Database) {
		return pluginapi.Answer{}, errors.New("this is not an export this plugin wrote")
	}
	if manifest.Incremental != in.Move.Incremental {
		return pluginapi.Answer{}, errors.New("the export and the agent disagree on whether this is an increment")
	}
	state, known, err := loadState(req.StateDir)
	if err != nil || !known {
		return pluginapi.Answer{}, errors.New("this plugin has no ClickHouse server on this host")
	}
	uid, gid, err := m.account()
	if err != nil {
		return pluginapi.Answer{}, err
	}
	staged, from := basePath, backupRef(base)
	switch {
	case in.Move.Phase == "bulk":
	case in.Move.Phase == "final" && manifest.Incremental:
		if !m.existsPath(filepath.Join(basePath, ".backup")) {
			return pluginapi.Answer{}, errors.New("the bulk copy this increment builds on is not on this server")
		}
		// The increment names the base where the source kept it; here it is
		// this server's copy.
		staged, from = finalPath, backupRef(final)+" SETTINGS base_backup = "+backupRef(base)
	case in.Move.Phase == "final":
		staged, from = finalPath, backupRef(final)
	default:
		return pluginapi.Answer{}, errors.New("unknown phase of a move")
	}
	if err := os.RemoveAll(staged); err != nil {
		return pluginapi.Answer{}, err
	}
	if in.Move.Phase == "final" {
		defer os.RemoveAll(staged)
	}
	if err := m.copyTree(filepath.Join(req.WorkDir, "backup"), staged, uid, gid); err != nil {
		return pluginapi.Answer{}, fmt.Errorf("the export could not be given to ClickHouse: %w", err)
	}
	if err := m.replaceDatabase(ctx, state, record.Database, manifest.Database, from); err != nil {
		return pluginapi.Answer{}, err
	}
	tables, _ := m.admin(ctx, state, "SELECT count() FROM system.tables WHERE database = "+literal(record.Database))
	what := "the bulk copy"
	if in.Move.Phase == "final" {
		what = "the final sync"
		if manifest.Incremental {
			what = "the final sync's changes"
		}
	}
	return pluginapi.Answer{OK: true, Messages: []string{"Imported " + what + " into " + record.Database + " (" + tables + " tables)."}}, nil
}
