# Completely Portable Tools

`cpt` is a portable package manager for single-binary CLI tools. It reads JSON
package lists, resolves the newest release directly from the web (GitHub by
default), verifies what it downloads, and installs everything into one flat
directory you put on `PATH`.

No package manager integration, no root, no language runtime. One static Go
binary and a `~/.cpt` directory.

```
cpt install fzf ripgrep jq
```

## Status

This is milestone **v0.1**: the core engine, the CLI and the master package
list are complete.

| Area | State |
|---|---|
| List schema, master list, add/refresh/remove lists | done |
| Version resolution without the GitHub API | done |
| Download, checksum verification, TOFU pinning | done |
| tar.gz / tar.xz / tar.zst / zip / raw assets | done |
| Install, update, remove, pin, verify, doctor | done |
| Interactive TUI | planned (M3) |
| `cptinit` shell integration and shell packages | planned (M2) |
| Self-extracting bundles for remote machines | planned (M4) |
| Windows shell packages | deferred |

See [PLAN.md](PLAN.md) for the full design and roadmap.

## Build

```sh
go build -o cpt ./cmd/cpt
install -m755 cpt ~/.cpt/bin/cpt   # ~/.cpt is created on first run
export PATH="$HOME/.cpt/bin:$PATH"
```

Requires Go 1.24 or newer. The binary is pure Go (`CGO_ENABLED=0`) and builds
for linux, darwin and windows on amd64 and arm64.

## Quick start

```sh
cpt list                       # everything in the configured lists
cpt search json                # fuzzy-ish substring search
cpt info jq                    # versions, asset, checksum state
cpt install fzf ripgrep jq
cpt install micro@2.0.15       # exact version
cpt list --updates             # what has a newer release
cpt update --dry-run
cpt update
cpt verify                     # re-hash installed binaries
cpt doctor                     # PATH, conflicts, untracked files
cpt remove micro
```

`cpt pin jq@1.7.1` freezes an installed package; `cpt unpin jq` releases it.

All commands accept `--json` and emit a stable envelope:

```json
{ "ok": true, "data": [ ... ] }
```

Exit codes: `0` success, `1` generic, `2` usage, `3` integrity failure,
`4` conflict, `5` network.

## Package lists

The master list ships inside the binary and is also published at the repository
root as [`cpt.json`](cpt.json). It refreshes from
`https://raw.githubusercontent.com/SuSonicTH/cpt/master/cpt.json` when stale
(24h by default).

Add more sources:

```sh
cpt lists add github.com/acme/tools                      # -> cpt.json at repo root
cpt lists add github.com/acme/tools#main:ci/cpt.json     # branch + path
cpt lists add https://example.com/cpt.json --name acme
cpt lists refresh
cpt lists remove acme
cpt lists lint --network         # maintainer check: every asset URL still exists
```

Any HTTP(S) URL works. Adding a list is a trust decision: lists control which
binaries cpt downloads.

A list is JSON:

```json
{
  "schema": 1,
  "name": "acme",
  "description": "Acme internal tools",
  "packages": [
    {
      "name": "fzf",
      "description": "Command-line fuzzy finder",
      "homepage": "https://github.com/junegunn/fzf",
      "tags": ["shell", "search"],
      "license": "MIT",
      "source": {
        "type": "github",
        "repo": "junegunn/fzf",
        "bins": ["fzf"],
        "checksums": ["fzf_{version}_checksums.txt"],
        "asset": {
          "linux/amd64":  "fzf-{version}-linux_amd64.tar.gz",
          "linux/arm64":  "fzf-{version}-linux_arm64.tar.gz",
          "darwin/amd64": "fzf-{version}-darwin_amd64.tar.gz",
          "darwin/arm64": "fzf-{version}-darwin_arm64.tar.gz",
          "windows/amd64":"fzf-{version}-windows_amd64.zip"
        }
      }
    }
  ]
}
```

- `{tag}` is the raw release tag, `{version}` is the tag with `version_strip`
  applied (default `^v`).
- `asset` maps each supported platform to an asset name. No guessing, no
  aliases: if a platform is absent the package is unavailable there.
- `bins` names the executables to install. When omitted, executable files in
  the archive are used.
- `checksums` lists checksum files to try before the built-in conventions
  (`<asset>.sha256`, `<asset>.sha`, `SHA256SUMS`, `checksums.txt`, ...).
- A package may instead use `"type": "url"` with a pinned `"version"` and a URL
  template containing `{version}`, `{os}` and `{arch}`.

### How versions are found without the GitHub API

`cpt` follows the redirect from `https://github.com/owner/repo/releases/latest`
and reads the tag from the final URL. That costs one HEAD request per
repository per cache TTL, uses no API quota, and survives repository renames.
Tag-only repositories (no GitHub releases) are not supported; pin a version and
use a `url` source instead.

### Integrity

When a checksum file is published, the download must match it. Otherwise cpt
pins the SHA256 on first install (TOFU) and refuses to change it for that
version unless you pass `--force-checksum`. A mismatch against a
list-declared checksum is always a hard failure.

## Layout

```
~/.cpt/
├── config.json      # optional; defaults are built in
├── state.json       # installed packages, hashes, list metadata
├── bin/             # every installed binary lives here
├── lists/           # core.json and any lists you add
├── cache/probe/     # cached upstream tags
├── logs/cpt.log
└── tmp/             # staging, cleaned per operation
```

Override the root with `--root DIR` or `CPT_ROOT`.

## Development

```sh
go test ./...
go vet ./...
```

## License

MIT — see [LICENSE.txt](LICENSE.txt).
