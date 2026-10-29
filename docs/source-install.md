# Source install

Quipu installs from source. No release binary or marketplace package is supported before the v0 Gate passes.

## Requirements

- macOS or Linux
- Go 1.26.4
- GitHub CLI, logged in with `gh auth login` (local commands use that credential in memory and never store it)
- For the repository owner: the `QUIPU_WRITER_TOKEN` Actions secret ([owner guide](owner-guide.md#setup-checklist))

## Install

```sh
gh repo clone isnakolah/quipu
cd quipu
GOBIN="$HOME/.local/bin" go install ./cmd/quipu
export PATH="$HOME/.local/bin:$PATH"   # add to your shell profile
quipu doctor
```

`quipu` must be on the `PATH` that your agent tools see, because their hooks call it. If it is missing, the hooks deny every guarded tool call rather than let it through.

## Agent hooks and plugins

Every Quipu repository commits two hook files that run `quipu guard` before each agent tool call:

- `.claude/settings.json`: Claude Code, all tools
- `.codex/hooks.json`: Codex, shell, patch, and MCP tools

`quipu hooks --apply` writes or repairs them, keeping any other settings in those files. Claude Code and Codex run project hooks only after a person approves them, once per machine. Review the hook command before you approve it; never bypass the approval.

The plugins in `plugins/claude` and `plugins/codex` add the same guard plus a session hook that tells the agent what its worktree holds and what to run next. Install them from this checkout, then approve their hooks the same way.

There is nothing to export. The lease lives in each worktree's git directory, written by `quipu start`; the agent identity is created once per worktree. Unset any old `QUIPU_LEASE_*` or `QUIPU_BOOTSTRAP_ADMIN` variables: `quipu doctor` fails while they are set.

## Upgrade

```sh
cd quipu && git pull
GOBIN="$HOME/.local/bin" go install ./cmd/quipu
quipu doctor
```

Reinstall or update the plugins after an upgrade that changes `plugins/`, and approve the changed hooks.

## Recovery

GitHub is the only record, so local state is disposable:

- The metadata cache can be deleted at any time.
- A request that needs correcting gets a new request; never edit request or receipt comments (edited requests are rejected).
- A worktree's lease file is rewritten by `quipu start N`.

See the [owner guide](owner-guide.md#recovery) for stuck work.

## Proof status

Unit, vet, plugin-hook, and guide-sync checks run in CI. The `Source install proof` workflow installs from source on hosted macOS and Linux runners. Trusted-plugin behaviour and the disposable-repository Writer proof (`scripts/live-proof.sh`) remain release-Gate requirements.
