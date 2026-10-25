# cpt — Completely Portable Tools

`cpt` is one static Go binary that is simultaneously a package manager, a TUI, and a
self-extracting installer. It resolves portable binaries straight from the web (mostly
GitHub releases) using declarative JSON package lists, installs them into a flat
`~/.cpt/bin`, and can freeze a chosen set of tools into a single native binary that sets
up a working `cpt` environment on another machine.

Status: design accepted; M0–M6 implemented (core, CLI, shell integration, TUI,
bundles, Windows/macOS platform support and saved SSH targets), plus the completed
M5.1 bundle/UI polish milestone. Release engineering moved from M6 to M7.

---

## 1. Decisions

| Area | Decision |
|---|---|
| Scope | Local package manager **and** remote provisioning, equal weight |
| Platforms | Linux, macOS, Windows; amd64 + arm64 |
| Go style | Pure Go, `CGO_ENABLED=0`, dependencies allowed (Bubble Tea stack) |
| Versioning | Latest by default, optional per-package pin |
| Bundle | One native binary = cpt + concatenated payload; no Go toolchain needed to build |
| GitHub | **No API at all** — resolve tags from the `releases/latest` 302 redirect |
| Layout | Flat `~/.cpt/bin`; `state.json` is the source of truth |
| Sources | Any https URL or GitHub repo; `cpt.json` at repo root; auto-refresh, TTL 24h |
| Integrity | SHA256 when discoverable, TOFU pin otherwise |
| Conflicts | Duplicate package names are a hard error |
| Shell | Generated `cptinit.sh` / `cptinit.ps1`, opt-in rc edit; Windows shell packages deferred |
| Target state | Live environment by default, `--frozen` for air-gapped targets |
| SSH targets | System `ssh`/`scp`; saved destination + bundle; session-scoped init; never edits remote rc |
| Repo | Public, `github.com/SuSonicTH/cpt` |
| CI | Full build matrix, goreleaser, nightly list lint |

---

## 2. `~/.cpt` layout

```
~/.cpt/
├── config.json                # user preferences
├── state.json                 # installed packages, hashes, list metadata, bin ownership
├── ssh.json                   # saved SSH targets (bundle + ssh destination)
├── .lock                      # flock/LockFileEx: one mutating cpt at a time
├── .ssh.lock                  # serializes ssh.json updates
├── bin/                       # flat, on PATH; cpt itself lives here
├── lists/
│   ├── core.json              # master list (builtin, refreshable)
│   ├── acme.json
│   └── acme.lock.json         # snapshot of exact versions+hashes from the last resolve
├── cache/
│   ├── probe/<hash>.json      # resolved tag per repo, short TTL
│   ├── shell/<sha256>         # verified URL shell snippets
│   ├── self/cpt_1.2.0_linux_arm64   # bundle base binaries
│   └── downloads/<sha256>.<ext>     # content-addressed, optional reuse
├── bundles/
│   ├── devbox-linux-arm64
│   ├── devbox.exe
│   └── devbox.json            # manifest: exact versions/hashes, target, frozen
├── logs/cpt.log
├── tmp/                       # staging, wiped on start
├── cptinit.sh
└── cptinit.ps1
```

`--root` overrides `~/.cpt` (tests, portable USB usage).

---

## 3. Schemas

### 3.1 `config.json`

```json
{
  "schema": 1,
  "bin_dir": "",
  "parallel": 4,
  "list_ttl_hours": 24,
  "probe_ttl_hours": 6,
  "checksum_policy": "require-if-available",
  "allow_shell_packages": true,
  "http": { "timeout_seconds": 60, "retries": 3, "user_agent": "cpt/1.0 (+https://github.com/SuSonicTH/cpt)" },
  "ui": { "theme": "auto" },
  "logs": { "level": "info" }
}
```

`checksum_policy`: `off | prefer | require-if-available | require`.
No GitHub token is required anywhere because the API is never used.

### 3.2 `cpt.json` — a package list

```json
{
  "schema": 1,
  "name": "core",
  "description": "Portable CLI tools",
  "homepage": "https://github.com/SuSonicTH/cpt",
  "maintainer": "SuSonicTH",
  "updated": "2026-10-21",
  "packages": [
    {
      "name": "fzf",
      "description": "Fuzzy finder",
      "homepage": "https://github.com/junegunn/fzf",
      "tags": ["shell", "search"],
      "license": "MIT",
      "source": {
        "type": "github",
        "repo": "junegunn/fzf",
        "version_strip": "^v",
        "format": "auto",
        "asset": {
          "linux/amd64":   "fzf-{version}-linux_amd64.tar.gz",
          "linux/arm64":   "fzf-{version}-linux_arm64.tar.gz",
          "darwin/amd64":  "fzf-{version}-darwin_amd64.tar.gz",
          "darwin/arm64":  "fzf-{version}-darwin_arm64.tar.gz",
          "windows/amd64": "fzf-{version}-windows_amd64.zip",
          "windows/arm64": "fzf-{version}-windows_arm64.zip"
        },
        "bins": ["fzf"],
        "checksums": ["fzf_{version}_checksums.txt"]
      }
    }
  ]
}
```

