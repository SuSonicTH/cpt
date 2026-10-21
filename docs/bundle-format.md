# Bundle format

A bundle is a normal `cpt` executable with a compressed payload appended. ELF,
Mach-O and PE loaders ignore trailing bytes, so the result stays runnable on the
target machine.

```
[ cpt base binary ][ zstd-compressed tar payload ][ 64-byte footer ]
```

## Footer

All integers are little endian.

| Offset | Size | Field |
|---|---|---|
| 0 | 8 | magic `CPTBDL01` |
| 8 | 8 | payload offset (uint64) |
| 16 | 8 | payload length (uint64) |
| 24 | 32 | SHA256 of the compressed payload |
| 56 | 4 | CRC32C (Castagnoli) of the compressed payload |
| 60 | 2 | format version (`1`) |
| 62 | 2 | flags (bit 0: frozen) |

`payload offset + payload length + 64` must equal the file size. cpt verifies
both hashes before extracting anything. Unknown format versions are rejected.

## Payload

The payload is a zstd-compressed tar archive with deterministic entries:

```
bundle.json
lists/<name>.json
bin/<binary>
cptinit.sh
cptinit.ps1
```

`bundle.json` follows schema 1:

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
      "list": "core",
      "tag": "v0.74.5",
      "version": "0.74.5",
      "asset": "fzf-0.74.5-linux_arm64.tar.gz",
      "download_sha256": "...",
      "bins": ["fzf"],
      "checksum_status": "verified",
      "shell": {
        "version": "0.74.5",
        "bash": "export FZF_DEFAULT_OPTS='--height 40%'",
        "sha256": "..."
      }
    }
  ],
  "lists": {
    "core": {
      "sha256": "...",
      "origin": "https://raw.githubusercontent.com/SuSonicTH/cpt/master/cpt.json",
      "packages": 25
    }
  },
  "output": "devbox-linux-arm64"
}
```

`list`, `shell`, `origin`, `packages` and `output` are optional extensions used
to rebuild the same environment. `shell` carries snippets that were already
approved on the build machine; the target still honours
`allow_shell_packages: false`. `output` records the actual executable name
(relative to `bundles/` where possible) so that `rebuild` and `remove` keep
pointing at the same file.

`cpt_version` records the builder's version. When `--base PATH` supplies a
different cpt binary, it cannot describe that base.

The `lists/` snapshot beside the manifest (`bundles/<name>.d/`) is used by
`rebuild`, so rebuilding does not depend on the local lists or shell approval
state being unchanged. The `cptinit.*` files in the payload are informational:
`bundle run` regenerates them from the merged state for the actual destination.

## Build

`cpt bundle create <name>` resolves every selected package for `--target`, not
the host. When the target differs from the host it downloads the matching cpt
release into `cache/self/`; pass `--base PATH` to supply a base binary
explicitly. Nothing is installed into the local `bin` directory.

`cpt bundle rebuild <name>` reads `bundles/<name>.json` and the saved list
snapshot, resolves the exact saved tags, and verifies downloads against
`download_sha256` from the manifest before writing the new bundle. A changed
upstream hash is fatal unless `--force-checksum` is passed. Cross-target base
binaries are checked against the release checksum file when one is published.

Building the same name for a second target retires the previous executable, so
one manifest always describes one runnable file.

On macOS, appending a payload invalidates an existing Mach-O signature. A bundle
built on a darwin host is ad-hoc signed after assembly (`codesign --force
--sign -`) when the tool is available. Cross-built darwin bundles are emitted
unsigned with a warning; sign them on macOS before distributing.

## Run

On startup cpt stats its own executable, reads the last 64 bytes, and enters
bundle mode when the footer is valid. It verifies the payload, checks that the
target matches the host, and then:

1. extracts into a staging directory under `<dest>/tmp`,
2. installs binaries through the normal flat-bin manager (`origin: "bundle"`),
3. writes the bundled lists (`readonly: true` when frozen),
4. writes the clean base binary to `<dest>/bin/cpt[.exe]` by copying only the
   bytes before the payload offset,
5. merges shell snippet approval state and generates `cptinit.*`.

Running the same bundle again is idempotent: packages whose version, download
hash and installed binary hashes still match are reported as `unchanged`.

Frozen bundles make `cpt update` explain itself until `cpt update --unfreeze`
clears the read-only flag on the affected lists.
