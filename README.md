# ddns

A small, single-binary dynamic DNS (DDNS) updater for **Cloudflare**, written in Go.

Each time it runs, `ddns` detects the machine's current public IPv4 address (and optionally IPv6) and
makes sure one or more Cloudflare DNS **A** (and optionally **AAAA**) records point to it — creating the record if it does not exist, updating it if the IP
changed, and doing nothing if it is already up to date. It is designed for home servers, NAS boxes and
other hosts on a residential connection whose public IP changes from time to time.

## Features

- **One-shot run**: detects the IP, syncs the records, then exits. Schedule it with cron or a systemd
  timer to run periodically.
- **Multiple hostnames** in a single Cloudflare zone, configured in one `config.yaml`.
- **Short names**: `home` or `@` are expanded to `home.example.com` / `example.com` using the zone name.
- **Create, update or skip**: missing records are created; records with a different IP are updated
  (every matching record, if there are duplicates); records that already match are left untouched.
- **Optional IPv6**: set `ipv6: true` to also manage AAAA records.
- **Configurable TTL and proxy status**, preserving the existing values when not configured.
- **Validated public IP detection with fallback**: IPv4 is looked up over IPv4-only connections from
  `https://api.ipify.org`, then `https://ipv4.icanhazip.com`, then `https://ifconfig.me/ip`. A response
  is accepted only if it is HTTP 200 and parses as an IPv4 address; otherwise the next service is tried.
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
  Adding **Zone → Zone → Read** lets `ddns` look up the zone name to expand short names such as `home`
  or `@`; without it, write hostnames as fully qualified names.
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
3. The DNS names to update, comma-separated (for example `home.example.com,nas,@`)

All three are required. The file is written to `./config.yaml` with mode `0600` (overwriting any existing
one). The optional settings below can then be added by hand.

### Option 2: write the file by hand

```yaml
api_token: "your-cloudflare-api-token"
zone_id: "your-zone-id"
hostnames:
  - home.example.com
  - nas            # expanded to nas.example.com
  - "@"            # the zone apex, example.com

# Optional (defaults shown in the table below)
ipv6: false
ttl: 1
proxied: false
```

| Key         | Type            | Required | Description |
|-------------|-----------------|----------|-------------|
| `api_token` | string          | yes      | Cloudflare API token with Zone → DNS → Edit permission. |
| `zone_id`   | string          | yes      | ID of the Cloudflare zone that contains the hostnames. |
| `hostnames` | list of strings | yes      | DNS names to point at this host. Full names, short names (`home`) or `@`. |
| `ipv6`      | bool            | no       | Also detect the public IPv6 address and manage AAAA records. Default `false`. |
| `ttl`       | int             | no       | `1` (Cloudflare "Auto") or 30–86400 seconds. If unset: `1` for new records, existing TTL kept on update. |
| `proxied`   | bool            | no       | Cloudflare proxy ("orange cloud"). If unset: `false` for new records, existing value kept on update. |

If a required key is missing or empty, or `ttl` is out of range, `ddns` exits with an error.
Configs written by earlier versions keep working unchanged.

**Hostname expansion**: at startup `ddns` fetches the zone name. A hostname that is `@`, equals the zone
name, or already ends with `.<zone>` is used as is (a trailing dot is removed); anything else gets
`.<zone>` appended, e.g. `home` → `home.example.com`. If the zone name cannot be fetched (for example the
token lacks Zone Read), a warning is logged and hostnames are used exactly as written, so use fully
qualified names in that case.

> `config.yaml` contains your API token. Keep it private (`chmod 600 config.yaml`) and never commit it.

### Record behavior

- For each hostname, `ddns` manages `A` records (and `AAAA` records when `ipv6: true`).
- If no record of that type exists, one is created with the configured (or default) TTL and proxy status.
- If records exist, **every** record of that type with that name is updated when its IP, or an explicitly
  configured `ttl` / `proxied` value, differs. Otherwise it is skipped.
- With `ipv6: true`, the IPv6 address is looked up over IPv6-only connections from
  `https://api6.ipify.org`, `https://ipv6.icanhazip.com` and `https://ifconfig.me/ip`, and must parse as an
  IPv6 address. If no IPv6 address can be found, AAAA records are skipped with a log message and the IPv4
  update still proceeds.

## Usage

Run from the directory that contains `config.yaml`:

```sh
./ddns
```

Example output:

```
2026/10/18 22:40:00 [ddns] 目前公網 IP: 203.0.113.42
2026/10/18 22:40:01 [ddns] 已更新 home.example.com (A)：198.51.100.7 → 203.0.113.42
2026/10/18 22:40:01 [ddns] nas.example.com (A) 已是最新 IP (203.0.113.42)，略過
2026/10/18 22:40:01 [ddns] 全部處理完成
```

(Log messages are currently in Traditional Chinese: "current public IP", "updated", "already up to date,
skipped", "all done".)

### Logging and exit codes

Logs go to standard error with a timestamp and the `[ddns]` prefix.

| Situation                                                    | Exit code |
|--------------------------------------------------------------|-----------|
| All records processed, or at least one hostname/type succeeded | `0`     |
| `config.yaml` does not exist (nothing to do)                 | `0`       |
| `config.yaml` is invalid or incomplete                       | `1`       |
| Public IP could not be detected from any source              | `1`       |
| `ttl` out of range                                           | `1`       |
| Every hostname failed to update                              | `1`       |

A failure on one hostname is logged and the remaining hostnames are still processed. Cloudflare API calls
have a 30-second timeout per hostname and record type. A failed IPv6 lookup does not cause a non-zero exit.

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

- All hostnames must belong to a single Cloudflare zone.
- TTL and proxy settings apply to all hostnames; they cannot be set per hostname.
- No prebuilt release binaries yet; build from source.

## Development

```sh
go vet ./...
go test ./...
```

Tests use local `httptest` servers only (no network access or Cloudflare credentials needed). CI
([`.github/workflows/ci.yaml`](.github/workflows/ci.yaml)) runs `go vet`, `go test -race` and `go build`
on every push to `main` and on every pull request.

## Contributing

Issues and pull requests are welcome at
[github.com/fanice1998/ddns](https://github.com/fanice1998/ddns).

## License

Licensed under the [Apache License 2.0](LICENSE).
