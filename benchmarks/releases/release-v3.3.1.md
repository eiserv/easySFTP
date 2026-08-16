# easySFTP benchmark: release v3.3.1

| Field | Value |
|---|---|
| Kind | release (official reference) |
| Version | `v3.3.1` |
| Recorded | 2026-07-27T15:55:56Z |
| Commit | `3bf56021e757a468dab39fbaa88b923b9ae00f83` |
| Workflow run | https://github.com/eiserv/easySFTP/actions/runs/30282308009 |
| Raw data | [release-v3.3.1.json](release-v3.3.1.json) |

## easySFTP benchmark

| Setting | Value |
|---|---|
| Candidate | `v3.3.1 (3bf5602)` |
| Baseline | `none` |
| Repeats per scenario | 3 |
| Runner | Linux 6.17.0-1020-azure, 4 cpu |
| Settings | easySFTP defaults: concurrency 4, request_concurrency 16, retries 2, timeout 30s, mode overlay |

| Scenario | Build | Files | Size | Median | Min | Max | MiB/s | files/s | Retries | Errors | Failed runs | Delta |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | 300 | 1.2 MiB | 64892 ms | 64011 ms | 65056 ms | 0 | 4.6 | 0 | 0 | 0 | - |
| mixed | candidate | 56 | 11.6 MiB | 34357 ms | 34166 ms | 34686 ms | 0.3 | 1.6 | 0 | 0 | 0 | - |
| large | candidate | 2 | 32 MiB | 81803 ms | 80044 ms | 81973 ms | 0.4 | 0 | 0 | 0 | 0 | - |

Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158.