Rules:

- `{tag}` is the raw release tag; `{version}` is the tag after `version_strip`
  (a regex, default `^v`); `{asset}` is only valid inside `checksums` templates.
- The per-platform `asset` map is the only source of truth for naming. There is no
  runtime alias guessing. An omitted platform means "listed but unavailable here".
- `format`: `auto | tar.gz | tar.xz | tar.zst | zip | raw` (auto by extension).
- `bins` is optional. When omitted, every regular executable file at depth <= 2 after
  `strip_prefix` is installed (on Windows: `*.exe`).
- `type: "url"` entries use `"url": "https://.../{version}/..."` plus a required pinned
  `"version"` and `"version_check": "none"`.
- A `version` field in the list pins that package. Precedence:
  CLI `pkg@ver` > user pin in state > list pin > upstream latest.
- A `shell` block (see section 7) may carry `bash` and/or `pwsh` snippets, inline or
  URL-referenced.

### 3.3 `state.json`

```json
{
  "schema": 1,
  "generated": "2026-10-21T10:00:00Z",
  "packages": {
    "fzf": {
      "tag": "v0.74.5",
      "version": "0.74.5",
      "list": "core",
      "source_id": "github:junegunn/fzf",
      "asset": "fzf-0.74.5-linux_amd64.tar.gz",
      "download_sha256": "...",
      "checksum_status": "verified",
      "checksum_source": "fzf_0.74.5_checksums.txt",
      "bins": { "fzf": "sha256:..." },
      "pinned": "",
      "origin": "install",
      "installed_at": "2026-10-21T10:00:01Z"
    }
  },
  "provides": { "fzf": "fzf", "rg": "ripgrep" },
  "lists": {
    "core": {
      "origin": "https://raw.githubusercontent.com/SuSonicTH/cpt/master/cpt.json",
      "etag": "W/...",
      "sha256": "...",
      "fetched_at": "2026-10-21T09:00:00Z",
      "builtin": true,
      "readonly": false
    }
  },
  "shell": {
    "fzf": {
      "snippet_sha256": "...",
      "approved": false,
      "live": { "version": "0.74.5", "bash": "export FZF_DEFAULT_OPTS='...'" },
      "pending": { "version": "0.75.0", "bash": "..." },
      "pending_sha256": "...",
      "rejected_sha256": "",
      "last_error": "",
      "sync_pending": false,
      "sync_attempt": "2026-10-21T10:00:00Z"
    }
  }
}
```

All writes are temp file + fsync + rename, serialized under `~/.cpt/.lock`.

---

## 4. Resolution without the GitHub API

For `type: github`:

1. `GET https://github.com/{repo}/releases/latest`, following redirects manually
   (max 5) and recording the chain.
2. The final `Location` must match `/releases/tag/{tag}`; that is the tag. A 301 to
   another repository (for example `zyedidia/micro` -> `micro-editor/micro`, verified
   live) is cached as the canonical repo.
3. A 404 means `ErrNoReleases`: report it and suggest a pinned version or `type: url`.
   Tag-only repositories are not supported in v1.
4. Cache the tag in `cache/probe/` for `probe_ttl_hours`, keyed by canonical repo.
5. Asset URL is
   `https://github.com/{canonical_repo}/releases/download/{tag}/{interpolated asset}`.
6. If that 404s, fall back to
   `https://github.com/{repo}/releases/latest/download/{interpolated asset}`, but
   only for unpinned, version-less asset names. Pinned installs never fall back, so
   a pin can never silently install a newer release.

Cost per package is one HEAD per TTL, so the anonymous API quota is irrelevant. GitHub
HTML can still throttle: use `parallel=4`, honour `Retry-After`, back off on 429.

Archive support: tar.gz, zip and raw via the standard library, plus
`klauspost/compress/zstd` and `ulikunitz/xz` (both pure Go).

---

## 5. Install pipeline

```
resolve -> probe cache -> plan (url, version, checksum source)
  -> download to tmp/*.part (progress) -> verify
  -> safe-extract -> select bins -> atomic rename into bin/ -> update state
  -> regenerate cptinit if shell packages changed -> GC
```

### Checksum discovery

In order:

1. Templates declared in the list.
2. `{asset}.sha256` (used by ripgrep, starship, zellij).
3. `SHA256SUMS`, `checksums.txt`, `sha256sums.txt`, `checksums`, `SHA2-256SUMS`,
   `{name}_{version}_checksums.txt` (for example `gh_2.102.1_checksums.txt`,
   `fzf_0.74.5_checksums.txt`).
4. Parse either a bare hash or `<hash>  <filename>` lines matched by asset basename.
   Non-standard formats (yq publishes a multi-algorithm `checksums`) are ignored
   rather than misparsed.

Not found means TOFU: the hash is pinned in state. A later mismatch is a hard failure
with expected/actual values; `--force-checksum` is the explicit escape hatch. A
mismatch against any published checksum is equally fatal unless `--force-checksum`
is passed.

