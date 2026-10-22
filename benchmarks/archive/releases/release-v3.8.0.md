# easySFTP benchmark: release v3.8.0

| Field | Value |
|---|---|
| Kind | release (official reference) |
| Version | `v3.8.0` |
| Recorded | 2026-08-30T20:40:16Z |
| Commit | `12422ea7c8079441fbdb0d4d20a3eba7c84bb94c` |
| Workflow run | https://github.com/eiserv/easySFTP/actions/runs/33334275949 |
| Raw data | [release-v3.8.0.json](release-v3.8.0.json) |
| Flat export | [release-v3.8.0.csv](release-v3.8.0.csv) |

## easySFTP benchmark

| Setting | Value |
|---|---|
| Candidate | `v3.8.0 (12422ea)` |
| Baseline | `none` |
| Repeats per scenario | 3 |
| Runner | Linux 7.0.0-30-generic, 10 cpu |
| Link profiles | the real line |
| Settings | easySFTP defaults (no advanced.* overrides): connections auto, concurrency auto, request_concurrency auto, retries 2, timeout 30s, mode overlay |

### The link

| Profile | When | RTT p50 | RTT p90 | Handshake | Control 1 stream | Control N streams | Host load |
|---|---|---|---|---|---|---|---|
| baseline | start | 13.08 ms | 13.36 ms | 403.66 ms | 0.39 MiB/s | 1.04 MiB/s | n/a |
| baseline | end | 12.89 ms | 13.2 ms | 385.99 ms | 0.38 MiB/s | 1.05 MiB/s | n/a |

No link shaping was requested: every profile here is the real line.

The control measurement uses `x/crypto/ssh` and `pkg/sftp` directly, never easySFTP's uploader. It separates "the line is slow" from "easySFTP is slow", and a single-stream control close to a scenario's own MiB/s means the run was network bound, where a code delta says nothing.

### Throughput

| Scenario | Build | Profile | Files | Size | Median | Min | Max | MAD | MiB/s | files/s | Retries | Errors | Failed runs | Delta |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | baseline | 300 | 1.2 MiB | 3633 ms | 3482 ms | 3857 ms | 151 ms | 0.32 | 82.58 | 0 | 0 | 0 | - |
| mixed | candidate | baseline | 56 | 11.6 MiB | 11720 ms | 11674 ms | 12249 ms | 46 ms | 0.99 | 4.78 | 0 | 0 | 0 | - |
| large | candidate | baseline | 2 | 32 MiB | 48954 ms | 48705 ms | 50432 ms | 249 ms | 0.65 | 0.04 | 0 | 0 | 0 | - |

Delta compares each build's median against the `candidate` build **on the same link profile**; negative is faster. MAD is the median absolute deviation of the repeats: a delta smaller than it is inside this host's own noise.

### Resources (median per run)

| Scenario | Build | Profile | User CPU | Sys CPU | CPU % | Peak RSS | Go allocs | GCs | GC pause | Peak goroutines | Net sent |
|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | baseline | 178.81 ms | 264.52 ms | 12.04% | 12.3 MiB | 11.6 MiB | 5 | 1.124128 ms | 115 | 1.4 MiB |
| mixed | candidate | baseline | 201.09 ms | 232.23 ms | 3.54% | 11.1 MiB | 4.3 MiB | 2 | 0.437812 ms | 109 | 11.7 MiB |
| large | candidate | baseline | 411.97 ms | 542.07 ms | 1.93% | 8.3 MiB | 1.2 MiB | 0 | 0 ms | 25 | 32.1 MiB |

### Where the time goes

Phases are wall clock and add up to roughly the run's duration. Operation totals are **cumulative across parallel workers** and are normally larger than the phase they belong to; read them for their share and their per-call cost, never as wall clock.

