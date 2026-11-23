package main

// An application's ClickHouse: a database, a settings profile and a user of
// the same name on the host's server, its backup and its restore.

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
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/pluginapi"
)

// grants are the application user's privileges, on its own database only.
// CREATE DATABASE on it lets migrations run "CREATE DATABASE IF NOT EXISTS"
// for their own name.
const grants = "SELECT, INSERT, ALTER, CREATE DATABASE, CREATE TABLE, CREATE VIEW, CREATE DICTIONARY, DROP TABLE, DROP VIEW, DROP DICTIONARY, TRUNCATE, OPTIMIZE, SHOW TABLES, SHOW COLUMNS, SHOW DICTIONARIES, dictGet"

const previousSuffix = "_lidza_previous"

var (
	appPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	operationID = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)
)

// serviceRecord is what the agent keeps for an application's database and
// hands back on every later hook.
type serviceRecord struct {
	Database       string `json:"database"`
	HTTPPort       int    `json:"http_port"`
	QueryMemoryMiB int    `json:"query_memory_mib,omitempty"`
}

// backupManifest describes a backup directory.
type backupManifest struct {
	Format   int    `json:"format"`
	Database string `json:"database"`
	Version  string `json:"version"`
	Created  string `json:"created"`
}

func ident(name string) string { return "`" + name + "`" } // names match namePattern

func literal(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(value) + "'"
}

// databaseName is the database, user and profile of an application.
func databaseName(app string) (string, error) {
	if !appPattern.MatchString(app) {
		return "", errors.New("invalid application id")
	}
	return "app_" + strings.ReplaceAll(app, "-", "_"), nil
}

// service reads the application's record, as provision answered it.
func service(in pluginapi.AppInput) (serviceRecord, error) {
	var record serviceRecord
	if len(in.Record) == 0 {
		return record, errors.New("the application has no ClickHouse database from this plugin")
	}
	if err := json.Unmarshal(in.Record, &record); err != nil || !namePattern.MatchString(record.Database) {
		return record, errors.New("the application's ClickHouse record is not understood")
	}
	if name, err := databaseName(in.App); err != nil || name != record.Database {
		return record, errors.New("the ClickHouse record belongs to another application")
	}
	return record, nil
}

func provisionSQL(name, password string, queryMiB int) []string {
	var salt [16]byte
	_, _ = rand.Read(salt[:])
	saltHex := hex.EncodeToString(salt[:])
	sum := sha256.Sum256([]byte(password + saltHex))
	identified := fmt.Sprintf("IDENTIFIED WITH sha256_hash BY '%s' SALT '%s' HOST IP '127.0.0.1', IP '::1' DEFAULT DATABASE %s SETTINGS PROFILE %s", hex.EncodeToString(sum[:]), saltHex, ident(name), literal(name))
	return []string{
		"CREATE DATABASE IF NOT EXISTS " + ident(name) + " ENGINE = Atomic",
		"CREATE SETTINGS PROFILE IF NOT EXISTS " + ident(name),
		fmt.Sprintf("ALTER SETTINGS PROFILE %s SETTINGS max_memory_usage = %d, max_execution_time = %d, max_concurrent_queries_for_user = 32", ident(name), int64(queryMiB)<<20, maxExecution),
		"CREATE USER IF NOT EXISTS " + ident(name) + " " + identified,
		"ALTER USER " + ident(name) + " " + identified,
		"REVOKE ALL ON *.* FROM " + ident(name),
		"GRANT " + grants + " ON " + ident(name) + ".* TO " + ident(name),
	}
}

// provision creates or repairs the application's database, profile and
// user. A retry keeps the password it answered before.
func provision(ctx context.Context, m *machine, req pluginapi.Request) (pluginapi.Answer, error) {
	var in pluginapi.AppInput
	if err := req.Decode(&in); err != nil {
		return pluginapi.Answer{}, err
	}
	name, err := databaseName(in.App)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	var want options
	if len(in.Options) > 0 {
		if err := json.Unmarshal(in.Options, &want); err != nil || want.Series != "" && !seriesPattern.MatchString(want.Series) {
			return pluginapi.Answer{}, errors.New("the series option must be a release series such as 26.8")
		}
	}
	password := in.Secrets["password"]
	if password == "" {
		password = randomSecret()
	}
	state, err := m.ensureServer(ctx, req, want.Series)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	_, _, queryMiB := memory(req.Server)
	for _, statement := range provisionSQL(name, password, queryMiB) {
		if _, err := m.admin(ctx, state, statement); err != nil {
			return pluginapi.Answer{}, fmt.Errorf("the application's database and user could not be created: %w", err)
		}
	}
	// Prove the application's own credential before handing it out.
	if who, err := m.sql(ctx, state.HTTPPort, name, password, "SELECT currentUser(), currentDatabase()"); err != nil || who != name+"\t"+name {
		return pluginapi.Answer{}, errors.New("the application's ClickHouse user could not sign in")
	}
	if !slices.Contains(state.Databases, name) {
		state.Databases = append(state.Databases, name)
		slices.Sort(state.Databases)
		if err := saveState(req.StateDir, state); err != nil {
			return pluginapi.Answer{}, err
		}
	}
	return pluginapi.Answer{OK: true, Messages: []string{"ClickHouse " + state.Version + " serves the database " + name + "."}}.WithOutput(pluginapi.Provisioned{
		Env:     map[string]string{"CLICKHOUSE_URL": fmt.Sprintf("http://127.0.0.1:%d", state.HTTPPort), "CLICKHOUSE_DATABASE": name, "CLICKHOUSE_USER": name, "CLICKHOUSE_PASSWORD": password},
		Record:  serviceRecord{Database: name, HTTPPort: state.HTTPPort, QueryMemoryMiB: queryMiB},
		Secrets: map[string]string{"password": password},
	})
}

