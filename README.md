# ClickHouse plugin for Līdza Deploy

A [Līdza Deploy](https://github.com/agim/lidza-deploy) plugin that knows
ClickHouse, so the deploy agent does not have to. It is a separate, signed
program the agent runs with limits; see the
[plugin contract](https://github.com/agim/lidza-deploy/blob/main/docs/plugins-proposal.md).

## What it does

It runs the ClickHouse of a server for the applications on it, and looks at
that server and repairs what has one safe answer.

| Hook | Does |
| --- | --- |
| `check` | Says which ClickHouse releases the server's processor can run (releases from 26.6 on need AVX2), whether the ClickHouse server is running, whether packages of an installation that never completed are in the way, and, for an application that is about to move in, whether its data fits on the disk ClickHouse keeps data on. It changes nothing. |
| `status` | Reports the installed version, whether the server is running and the free space. |
| `repair` | Starts a ClickHouse server whose service **failed**, and removes the packages of an installation that never completed, but only while no ClickHouse data exists on the server. A server that was stopped on purpose is left alone. |
| `provision` | Installs ClickHouse when the server has none (from ClickHouse's own package repository, after checking its signing key's fingerprint, pinned to one release series), then gives the application a database, a user and a settings profile of the same name. The application gets `CLICKHOUSE_URL`, `CLICKHOUSE_DATABASE`, `CLICKHOUSE_USER` and `CLICKHOUSE_PASSWORD`. |
| `release` | Removes the application's user, profile and database. When no application is left the server is stopped; its packages and data directory stay. |
| `backup` | Writes a ClickHouse `BACKUP` of the application's database into the directory the agent gives it. The agent archives, stores and schedules it. |
| `restore` | Replaces the application's database with a backup. The current database is set aside first and only dropped once the restore succeeded; a failed restore puts it back. |
| `maintain` | Hourly: gives ClickHouse's existing log tables their limits (a few days of queries, part operations, warnings and errors) and empties the profiling and metrics logs. A second run changes nothing. |

### The server it sets up

- one ClickHouse server per host, listening on loopback only (HTTP 8123 and
  native 9000, or the first free ports from 18123 and 19000 when those are
  taken);
- a random administrator password, kept in the plugin's state directory in a
  file only root can read; the `default` user has no access without it;
- a memory cap of a quarter of the server's memory, enforced by ClickHouse
  and, a little above it, by systemd; each application's queries are capped
  at half of that and at five minutes;
- ClickHouse's own log tables keep a few days of queries, part operations,
  warnings and errors; its profiling and metrics logs are switched off;
- each application's user may only use its own database.

A ClickHouse that was on the server before, installed by something else, is
never touched: `provision` refuses and says so.

Not in this version: moving a database between servers, taking over a
ClickHouse that Līdza Deploy's built-in support installed, and tuning.

## Installing it on a server

In the control panel, open the server and its **Plugins** tab.

1. Under **Trusted signing keys**, add this plugin's public key and save:

   ```
   e6987c141ca07825da20ecebe9d7c47ef9127975490971c106954fbb215bf84a
   ```

2. Under **Install a plugin**, enter the repository
   `agim/lidza-deploy-plugin-clickhouse` and a version from the
   [releases](https://github.com/agim/lidza-deploy-plugin-clickhouse/releases),
   without the leading `v`.

The server verifies the release's signature with that key and the archive
against the signed manifest before anything is installed. **Check server**
then shows what the plugin found; the same checks appear in the server's
readiness report and in the review of a move to that server.

## Options

An application that asks for the service may name the release series it
wants (usually the one it comes from). It is the series that gets installed
on a server without ClickHouse, and the check says what a move means on an
older processor:

```json
{"service": "clickhouse", "options": {"series": "26.8"}}
```

## Building

```sh
go test ./...
scripts/build.sh 0.2 dist     # plugin-amd64.tar.gz, plugin-arm64.tar.gz, plugin.json
```

`scripts/integration.sh` takes the built plugin through a real server:
install, provision, backup, restore, release. It changes the machine it runs
on, so it only runs in CI, where every push and every release runs it.

A release is cut by pushing a tag `v<version>`; the workflow tests, builds,
signs the manifest and publishes the four files.
