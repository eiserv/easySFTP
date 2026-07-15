# Changelog

All notable changes to this project will be documented in this file.
New entries are generated automatically by [Release Please](https://github.com/googleapis/release-please)
from [Conventional Commits](https://www.conventionalcommits.org/); this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.2](https://github.com/eiserv/easySFTP/compare/v1.2.1...v1.2.2) (2026-07-15)


### Bug Fixes

* inject version into release binaries ([375a585](https://github.com/eiserv/easySFTP/commit/375a5854e1a3d0269b6aec42f2c721e44978b945))
* log build version and revision at startup ([a8d1879](https://github.com/eiserv/easySFTP/commit/a8d1879706fed67a899d4dc46948df6384d455de))
* log build version and revision at startup ([38b45ef](https://github.com/eiserv/easySFTP/commit/38b45ef8fb107d8c4a8bc1252fbe09a32e1a719f))
* report partial progress on failed runs ([d1f35b4](https://github.com/eiserv/easySFTP/commit/d1f35b4908b3fb67be7171357bf7ad05544eba13))
* report partial progress on failed runs ([d3f6861](https://github.com/eiserv/easySFTP/commit/d3f6861191b5262b9eec560fa0f16006da9a3182))


### Performance

* create only leaf remote directories to cut round-trips ([8dc5dfb](https://github.com/eiserv/easySFTP/commit/8dc5dfbe81f7da0a46b9cf88d4e8d62beaecbd0e))
* create only leaf remote directories to cut round-trips ([753688c](https://github.com/eiserv/easySFTP/commit/753688c4d31897996b2ef4fcbab859a5b060d74c))
* hash sync files through a bounded worker pool ([1f9cb84](https://github.com/eiserv/easySFTP/commit/1f9cb846939eccb49dc867a2e75e511d7d438898))
* hash sync files through a bounded worker pool ([301ba03](https://github.com/eiserv/easySFTP/commit/301ba03db1748a2aeedfc839ba8171adec2d40f6))

## [1.2.1](https://github.com/eiserv/easySFTP/compare/v1.2.0...v1.2.1) (2026-07-14)


### Bug Fixes

* **ci:** derive the prebuilt test ref from .easysftp-version ([9554ccf](https://github.com/eiserv/easySFTP/commit/9554ccf58112e1e5c231f4adf8ae04521285188b))
* **ci:** derive the prebuilt test ref from .easysftp-version ([416767a](https://github.com/eiserv/easySFTP/commit/416767a8b87ec08ff792a1968ded693fe6dc312a))

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