// release removes the application's user first, so no new session starts,
// then its queries, profile and database. The server stops when no
// application uses it any more; its packages and data directory stay.
func release(ctx context.Context, m *machine, req pluginapi.Request) (pluginapi.Answer, error) {
	var in pluginapi.AppInput
	if err := req.Decode(&in); err != nil {
		return pluginapi.Answer{}, err
	}
	name, err := databaseName(in.App)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	state, known, err := loadState(req.StateDir)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	if !known || !state.Installed || state.Version == "" {
		// The server never ran, so there is nothing of the application on it.
		return pluginapi.Answer{OK: true, Messages: []string{"There was no ClickHouse database to remove."}}, nil
	}
	for _, statement := range []string{
		"DROP USER IF EXISTS " + ident(name),
		"KILL QUERY WHERE user = " + literal(name) + " ASYNC",
		"DROP SETTINGS PROFILE IF EXISTS " + ident(name),
		"DROP DATABASE IF EXISTS " + ident(name) + " SYNC",
	} {
		if _, err := m.admin(ctx, state, statement); err != nil {
			return pluginapi.Answer{}, fmt.Errorf("the application's ClickHouse database could not be removed; retry the removal: %w", err)
		}
	}
	state.Databases = slices.DeleteFunc(state.Databases, func(existing string) bool { return existing == name })
	if err := saveState(req.StateDir, state); err != nil {
		return pluginapi.Answer{}, err
	}
	messages := []string{"Removed the database, user and settings profile " + name + "."}
	if len(state.Databases) == 0 {
		if out, err := m.run(ctx, "/usr/bin/systemctl", "disable", "--now", unit); err != nil {
			return pluginapi.Answer{}, errors.New("the unused ClickHouse server could not be stopped: " + lastLine(out))
		}
		messages = append(messages, "No application uses ClickHouse any more; its server was stopped. Its packages and data directory stay.")
	}
	return pluginapi.Answer{OK: true, Messages: messages}, nil
}

// pollPause paces status polls of BACKUP and RESTORE; tests shorten it.
var pollPause = time.Second

// operation starts BACKUP or RESTORE with ASYNC and waits for its final
// status, so a long operation never depends on one HTTP request. A stopped
// hook kills the operation instead of leaving it running.
func (m *machine) operation(ctx context.Context, state serverState, statement string) error {
	out, err := m.admin(ctx, state, statement+" ASYNC")
	if err != nil {
		return err
	}
	id, _, _ := strings.Cut(strings.SplitN(out, "\n", 2)[0], "\t")
	if !operationID.MatchString(id) {
		return errors.New("ClickHouse did not report the operation's id")
	}
	finished := false
	defer func() {
		if finished {
			return
		}
		kill, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if query, err := m.admin(kill, state, "SELECT query_id FROM system.backups WHERE id = "+literal(id)); err == nil && query != "" && !strings.ContainsAny(query, "\n\t") {
			_, _ = m.admin(kill, state, "KILL QUERY WHERE query_id = "+literal(query)+" ASYNC")
		}
	}()
	missing := time.Time{}
	for {
		row, err := m.admin(ctx, state, "SELECT status, replaceAll(error, '\\n', ' ') FROM system.backups WHERE id = "+literal(id))
		if err != nil {
			return err
		}
		status, detail, _ := strings.Cut(row, "\t")
		switch {
		case row == "":
			if missing.IsZero() {
				missing = time.Now()
			} else if time.Since(missing) > 30*time.Second {
				return errors.New("ClickHouse lost track of the operation; was the server restarted?")
			}
		case status == "BACKUP_CREATED" || status == "RESTORED":
			finished = true
			return nil
		case strings.HasSuffix(status, "_FAILED") || strings.HasSuffix(status, "_CANCELLED"):
			finished = true
			return errors.New("ClickHouse reported: " + strings.TrimSpace(detail))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollPause):
		}
	}
}