### Safety

Reject absolute paths, `..`, symlinks or hardlinks escaping the extraction root, and
entries above configurable size caps. Always extract into a fresh `tmp/` directory.

### Flat bin semantics

Binaries are copied or symlinked into `~/.cpt/bin` and ownership is recorded in
`provides`. Two packages claiming the same binary is an install error naming both.
There is no rollback by design; `cpt install pkg@oldver` is the rollback.
On Windows binaries are plain copies of `.exe` files; if a target is running, install
as `<name>.new.exe` and swap on next start with a warning.

---

## 6. CLI surface

```bash
cpt                                   # TUI
cpt list --updates                    # text output
cpt search json
cpt install fzf ripgrep jq
cpt install fzf@0.60.0                # explicit version
cpt update --all --dry-run
cpt remove micro
cpt pin fzf@0.74.5
cpt unpin fzf
cpt verify                            # re-hash installed files
cpt doctor                            # PATH, conflicts, stale entries, permissions
cpt lists add https://raw.githubusercontent.com/acme/tools/main/cpt.json --name acme
cpt lists add github.com/acme/tools#main:ci/cpt.json
cpt lists refresh --force
cpt init bash                         # print cptinit.sh
cpt init bash --install               # marker block in .bashrc
cpt init bash --uninstall             # remove the marker block
cpt init pwsh --install --file PATH   # explicit startup file
cpt shell                             # snippet approval status
cpt shell diff fzf                    # pending change for one package
cpt shell approve fzf                 # promote a pending snippet
cpt shell reject --all                # discard every pending change
cpt bundle create devbox --packages fzf,jq,rg --target linux/arm64 --frozen
cpt bundle rebuild devbox
cpt ssh                        # list saved SSH targets
cpt ssh staging                # copy the target's bundle and open an interactive session
cpt ssh add staging --bundle devbox --config-host staging-box --cleanup
cpt ssh add lab --bundle devbox --host 10.0.0.7 --user mike --port 2222
cpt ssh edit staging --shell zsh
cpt ssh remove staging
cpt self-update
cpt version
```

Global flags: `--root`, `--json`, `--yes`, `--verbose`, `--quiet`, `--no-color`,
`--offline`.

Exit codes: `0` ok, `1` generic, `2` usage, `3` integrity failure, `4` conflict,
`5` network. `--json` emits stable envelopes for scripting and is also what the TUI
consumes from the core.

---

## 7. `cptinit` and shell packages

```sh
# generated by cpt — do not edit
export CPT_HOME="$HOME/.cpt"
case ":$PATH:" in *":$CPT_HOME/bin:"*) ;; *) PATH="$CPT_HOME/bin:$PATH" ;; esac
export PATH

# >>> cpt:fzf v0.74.5 sha256:abcd1234
export FZF_DEFAULT_OPTS='--height 40% --border'
# <<< cpt:fzf
```

- Usable via `eval "$(cpt init bash)"`, a generated file, or a marker block appended
  by `--install` / removed by `--uninstall` (idempotent; `.bashrc`, `.zshrc`,
  `$PROFILE`).
- The PowerShell version sets `$env:PATH`, `$env:CPT_HOME` and defines functions.
  Windows shell packages are deferred, but PATH and init work from day one.
- **Snippet changes require approval.** On list refresh, changed snippets are queued
  (`state.shell[name].approved=false`), the previous snippet stays live, and the TUI
  or CLI shows a diff. Silent auto-refresh must never become silent remote code
  execution.
- Snippets can be inline (`functions` plus an `env` map) or referenced by
  `functions_url` plus `functions_sha256`. URL snippets are cached and hashed like
  binaries.
- Removing a `shell` block (or its list) queues a removal with an empty pending
  body; the previous snippet stays live until the removal is approved or
  rejected. A rejection stores the candidate hash so it is not offered again
  until the list-provided body changes (`cpt shell reset` clears it).
- Rendering order is deterministic (sorted by package name) so diffs stay small.

---

## 8. Bundle format

### Container

```
[cpt base binary][zstd-compressed tar payload][64-byte footer]
```

Footer layout (little endian):

```
 0..8    "CPTBDL01"
 8..16   payload_offset  (uint64)
16..24   payload_len     (uint64)
24..56   sha256(compressed payload)
56..60   crc32c(compressed payload)
60..64   format version + flags
```

Trailing bytes are legal in ELF and PE, so a bundle is just a slightly fatter
executable. On startup cpt stats itself, reads the last 64 bytes, and enters bundle
mode on a valid footer.

### Payload

`bundle.json`, `lists/`, `bin/`, `cptinit.*`:

```json
{
  "schema": 1,
  "name": "devbox",
  "created": "2026-10-21T10:00:00Z",
  "target": "linux/arm64",
  "cpt_version": "1.0.0",
  "frozen": true,
  "packages": [
    {
      "name": "fzf",
      "tag": "v0.74.5",
      "version": "0.74.5",
      "asset": "fzf-0.74.5-linux_arm64.tar.gz",
      "download_sha256": "...",
      "bins": ["fzf"],
      "checksum_status": "verified"
    }
  ],
  "lists": { "core": { "sha256": "..." } }
}
```

