# easySFTP benchmark: release v3.4.0

| Field | Value |
|---|---|
| Kind | release (official reference) |
| Version | `v3.4.0` |
| Recorded | 2026-07-31T09:49:19Z |
| Commit | `ae799b77ec3a23553e2f3ad463061d7a10269803` |
| Workflow run | https://github.com/eiserv/easySFTP/actions/runs/30621299862 |
| Raw data | [release-v3.4.0.json](release-v3.4.0.json) |
| Flat export | [release-v3.4.0.csv](release-v3.4.0.csv) |

## easySFTP benchmark

| Setting | Value |
|---|---|
| Candidate | `v3.4.0 (ae799b7)` |
| Baseline | `none` |
| Repeats per scenario | 3 |
| Runner | Linux 7.0.0-28-generic, 10 cpu |
| Link profiles | the real line |
| Settings | easySFTP defaults (no advanced.* overrides): concurrency 4, request_concurrency 16, retries 2, timeout 30s, mode overlay |

### The link

| Profile | When | RTT p50 | RTT p90 | Handshake | Control 1 stream | Control N streams | Host load |
|---|---|---|---|---|---|---|---|
| baseline | start | 13.15 ms | 13.4 ms | 345.56 ms | 0.39 MiB/s | 1.07 MiB/s | n/a |
| baseline | end | 13.14 ms | 13.44 ms | 365.34 ms | 0.37 MiB/s | 0.93 MiB/s | n/a |

No link shaping was requested: every profile here is the real line.

The control measurement uses `x/crypto/ssh` and `pkg/sftp` directly, never easySFTP's uploader. It separates "the line is slow" from "easySFTP is slow", and a single-stream control close to a scenario's own MiB/s means the run was network bound, where a code delta says nothing.

### Throughput

| Scenario | Build | Profile | Files | Size | Median | Min | Max | MAD | MiB/s | files/s | Retries | Errors | Failed runs | Delta |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | baseline | 300 | 1.2 MiB | 13644 ms | 13086 ms | 13984 ms | 340 ms | 0.09 | 21.99 | 0 | 0 | 0 | - |
| mixed | candidate | baseline | 56 | 11.6 MiB | 32035 ms | 30269 ms | 33643 ms | 1608 ms | 0.36 | 1.75 | 0 | 0 | 0 | - |
| large | candidate | baseline | 2 | 32 MiB | 90269 ms | 77612 ms | 91472 ms | 1203 ms | 0.35 | 0.02 | 0 | 0 | 0 | - |

Delta compares each build's median against the `candidate` build **on the same link profile**; negative is faster. MAD is the median absolute deviation of the repeats: a delta smaller than it is inside this host's own noise.

### Resources (median per run)

| Scenario | Build | Profile | User CPU | Sys CPU | CPU % | Peak RSS | Go allocs | GCs | GC pause | Peak goroutines | Net sent |
|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | baseline | 233.27 ms | 362.87 ms | 4.36% | 10.8 MiB | 11.3 MiB | 3 | 0.652315 ms | 18 | 1.4 MiB |
| mixed | candidate | baseline | 236.38 ms | 277.75 ms | 1.58% | 8.7 MiB | 2.8 MiB | 1 | 0.190206 ms | 18 | 11.7 MiB |
| large | candidate | baseline | 555.03 ms | 567.67 ms | 1.29% | 7.1 MiB | 1 MiB | 0 | 0 ms | 16 | 32.1 MiB |

### Where the time goes

Phases are wall clock and add up to roughly the run's duration. Operation totals are **cumulative across parallel workers** and are normally larger than the phase they belong to; read them for their share and their per-call cost, never as wall clock.

