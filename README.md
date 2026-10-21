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

This is milestone **v0.2**: the core engine, the CLI, the master package list,
shell integration, and the interactive TUI are complete.

| Area | State |
|---|---|
| List schema, master list, add/refresh/remove lists | done |
| Version resolution without the GitHub API | done |
| Download, checksum verification, TOFU pinning | done |
| tar.gz / tar.xz / tar.zst / zip / raw assets | done |
| Install, update, remove, pin, verify, doctor | done |
| `cptinit` for bash/zsh/PowerShell, `init --install` | done |
| Shell packages (inline and URL snippets) with approval | done |
| Interactive TUI (packages, lists, log, progress, multi-select) | done |
| Self-extracting bundles for remote machines | planned (M4) |
| Windows shell packages | deferred |

See [PLAN.md](PLAN.md) for the full design and roadmap.

## Build

```sh
go build -o cpt ./cmd/cpt
mkdir -p ~/.cpt/bin
install -m755 cpt ~/.cpt/bin/cpt
export PATH="$HOME/.cpt/bin:$PATH"
```

`~/.cpt` is created on first run; create `~/.cpt/bin` yourself before copying
the binary there.

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
eval "$(cpt init bash)"        # put ~/.cpt/bin on PATH and load snippets
cpt init bash --install        # ... or add a marker block to ~/.bashrc
```

`cpt pin jq@1.7.1` freezes an installed package; `cpt unpin jq` releases it.

## Interactive TUI

Running `cpt` with no arguments in a terminal opens the TUI:

```
cpt
```

Tabs cover packages, lists and the log. Installs and updates run in the
background with per-package spinners and an aggregate progress bar. Use
`space` to multi-select packages, `/` to search, `i` to install, `u` to update,
`d` to remove, `X` to force-remove modified files, `p` to pin, `enter` for a
full-screen detail view, and `?` for the complete key map. `esc` cancels a
running operation or a background version check.

The TUI never writes outside the terminal: progress events come from the same
engine the CLI uses, and the log tab tails `~/.cpt/logs/cpt.log`. If cpt is run
without a terminal (for example from a script or CI) or with `--no-tui`, it
prints a short explanation and the command help instead.

`--offline` blocks all network access: list refreshes are skipped, adding lists
fails, and installing anything not already present fails. Re-running `install`
for a package that is already current is a no-op, so it works offline.

List, search, info, install, update, remove, verify, doctor, `init`, `shell`
and the `lists` subcommands accept `--json` and emit a stable envelope (help
stays plain text; `cpt init <shell>` prints a raw script unless `--json` is
given):

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
- An optional `shell` block adds inline or URL-referenced shell code, with
  explicit approval for changes. See [Shell integration](#shell-integration).

### How versions are found without the GitHub API

`cpt` follows the redirect from `https://github.com/owner/repo/releases/latest`
and reads the tag from the final URL. That costs one HEAD request per
repository per cache TTL, uses no API quota, and survives repository renames.
Tag-only repositories (no GitHub releases) are not supported; pin a version and
use a `url` source instead.

### Integrity

When a checksum file is published, the download must match it. Otherwise cpt
pins the SHA256 on first install (TOFU) and refuses to change it for that
version. A mismatch is fatal unless you explicitly pass `--force-checksum`,
which records the install as `forced`.

## Shell integration

`cpt init` prints a script that puts the cpt binary directory on `PATH` and
loads approved shell snippets:

```sh
eval "$(cpt init bash)"        # bash or zsh
cpt init bash                  # same script, to review or redirect
cpt init pwsh                  # PowerShell 7+
cpt init powershell            # Windows PowerShell 5.1
```

`cpt init` prints the approved script verbatim, because the output is meant to
be evaluated. Review pending snippet changes with `cpt shell diff` before
approving them.

To make it permanent, let cpt add an idempotent marker block to your startup
file. Repeated runs replace the block instead of adding another one, and
`--uninstall` removes it:

```sh
cpt init bash --install        # ~/.bashrc
cpt init zsh  --install        # ~/.zshrc
cpt init pwsh --install        # $PROFILE (or --file PATH)
cpt init powershell --install  # Windows PowerShell 5.1 profile
cpt init bash --uninstall      # remove the block again
```

`--file PATH` edits a specific startup file, for example a dotfiles checkout.
PowerShell profiles are resolved from `pwsh` so redirected Documents folders and
custom `$PROFILE` locations work. With a non-default `--root`, cpt refuses to
guess an rc file: pass `--file`. The generated scripts live at
`<root>/cptinit.sh` and `<root>/cptinit.ps1` (default `~/.cpt`).

### Shell packages

A package may declare shell code in its list entry. cpt renders approved
snippets into `cptinit`, sorted by package name, with a hash marker:

```json
{
  "name": "fzf",
  "source": { "...": "..." },
  "shell": {
    "bash": {
      "functions": "export FZF_DEFAULT_OPTS='--height 40% --border'",
      "env": { "FZF_ALT_C_COMMAND": "fd --type d" },
      "path": ["/opt/fzf/bin"]
    },
    "pwsh": {
      "functions_url": "https://example.com/fzf.ps1",
      "functions_sha256": "0123456789abcdef..."
    }
  }
}
```

- `functions` is inline shell code; `env` exports variables and `path` prepends
  directories. Environment variables and paths are quoted by cpt.
- `functions_url` must be an `https` URL (loopback may use `http`) and is
  fetched with a required `functions_sha256` (64 hex characters). Verified
  snippets are cached in `cache/shell/<sha256>`.
- A first snippet is approved as part of an explicit `cpt install`, even when
  the package is already up to date. Later changes, including changes picked up
  by list refresh, are **queued, not executed**: the approved snippet stays
  live until you review it.
- A snippet removed from a list is also queued as a removal; the old snippet
  keeps running until you approve or reject the removal.
- `cpt shell reject` remembers that exact candidate and will not offer it again
  until the list-provided snippet changes. `cpt shell reset` clears that memory
  if you rejected by mistake.
- Resolution is all-or-nothing per package: if one flavor's URL fails, the
  previous complete live snippet is kept rather than partially updating state.

```sh
cpt shell                      # approval status
cpt shell diff fzf             # what would change
cpt shell approve fzf          # promote the pending snippet
cpt shell approve --all
cpt shell reject fzf           # keep the live snippet, discard the change
cpt shell reset fzf            # undo a rejection and review it again
```

Lists are remote code. Setting `"allow_shell_packages": false` in
`~/.cpt/config.json` disables snippets while leaving binaries and `PATH` setup
working; already-running shells keep any functions they loaded until restarted.
List changes made while snippets are disabled are recorded and re-synced when
they are enabled again.

## Layout

```
~/.cpt/
├── config.json      # optional; defaults are built in
├── state.json       # installed packages, hashes, list metadata, snippet approval
├── bin/             # every installed binary lives here
├── lists/           # core.json and any lists you add
├── cache/probe/     # cached upstream tags
├── cache/shell/     # verified URL snippets, content addressed
├── cptinit.sh       # generated; source this from a shell
├── cptinit.ps1      # generated; dot-source this from PowerShell
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
