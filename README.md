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
| M5 systemd packaging and `.deb` | not started |

## Commands

```bash
upsignal-agent version
upsignal-agent enroll   [--config path] [--force]   # reads the enrollment token from the terminal or stdin
upsignal-agent status   [--config path]
upsignal-agent diagnose [--config path]   # config, enrollment, DNS, TLS and API checks, plus a test sample
upsignal-agent run      [--config path]   # what the systemd service runs
```

Exit codes: `0` success, `1` error, `2` usage error, `3` the agent was revoked (systemd should not restart it:
`RestartPreventExitStatus=3`).

The config file defaults to `/etc/upsignal-agent/config.yaml`; see
[packaging/config.example.yaml](packaging/config.example.yaml). Unknown keys are rejected, and `api.base_url`
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