### Build — `cpt bundle create`

1. Resolve every selected package **for the target platform**, not the host, using
   the same engine in download-only mode.
2. Base binary: when target equals host, reuse the running executable (truncating any
   existing payload). Otherwise fetch
   `https://github.com/SuSonicTH/cpt/releases/download/v{ver}/cpt_{ver}_{os}_{arch}.tar.gz`
   and cache it in `cache/self/`. This is how arm64 bundles are built on amd64 with
   no Go installed.
3. Assemble the payload and write `base + zstd(tar) + footer` to
   `~/.cpt/bundles/<name>-<os>-<arch>[.exe]`.
4. Save `bundles/<name>.json` so `cpt bundle rebuild` reproduces exact versions.
5. Print size and sha256. Never install anything locally.

### Run — on the target

```
verify footer/sha256 -> check target -> extract into ~/.cpt (or --dest)
-> merge state (origin: "bundle") -> write lists (readonly when frozen)
-> install clean bin/cpt by truncating self at payload_offset
-> generate cptinit -> print: source "$HOME/.cpt/cptinit.sh"
```

Flags: `--dest`, `--list`, `--dry-run`, `--force`, `--no-init`, `--install-init`.
Re-running is idempotent. Live bundles leave lists writable so `cpt update` works;
`--frozen` sets `readonly: true` and makes `cpt update` explain itself until
`--unfreeze`.

---

## 9. TUI

Bubble Tea + Lip Gloss + Bubbles. Tabs: **Packages · Lists · Bundles · SSH · Log**.

```
┌ cpt ──────────────────────────────────────────────────────────────┐
│ Packages   Lists   Bundles   Log                        4 updates │
│ / fzf                                                             │
│ ┌───────────────────────────────┬─────────────────────────────────┐│
│ │ ● bat        0.26.1           │ bat                             ││
│ │ ↑ fzf        0.73.0 → 0.74.5  │ cat with syntax highlighting    ││
│ │ ○ jq                         │ list     core                   ││
│ │ ● micro      2.0.15           │ asset    bat-v0.26.1-x86_...    ││
│ │ ✓ ripgrep    15.2.0           │ hash     verified (SHA256SUMS)  ││
│ └───────────────────────────────┴─────────────────────────────────┘│
│ space select  i install  u update  d remove  p pin  enter detail   │
└────────────────────────────────────────────────────────────────────┘
```

- `/` fuzzy search over name, description and tags. `a` toggles all updates.
  Space multi-selects across tabs.
- States: `●` installed, `↑` update available, `✓` pinned, `○` available, `✗` error,
  `⧗` working.
- Installs run concurrently with a per-package spinner and an aggregate footer bar.
  `esc` cancels (context cancel, `.part` cleanup).
- Lists tab: source, origin URL, age, package count, conflicts; `n` add, `r` refresh,
  `x` remove. Changed shell snippets surface here for approval with a diff.
- Bundles tab: two columns like Packages. The left table lists saved bundles; the
  selected bundle's details and full included-package list appear on the right.
- SSH tab: the same two-column layout. The left table lists saved targets;
  the right side shows the referenced bundle, effective SSH destination, shell,
  remote root and cleanup policy. `n`/`e` open the target wizard, `enter` or `c`
  connects, and `d` deletes the local target. Connecting hands the terminal to
  `cpt --root <root> ssh <target>` through `tea.ExecProcess`, so SSH password and
  key-passphrase prompts work normally; the TUI resumes when the session ends.
- `n` and `e` open the same four-step wizard (name → target platform → packages →
  live/frozen + build). Editing pre-fills the name, target, package selection and
  mode. Targets are chosen from a selectable platform list and packages from a
  searchable checkbox list filtered to that target; nothing has to be typed from
  memory. `r` rebuilds exact versions and `d` removes.
- Exactly one shortcut line is rendered, at the bottom. Keys are bracketed and
  yellow (`[space] select`, `[e] edit`, …), the tab strip carries a
  `[tab] switch tabs` hint, and prompt labels are never ellipsised.
- Non-TTY or `--no-tui` prints a clear message and routes to the CLI.

---

## 9.1 SSH targets (M6)

An SSH target is a saved local recipe that pairs one already-built bundle with an
SSH destination. `cpt ssh <name>` copies the bundle to the remote host, runs it
there to install or refresh `~/.cpt`, then opens an interactive SSH session whose
shell has the generated `cptinit` loaded. It **never** edits `.bashrc`, `.zshrc`,
`.profile` or any other remote startup file, and it never passes `--install-init`.
When the session ends, the remote cpt root is removed only when the target opts
in to cleanup.

### Storage

`~/.cpt/ssh.json`, schema 1, written atomically under a dedicated `.ssh.lock`:

```json
{
  "schema": 1,
  "targets": {
    "staging": {
      "name": "staging",
      "bundle": "devbox",
      "ssh": {
        "source": "config",
        "host": "staging-box",
        "user": "",
        "port": 0,
        "identity_file": "",
        "proxy_jump": ""
      },
      "remote_root": "",
      "shell": "auto",
      "cleanup_on_exit": false,
      "force": false,
      "created": "2026-10-25T10:00:00Z",
      "updated": "2026-10-25T10:00:00Z",
      "last_connected": "2026-10-25T10:05:00Z",
      "last_copied": "2026-10-25T10:05:00Z",
      "copied_sha256": "abcdef...",
      "copied_key": "abcdef..."
    }
  }
}
```

- Target and bundle names use the bundle-name rules. `bundle` must name an
  existing manifest with a valid executable under `bundles/`; a missing or
  corrupt bundle is an error and the target wizard never builds one implicitly.
- `ssh.source = "config"`: `host` is an alias from `~/.ssh/config`. `ssh` and
  `scp` receive the alias, so `HostName`, `User`, `Port`, `IdentityFile`,
  `ProxyJump`, `Match` and `Include` keep their normal OpenSSH semantics. The
  non-empty `user`, `port`, `identity_file` and `proxy_jump` fields are explicit
  overrides passed as flags. `ssh -G <alias>` is used only to display the
  effective destination.
- `ssh.source = "custom"`: `host` is a hostname or IP and `user`, `port`,
  `identity_file`, `proxy_jump` are passed explicitly. This is the "new server
  / user combo" path; it does not write `~/.ssh/config`.
- `remote_root` empty means the remote `~/.cpt`. A non-empty value is an absolute
  POSIX path. Cleanup refuses `/`, top-level directories, `$HOME` and paths that
  contain `$HOME`. The remote bundle is installed with `--dest <remote_root>`
  when a custom root is configured.
- `shell`: `auto`, `bash`, `zsh` or `sh`. `auto` uses the remote `$SHELL`.
- `cleanup_on_exit` defaults false. `force` is passed to the bundle only when
  explicitly enabled to replace unmanaged remote files.
- No passwords or passphrases are stored. Authentication is whatever the system
  `ssh`/`scp` does (agent, key file, keyboard-interactive).
- M6 targets POSIX remotes (Linux/macOS) with `bash`, `zsh` or `sh`. Windows
  OpenSSH/PowerShell remotes are deferred.

`~/.ssh/config` discovery for the wizard parses `Host` and `Include` entries,
skips wildcard and negated patterns, and deduplicates aliases. M6 reads the
default path only; there is no `$CPT_SSH_CONFIG`/`--ssh-config` override. Ports,
users and keys shown in the detail pane come from `ssh -G`, not from a second
config parser, so display matches what OpenSSH will actually use.

### Connect flow

1. Load and validate the target, resolve the referenced bundle manifest and
   executable, and reject offline mode. The bundle's target platform remains the
   authority; the remote bootstrap checks `uname -s`/`uname -m` when possible and
   aborts with a clear mismatch message before running the bundle.
2. Copy the bundle with `scp` to a shell-safe upload path in the remote home.
   The destination root is not touched before the bundle runs, so a first install
   still passes `bundle.validateDest` with a clean root.
3. Start `ssh -tt` with a generated POSIX bootstrap. When a copy is needed it runs
   the uploaded bundle with its normal install/default-init behavior (never
   `--install-init`), verifies `cptinit`, then moves the bundle into
   `$remote_root/incoming/` as the cache and writes a marker. Otherwise it skips
   installation and starts the shell directly.
4. The bootstrap writes a temporary rc file that sources the user's normal rc
   and then `$remote_root/cptinit.sh`: bash uses `--rcfile`, zsh uses a temporary
   `ZDOTDIR`, and POSIX sh uses `ENV`. This loads PATH, environment variables and
   shell functions without touching the real startup file.
5. When the interactive shell exits, the bootstrap removes the temporary rc and,
   only when `cleanup_on_exit` is true, removes `$remote_root` recursively. The
   local `cpt` exits with the remote shell/SSH status.

### Copy cache

The target records `last_copied` and the SHA256 of the copied bundle. On connect,
cpt copies and executes the bundle only when it is missing, older than 24 hours,
the local bundle hash changed, the target's bundle/connection settings changed,
or `cleanup_on_exit` is enabled. If the cached bundle is fresh, only the
interactive shell is started with `cptinit`; the bundle is not re-run.

- Rebuilding a bundle changes its hash, so the next connect copies and executes
  it immediately.
- If the remote bundle or `cptinit` vanished (manual deletion, wiped home), the
  remote bootstrap exits with a dedicated code before installing; cpt re-copies
  once and retries the same connection flow.
- `cleanup_on_exit` deletes `$remote_root`, which contains the cached bundle.
  Cleanup targets therefore ignore the copy cache and always copy and execute.

