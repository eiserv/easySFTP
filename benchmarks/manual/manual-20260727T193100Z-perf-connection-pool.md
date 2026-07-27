# easySFTP benchmark: manual run perf-connection-pool

| Field | Value |
|---|---|
| Kind | manual (not an official reference) |
| Recorded | 2026-07-27T19:31:00Z |
| Commit | `49dcaf38f2879248cfffbdfd02d3b87381992e51` |
| Workflow run | https://github.com/eiserv/easySFTP/actions/runs/30298465159 |
| Raw data | [manual-20260727T193100Z-perf-connection-pool.json](manual-20260727T193100Z-perf-connection-pool.json) |

## easySFTP benchmark

| Setting | Value |
|---|---|
| Candidate | `49dcaf38f2879248cfffbdfd02d3b87381992e51 (49dcaf3)` |
| Baseline | `main (804d0c6)` |
| Repeats per scenario | 3 |
| Runner | Linux 7.0.0-28-generic, 10 cpu |
| Settings | easySFTP defaults (no advanced.* overrides): concurrency 4, request_concurrency 16, retries 2, timeout 30s, mode overlay; the pool4 build is the same binary with advanced.connections: 4 |

| Scenario | Build | Files | Size | Median | Min | Max | MiB/s | files/s | Retries | Errors | Failed runs | Delta |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| small | candidate | 300 | 1.2 MiB | 13404 ms | 12624 ms | 14550 ms | 0.1 | 22.4 | 0 | 0 | 0 | +7.0% |
| small | baseline | 300 | 1.2 MiB | 12531 ms | 11606 ms | 13293 ms | 0.1 | 23.9 | 0 | 0 | 0 | - |
| small | pool4 | 300 | 1.2 MiB | 11457 ms | 11241 ms | 11783 ms | 0.1 | 26.2 | 0 | 0 | 0 | -8.6% |
| mixed | candidate | 56 | 11.6 MiB | 27665 ms | 26608 ms | 29508 ms | 0.4 | 2 | 0 | 0 | 0 | -9.6% |
| mixed | baseline | 56 | 11.6 MiB | 30615 ms | 27675 ms | 32435 ms | 0.4 | 1.8 | 0 | 0 | 0 | - |
| mixed | pool4 | 56 | 11.6 MiB | 18503 ms | 16950 ms | 18550 ms | 0.6 | 3 | 0 | 0 | 0 | -39.6% |
| large | candidate | 2 | 32 MiB | 70694 ms | 70541 ms | 76330 ms | 0.5 | 0 | 0 | 0 | 0 | -0.8% |
| large | baseline | 2 | 32 MiB | 71267 ms | 71167 ms | 79181 ms | 0.4 | 0 | 0 | 0 | 0 | - |
| large | pool4 | 2 | 32 MiB | 48094 ms | 48069 ms | 50646 ms | 0.7 | 0 | 0 | 0 | 0 | -32.5% |

Delta compares each build's median against the `baseline` build; negative is faster.

Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158.