<details><summary><code>small</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 2922.08 ms |
| candidate | baseline | connect | 468.02 ms |
| candidate | baseline | create_dirs | 61.06 ms |
| candidate | baseline | cleanup | 54.58 ms |
| candidate | baseline | sweep_stale_temps | 54.13 ms |
| candidate | baseline | local_scan | 4.59 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 300 | 178630.77 ms | 595.44 ms | 363.84 ms | 1456.44 ms | 2320.79 ms | 2342.53 ms |
| candidate | baseline | sftp_write | 300 | 52313.91 ms | 174.38 ms | 117.34 ms | 592.32 ms | 630.15 ms | 632.73 ms |
| candidate | baseline | sftp_rename | 300 | 20043.5 ms | 66.81 ms | 35.81 ms | 182.88 ms | 352.36 ms | 386.25 ms |
| candidate | baseline | sftp_open | 300 | 17643.67 ms | 58.81 ms | 42.37 ms | 119.4 ms | 414.53 ms | 550.79 ms |
| candidate | baseline | sftp_chmod | 300 | 16789.89 ms | 55.97 ms | 55.15 ms | 83.17 ms | 253.42 ms | 311.72 ms |
| candidate | baseline | sftp_readdir | 9 | 483.51 ms | 53.72 ms | 53.73 ms | 53.85 ms | 53.85 ms | 53.85 ms |
| candidate | baseline | ssh_connect | 1 | 428.83 ms | 428.83 ms | 428.83 ms | 428.83 ms | 428.83 ms | 428.83 ms |
| candidate | baseline | sftp_mkdirall | 8 | 414.73 ms | 51.84 ms | 51.97 ms | 60.84 ms | 60.84 ms | 60.84 ms |
| candidate | baseline | sftp_realpath | 3 | 40.05 ms | 13.35 ms | 13.07 ms | 13.97 ms | 13.97 ms | 13.97 ms |

</details>

<details><summary><code>mixed</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 11038.63 ms |
| candidate | baseline | connect | 432.18 ms |
| candidate | baseline | cleanup | 108.15 ms |
| candidate | baseline | sweep_stale_temps | 54.15 ms |
| candidate | baseline | create_dirs | 52.19 ms |
| candidate | baseline | local_scan | 1.32 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 56 | 183713.41 ms | 3280.6 ms | 3339.42 ms | 5193.36 ms | 11037.83 ms | 11037.83 ms |
| candidate | baseline | sftp_write | 56 | 81354.37 ms | 1452.76 ms | 440.38 ms | 3213.41 ms | 8638.81 ms | 8638.81 ms |
| candidate | baseline | sftp_rename | 56 | 8074.61 ms | 144.19 ms | 118.44 ms | 325.98 ms | 468.45 ms | 468.45 ms |
| candidate | baseline | sftp_chmod | 56 | 7175.36 ms | 128.13 ms | 106.04 ms | 317.75 ms | 506.56 ms | 506.56 ms |
| candidate | baseline | sftp_open | 56 | 1155.24 ms | 20.63 ms | 19.62 ms | 28.86 ms | 35.91 ms | 35.91 ms |
| candidate | baseline | sftp_readdir | 9 | 485.59 ms | 53.95 ms | 53.97 ms | 54.01 ms | 54.01 ms | 54.01 ms |
| candidate | baseline | ssh_connect | 1 | 392.98 ms | 392.98 ms | 392.98 ms | 392.98 ms | 392.98 ms | 392.98 ms |
| candidate | baseline | sftp_mkdirall | 8 | 375.48 ms | 46.94 ms | 48.23 ms | 51.98 ms | 51.98 ms | 51.98 ms |
| candidate | baseline | sftp_realpath | 3 | 39.19 ms | 13.06 ms | 13.1 ms | 13.12 ms | 13.12 ms | 13.12 ms |

</details>