The bootstrap is generated from validated fields, not concatenated from raw user
input. Remote paths and names are single-quoted, the bootstrap applies
`umask 077`, chmods the uploaded bundle to 0700, and executes a file the user
owns. The remote root is not deleted on cleanup unless it was validated at
save time.

### CLI

```bash
cpt ssh                              # list targets
cpt ssh list [--json]
cpt ssh <target>                     # connect (interactive; terminal inherited)
cpt ssh add <name> --bundle NAME --config-host ALIAS [options]
cpt ssh add <name> --bundle NAME --host HOST [--user U] [--port N]
cpt ssh edit <name> [options]
cpt ssh remove <name>
```

Options mirror the schema: `--config-host`, `--host`, `--user`, `--port`,
`--identity`, `--proxy-jump`, `--remote-root`, `--shell`, `--cleanup`, `--force`.
`list`, `add`, `edit` and `remove` honour `--json`; `ssh <target>` is interactive
and refuses `--json`. `--offline` blocks connecting but still allows local target
management.

### TUI

- Tab order: **Packages · Lists · Bundles · SSH · Log**; `1`–`5` jump directly.
- Left table: `NAME  SSH  BUNDLE  CLEANUP`. Right pane: bundle target/size,
  effective `user@host:port`, identity/proxy source, shell, remote root, cleanup
  and last connection.
- `n` and `e` open a five-step wizard: name → connection source (searchable SSH
  config aliases or custom server) → destination details → bundle picker →
  shell/root/cleanup/force options. Editing pre-fills every value. `d` removes
  the local target; `r` reloads the target file and SSH config aliases.
- Connecting uses `tea.ExecProcess` for the full `cpt ssh` flow, so password
  prompts and the interactive session own the real terminal. The TUI refreshes
  targets and `last_connected` after the process exits.

### Implementation shape

- `internal/sshtarget/`: target schema/validation/store, SSH config discovery,
  argv construction, remote bootstrap rendering.
- `internal/app/ssh.go`: target CRUD, bundle lookup, connect orchestration and
  locking.
- `internal/cli/ssh.go`: `cpt ssh` subcommands and JSON envelopes.
- `internal/tui/`: target table/detail, wizard and `tea.ExecProcess` handoff;
  `snapshotMsg` carries targets.
- `config.Paths` gains `SSHFile` (`<root>/ssh.json`) and `SSHLockFile`
  (`<root>/.ssh.lock`).
- `docs/ssh-targets.md`: schema, examples, security notes and remote behavior.

### Safety and tests

- The remote rc is never edited; a test asserts the generated remote scripts
  never contain `--install-init`, never use an appending redirect, and are
  syntax-checked with `sh -n`. Session files are per-pid and removed on exit.
- Cleanup is opt-in, disabled by default, rejected for dangerous paths, and only
  starts after the interactive shell has exited.
- The copy cache respects bundle SHA256 and the 24-hour window; a fresh cache
  starts only the shell with cptinit. Cleanup targets always copy and execute.
  Tests cover fresh cache, changed bundle, missing remote file retry and cleanup
  interaction.
- Fake `ssh`/`scp` executables in tests capture argv and exercise config vs
  custom targets, identity/port/proxy flags, missing bundles, offline mode,
  cleanup on/off, platform mismatch and shell selection. Both remote scripts are
  rendered, checked against forbidden directives, and syntax-checked with
  `sh -n`; every shell branch (bash, zsh, sh) is executed against a fake bundle,
  and a session status of 240/241/242 is asserted to propagate as a status
  rather than a prepare sentinel.
- TUI model tests cover tab navigation, detail rendering, wizard create/edit,
  prefill, delete confirmation and the connect command construction.

---

## 10. Master list (25 entries, naming verified live)

Every entry carries a full `linux/darwin x amd64/arm64` plus `windows/amd64` map.
The linux/amd64 asset below is what GitHub actually serves today.

