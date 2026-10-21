package cli

const usage = `cpt — Completely Portable Tools

Usage:
  cpt [global flags] <command> [arguments]

Commands:
  list                     List available and installed packages
  search <query>           Search packages by name, description or tag
  info <package>           Show details for one package
  install <pkg[@version]>  Install one or more packages
  update [package...]      Update installed packages (all when omitted)
  remove <package...>      Remove packages and their binaries
  pin <pkg>@<version>      Pin an installed package to a version
  unpin <package>          Remove a version pin
  verify [package...]      Re-hash installed binaries
  doctor                   Diagnose the installation
  lists [subcommand]       Manage package lists (add, remove, refresh, show, lint)
  version                  Print version information
  help                     Show this help

Global flags:
  --root DIR    Use DIR instead of ~/.cpt
  --json        Emit machine readable JSON
  --offline     Do not use the network
  --yes, -y     Assume yes
  --verbose     Enable debug logging
  --quiet       Suppress informational output
  --no-color    Disable colour output

Package lists are fetched from cpt.json in a repository root, from a raw
https URL, or from an already downloaded file. The master list ships inside
this binary and refreshes from the cpt repository when it is stale.

Examples:
  cpt install fzf ripgrep jq
  cpt install fzf@0.74.5
  cpt update --dry-run
  cpt lists add github.com/acme/tools#main:ci/cpt.json
  cpt lists add https://example.com/cpt.json --name private
  cpt verify
`
