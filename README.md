# upsignal-agent

A small Go agent that runs on a Linux VPS and reports host health metrics to Upsignal. It only reads: it
listens on no port, executes no commands, and runs as an unprivileged user.

Targets Ubuntu 22.04+ and Debian 12+ with systemd, on `amd64` and `arm64`.

## Status

| Milestone | State |
| --- | --- |
| M1 Go foundation: CLI, config validation, logging, graceful shutdown | done |
| M2 Linux collectors: CPU, memory, swap, disk, inodes, load, uptime, network | done |
| M3 Enrollment and credentials | blocked on the backend agent API |
| M4 Telemetry delivery: batching, spool, retry | not started |
| M5 systemd packaging and `.deb` | not started |

## Commands

```bash
upsignal-agent version
upsignal-agent status   [--config path]
upsignal-agent diagnose [--config path]   # validates config and prints a test sample as JSON
upsignal-agent run      [--config path]   # what the systemd service runs
```

The config file defaults to `/etc/upsignal-agent/config.yaml`; see
[packaging/config.example.yaml](packaging/config.example.yaml). Unknown keys are rejected, and `api.base_url`
must be HTTPS. There is no setting to turn TLS verification off.

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

The payload shape lives in [internal/telemetry/sample.go](internal/telemetry/sample.go). It is a
**proposed** contract until the backend publishes the agent API in OpenAPI.
