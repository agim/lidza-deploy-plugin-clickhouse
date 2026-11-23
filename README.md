# ClickHouse plugin for Līdza Deploy

A [Līdza Deploy](https://github.com/agim/lidza-deploy) plugin that knows
ClickHouse, so the deploy agent does not have to. It is a separate, signed
program the agent runs with limits; see the
[plugin contract](https://github.com/agim/lidza-deploy/blob/main/docs/plugins-proposal.md).

## What this version does

It looks at a server's ClickHouse and repairs what has one safe answer.

| Hook | Does |
| --- | --- |
| `check` | Says which ClickHouse releases the server's processor can run (releases from 26.6 on need AVX2), whether the ClickHouse server is running, whether packages of an installation that never completed are in the way, and, for an application that is about to move in, whether its data fits on the disk ClickHouse keeps data on. It changes nothing. |
| `status` | Reports the installed version, whether the server is running and the free space. |
| `repair` | Starts a ClickHouse server whose service **failed**, and removes the packages of an installation that never completed, but only while no ClickHouse data exists on the server. A server that was stopped on purpose is left alone. |

Creating databases, backups, restores and moving data between servers are
still done by Līdza Deploy's built-in ClickHouse support. They move into this
plugin in later versions.

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
comes from, so the check can say what a move means on an older processor:

```json
{"service": "clickhouse", "options": {"series": "26.8"}}
```

## Building

```sh
go test ./...
scripts/build.sh 0.1 dist     # plugin-amd64.tar.gz, plugin-arm64.tar.gz, plugin.json
```

A release is cut by pushing a tag `v<version>`; the workflow tests, builds,
signs the manifest and publishes the four files.