<details><summary><code>small</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 12498.45 ms |
| candidate | baseline | sweep_stale_temps | 475.99 ms |
| candidate | baseline | create_dirs | 328.53 ms |
| candidate | baseline | connect | 323.16 ms |
| candidate | baseline | cleanup | 13.58 ms |
| candidate | baseline | local_scan | 4.23 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 300 | 49694.32 ms | 165.65 ms | 160.65 ms | 222.65 ms | 351.88 ms | 426.39 ms |
| candidate | baseline | sftp_write | 300 | 21211.47 ms | 70.7 ms | 70.62 ms | 106 ms | 182.27 ms | 284.46 ms |
| candidate | baseline | sftp_chmod | 300 | 11426.3 ms | 38.09 ms | 44.08 ms | 67.52 ms | 74.12 ms | 114.38 ms |
| candidate | baseline | sftp_rename | 300 | 8948.36 ms | 29.83 ms | 16.32 ms | 62.32 ms | 107.59 ms | 139.88 ms |
| candidate | baseline | sftp_open | 300 | 8160.86 ms | 27.2 ms | 14.47 ms | 62.75 ms | 72.97 ms | 252 ms |
| candidate | baseline | sftp_readdir | 9 | 475.77 ms | 52.86 ms | 52.38 ms | 55.86 ms | 55.86 ms | 55.86 ms |
| candidate | baseline | sftp_mkdirall | 8 | 328.5 ms | 41.06 ms | 40.89 ms | 42.1 ms | 42.1 ms | 42.1 ms |
| candidate | baseline | ssh_connect | 1 | 323.15 ms | 323.15 ms | 323.15 ms | 323.15 ms | 323.15 ms | 323.15 ms |

</details>

<details><summary><code>mixed</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 30882.21 ms |
| candidate | baseline | sweep_stale_temps | 474.27 ms |
| candidate | baseline | create_dirs | 336.75 ms |
| candidate | baseline | connect | 330.6 ms |
| candidate | baseline | cleanup | 13.54 ms |
| candidate | baseline | local_scan | 3.83 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 56 | 107503.42 ms | 1919.7 ms | 824.62 ms | 2942.29 ms | 17205.28 ms | 17205.28 ms |
| candidate | baseline | sftp_write | 56 | 89133.2 ms | 1591.66 ms | 410.17 ms | 2359.44 ms | 16926.81 ms | 16926.81 ms |
| candidate | baseline | sftp_open | 56 | 7190.87 ms | 128.41 ms | 140.48 ms | 230.66 ms | 359.73 ms | 359.73 ms |
| candidate | baseline | sftp_rename | 56 | 5447.64 ms | 97.28 ms | 89.98 ms | 192.67 ms | 281.85 ms | 281.85 ms |
| candidate | baseline | sftp_chmod | 56 | 5410.03 ms | 96.61 ms | 78.6 ms | 193.47 ms | 236.56 ms | 236.56 ms |
| candidate | baseline | sftp_readdir | 9 | 474.01 ms | 52.67 ms | 52.63 ms | 53.45 ms | 53.45 ms | 53.45 ms |
| candidate | baseline | sftp_mkdirall | 8 | 336.7 ms | 42.09 ms | 40.91 ms | 48.64 ms | 48.64 ms | 48.64 ms |
| candidate | baseline | ssh_connect | 1 | 330.6 ms | 330.6 ms | 330.6 ms | 330.6 ms | 330.6 ms | 330.6 ms |

</details>

