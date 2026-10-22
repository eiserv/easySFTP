# easySFTP benchmark: release v3.8.6

| Field | Value |
|---|---|
| Kind | release (official reference) |
| Version | `v3.8.6` |
| Recorded | 2026-10-22T02:07:29Z |
| Commit | `3ce56c4094f8b49f470ffe275ba2baed6e7a9b3f` |
| Workflow run | https://github.com/eiserv/easySFTP/actions/runs/39709499832 |
| Raw data | [release-v3.8.6.json](release-v3.8.6.json) |
| Flat export | [release-v3.8.6.csv](release-v3.8.6.csv) |

## easySFTP benchmark

| Setting | Value |
|---|---|
| Candidate | `v3.8.6 (3ce56c4)` |
| Baseline | `none` |
| Repeats per scenario | 3 |
| Runner | Linux 7.0.0-38-generic, 4 cpu |
| Link profiles | the real line |
| Settings | easySFTP defaults (no advanced.* overrides): connections auto, concurrency auto, request_concurrency auto, retries 2, timeout 30s, mode overlay |

### The link

| Profile | When | RTT p50 | RTT p90 | Handshake | Control 1 stream | Control N streams | Host load |
|---|---|---|---|---|---|---|---|
| baseline | start | 13.09 ms | 13.3 ms | 380.28 ms | 0.4 MiB/s | 1.18 MiB/s | n/a |
| baseline | end | 13.15 ms | 13.86 ms | 464.85 ms | 0.41 MiB/s | 1.2 MiB/s | n/a |

No link shaping was requested: every profile here is the real line.

The control measurement uses `x/crypto/ssh` and `pkg/sftp` directly, never easySFTP's uploader. It separates "the line is slow" from "easySFTP is slow", and a single-stream control close to a scenario's own MiB/s means the run was network bound, where a code delta says nothing.

### Throughput

| Scenario | Build | Profile | Files | Size | Median | Min | Max | MAD | MiB/s | files/s | Retries | Errors | Failed runs | Delta |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | baseline | 300 | 1.2 MiB | 3918 ms | 3529 ms | 4196 ms | 278 ms | 0.3 | 76.57 | 0 | 0 | 0 | - |
| mixed | candidate | baseline | 56 | 11.6 MiB | 10899 ms | 10867 ms | 11352 ms | 32 ms | 1.07 | 5.14 | 0 | 0 | 0 | - |
| large | candidate | baseline | 2 | 32 MiB | 49331 ms | 42776 ms | 57454 ms | 6555 ms | 0.65 | 0.04 | 0 | 0 | 0 | - |

Delta compares each build's median against the `candidate` build **on the same link profile**; negative is faster. MAD is the median absolute deviation of the repeats: a delta smaller than it is inside this host's own noise.

### Resources (median per run)

| Scenario | Build | Profile | User CPU | Sys CPU | CPU % | Peak RSS | Go allocs | GCs | GC pause | Peak goroutines | Net sent |
|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | baseline | 204.14 ms | 224.96 ms | 10.73% | 13.1 MiB | 12.2 MiB | 4 | 2.529759 ms | 270 | 1.4 MiB |
| mixed | candidate | baseline | 225.23 ms | 247.74 ms | 4.32% | 14.3 MiB | 5.2 MiB | 2 | 1.218829 ms | 1246 | 11.7 MiB |
| large | candidate | baseline | 528.46 ms | 558.33 ms | 2.18% | 8.9 MiB | 1.2 MiB | 0 | 0 ms | 159 | 32.1 MiB |

### Where the time goes

Phases are wall clock and add up to roughly the run's duration. Operation totals are **cumulative across parallel workers** and are normally larger than the phase they belong to; read them for their share and their per-call cost, never as wall clock.

