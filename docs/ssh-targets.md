# SSH targets

An SSH target pairs one saved bundle with one SSH destination. It lets a local
`cpt` copy the bundle to a remote machine, install or refresh `~/.cpt` there, and
open an interactive shell with the generated `cptinit` loaded for that session.

The remote shell startup files are **never** edited. There is no marker block in
`.bashrc`, `.zshrc`, `.profile` or any other file, and `--install-init` is never
passed to the remote bundle.

M6 supports POSIX remotes (Linux and macOS) using `bash`, `zsh` or `sh`. Windows
OpenSSH remotes are not supported yet. The local machine needs the standard
`ssh` and `scp` executables on `PATH`.

## CLI

```sh
cpt ssh                                      # list targets
cpt ssh list [--json]
cpt ssh staging                              # copy/install when needed, then connect
cpt ssh connect staging
cpt ssh add staging --bundle devbox --config-host staging-box
cpt ssh add lab --bundle devbox --host 10.0.0.7 --user mike --port 2222
cpt ssh edit staging --shell zsh --cleanup
cpt ssh edit staging --host other-box --no-cleanup
cpt ssh remove staging
```

`cpt ssh <target>` is interactive. It refuses `--json`; the management
subcommands emit the normal JSON envelope.

Target flags:

| Flag | Meaning |
|---|---|
| `--bundle NAME` | Existing bundle manifest and valid executable under `~/.cpt/bundles` |
| `--config-host ALIAS` | Use an alias from `~/.ssh/config` |
| `--host HOST` | Explicit hostname or IP (custom source) |
| `--user USER` | SSH user override |
| `--port PORT` | SSH port |
| `--identity PATH` | Identity file |
| `--proxy-jump SPEC` | ProxyJump destination |
| `--remote-root PATH` | Absolute remote cpt root; default `~/.cpt` |
| `--shell SHELL` | `auto`, `bash`, `zsh` or `sh` |
| `--cleanup` / `--no-cleanup` | Remove the remote root after the session |
| `--force` / `--no-force` | Pass `--force` to the remote bundle |

## Storage

Targets live in `~/.cpt/ssh.json` (schema 1, mode 0600 on POSIX):

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
      "remote_root": "/srv/cpt",
      "shell": "auto",
      "cleanup_on_exit": false,
      "force": false,
      "created": "2026-10-25T10:00:00Z",
      "updated": "2026-10-25T10:00:00Z",
      "last_connected": "2026-10-25T10:05:00Z",
      "last_copied": "2026-10-25T10:05:00Z",
      "copied_sha256": "...",
      "copied_key": "..."
    }
  }
}
```

- `source: "config"`: `host` is an OpenSSH config alias. `ssh` and `scp` receive
  the alias, so `HostName`, `User`, `Port`, `IdentityFile`, `ProxyJump`, `Match`
  and `Include` keep their normal semantics. Non-empty override fields are passed
  as explicit flags.
- `source: "custom"`: `host` is a hostname or IP and the other fields are passed
  explicitly. This does not modify `~/.ssh/config`.
- `remote_root` empty means remote `~/.cpt`. A custom value must be an absolute
  POSIX path without `..`. Cleanup additionally requires a nested path such as
  `/srv/cpt`, not a top-level directory such as `/tmp`.
- No password or passphrase is stored. Authentication is handled by the system
  `ssh`/`scp` (agent, key file, keyboard-interactive).

The TUI discovers concrete `Host` aliases from `~/.ssh/config`, including
`Include` files. Wildcard and negated patterns are skipped. Effective values for
the detail pane come from `ssh -G`.

## Connect flow

1. Resolve the target and its bundle locally.
2. If the copy cache is stale, upload the bundle with `scp` to
   `$HOME/.cpt-ssh-upload-<target>.bundle`.
3. Start `ssh -tt` with a generated POSIX bootstrap.
4. In install mode, the bootstrap runs the uploaded bundle directly against the
   remote root, so a first install still sees a clean destination:

   ```sh
   <uploaded-bundle> --dest <remote_root>
   ```

   After the install succeeds and `cptinit` exists, it creates the remote root's
   `incoming/` directory, moves the uploaded bundle there as the cache, and
   writes a cache marker. It never passes `--install-init`.
5. The bootstrap starts the requested shell with a temporary rc that sources the
   user's normal rc and then `<remote_root>/cptinit.sh`:
   - bash: `--rcfile`
   - zsh: temporary `ZDOTDIR`
   - sh/dash/ash/ksh: `ENV`
6. When the shell exits, the temporary rc is removed. The remote root is removed
   only when `cleanup_on_exit` is enabled.

The remote bootstrap probes `uname -s` and `uname -m` and aborts before
installation if the host does not match the bundle's target platform. The bundle
itself also performs its own platform check.

## Copy cache

The target stores `last_copied`, the bundle SHA256 and a key derived from the
bundle and connection settings.

- A fresh copy is reused for 24 hours unless `cleanup_on_exit` is enabled.
- A fresh copy starts **only the interactive shell** with `cptinit`; the bundle
  is not run again.
- Rebuilding the bundle changes its hash, so the next connect copies and runs it.
- Editing the bundle reference or connection settings invalidates the cache.
- A missing remote bundle, missing `cptinit`, or missing cache marker makes the
  remote bootstrap return a dedicated status. `cpt` then uploads and installs
  once and retries.
- Targets with `cleanup_on_exit` always copy and install, because cleanup removes
  the cached bundle.

## Cleanup

Cleanup is off by default. When enabled it runs after the interactive shell has
exited and refuses to remove:

- `/`
- top-level directories such as `/tmp`, `/var` or `/home`
- the remote `$HOME` itself
- a path that is a parent of `$HOME`

It removes the entire configured remote root, including the installed cpt
environment and cached bundle. The local target and local bundles are untouched.

## Security notes

- Remote command values are validated and single-quoted; target fields are not
  interpolated into a shell command unquoted.
- The bootstrap applies `umask 077` and chmods the uploaded bundle to `0700`
  before executing it from the user's own home/root.
- `cptinit` is loaded only for the current session. No remote startup file is
  modified.
- The local SSH target file contains no secrets.