| package | repo | linux/amd64 asset | checksum |
|---|---|---|---|
| bat | sharkdp/bat | `bat-{tag}-x86_64-unknown-linux-gnu.tar.gz` | TOFU |
| fzf | junegunn/fzf | `fzf-{version}-linux_amd64.tar.gz` | `fzf_{version}_checksums.txt` |
| micro | micro-editor/micro | `micro-{version}-linux64.tar.gz` | TOFU |
| jq | jqlang/jq | `jq-linux-amd64` (raw) | TOFU |
| ripgrep | BurntSushi/ripgrep | `ripgrep-{version}-x86_64-unknown-linux-musl.tar.gz` | `{asset}.sha256` |
| fd | sharkdp/fd | `fd-v{version}-x86_64-unknown-linux-gnu.tar.gz` | TOFU |
| yq | mikefarah/yq | `yq_linux_amd64.tar.gz` | non-standard `checksums` -> TOFU |
| gh | cli/cli | `gh_{version}_linux_amd64.tar.gz` | `gh_{version}_checksums.txt` |
| k9s | derailed/k9s | `k9s_Linux_amd64.tar.gz` (no version) | `checksums.sha256` |
| lazygit | jesseduffield/lazygit | `lazygit_{version}_linux_x86_64.tar.gz` | `checksums.txt` |
| delta | dandavison/delta | `delta-{version}-x86_64-unknown-linux-gnu.tar.gz` | TOFU |
| zoxide | ajeetdsouza/zoxide | `zoxide-{version}-x86_64-unknown-linux-musl.tar.gz` | TOFU |
| starship | starship/starship | `starship-x86_64-unknown-linux-musl.tar.gz` (no version) | `{asset}.sha256` |
| eza | eza-community/eza | `eza_x86_64-unknown-linux-gnu.tar.gz` (no version) | TOFU |
| dust | bootandy/dust | `dust-v{version}-x86_64-unknown-linux-gnu.tar.gz` | TOFU |
| bottom | ClementTsang/bottom | `bottom_x86_64-unknown-linux-gnu.tar.gz` | TOFU |
| hyperfine | sharkdp/hyperfine | `hyperfine-v{version}-x86_64-unknown-linux-musl.tar.gz` | TOFU |
| sd | chmln/sd | `sd-v{version}-x86_64-unknown-linux-musl.tar.gz` | TOFU |
| choose | theryangeary/choose | `choose-x86_64-unknown-linux-musl` (raw) | TOFU |
| just | casey/just | `just-{version}-x86_64-unknown-linux-musl.tar.gz` | `SHA256SUMS` |
| shfmt | mvdan/sh | `shfmt_v{version}_linux_amd64` (raw) | TOFU |
| shellcheck | koalaman/shellcheck | `shellcheck-v{version}.linux.x86_64.tar.gz` | TOFU |
| direnv | direnv/direnv | `direnv.linux-amd64` (raw) | TOFU |
| yt-dlp | yt-dlp/yt-dlp | `yt-dlp_linux` (raw) | `SHA2-256SUMS` |
| jj | jj-vcs/jj | `jj-v{version}-x86_64-unknown-linux-musl.tar.gz` | TOFU |

Watch the `{tag}` vs `{version}` split per row: Rust tools often keep the `v`, others
do not, jq uses `jq-1.8.2`, just uses `1.59.0`.

The maintainer command `cpt lists lint --network` HEAD-checks every entry across five
targets and runs in a nightly CI job. That is the only realistic defence against
upstream renames.

---

## 11. Source layout

```
cmd/cpt/                 main
internal/cli/            small subcommand router, flag parsing, JSON output
internal/config/         config.json, defaults, --root
internal/list/           schema, parse, validate, merge, conflicts, lint
internal/source/         github probe, url/repo fetch, ETag, retry, cache
internal/resolve/        package -> concrete (tag, version, url, checksums)
internal/fetch/          download, progress, checksum discovery/parsing
internal/archive/        tar.gz/xz/zst/zip/raw, safe extraction, bin selection
internal/install/        atomic flat-bin install, ownership, GC
internal/state/          state.json, locking, migrations
internal/shellinit/      bash/pwsh rendering, rc marker blocks, snippet resolution
internal/app/shell.go    approval state machine and cptinit regeneration
internal/bundle/         footer, build, run/unpack, self-truncate
internal/sshtarget/      saved SSH targets, ssh config discovery, remote bootstrap
internal/app/ssh.go     SSH target CRUD and connect orchestration
internal/tui/            bubbletea model, tabs, views
internal/progress/       shared event bus (TUI and CLI renderers)
cpt.json                 master package list (repo root, by convention)
docs/cpt-json.md         list schema reference
docs/bundle-format.md    trailer + payload spec
docs/ssh-targets.md      SSH target schema and remote execution contract
install.sh               curl | sh installer
```

The TUI and the CLI share one engine through an event bus; neither shells out to the
other.

---

## 12. Milestones

| Phase | Content | Acceptance |
|---|---|---|
| **v0.1 M0** | skeleton, config/state/locking/logging, CLI router, HTTP layer | `cpt version`, `cpt doctor` on an empty home |
| **v0.1 M1** | lists, probe, resolve, download, checksums, archives, install/update/remove, master list | `cpt install fzf jq bat` on linux/amd64; re-run is a no-op; a bad hash fails loudly |
| **v0.1 M2** | cptinit, shell packages (inline + URL), `init --install`, snippet approval | a new shell gets the functions; marker block is idempotent |
| **v0.2 M3** | TUI packages/lists/log, progress, search, multi-select | install and update purely from the TUI; non-TTY fallback works |
| **v0.3 M4** | bundle footer, build, run, frozen/live, cross-target base fetch, bundles tab | build arm64 bundle on amd64, run it in a temp HOME, re-run is idempotent |
| **v0.4 M5** | Windows (zip, `.exe`, PowerShell init, file locking), darwin signing verdict, GC/doctor polish | CI green on windows/amd64 and darwin/arm64 |
| **v0.4.1 M5.1 (done)** | TUI bundle/UI polish: two-column Bundles tab with included-package detail; `n`/`e` share a four-step wizard; selectable target platform and searchable package checklist; edit pre-fills every current value; one bracketed/yellow footer shortcut line; `[tab] switch tabs` header hint; untruncated prompt labels | create and edit bundles without typing platforms or package lists; selected bundle shows its included packages; no duplicate bundle shortcuts; `go test ./...` passes |
| **v0.5 M6 (done)** | saved SSH targets: `ssh.json`, SSH config discovery, POSIX remote bootstrap, configurable remote root, `cpt ssh <target>`, daily bundle copy cache with fresh-cache shell-only sessions, session-scoped cptinit, opt-in remote-root cleanup, SSH tab + five-step wizard, full `list/add/edit/remove` CLI | create a target from `~/.ssh/config` or a custom host/user; `cpt ssh name` copies/executes only when needed and otherwise opens the shell with cptinit loaded; no remote rc file is modified; cleanup removes the remote root only when enabled; `go test ./...` passes |
| **v0.6 M7** | goreleaser, install.sh, self-update, release docs, nightly list lint | tagged release installable via one-liner |