<details><summary><code>small</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 3344.34 ms |
| candidate | baseline | connect | 421.25 ms |
| candidate | baseline | cleanup | 55.94 ms |
| candidate | baseline | sweep_stale_temps | 54.63 ms |
| candidate | baseline | create_dirs | 41.82 ms |
| candidate | baseline | local_scan | 8.08 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 300 | 194519.89 ms | 648.4 ms | 507.94 ms | 1569.68 ms | 2129.15 ms | 2129.43 ms |
| candidate | baseline | sftp_write | 300 | 65867 ms | 219.56 ms | 194.56 ms | 489.71 ms | 590.21 ms | 751.08 ms |
| candidate | baseline | sftp_rename | 300 | 21557.85 ms | 71.86 ms | 39.41 ms | 210.88 ms | 339.05 ms | 375.22 ms |
| candidate | baseline | sftp_chmod | 300 | 20290.93 ms | 67.64 ms | 46.23 ms | 137.84 ms | 373.54 ms | 448.72 ms |
| candidate | baseline | sftp_open | 300 | 19368.16 ms | 64.56 ms | 36.56 ms | 199.21 ms | 420.65 ms | 472.65 ms |
| candidate | baseline | sftp_readdir | 9 | 484.65 ms | 53.85 ms | 54.31 ms | 54.37 ms | 54.37 ms | 54.37 ms |
| candidate | baseline | ssh_connect | 1 | 381.44 ms | 381.44 ms | 381.44 ms | 381.44 ms | 381.44 ms | 381.44 ms |
| candidate | baseline | sftp_mkdirall | 8 | 326.22 ms | 40.78 ms | 40.92 ms | 41.46 ms | 41.46 ms | 41.46 ms |
| candidate | baseline | sftp_realpath | 3 | 40.27 ms | 13.42 ms | 13.25 ms | 13.97 ms | 13.97 ms | 13.97 ms |

</details>

<details><summary><code>mixed</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 10259.9 ms |
| candidate | baseline | connect | 418.11 ms |
| candidate | baseline | cleanup | 108.44 ms |
| candidate | baseline | sweep_stale_temps | 56.26 ms |
| candidate | baseline | create_dirs | 40.5 ms |
| candidate | baseline | local_scan | 1.86 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 56 | 220939.63 ms | 3945.35 ms | 4234.89 ms | 6172.59 ms | 10258.97 ms | 10258.97 ms |
| candidate | baseline | sftp_write | 56 | 96427.06 ms | 1721.91 ms | 1089.4 ms | 2871.36 ms | 8279.25 ms | 8279.25 ms |
| candidate | baseline | sftp_chmod | 56 | 28726.25 ms | 512.97 ms | 542.13 ms | 1089.73 ms | 1568.55 ms | 1568.55 ms |
| candidate | baseline | sftp_rename | 56 | 20506.34 ms | 366.18 ms | 354.56 ms | 1033.21 ms | 1344.46 ms | 1344.46 ms |
| candidate | baseline | sftp_open | 56 | 860.06 ms | 15.36 ms | 14.6 ms | 18.2 ms | 19.42 ms | 19.42 ms |
| candidate | baseline | sftp_readdir | 9 | 503.01 ms | 55.89 ms | 56.05 ms | 56.17 ms | 56.17 ms | 56.17 ms |
| candidate | baseline | ssh_connect | 1 | 379.18 ms | 379.18 ms | 379.18 ms | 379.18 ms | 379.18 ms | 379.18 ms |
| candidate | baseline | sftp_mkdirall | 8 | 319.28 ms | 39.91 ms | 39.92 ms | 40.4 ms | 40.4 ms | 40.4 ms |
| candidate | baseline | sftp_realpath | 3 | 39.11 ms | 13.04 ms | 13.02 ms | 13.11 ms | 13.11 ms | 13.11 ms |

</details>

<details><summary><code>large</code> phases and round-trips</summary>

| Build | Profile | Phase | Wall |
|---|---|---|---|
| candidate | baseline | upload | 48654.75 ms |
| candidate | baseline | connect | 507.28 ms |
| candidate | baseline | sweep_stale_temps | 57.39 ms |
| candidate | baseline | create_dirs | 40.25 ms |
| candidate | baseline | cleanup | 27.1 ms |
| candidate | baseline | local_scan | 7.12 ms |

| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| candidate | baseline | file_upload | 2 | 93119.89 ms | 46559.95 ms | 48653.58 ms | 48653.58 ms | 48653.58 ms | 48653.58 ms |
| candidate | baseline | sftp_write | 2 | 92018.94 ms | 46009.47 ms | 48609.54 ms | 48609.54 ms | 48609.54 ms | 48609.54 ms |
| candidate | baseline | ssh_connect | 1 | 466.93 ms | 466.93 ms | 466.93 ms | 466.93 ms | 466.93 ms | 466.93 ms |
| candidate | baseline | sftp_readdir | 3 | 161.57 ms | 53.86 ms | 52.84 ms | 57.28 ms | 57.28 ms | 57.28 ms |
| candidate | baseline | sftp_mkdirall | 2 | 80.1 ms | 40.05 ms | 40.17 ms | 40.17 ms | 40.17 ms | 40.17 ms |
| candidate | baseline | sftp_chmod | 2 | 44.75 ms | 22.38 ms | 31.54 ms | 31.54 ms | 31.54 ms | 31.54 ms |
| candidate | baseline | sftp_realpath | 3 | 39.48 ms | 13.16 ms | 13.14 ms | 13.21 ms | 13.21 ms | 13.21 ms |
| candidate | baseline | sftp_rename | 2 | 30.42 ms | 15.21 ms | 17.08 ms | 17.08 ms | 17.08 ms | 17.08 ms |
| candidate | baseline | sftp_open | 2 | 27.08 ms | 13.54 ms | 13.57 ms | 13.57 ms | 13.57 ms | 13.57 ms |

</details>

### Delete sweeps

The pre-clean before every measured run wipes the tree the previous repeat left behind, which makes it a pure delete sweep. It costs no extra time (it has always run) and its numbers never enter the upload tables above. Sweeps that found an empty directory are not counted.

| Scenario | Build | Profile | Sweeps | Files deleted | Median | files/s | remote_scan | delete_sweep |
|---|---|---|---|---|---|---|---|---|
| large | candidate | baseline | 2 | 2 | 751 ms | 2.66 | 108.46 ms | 27.88 ms |
| mixed | candidate | baseline | 2 | 56 | 739 ms | 75.78 | 107.6 ms | 69.74 ms |
| small | candidate | baseline | 2 | 300 | 733 ms | 409.28 | 121.75 ms | 180.84 ms |

| Scenario | Build | Profile | Operation | Count | Cumulative | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|---|---|
| large | candidate | baseline | ssh_connect | 1 | 435.28 ms | 435.28 ms | 435.28 ms | 435.28 ms | 435.28 ms |
| large | candidate | baseline | sftp_readdir | 4 | 213.57 ms | 53.07 ms | 55.3 ms | 55.3 ms | 55.3 ms |
| large | candidate | baseline | sftp_rmdir | 2 | 27.97 ms | 14.08 ms | 14.08 ms | 14.08 ms | 14.08 ms |
| large | candidate | baseline | sftp_remove | 2 | 27.09 ms | 13.55 ms | 13.55 ms | 13.55 ms | 13.55 ms |
| mixed | candidate | baseline | sftp_remove | 56 | 1889.3 ms | 26.72 ms | 47.86 ms | 49.16 ms | 49.16 ms |
| mixed | candidate | baseline | sftp_readdir | 10 | 530.28 ms | 53.27 ms | 54.05 ms | 54.05 ms | 54.05 ms |
| mixed | candidate | baseline | ssh_connect | 1 | 493.42 ms | 493.42 ms | 493.42 ms | 493.42 ms | 493.42 ms |
| mixed | candidate | baseline | sftp_rmdir | 8 | 113.1 ms | 14.23 ms | 15.16 ms | 15.16 ms | 15.16 ms |
| small | candidate | baseline | sftp_remove | 300 | 9384.79 ms | 32.27 ms | 42.26 ms | 53.06 ms | 53.42 ms |
| small | candidate | baseline | sftp_readdir | 10 | 600.39 ms | 57.34 ms | 69.34 ms | 69.34 ms | 69.34 ms |
| small | candidate | baseline | ssh_connect | 1 | 364.85 ms | 364.85 ms | 364.85 ms | 364.85 ms | 364.85 ms |
| small | candidate | baseline | sftp_rmdir | 8 | 137.41 ms | 17.33 ms | 19.72 ms | 19.72 ms | 19.72 ms |

Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158 and to show where a run spends its time.
