# ddns

A small, single-binary dynamic DNS (DDNS) updater for **Cloudflare**, written in Go.

Each time it runs, `ddns` detects the machine's current public IPv4 address and makes sure one or more
Cloudflare DNS **A records** point to it — creating the record if it does not exist, updating it if the IP
changed, and doing nothing if it is already up to date. It is designed for home servers, NAS boxes and
other hosts on a residential connection whose public IP changes from time to time.

## Features

- **One-shot run**: detects the IP, syncs the records, then exits. Schedule it with cron or a systemd
  timer to run periodically.
- **Multiple hostnames** in a single Cloudflare zone, configured in one `config.yaml`.
- **Create, update or skip**: missing A records are created; records with a different IP are updated;
  records that already match are left untouched (no unnecessary API writes).
- **Public IP detection with fallback**: tries `https://api.ipify.org`, then `https://ipv4.icanhazip.com`,
  then `https://ifconfig.me/ip` (8-second timeout each), using the first non-empty answer.
- **Interactive setup**: `ddns init` asks for the token, zone ID and hostnames and writes `config.yaml`
  with `0600` permissions.
- **Boot-friendly**: if `config.yaml` does not exist, it logs a message and exits successfully, so it is
  safe to run at startup before it has been configured.
- **Readable errors**: common Cloudflare API and network failures (bad token, missing zone/permission,
  timeouts, DNS resolution) are translated into short, actionable messages.
- Uses the official [`cloudflare-go`](https://github.com/cloudflare/cloudflare-go) v6 SDK and the
  Cloudflare API token authentication (no global API key).

## Requirements

- Go **1.26.5** or newer (as declared in `go.mod`) to build from source.
- A domain whose DNS is hosted on Cloudflare, and that zone's **Zone ID** (shown on the zone's
  *Overview* page in the Cloudflare dashboard).
- A Cloudflare **API token** with the permission **Zone → DNS → Edit**, scoped to that zone
  (`ddns` lists, creates and updates DNS records). The dashboard's "Edit zone DNS" template is sufficient.
- Outbound HTTPS access to the Cloudflare API and to the IP-detection services listed above.

## Build

```sh
git clone https://github.com/fanice1998/ddns.git
cd ddns
go build -o ddns .
```

This produces a single static-friendly `ddns` binary in the current directory. To build for another
platform, e.g. a Raspberry Pi:

```sh
GOOS=linux GOARCH=arm64 go build -o ddns .
```

## Configuration

`ddns` reads its configuration from a file named **`config.yaml` in the current working directory**.
There are no command-line flags or environment variables; the file is the only configuration source.

### Option 1: interactive

```sh
./ddns init
```

You will be prompted for:

1. `Cloudflare API Token`
2. `Zone ID`
3. The DNS names to update, comma-separated (for example `home.example.com,nas.example.com`)

All three are required. The file is written to `./config.yaml` with mode `0600` (overwriting any existing
one).

### Option 2: write the file by hand

```yaml
api_token: "your-cloudflare-api-token"
zone_id: "your-zone-id"
hostnames:
  - home.example.com
  - nas.example.com
```

| Key         | Type            | Description                                              |
|-------------|-----------------|----------------------------------------------------------|
| `api_token` | string          | Cloudflare API token with Zone → DNS → Edit permission.  |
| `zone_id`   | string          | ID of the Cloudflare zone that contains the hostnames.   |
| `hostnames` | list of strings | DNS names whose A record should point to this host's IP. |

All keys are required; if any is missing or empty, `ddns` exits with an error asking you to run `init`
again.

Use **fully qualified names** (e.g. `home.example.com`, or `example.com` for the zone apex): existing
records are looked up by exact name match.

> `config.yaml` contains your API token. Keep it private (`chmod 600 config.yaml`) and never commit it.

### Record settings

Records created or updated by `ddns` are always:

- type `A` (IPv4 only),
- TTL `1` (Cloudflare "Auto"),
- **not proxied** (`proxied: false`, "DNS only"). Note that an existing proxied record will be switched to
  DNS-only when its IP is updated.

If more than one A record exists with the same name, only the first one returned by the API is updated.

## Usage

Run from the directory that contains `config.yaml`:

```sh
./ddns
```

Example output:

```
2026/10/18 22:40:00 [ddns] 目前公網 IP: 203.0.113.42
2026/10/18 22:40:01 [ddns] 已更新 home.example.com：198.51.100.7 → 203.0.113.42
2026/10/18 22:40:01 [ddns] nas.example.com 已是最新 IP (203.0.113.42)，略過
2026/10/18 22:40:01 [ddns] 全部處理完成
```

(Log messages are currently in Traditional Chinese: "current public IP", "updated", "already up to date,
skipped", "all done".)

### Logging and exit codes

Logs go to standard error with a timestamp and the `[ddns]` prefix.

| Situation                                                    | Exit code |
|--------------------------------------------------------------|-----------|
| All hostnames processed, or at least one succeeded           | `0`       |
| `config.yaml` does not exist (nothing to do)                 | `0`       |
| `config.yaml` is invalid or incomplete                       | `1`       |
| Public IP could not be detected from any source              | `1`       |
| Every hostname failed to update                              | `1`       |

A failure on one hostname is logged and the remaining hostnames are still processed. All Cloudflare
calls in one run share a 30-second timeout.

## Running periodically

Because `ddns` runs once and exits, use a scheduler. Since it reads `config.yaml` from the **working
directory**, make sure the scheduler changes into the directory that holds it.

### cron

Every 5 minutes, assuming the binary and `config.yaml` live in `/opt/ddns`:

```cron
*/5 * * * * cd /opt/ddns && ./ddns >> /var/log/ddns.log 2>&1
```

### systemd timer

`/etc/systemd/system/ddns.service`:

```ini
[Unit]
Description=Cloudflare DDNS updater
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
WorkingDirectory=/opt/ddns
ExecStart=/opt/ddns/ddns
```

`/etc/systemd/system/ddns.timer`:

```ini
[Unit]
Description=Run Cloudflare DDNS updater every 5 minutes

[Timer]
OnBootSec=1min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
```

Enable it and check the logs:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now ddns.timer
journalctl -u ddns.service
```

## Limitations

- IPv4 / A records only; AAAA (IPv6) records are not managed.
- All hostnames must belong to a single Cloudflare zone.
- TTL and proxy status are fixed (Auto, DNS only) and not configurable.

## Contributing

Issues and pull requests are welcome at
[github.com/fanice1998/ddns](https://github.com/fanice1998/ddns).

## License

Licensed under the [Apache License 2.0](LICENSE).
