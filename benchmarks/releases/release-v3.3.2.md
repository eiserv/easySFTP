# easySFTP benchmark: release v3.3.2

| Field | Value |
|---|---|
| Kind | release (official reference) |
| Version | `v3.3.2` |
| Recorded | 2026-07-27T16:56:32Z |
| Commit | `afe5aaf7cac2a29675887f36f63c3c82b53aa4c3` |
| Workflow run | https://github.com/eiserv/easySFTP/actions/runs/30286969318 |
| Raw data | [release-v3.3.2.json](release-v3.3.2.json) |

## easySFTP benchmark

| Setting | Value |
|---|---|
| Candidate | `v3.3.2 (afe5aaf)` |
| Baseline | `none` |
| Repeats per scenario | 3 |
| Runner | Linux 7.0.0-28-generic, 10 cpu |
| Settings | easySFTP defaults: concurrency 4, request_concurrency 16, retries 2, timeout 30s, mode overlay |

| Scenario | Build | Files | Size | Median | Min | Max | MiB/s | files/s | Retries | Errors | Failed runs | Delta |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | 300 | 1.2 MiB | 13044 ms | 12069 ms | 13071 ms | 0.1 | 23 | 0 | 0 | 0 | - |
| mixed | candidate | 56 | 11.6 MiB | 28419 ms | 27829 ms | 29459 ms | 0.4 | 2 | 0 | 0 | 0 | - |
| large | candidate | 2 | 32 MiB | 76306 ms | 76005 ms | 81433 ms | 0.4 | 0 | 0 | 0 | 0 | - |

Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158.
