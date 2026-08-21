# easySFTP documentation

| Document | What's in it |
|---|---|
| [Configuration reference](configuration.md) | Every input, output, exclude patterns and the YAML config file. |
| [Transfer tuning](tuning.md) | What `auto` does for `connections`, `concurrency` and `request_concurrency`, and when to override it. |
| [Strategies](strategies.md) | `overlay` vs. `sync` vs. `clean`, the sync manifest, delete guards, dry runs. |
| [Examples & use cases](examples.md) | Copy-paste recipes: static sites, mirroring, multi-target deploys, PR previews, outputs. |
| [Security guide](security.md) | Host key pinning, credential handling, least privilege, supply-chain safety. |
| [Troubleshooting & FAQ](troubleshooting.md) | Common errors and what to do about them. |
| [Provider notes](providers.md) | Host-specific ports, paths and quirks; how to find your own; what easySFTP needs from a server. |
| [Migrating from Dylan700/sftp-upload-action](migration.md) | Input mapping, before/after workflow, behavior differences. |
| [Migrating from v2 to v3](migration-v3.md) | Renamed and removed inputs, the config-file format change. |
| [Releasing](RELEASING.md) | How releases, tags and the changelog are automated (maintainers). |
| [easysftp.example.yml](easysftp.example.yml) | Fully commented example config file. |

New here? Start with the [README](../README.md) quick start, then the
[examples](examples.md).
