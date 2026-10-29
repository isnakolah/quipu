# Source install and recovery

`quipu` is source-first. No release binary or marketplace package is a supported install path before Quipu v0 Gate passes.

## Requirements

- macOS or Linux host;
- Go 1.26.4;
- GitHub CLI authenticated to target owner/repository;
- an active `gh auth login` credential for local commands;
- a dedicated fine-grained `QUIPU_WRITER_TOKEN` configured as a repository Actions secret. Grant only target-repository Contents, Issues, Pull requests, and Actions access, plus the owning user Project read/write authority required by the installation. Never place its value in config, cache, comments, or logs.

## Install

```sh
gh repo clone isnakolah/quipu
cd quipu
GOBIN="$HOME/.local/bin" go install ./cmd/quipu
export PATH="$HOME/.local/bin:$PATH"
quipu doctor --json
quipu context --json
```

`quipu doctor` reports local config and whether `GITHUB_TOKEN` or active `gh` authentication is available. It does not print or persist token values, and does not prove Writer authority, hook trust, target-host behavior, or provider state.

## Plugin source install

Install local Codex and Claude plugin entries from checked-out `plugins/` source. Review every hook script before trust. Trust by reviewed hash in each client; installation must never bypass hook trust.

Before editing work, claim an accepted leaf and export its context:

```sh
export QUIPU_LEASE_ISSUE=123
export QUIPU_LEASE_HOLDER='provider/profile/session'
export QUIPU_LEASE_BRANCH='123/short-description'
```

Hooks block file writes and state-changing shell commands unless all three values match live Quipu state; safe discovery and test commands remain allowed. `QUIPU_BOOTSTRAP_ADMIN=1` permits only documented repository bootstrap administration; remove it immediately after that operation.

## Upgrade and recovery

Pull source, rerun `go install`, then `quipu doctor`. Recreate local cache freely: cache is fetched metadata only and GitHub remains authority. If a request needs correction, create a new request comment; do not edit prior request or receipt comments.

## Proof status

Local unit, vet, template, and plugin-hook checks exist. The `Source install proof` workflow runs clean source installs on hosted macOS and Linux runners. Fresh trusted-plugin behavior and disposable-repository Writer proof remain release-Gate requirements.
