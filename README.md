# upsignal-agent

A small Go agent that runs on a Linux VPS and reports host health metrics to Upsignal. It only reads: it
listens on no port, executes no commands, and runs as an unprivileged user.

Targets Ubuntu 22.04+ and Debian 12+ with systemd, on `amd64` and `arm64`.

## Status

| Milestone | State |
| --- | --- |
| M1 Go foundation: CLI, config validation, logging, graceful shutdown | done |
| M2 Linux collectors: CPU, memory, swap, disk, inodes, load, uptime, network | done |
| M3 Enrollment and credentials | done |
| M4 Telemetry delivery: batching, spool, retry | done |
| M5 systemd packaging, `.deb` and verified installer | done |

## Install

On Ubuntu 22.04+ or Debian 12+ (`amd64` or `arm64`), create a server in Upsignal, then:

```bash
curl -fsSLO https://github.com/upsignal/agent/releases/download/v<version>/install.sh
less install.sh
sudo sh install.sh
```

The installer checks the OS and architecture, downloads that release's `.deb`, and verifies it against the
release's `SHA256SUMS` before installing anything. It then asks for the enrollment token and starts the service.
Each published `install.sh` installs exactly its own release; `--version` picks another one. Other options:
`--api-url` for self-hosted Upsignal, `--no-enroll` to only install, and `--base-url` for a mirror.

Installing the `.deb` directly works too (`sudo apt install ./upsignal-agent_<version>_<arch>.deb`). The service
stays inactive until `sudo upsignal-agent enroll` has stored a credential; then
`sudo systemctl enable --now upsignal-agent`.

| Path | Contents | Mode |
| --- | --- | --- |
| `/usr/bin/upsignal-agent` | binary | `0755 root` |
| `/etc/upsignal-agent/config.yaml` | settings, kept across upgrades | `0640 root:upsignal-agent` |
| `/var/lib/upsignal-agent/` | credential, sequence counter, status, spool | `0700 upsignal-agent` |
| `/usr/lib/systemd/system/upsignal-agent.service` | hardened unit | `0644 root` |

**Upgrades** (`sudo sh install.sh --version <new>`, or `apt install` of a newer `.deb`) restart the running
service and keep the credential, local config edits, spooled telemetry, and the sample sequence.
**Rollback** is installing the older `.deb` the same way. `apt remove` stops the service and keeps the
enrollment; `apt purge` also deletes `/etc/upsignal-agent` and `/var/lib/upsignal-agent`.

The service runs as the `upsignal-agent` system user (no shell, no home) with no capabilities, a read-only view
of the system except its state directory, a restricted syscall set, and `MemoryMax=128M`; see
[packaging/systemd/upsignal-agent.service](packaging/systemd/upsignal-agent.service). It restarts after a crash,
but not after exit code `3` (revoked).

## Commands

```bash
upsignal-agent version
upsignal-agent enroll   [--config path] [--force]   # reads the enrollment token from the terminal or stdin
upsignal-agent status   [--config path]          # needs sudo: the state is readable only by root and the agent
upsignal-agent diagnose [--config path]   # config, enrollment, DNS, TLS and API checks, plus a test sample
upsignal-agent run      [--config path]   # what the systemd service runs
```

Exit codes: `0` success, `1` error, `2` usage error, `3` the agent was revoked.

The config file defaults to `/etc/upsignal-agent/config.yaml`; see
[packaging/config.yaml](packaging/config.yaml). Unknown keys are rejected, and `api.base_url`
must be HTTPS. There is no setting to turn TLS verification off.

## Enrollment

Create a server in Upsignal to get a one-time enrollment token (valid for 10 minutes), then on the server:

```bash
sudo upsignal-agent enroll
```

The token is typed at a hidden prompt, or piped on stdin (`printf '%s\n' "$TOKEN" | sudo upsignal-agent enroll`).
It is never accepted as an argument or environment variable, so it does not reach shell history or `ps`.
Enrollment exchanges it for a long-lived agent credential, stored in `/var/lib/upsignal-agent/agent-credential`
(mode `0600`, owned by the `upsignal-agent` user when that user exists). `--force` replaces an existing
enrollment, for example after the agent was revoked.

## Delivery

The agent collects a sample every `collection.interval` and sends everything collected every
`collection.upload_interval` (each request, even an empty one, is the liveness heartbeat). Every sample has a
durable sequence number, so the API deduplicates retries and a partly sent batch can safely be resent.

| API response | Agent behaviour |
| --- | --- |
| `200` | Spool is drained oldest first, up to 10 requests per upload tick |
| network error, `5xx` | Batch goes to the spool; retry with exponential backoff (5s to 5m, jittered) |
| `429` | Spool; wait for `Retry-After` (capped at 1h) |
| `401` invalid credential | Spool; retry hourly and log an error (usually the wrong `base_url`) |
| `403` revoked | Spool, record it in status, exit with code `3` |
| `400`, `413` | Drop that batch, since resending cannot succeed |

The spool lives in `/var/lib/upsignal-agent/spool`, bounded by `spool.max_age` and `spool.max_size` (oldest
batches are dropped first). With `spool.enabled: false`, batches that cannot be sent are dropped. On shutdown the
agent makes one last upload attempt for up to 5 seconds.

## Development

```bash
make test    # unit tests (they run on macOS too; only statfs is Linux-specific)
make vet     # vet for the host OS and Linux
make linux   # dist/upsignal-agent-linux-{amd64,arm64} plus SHA256SUMS
make deb     # also the .deb packages and a version-pinned install.sh (uses Docker if dpkg-deb is missing)
```

## Metrics

All counters are raw integers, and the backend derives rates. A metric that cannot be read is sent as `null`,
never as `0`.

- **CPU**: usage, user, system, iowait and steal, as percentages over the last interval. The first sample
  after startup is `null`.
- **Memory**: total, available (`MemAvailable`), used, and swap.
- **Filesystems**: local filesystem types only (ext4, xfs, btrfs, zfs, …), one entry per device. Pseudo,
  overlay and network filesystems are skipped. Inode fields are `null` where the filesystem has no inode
  counts.
- **Network**: per-interface byte, packet, error and drop counters, excluding loopback. No addresses or MACs.
- **Load average, uptime, and `boot_id`**: `boot_id` lets the backend detect counter resets after a reboot.
  The agent never reads `/etc/machine-id`.
- **Host**: hostname, OS ID, name and version, kernel release, architecture, and logical CPU count.

The payload shape lives in [internal/telemetry/sample.go](internal/telemetry/sample.go) and matches the API's
`POST /v1/agents/telemetry` contract (see `docs/agents.md` in the API repository).
