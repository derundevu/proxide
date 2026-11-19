# proxide

## proxide releases

An [Xray-core](https://github.com/XTLS/Xray-core) fork for custom client changes. It is used by [yaxc](https://github.com/derundevu/yaxc).

### Branches and versions

- `main` mirrors an adopted **stable upstream release tag**, not upstream's
  moving development branch. Update it only when adopting a new Xray release.
- `fork/main` is the fork's integration and default branch. It contains the
  upstream base and all fork changes.
- `fork/migration-*` branches isolate upstream upgrades from the working fork.
  Merge them into `fork/main` after resolving conflicts and checking consumers.
- Fork release tags have four numeric components: `v26.10.31.1`,
  `v26.10.31.2`, then revision `1` again on the next upstream base.

The upstream version stays in `core/core.go`; the fork revision is
`ProxideRevision` in `core/proxide_version.go`. `core.Version()` and `xray version`
report the full four-part version, including when core is built as a library
without release linker flags. Build metadata identifies the source commit.
Configuration `version.min`/`version.max` and the REALITY protocol version
continue to use the three-part upstream version.

Four-part release tags are not Go module SemVer. Keep the module path
`github.com/xtls/xray-core`; Go consumers should pin a commit/pseudo-version,
not assume `go get @v26.10.31.1` is a module release.

### Adopting an upstream release

1. Fetch the chosen stable tag from `https://github.com/XTLS/Xray-core.git`
   (`upstream`), then fast-forward `main` to that exact tag. Do not merge
   upstream's development branch or force-reset published history.
2. Create a migration branch **from `fork/main`**, then merge `main` into it.
   Existing fork changes remain in the history; do not collect or replay
   patches from old releases.
3. Resolve conflicts and check behavior, including yaxc's Android build. A clean Git merge is not a compatibility check.
4. Reset `ProxideRevision` to `1` for the new base, then merge the migration into
   `fork/main`. Do not rebase already released commits.

The migration branch is a temporary workspace for adapting the fork to the new
upstream, not a separate permanent release line. Ordinary fork-only fixes do not
require a migration branch.

While migration work is in progress, `fork/main` stays on the previous working
base. Publish the upgraded fork only after merging the verified migration.

For another fork-only release on the same upstream base, increment
`ProxideRevision` instead; leave `main` unchanged.

### Xray v26.11.9 compatibility

This base uses [Xray v26.11.9](https://github.com/XTLS/Xray-core/releases/tag/v26.11.9)
with fork revision `1`, producing source version `26.11.9.1`.

- Freedom `finalRules` use `delay` and support per-rule `logLevel`; upstream
  still accepts `blockDelay` with a deprecation warning. Review the upstream
  safe-policy changes for MASQUE before deploying server configurations.
- Generated log configuration now uses the canonical `logLevel` key.
  Existing `loglevel` input is still accepted; consumers must not rely on
  preserving the input key's spelling when re-serializing a configuration.
- Mux writes use upstream's mutex wrapper while preserving proxide's writer
  close/interrupt propagation and connection-observation completion.
- Android executables retain `-checklinkname=0` and use upstream's
  `-extldflags=-Wl,--pack-dyn-relocs=android`. Rebuild yaxc's `XrayCore.aar`
  against this base; CLI archives are not a replacement for the AAR.
- Docker still uses the verified, shared geodata snapshot, not independent
  downloads from a moving upstream release.

### Naive Chromium outbound

Optional `naive` outbound on the real Chromium/Cronet TLS, HTTP/2 and HTTP/3
stack. Disabled by default; normal builds and release archives are unaffected.
Tagged builds link GPL-3.0-or-later components and are distributed as GPLv3
combined works. No Naive inbound is included.

Build: `CGO_ENABLED=0 go build -tags with_naive_outbound,with_purego ./main`
plus the matching `libcronet` library next to the executable. Configuration,
supported options and limitations: [proxy/naive/README.md](proxy/naive/README.md).

### Publishing

Local checks need the geodata fixture at `resources/` (and an authenticated
GitHub CLI for resolving its source commit):

```sh
bash .github/build/download-resources.sh --geodata-only
go test -timeout 10m ./...
```

Commit and publish the reviewed `fork/main` changes first. Create an annotated
tag matching the source version, for example `v26.10.31.1`, and publish that
tag. Do not create a public GitHub Release manually.

The [release workflow](.github/workflows/release.yml) validates the tag,
source version, fork ancestry and original upstream release base. It runs the
checks, builds the platform archives and Docker images using one resource
snapshot, and publishes the completed release with `SHA256SUMS`. Archive names
remain compatible with upstream, such as `Xray-linux-64.zip`.

The snapshot preserves the usual databases in ZIPs and the full/expanded
databases in Docker. Windows 7 packages require the matching
`XTLS/go-win7` release `patched-<resolved Go version>`; a missing builder fails
the release rather than silently omitting those packages.

Docker images are published at `ghcr.io/derundevu/proxide:26.10.31.1`
(without the leading `v`). `latest` advances only for a newer stable fork
release. Released tags, assets and versioned image tags must not be overwritten;
publish another revision for corrections. For a failed run, use GitHub Actions'
failed-job rerun, or dispatch the release workflow on the existing release tag,
not on a branch.

```sh
gh workflow run release.yml --ref v26.10.31.1 -f tag=v26.10.31.1
```

If Docker promotion already succeeded, rerun the failed publication job rather
than rebuilding everything: a different digest cannot replace a versioned tag.

Repository setup after the first branch publication:

- Set the GitHub default branch to `fork/main` and enable Actions in the fork.
- Permit release jobs to write repository contents and GHCR packages.
- Protect `main`/`fork/main` and prohibit release-tag updates/deletions.
- Make the GHCR package public if consumers need anonymous image pulls.

In each local checkout, also select this fork for GitHub CLI commands; otherwise
`gh` may select the parent Xray repository:

```sh
gh repo set-default derundevu/proxide
```

### Release notes

Release notes are generated from Git history, not merged PRs. Each release has:

- **Upstream:** the original Xray release link, plus an upstream comparison when
  the base version changes.
- **Fork changes:** new fork commit subjects with commit links. Upstream commits
  and commits already reachable from the previous fork release are excluded.
  Fork merge commits remain visible because they can contain conflict resolutions.
- **Full diff:** the complete comparison with the previous fork release; for
  the first release, comparison with the original upstream commit. This full
  comparison includes upstream changes when the base version changes.
- **Build:** source version, commit, Docker tag and image digest.

The previous fork release is the numerically greatest older published stable
four-part version whose tag is an ancestor of the released commit. Drafts,
prereleases and unrelated release branches are not used as the baseline.
If no such release exists, the notes describe all fork commits relative to the
current upstream base.

Preview notes for committed `HEAD` from a full checkout with fork release tags:

```sh
node .github/build/release-notes.cjs v26.10.31.1 derundevu/proxide
```

The preview needs read access through `gh`, does not create or publish anything,
and omits the image digest until the release build supplies it. Uncommitted
changes are not part of the preview.

### Consumers

- **yaxc:** use `https://github.com/derundevu/proxide.git` as the URL of the
  `XrayCore/Xray-core` submodule; keep its path and the local Go `replace`.
  Pin it to the released tag's commit and build `XrayCore.aar` in yaxc;
  a release's Android executable is not an AAR replacement.

## Credits

- [Xray-core v1.0.0](https://github.com/XTLS/Xray-core/releases/tag/v1.0.0) was forked from [v2fly-core 9a03cc5](https://github.com/v2fly/v2ray-core/commit/9a03cc5c98d04cc28320fcee26dbc236b3291256), and we have made & accumulated a huge number of enhancements over time, check [the release notes for each version](https://github.com/XTLS/Xray-core/releases).
- For third-party projects used in [Xray-core](https://github.com/XTLS/Xray-core), check your local or [the latest go.mod](https://github.com/XTLS/Xray-core/blob/main/go.mod).

### Bundled Third-Party Components Redistribution

**Certain optional features dynamically load third-party components. These optional components are separate works distributed under their own licenses, and are bundled into the ZIP package for ease of use. Users may replace these components under the licenses from these components.**

These components include:

#### Wintun

This distribution contains unmodified official precompiled and pre-signed Wintun binaries.

- Project: Wintun
- Copyright: Copyright (C) 2018-2021 WireGuard LLC. All Rights Reserved.
- Redistribution License: Prebuilt Binaries License (PBL) bundled with official precompiled and pre-signed binaries from wintun.net
- Component(s): wintun.dll
- Source: https://www.wintun.net/
- Included in:
  - Windows x86 (windows-32, win7-32)
  - Windows x86-64 (windows-64, win7-64)
  - Windows AArch64 (windows-arm64)
- Notes: Wintun is an optional runtime-loaded component only used for TUN inbound functionality on supported Windows platforms.

## One-line Compilation

### Windows (PowerShell)

```powershell
$env:CGO_ENABLED=0
go build -o xray.exe -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -v ./main
```

### Linux / macOS

```bash
CGO_ENABLED=0 go build -o xray -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -v ./main
```

### Reproducible Releases

Make sure that you are using the same Go version, and remember to set the git commit id (7 bytes):

```bash
CGO_ENABLED=0 go build -o xray -trimpath -buildvcs=false -gcflags="all=-l=4" -ldflags="-X github.com/xtls/xray-core/core.build=REPLACE -s -w -buildid=" -v ./main
```

For Android:

```bash
GOOS=android GOARCH=arm64 CGO_ENABLED=1 CC=/path/to/aarch64-linux-android24-clang go build -o xray -trimpath -buildvcs=false -gcflags="all=-l=4" -ldflags="-X github.com/xtls/xray-core/core.build=REPLACE -s -w -buildid= -checklinkname=0 -extldflags=-Wl,--pack-dyn-relocs=android" -v ./main
GOOS=android GOARCH=amd64 CGO_ENABLED=1 CC=/path/to/x86_64-linux-android24-clang go build -o xray -trimpath -buildvcs=false -gcflags="all=-l=4" -ldflags="-X github.com/xtls/xray-core/core.build=REPLACE -s -w -buildid= -checklinkname=0 -extldflags=-Wl,--pack-dyn-relocs=android" -v ./main
```

If you are compiling a 32-bit MIPS/MIPSLE target, use this command instead:

```bash
CGO_ENABLED=0 go build -o xray -trimpath -buildvcs=false -gcflags="-l=4" -ldflags="-X github.com/xtls/xray-core/core.build=REPLACE -s -w -buildid=" -v ./main
```

## License

[Mozilla Public License Version 2.0](LICENSE)