func newBackupName(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

func backupRef(name string) string {
	return "Disk('backups', '" + backupSubdir + "/" + name + "/')"
}

// backup writes a ClickHouse BACKUP of the application's database into the
// directory the agent gave, with a manifest that says what it holds.
func backup(ctx context.Context, m *machine, req pluginapi.Request) (pluginapi.Answer, error) {
	var in pluginapi.AppInput
	if err := req.Decode(&in); err != nil {
		return pluginapi.Answer{}, err
	}
	record, err := service(in)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	if req.WorkDir == "" {
		return pluginapi.Answer{}, errors.New("the agent gave no directory for the backup")
	}
	state, known, err := loadState(req.StateDir)
	if err != nil || !known {
		return pluginapi.Answer{}, errors.New("this plugin has no ClickHouse server on this host")
	}
	name := newBackupName("backup")
	written := m.path(filepath.Join(backupDir, backupSubdir, name))
	defer os.RemoveAll(written)
	if err := m.operation(ctx, state, "BACKUP DATABASE "+ident(record.Database)+" TO "+backupRef(name)); err != nil {
		return pluginapi.Answer{}, fmt.Errorf("the backup of %s failed: %w", record.Database, err)
	}
	if err := m.moveTree(written, filepath.Join(req.WorkDir, "backup")); err != nil {
		return pluginapi.Answer{}, fmt.Errorf("the backup could not be handed to the agent: %w", err)
	}
	manifest, _ := json.Marshal(backupManifest{Format: 1, Database: record.Database, Version: state.Version, Created: time.Now().UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(req.WorkDir, "manifest.json"), manifest, 0o600); err != nil {
		return pluginapi.Answer{}, err
	}
	return pluginapi.Answer{OK: true, Messages: []string{"Backed up the database " + record.Database + " from ClickHouse " + state.Version + "."}}, nil
}

// restore replaces the application's database with a backup so that a
// failure leaves the database as it was: the current database is renamed
// aside, the backup is restored under the original name, and the aside copy
// is dropped only after that succeeded.
func restore(ctx context.Context, m *machine, req pluginapi.Request) (pluginapi.Answer, error) {
	var in pluginapi.AppInput
	if err := req.Decode(&in); err != nil {
		return pluginapi.Answer{}, err
	}
	record, err := service(in)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	raw, err := os.ReadFile(filepath.Join(req.WorkDir, "manifest.json"))
	var manifest backupManifest
	if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Format != 1 || !namePattern.MatchString(manifest.Database) {
		return pluginapi.Answer{}, errors.New("this is not a backup this plugin wrote")
	}
	state, known, err := loadState(req.StateDir)
	if err != nil || !known {
		return pluginapi.Answer{}, errors.New("this plugin has no ClickHouse server on this host")
	}
	uid, gid, err := m.account()
	if err != nil {
		return pluginapi.Answer{}, err
	}
	name := newBackupName("restore")
	staged := m.path(filepath.Join(backupDir, backupSubdir, name))
	defer os.RemoveAll(staged)
	if err := m.copyTree(filepath.Join(req.WorkDir, "backup"), staged, uid, gid); err != nil {
		return pluginapi.Answer{}, fmt.Errorf("the backup could not be given to ClickHouse: %w", err)
	}
	target, previous := record.Database, record.Database+previousSuffix
	present := func(database string) (bool, error) {
		out, err := m.admin(ctx, state, "SELECT count() FROM system.databases WHERE name = "+literal(database))
		return out == "1", err
	}
	hasTarget, err := present(target)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	hasPrevious, err := present(previous)
	if err != nil {
		return pluginapi.Answer{}, err
	}
	drop := func(ctx context.Context, database string) error {
		_, err := m.admin(ctx, state, "DROP DATABASE IF EXISTS "+ident(database)+" SYNC")
		return err
	}
	switch {
	case hasPrevious && hasTarget:
		// An earlier restore stopped after moving the database aside: the
		// aside copy is the one to keep; the other is partial.
		if err := drop(ctx, target); err != nil {
			return pluginapi.Answer{}, err
		}
	case hasTarget:
		if _, err := m.admin(ctx, state, "RENAME DATABASE "+ident(target)+" TO "+ident(previous)); err != nil {
			return pluginapi.Answer{}, fmt.Errorf("the current database could not be set aside; nothing was changed: %w", err)
		}
		hasPrevious = true
	}
	statement := "RESTORE DATABASE " + ident(manifest.Database)
	if manifest.Database != target {
		statement += " AS " + ident(target)
	}
	if err := m.operation(ctx, state, statement+" FROM "+backupRef(name)); err != nil {
		undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if hasPrevious && drop(undo, target) == nil {
			if _, renameErr := m.admin(undo, state, "RENAME DATABASE "+ident(previous)+" TO "+ident(target)); renameErr != nil {
				return pluginapi.Answer{}, fmt.Errorf("the restore failed: %w; the previous database is kept as %s", err, previous)
			}
		}
		return pluginapi.Answer{}, fmt.Errorf("the restore failed and the database is as it was: %w", err)
	}
	if hasPrevious {
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_ = drop(done, previous)
	}
	tables, _ := m.admin(ctx, state, "SELECT count() FROM system.tables WHERE database = "+literal(target))
	return pluginapi.Answer{OK: true, Messages: []string{"Restored the database " + target + " (" + tables + " tables) from the backup of " + manifest.Created + "."}}, nil
}