<details><summary><code>large</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 89679.56 ms |
| candidate | baseline | connect | 335.9 ms |
| candidate | baseline | sweep_stale_temps | 160.07 ms |
| candidate | baseline | create_dirs | 81.95 ms |
| candidate | baseline | cleanup | 13.36 ms |
| candidate | baseline | local_scan | 0.35 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 2 | 179344.58 ms | 89672.29 ms | 89679.45 ms | 89679.45 ms | 89679.45 ms | 89679.45 ms |
| candidate | baseline | sftp_write | 2 | 179256.5 ms | 89628.25 ms | 89634.46 ms | 89634.46 ms | 89634.46 ms | 89634.46 ms |
| candidate | baseline | ssh_connect | 1 | 335.9 ms | 335.9 ms | 335.9 ms | 335.9 ms | 335.9 ms | 335.9 ms |
| candidate | baseline | sftp_readdir | 3 | 160.05 ms | 53.35 ms | 53.08 ms | 54.53 ms | 54.53 ms | 54.53 ms |
| candidate | baseline | sftp_mkdirall | 2 | 81.93 ms | 40.97 ms | 41.45 ms | 41.45 ms | 41.45 ms | 41.45 ms |
| candidate | baseline | sftp_open | 2 | 31.03 ms | 15.52 ms | 16.32 ms | 16.32 ms | 16.32 ms | 16.32 ms |
| candidate | baseline | sftp_rename | 2 | 29.15 ms | 14.58 ms | 14.6 ms | 14.6 ms | 14.6 ms | 14.6 ms |
| candidate | baseline | sftp_chmod | 2 | 27.3 ms | 13.65 ms | 14.34 ms | 14.34 ms | 14.34 ms | 14.34 ms |

</details>

### Delete sweeps

The pre-clean before every measured run wipes the tree the previous repeat left behind, which makes it a pure delete sweep. It costs no extra time (it has always run) and its numbers never enter the upload tables above. Sweeps that found an empty directory are not counted.

| Scenario | Build | Profile | Sweeps | Files deleted | Median | files/s | remote_scan | delete_sweep |
|---|---|---|---|---|---|---|---|---|
| large | candidate | baseline | 2 | 2 | 624 ms | 3.21 | 156.21 ms | 58.32 ms |
| mixed | candidate | baseline | 2 | 56 | 1840 ms | 30.43 | 474.81 ms | 962.93 ms |
| small | candidate | baseline | 2 | 300 | 5922 ms | 50.66 | 491.78 ms | 5027.08 ms |

| Scenario | Build | Profile | Operation | Count | Cumulative | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| large | candidate | baseline | ssh_connect | 1 | 339.89 ms | 339.89 ms | 339.89 ms | 339.89 ms | 339.89 ms |
| large | candidate | baseline | sftp_readdir | 4 | 208.31 ms | 52.12 ms | 52.32 ms | 52.32 ms | 52.32 ms |
| large | candidate | baseline | sftp_rmdir | 2 | 29.53 ms | 14.86 ms | 14.86 ms | 14.86 ms | 14.86 ms |
| large | candidate | baseline | sftp_remove | 2 | 28.78 ms | 14.43 ms | 14.43 ms | 14.43 ms | 14.43 ms |
| mixed | candidate | baseline | sftp_remove | 56 | 841.71 ms | 14.64 ms | 16.94 ms | 21.24 ms | 21.24 ms |
| mixed | candidate | baseline | sftp_readdir | 10 | 526.85 ms | 52.42 ms | 55.11 ms | 55.11 ms | 55.11 ms |
| mixed | candidate | baseline | ssh_connect | 1 | 330.27 ms | 330.27 ms | 330.27 ms | 330.27 ms | 330.27 ms |
| mixed | candidate | baseline | sftp_rmdir | 8 | 120.96 ms | 14.87 ms | 17.45 ms | 17.45 ms | 17.45 ms |
| small | candidate | baseline | sftp_remove | 300 | 4872.86 ms | 14.63 ms | 16.58 ms | 38.76 ms | 234.23 ms |
| small | candidate | baseline | sftp_readdir | 10 | 545.09 ms | 54.02 ms | 58.92 ms | 58.92 ms | 58.92 ms |
| small | candidate | baseline | ssh_connect | 1 | 318.06 ms | 318.06 ms | 318.06 ms | 318.06 ms | 318.06 ms |
| small | candidate | baseline | sftp_rmdir | 8 | 123.39 ms | 15.23 ms | 16.98 ms | 16.98 ms | 16.98 ms |

Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158 and to show where a run spends its time.
