# Changelog

All notable changes to this project will be documented in this file.
New entries are generated automatically by [Release Please](https://github.com/googleapis/release-please)
from [Conventional Commits](https://www.conventionalcommits.org/); this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2.2.0](https://github.com/eiserv/easySFTP/compare/v2.1.0...v2.2.0) (2026-07-20)


### Features

* **connect:** retry the initial connection with backoff ([#94](https://github.com/eiserv/easySFTP/issues/94)) ([7f6e12a](https://github.com/eiserv/easySFTP/commit/7f6e12a4d6b95bd01aa0a6a5895ab9d88cae5dff)), closes [#12](https://github.com/eiserv/easySFTP/issues/12)
* **connect:** support connecting through a jump host (ProxyJump) ([#99](https://github.com/eiserv/easySFTP/issues/99)) ([06ce063](https://github.com/eiserv/easySFTP/commit/06ce06368623f045e743eb0a907769a5ebb6cdaa))
* **log:** add a log-level input (quiet/normal/verbose) ([#96](https://github.com/eiserv/easySFTP/issues/96)) ([8972a4c](https://github.com/eiserv/easySFTP/commit/8972a4ce084428f07f60e3b364ccd11623385e15)), closes [#17](https://github.com/eiserv/easySFTP/issues/17)
* **sync:** document the public manifest and add a manifest-name input ([#97](https://github.com/eiserv/easySFTP/issues/97)) ([e5f880c](https://github.com/eiserv/easySFTP/commit/e5f880ce3a9004219274382e12216e98a12bf498)), closes [#44](https://github.com/eiserv/easySFTP/issues/44)
* **upload:** optional preserve-times keeps local mtimes on uploaded files ([#95](https://github.com/eiserv/easySFTP/issues/95)) ([3b5c903](https://github.com/eiserv/easySFTP/commit/3b5c903460f24e7e0d520a50904c7ad0559a2b59)), closes [#19](https://github.com/eiserv/easySFTP/issues/19)


### Bug Fixes

* deleted "written in go" to get under 125 chars ([21777bb](https://github.com/eiserv/easySFTP/commit/21777bb074950c83c71bb094dec4313542274b84))


### Performance

* **plan:** prune ignored directories during the local walk ([#93](https://github.com/eiserv/easySFTP/issues/93)) ([3bb45ac](https://github.com/eiserv/easySFTP/commit/3bb45acaf0643cb09fecc1592519d2488d2fab80)), closes [#9](https://github.com/eiserv/easySFTP/issues/9)

## [2.1.0](https://github.com/eiserv/easySFTP/compare/v2.0.1...v2.1.0) (2026-07-19)


### Features

* accept OpenSSH known_hosts entries via a new known-hosts input ([#88](https://github.com/eiserv/easySFTP/issues/88)) ([bf9d5d7](https://github.com/eiserv/easySFTP/commit/bf9d5d7edd5434c000dec50594c336b125a38c5a)), closes [#7](https://github.com/eiserv/easySFTP/issues/7)
* add stall-timeout to fail fast when transfers stop progressing ([#89](https://github.com/eiserv/easySFTP/issues/89)) ([08359e8](https://github.com/eiserv/easySFTP/commit/08359e8031cf8f8edb89747c86e2f81eac2e739f)), closes [#45](https://github.com/eiserv/easySFTP/issues/45)
* added github pages site ([7031633](https://github.com/eiserv/easySFTP/commit/7031633ba51aae8e3cf2ab40121baed23d58ae0e))
* **overlay:** opt-in skip-unchanged skips same-size remote files ([#87](https://github.com/eiserv/easySFTP/issues/87)) ([8986191](https://github.com/eiserv/easySFTP/commit/89861912b8aa3fe7e6da3e783daddbe07834c622)), closes [#24](https://github.com/eiserv/easySFTP/issues/24)
* reconnect when the SSH connection drops mid-run ([#90](https://github.com/eiserv/easySFTP/issues/90)) ([66a780b](https://github.com/eiserv/easySFTP/commit/66a780b50f0b105ad68224c596cb13512a6934bd)), closes [#43](https://github.com/eiserv/easySFTP/issues/43)
* **sync:** persist a merged manifest when a run fails partway ([#86](https://github.com/eiserv/easySFTP/issues/86)) ([6a6a90d](https://github.com/eiserv/easySFTP/commit/6a6a90d1ed4ace19b99df55f28b6519a7f6dc231)), closes [#47](https://github.com/eiserv/easySFTP/issues/47)


### Bug Fixes

* renamed the self-test fixture dir to selftest-site/ in .github/workflows/ci.yml ([9619df7](https://github.com/eiserv/easySFTP/commit/9619df7d1f2516d82b9f90773eea7f958c358f1b))


### Performance

* **sync:** derive remote directories from the actual upload set ([#85](https://github.com/eiserv/easySFTP/issues/85)) ([37bc54d](https://github.com/eiserv/easySFTP/commit/37bc54d53fa337aca81dec8de3c590106f261120)), closes [#69](https://github.com/eiserv/easySFTP/issues/69)

## [2.0.1](https://github.com/eiserv/easySFTP/compare/v2.0.0...v2.0.1) (2026-07-19)


### Bug Fixes

* drop the unused and miscounting Stats.DirsCreated field ([#76](https://github.com/eiserv/easySFTP/issues/76)) ([39e8239](https://github.com/eiserv/easySFTP/commit/39e8239d89f8b6810e1780c35cec744345f6ffe4))
* reject negative timeout, document 0 as no-timeout escape hatch ([#74](https://github.com/eiserv/easySFTP/issues/74)) ([c318fb9](https://github.com/eiserv/easySFTP/commit/c318fb90821a887bbf3f7631287c39a590ee7857))
* report planned bytes in dry-run instead of always 0 ([#75](https://github.com/eiserv/easySFTP/issues/75)) ([74b00f0](https://github.com/eiserv/easySFTP/commit/74b00f0d8c0426fe3c03359c0d0ab8f2b5cef554))
* stop rejecting config-file runs over the max-deletes input default ([e7345a4](https://github.com/eiserv/easySFTP/commit/e7345a4925951c617da776b95de943a7875ae874))
* stop rejecting config-file runs over the max-deletes input default ([781426b](https://github.com/eiserv/easySFTP/commit/781426bcfc8ab9b229e6cead77a26dcc48d74ff4)), closes [#62](https://github.com/eiserv/easySFTP/issues/62)


### Documentation

* add missing punctuation in troubleshooting auth section ([#78](https://github.com/eiserv/easySFTP/issues/78)) ([eaead21](https://github.com/eiserv/easySFTP/commit/eaead21a95462cd4512e7cb50ee12f0537956135))
* recommend [@v2](https://github.com/v2) in README and doc snippets ([#81](https://github.com/eiserv/easySFTP/issues/81)) ([b0919a8](https://github.com/eiserv/easySFTP/commit/b0919a808296a98f1ea1eeefa01f3fc467e98aea))

## [2.0.0](https://github.com/eiserv/easySFTP/compare/v1.2.2...v2.0.0) (2026-07-19)


### ⚠ BREAKING CHANGES

* the 'delete' input was removed — use 'strategy: clean'.

### Features

* add dir-mode/file-mode inputs for remote permission control ([d542f2f](https://github.com/eiserv/easySFTP/commit/d542f2f535cdc3ee177ad22d65ea4e6d78de1f05))
* add dir-mode/file-mode inputs for remote permission control ([ab3239d](https://github.com/eiserv/easySFTP/commit/ab3239d62a623cc95353589292203b193c9c17dc)), closes [#48](https://github.com/eiserv/easySFTP/issues/48)
* add max-deletes input for the delete safety guard ([6ffd531](https://github.com/eiserv/easySFTP/commit/6ffd5315d9d3095a0754a09238536dc3e5326a52))
* add max-deletes input for the delete safety guard ([46ab007](https://github.com/eiserv/easySFTP/commit/46ab007f6178edd14ff0073cd6e87093a5309b9e)), closes [#49](https://github.com/eiserv/easySFTP/issues/49)
* add per-target breakdown to the job summary for multi-target deploys ([3e41a47](https://github.com/eiserv/easySFTP/commit/3e41a47526aafacc97f0f1b97c8f296535d010a2))
* add per-target breakdown to the job summary for multi-target deploys ([ed78340](https://github.com/eiserv/easySFTP/commit/ed78340d4f5c1cedb85763ebb6c109bbb0295586)), closes [#18](https://github.com/eiserv/easySFTP/issues/18)
* make per-file SFTP request concurrency configurable, lower default to 16 ([992e980](https://github.com/eiserv/easySFTP/commit/992e980634ababf64f2f3888f048e9fb9fe5cbee)), closes [#29](https://github.com/eiserv/easySFTP/issues/29)
* make SFTP per-file request concurrency configurable, lower default to 16 ([b6b2c35](https://github.com/eiserv/easySFTP/commit/b6b2c35a99a9a13445455251a434501675955d29))
* remove the 'delete' input in favor of 'strategy: clean' ([52eef32](https://github.com/eiserv/easySFTP/commit/52eef323a79e773df19c7851a1fd3998d07c6860)), closes [#50](https://github.com/eiserv/easySFTP/issues/50)
* send SSH keepalives to survive idle-timeout firewalls ([5c5f48f](https://github.com/eiserv/easySFTP/commit/5c5f48fbfb10fb07de9e9cd41f9e900d653ea887))
* send SSH keepalives to survive idle-timeout firewalls ([5232471](https://github.com/eiserv/easySFTP/commit/523247153fab369713804b11cb4ca817084b6ad9)), closes [#13](https://github.com/eiserv/easySFTP/issues/13)
* **sync:** add opt-in sync-fast-path to skip re-hashing unchanged files ([458b065](https://github.com/eiserv/easySFTP/commit/458b06557dc04d1c864acbd89761ad15bb829890))
* **sync:** add opt-in sync-fast-path to skip re-hashing unchanged files ([3c66e40](https://github.com/eiserv/easySFTP/commit/3c66e40d75802489a0255a4c6226976a6ea38c23))
* warn once per target when non-regular files are skipped ([b4e6c0c](https://github.com/eiserv/easySFTP/commit/b4e6c0c368ff4141b6df1d25de34ca5614484a17))
* warn once per target when non-regular files are skipped ([664a1a0](https://github.com/eiserv/easySFTP/commit/664a1a08160144f8e0b6cfc56af302e3b9f23a1b)), closes [#15](https://github.com/eiserv/easySFTP/issues/15)


### Bug Fixes

* **gha:** use file-based heredoc syntax for $GITHUB_OUTPUT ([625ecd5](https://github.com/eiserv/easySFTP/commit/625ecd59ea9d637c9e64d5d94108670a6d77e605))
* **gha:** use file-based heredoc syntax for $GITHUB_OUTPUT instead of workflow-command escaping ([d706c8a](https://github.com/eiserv/easySFTP/commit/d706c8ae300637ed45233924616f01c4d1a3b8e1)), closes [#46](https://github.com/eiserv/easySFTP/issues/46)
* make upload temp filenames collision-proof ([6e81dac](https://github.com/eiserv/easySFTP/commit/6e81dac9fc4252e1940964f3a58505c9ffe9d0bb))
* make upload temp filenames collision-proof ([e7ce65f](https://github.com/eiserv/easySFTP/commit/e7ce65fe21f5d689209e670bd7005ea7a9674ce4)), closes [#42](https://github.com/eiserv/easySFTP/issues/42)


### Documentation

* added CLAUDE.md ([0aa2ddf](https://github.com/eiserv/easySFTP/commit/0aa2ddf9a2739a52eaf7749c4d77130ca83277cf))
* fix grammar in README sync strategy description ([6bf7456](https://github.com/eiserv/easySFTP/commit/6bf7456ae461e48073d4df181410e26b77282d06))

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