---

## 13. Testing

- **Unit:** template interpolation (`{tag}`/`{version}` edge cases), checksum parsers
  built from the five real formats above, safe extraction (zip-slip, absolute paths,
  symlink escape, oversized entries), footer round-trip, state migration.
- **Fixture server:** `httptest` reproducing `github.com` paths — 301 repo rename
  (micro), 302 latest to tag, version-less assets, missing checksums, 429 with
  `Retry-After`.
- **Golden bundle:** build for the host, execute in a temp `HOME`, assert tree, state
  and cptinit, execute again for idempotency.
- **Network suite** (`-tags network`): the 25 entries across 5 targets, HEAD only.
- **TUI:** model unit tests plus one scripted teatest session for search, select and
  install.
- **SSH targets:** target validation and config parsing (including `Include`),
  argv construction for config/custom targets, executed bash/zsh/sh prepare and
  shell phases, fake `ssh`/`scp` end-to-end connect, session-status propagation,
  no-`--install-init` assertion, cleanup path guards and wizard create/edit
  tests.
- **CI:** Linux/macOS/Windows test matrix, `goreleaser` on tags with checksums,
  nightly list lint.

---

## 14. Risks, ranked

1. **Repository visibility — resolved.** The repo is public and returns 200
   unauthenticated. Its default branch is `master`, so the master list is fetched from
   `https://raw.githubusercontent.com/SuSonicTH/cpt/master/cpt.json`. That file does
   not exist yet; it ships in M1.
2. **No API means hand-maintained asset names.** Upstreams rename constantly
   (`lazygit_..._Linux_...` -> `..._linux_...`, micro changed orgs, yq checksums are
   non-standard). Mitigations: nightly lint, resolve errors that print the exact 404
   URL, `latest/download` fallback, and a diff shown before accepting list updates.
3. **macOS arm64 plus appended payload.** Appending to an ad-hoc-signed Mach-O may
   invalidate the signature on Apple Silicon. Test this first in M4. Mitigation: run
   `codesign --force --sign -` after appending when the builder runs on macOS; for
   cross-built bundles document the one-liner, or fall back to a
   `.tar.zst` + `install.sh` artifact for darwin only. Linux and Windows appending is
   unproblematic (PE ignores trailing data; Windows may flag MOTW/SmartScreen).
4. **Windows specifics:** no symlinks (fine, flat copies), rename over a running exe
   (`.new.exe` swap), zip entries lacking exec bits (detect by `.exe`), MOTW.
5. **TOFU is weak on the first download** (TLS and list trust only). Acceptable for
   v1; minisign/cosign for lists and bundles is the natural v0.6.
6. **Lists are remote code** through shell snippets. Mitigations: approval flow,
   diff, and an `allow_shell_packages: false` kill switch.
7. **Flat bin means no rollback and possible cross-package binary collisions.**
   Mitigations: explicit ownership map, hard conflict error, `pkg@version` reinstall.
8. **`releases/latest` excludes prereleases and tag-only repositories.** Clear error,
   manual pin, `type: url`.
9. **GitHub HTML endpoints are not a contract.** The probe is isolated behind one
   interface, with `--offline` and a cached-tag fallback.
10. **Remote shell startup differs by shell.** Session-scoped init uses a
   temporary rc (`--rcfile`, `ZDOTDIR`, `ENV`) rather than editing the real rc.
   Unknown shells fail loudly instead of opening a session without cptinit.
11. **Cleanup can delete remote data.** It is opt-in, off by default, guarded
   against `/`, top-level directories and `$HOME`, and only runs after the
   interactive shell exits.
12. **Bundle target and remote host can disagree.** The bundle's own platform
   check remains authoritative; the bootstrap probes `uname` and fails before
   installation with a clear os/arch mismatch, so the user rebuilds the bundle
   for the remote platform.

---

## 15. Open, non-blocking

- fish shell support (schema reserves it)
- PowerShell shell packages
- Prerelease opt-in per package
- Signing lists and bundles
- A bundle size warning threshold
- Whether `cpt` itself belongs in the master list
