# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - 2026-07-13

### Added

- Initial release.
- Recursive directory and single-file uploads via `local => remote` mappings.
- Password and private-key authentication (with optional passphrase).
- Optional host key pinning via SHA256 fingerprint.
- Gitignore-style exclude patterns (`ignore` input and `ignore-from` file).
- Delete mode for clean deploys.
- Dry-run mode.
- Parallel uploads (`concurrency`) and per-file retries with backoff (`retries`).
- Step outputs (`files-uploaded`, `files-deleted`, `bytes-uploaded`, `duration-ms`) and a job summary.
- Support for Linux, macOS and Windows runners.

[Unreleased]: https://github.com/eiserv/easySFTP/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/eiserv/easySFTP/releases/tag/v1.0.0
