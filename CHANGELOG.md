# Changelog

All notable changes to this project will be documented in this file.
New entries are generated automatically by [Release Please](https://github.com/googleapis/release-please)
from [Conventional Commits](https://www.conventionalcommits.org/); this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.0](https://github.com/eiserv/easySFTP/compare/v1.1.0...v1.2.0) (2026-07-14)


### Features

* add build-mode input and enhance prebuilt mode documentation ([67873f5](https://github.com/eiserv/easySFTP/commit/67873f5bdfc636d7e4ddc474a95dde6f89f266d1))


### Bug Fixes

* **ci:** keep version file LF and run prepare-action.sh via bash ([d3dc3a2](https://github.com/eiserv/easySFTP/commit/d3dc3a27ffff4799f25547e4655f4962aa1deb51))


### Performance

* use prebuilt binaries by default ([e390319](https://github.com/eiserv/easySFTP/commit/e390319b4016287f0246ff9bce5e7fce62ae2232))

## [1.1.0](https://github.com/eiserv/easySFTP/compare/v1.0.0...v1.1.0) (2026-07-14)


### Features

* enhance configuration management with YAML support ([60e8655](https://github.com/eiserv/easySFTP/commit/60e8655048af799553a81ec1677baf5a7b1ffae9))
* enhance file upload with atomic rename and error handling ([9669ad0](https://github.com/eiserv/easySFTP/commit/9669ad005fccdf80e0f7f489e436d4fb863b1d1e))


### Bug Fixes

* fail fast on remote file directory conflicts ([7ce964c](https://github.com/eiserv/easySFTP/commit/7ce964cb793f9c1551b6461c92e81df7341abdf1))
* fail fast on remote file directory conflicts ([e01657f](https://github.com/eiserv/easySFTP/commit/e01657f3b5971b92737063780bf8670a3ff89736))
* fix error message formatting in uploader test ([215fefc](https://github.com/eiserv/easySFTP/commit/215fefc8cd5e94ba3aac9204490a8abea0a9d268))


### Documentation

* drop em-dashes, move motivation up and add comparison matrix ([c819b55](https://github.com/eiserv/easySFTP/commit/c819b55f153c25aee0dc34516c487be3d16a1e4f))
* restructure documentation into docs/, add CONTRIBUTING and CODE_OF_CONDUCT ([3255553](https://github.com/eiserv/easySFTP/commit/32555538186c1ac0a72cb926d742be2410210ad8))
* restructure documentation into docs/, add CONTRIBUTING and CODE_OF_CONDUCT ([ae300f5](https://github.com/eiserv/easySFTP/commit/ae300f5ba788d8842d292e5adc625158de87a0b0))

## [1.0.0] - 2026-07-13

### Added

- Initial release
- Recursive directory and single-file uploads via `local => remote` mappings
- Password and private-key authentication (with optional passphrase)
- Optional host key pinning via SHA256 fingerprint
- Gitignore-style exclude patterns (`ignore` input and `ignore-from` file)
- Delete mode for clean deploys
- Dry-run mode
- Parallel uploads (`concurrency`) and per-file retries with backoff (`retries`)
- Step outputs (`files-uploaded`, `files-deleted`, `bytes-uploaded`, `duration-ms`) and a job summary
- Support for Linux, macOS and Windows runners

[1.0.0]: https://github.com/eiserv/easySFTP/releases/tag/v1.0.0
