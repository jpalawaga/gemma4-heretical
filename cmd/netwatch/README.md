# netwatch

Records internet outages to a log file. Intended to run unattended on a
Raspberry Pi.

While the link is healthy it sends one ICMP echo per interval to a single
primary target. Only when that probe fails does it also probe a secondary
target, so a second opinion costs nothing in the common case. An outage is
declared once both targets have failed in the same round `-failures` times in a
row, and closed out when connectivity returns.

```
2026-08-23T11:46:31-06:00 START primary=1.1.1.1 secondary=8.8.8.8 interval=5s timeout=2s failures=3 recover=both (unprivileged datagram socket)
2026-08-23T11:52:04-06:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-23T12:07:19-06:00 OUTAGE_END connectivity restored after 15m15s (started 2026-08-23T11:52:04-06:00)
```

## Flags

| Flag | Default | |
|---|---|---|
| `-primary` | `1.1.1.1` | probed every round |
| `-secondary` | `8.8.8.8` | probed only when the primary fails |
| `-interval` | `5s` | time between rounds |
| `-timeout` | `2s` | per-probe reply timeout |
| `-failures` | `3` | consecuti2026-08-24T17:48:30+01:00 OUTAGE_END connectivity restored after 25s (started 2026-08-23T19:07:14+01:00)
2026-08-24T23:47:09+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-24T23:47:15+01:00 OUTAGE_END connectivity restored after 6s (started 2026-08-24T23:47:09+01:00)
2026-08-25T17:09:09+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-25T17:09:20+01:00 OUTAGE_END connectivity restored after 11s (started 2026-08-25T17:09:09+01:00)
2026-08-25T19:02:44+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-25T19:02:50+01:00 OUTAGE_END connectivity restored after 6s (started 2026-08-25T19:02:44+01:00)
2026-08-25T19:19:54+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-25T19:19:55+01:00 OUTAGE_END connectivity restored after 1s (started 2026-08-25T19:19:54+01:00)
2026-08-25T20:06:54+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-25T20:07:05+01:00 OUTAGE_END connectivity restored after 11s (started 2026-08-25T20:06:54+01:00)
2026-08-25T21:41:49+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-25T21:42:15+01:00 OUTAGE_END connectivity restored after 26s (started 2026-08-25T21:41:49+01:00)
2026-08-25T21:49:44+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-25T21:50:20+01:00 OUTAGE_END connectivity restored after 36s (started 2026-08-25T21:49:44+01:00)
2026-08-25T22:31:59+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-25T22:32:15+01:00 OUTAGE_END connectivity restored after 16s (started 2026-08-25T22:31:59+01:00)
2026-08-26T03:53:49+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-26T03:55:25+01:00 OUTAGE_END connectivity restored after 1m36s (started 2026-08-26T03:53:49+01:00)
2026-08-26T13:16:14+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-26T13:16:15+01:00 OUTAGE_END connectivity restored after 1s (started 2026-08-26T13:16:14+01:00)
2026-08-26T20:04:09+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-26T20:04:15+01:00 OUTAGE_END connectivity restored after 6s (started 2026-08-26T20:04:09+01:00)
2026-08-26T20:44:09+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-26T20:44:35+01:00 OUTAGE_END connectivity restored after 26s (started 2026-08-26T20:44:09+01:00)
2026-08-27T03:28:29+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-27T03:29:25+01:00 OUTAGE_END connectivity restored after 56s (started 2026-08-27T03:28:29+01:00)
2026-08-27T04:08:59+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-27T04:09:25+01:00 OUTAGE_END connectivity restored after 26s (started 2026-08-27T04:08:59+01:00)
2026-08-27T09:28:09+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-27T09:28:45+01:00 OUTAGE_END connectivity restored after 36s (started 2026-08-27T09:28:09+01:00)
2026-08-27T19:38:34+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-27T19:39:20+01:00 OUTAGE_END connectivity restored after 46s (started 2026-08-27T19:38:34+01:00)
2026-08-27T23:43:09+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-27T23:46:15+01:00 OUTAGE_END connectivity restored after 3m6s (started 2026-08-27T23:43:09+01:00)
2026-08-28T23:45:39+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-28T23:46:05+01:00 OUTAGE_END connectivity restored after 26s (started 2026-08-28T23:45:39+01:00)
2026-08-29T00:32:14+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-29T00:32:25+01:00 OUTAGE_END connectivity restored after 11s (started 2026-08-29T00:32:14+01:00)
2026-08-30T00:20:59+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-30T00:21:05+01:00 OUTAGE_END connectivity restored after 6s (started 2026-08-30T00:20:59+01:00)
2026-08-30T02:22:49+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-30T02:23:35+01:00 OUTAGE_END connectivity restored after 46s (started 2026-08-30T02:22:49+01:00)
2026-08-30T03:06:29+01:00 OUTAGE_START both targets unreachable for 3 consecutive rounds; 1.1.1.1: timeout; 8.8.8.8: timeout
2026-08-30T03:07:20+01:00 OUTAGE_END connectivity restored after 51s (started 2026-08-30T03:06:29+01:00)ve both-down rounds before declaring an outage |
| `-recover` | `both` | `both` targets must answer to end an outage, or `any` one |
| `-log` | `/var/log/netwatch.log` | appended to; reopened on SIGHUP |
| `-quiet` | `false` | skip the copy written to stdout |

`-recover both` matches the strict reading: the outage stays open until both
targets answer in the same round. That means a target that goes away
permanently (blocked, retired, re-IP'd) leaves an outage open forever. If the
log is being used unattended, `-recover any` is the safer setting — it closes
the outage as soon as either target answers.

Targets are IPv4 only. IP literals skip DNS, which matters here: during an
outage the resolver is usually dead too, and a hostname turns each probe into a
DNS stall. Prefer literals unless you specifically want DNS included in what is
being monitored.

## Deploying to a Pi

```sh
GOOS=linux GOARCH=arm64 go build -o netwatch ./cmd/netwatch    # 64-bit Pi OS
GOOS=linux GOARCH=arm GOARM=7 go build -o netwatch ./cmd/netwatch  # 32-bit

scp netwatch pi@raspberrypi:/tmp/
ssh pi@raspberrypi 'sudo install -m755 /tmp/netwatch /usr/local/bin/netwatch'
scp cmd/netwatch/netwatch.service pi@raspberrypi:/tmp/
ssh pi@raspberrypi 'sudo install -m644 /tmp/netwatch.service /etc/systemd/system/ \
  && sudo systemctl enable --now netwatch'
```

### Permissions

netwatch first tries Linux's unprivileged ICMP datagram socket, which works for
any process whose GID is inside `net.ipv4.ping_group_range` (Raspberry Pi OS
usually allows this by default). If that fails it falls back to a raw socket,
which needs root or `CAP_NET_RAW` — the supplied unit grants the capability so
neither path requires running as root. The `START` line records which socket it
got.

### Rotation

The log is reopened on SIGHUP, so a logrotate stanza just needs to signal it:

```
/var/log/netwatch/netwatch.log {
    weekly
    rotate 12
    compress
    missingok
    notifempty
    postrotate
        systemctl reload netwatch
    endscript
}
```