<details><summary><code>large</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 48403.68 ms |
| candidate | baseline | connect | 425.53 ms |
| candidate | baseline | sweep_stale_temps | 52.5 ms |
| candidate | baseline | create_dirs | 43.3 ms |
| candidate | baseline | cleanup | 27.53 ms |
| candidate | baseline | local_scan | 0.41 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 2 | 94327.67 ms | 47163.84 ms | 48403.57 ms | 48403.57 ms | 48403.57 ms | 48403.57 ms |
| candidate | baseline | sftp_write | 2 | 93907.24 ms | 46953.62 ms | 48361.29 ms | 48361.29 ms | 48361.29 ms | 48361.29 ms |
| candidate | baseline | ssh_connect | 1 | 385.69 ms | 385.69 ms | 385.69 ms | 385.69 ms | 385.69 ms | 385.69 ms |
| candidate | baseline | sftp_readdir | 3 | 156.84 ms | 52.28 ms | 52.3 ms | 52.36 ms | 52.36 ms | 52.36 ms |
| candidate | baseline | sftp_mkdirall | 2 | 84.49 ms | 42.25 ms | 43.1 ms | 43.1 ms | 43.1 ms | 43.1 ms |
| candidate | baseline | sftp_realpath | 3 | 39.81 ms | 13.27 ms | 13.1 ms | 13.63 ms | 13.63 ms | 13.63 ms |
| candidate | baseline | sftp_rename | 2 | 29.69 ms | 14.85 ms | 15.42 ms | 15.42 ms | 15.42 ms | 15.42 ms |
| candidate | baseline | sftp_open | 2 | 29.33 ms | 14.67 ms | 14.88 ms | 14.88 ms | 14.88 ms | 14.88 ms |
| candidate | baseline | sftp_chmod | 2 | 26.28 ms | 13.14 ms | 13.24 ms | 13.24 ms | 13.24 ms | 13.24 ms |

</details>

### Delete sweeps

The pre-clean before every measured run wipes the tree the previous repeat left behind, which makes it a pure delete sweep. It costs no extra time (it has always run) and its numbers never enter the upload tables above. Sweeps that found an empty directory are not counted.

| Scenario | Build | Profile | Sweeps | Files deleted | Median | files/s | remote_scan | delete_sweep |
|---|---|---|---|---|---|---|---|---|
| large | candidate | baseline | 2 | 2 | 595 ms | 3.36 | 106.23 ms | 33.4 ms |
| mixed | candidate | baseline | 2 | 56 | 731 ms | 76.61 | 105.83 ms | 175.59 ms |
| small | candidate | baseline | 2 | 300 | 1283 ms | 233.83 | 121.97 ms | 637.53 ms |

| Scenario | Build | Profile | Operation | Count | Cumulative | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| large | candidate | baseline | ssh_connect | 1 | 372.73 ms | 372.73 ms | 372.73 ms | 372.73 ms | 372.73 ms |
| large | candidate | baseline | sftp_readdir | 4 | 212.92 ms | 53.22 ms | 53.85 ms | 53.85 ms | 53.85 ms |
| large | candidate | baseline | sftp_rmdir | 2 | 31.78 ms | 16.89 ms | 16.89 ms | 16.89 ms | 16.89 ms |
| large | candidate | baseline | sftp_remove | 2 | 31.56 ms | 16.4 ms | 16.4 ms | 16.4 ms | 16.4 ms |
| mixed | candidate | baseline | sftp_remove | 56 | 4876.9 ms | 88.55 ms | 121.64 ms | 131.47 ms | 131.47 ms |
| mixed | candidate | baseline | sftp_readdir | 10 | 525.22 ms | 52.6 ms | 53.51 ms | 53.51 ms | 53.51 ms |
| mixed | candidate | baseline | ssh_connect | 1 | 377.77 ms | 377.77 ms | 377.77 ms | 377.77 ms | 377.77 ms |
| mixed | candidate | baseline | sftp_rmdir | 8 | 172.18 ms | 22.03 ms | 27.78 ms | 27.78 ms | 27.78 ms |
| small | candidate | baseline | sftp_remove | 300 | 35569.87 ms | 111.26 ms | 145.82 ms | 169.19 ms | 172.77 ms |
| small | candidate | baseline | sftp_readdir | 10 | 588.33 ms | 57.49 ms | 68.05 ms | 68.05 ms | 68.05 ms |
| small | candidate | baseline | ssh_connect | 1 | 353.87 ms | 353.87 ms | 353.87 ms | 353.87 ms | 353.87 ms |
| small | candidate | baseline | sftp_rmdir | 8 | 165.42 ms | 21.26 ms | 26.62 ms | 26.62 ms | 26.62 ms |

Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158 and to show where a run spends its time.
