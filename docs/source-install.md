# Source install

Quipu installs from source. No release binary or marketplace package is supported before the v0 Gate passes.

## Requirements

- macOS or Linux
- Go 1.26.4
- GitHub CLI, logged in with `gh auth login` (local commands use that credential in memory and never store it)
- For the repository owner: the `QUIPU_WRITER_TOKEN` Actions secret, a classic PAT with `project` plus `public_repo` (public repository) or `repo` (private repository) ([owner guide](owner-guide.md#setup-checklist))

## Install

```sh
gh repo clone isnakolah/quipu
cd quipu
GOBIN="$HOME/.local/bin" go install ./cmd/quipu
export PATH="$HOME/.local/bin:$PATH"   # add to your shell profile
quipu doctor
```

Or install a tagged version directly: `GOBIN="$HOME/.local/bin" go install github.com/isnakolah/quipu/cmd/quipu@v0.2.0`. `quipu version` prints the tag, or the commit for a source build; `init` and `adopt` pin the generated workflows to it.

`quipu` must be on the `PATH` that your agent tools see, because their hooks call it. If it is missing, the hooks deny every guarded tool call rather than let it through.

## Agent hooks and plugins

Every Quipu repository commits two hook files that run `quipu guard` before each agent tool call:

- `.claude/settings.json`: Claude Code, all tools
- `.codex/hooks.json`: Codex, shell, patch, and MCP tools

`quipu hooks --apply` writes or repairs them, keeping any other settings in those files. Claude Code and Codex run project hooks only after a person approves them, once per machine. Review the hook command before you approve it; never bypass the approval.

The plugins add the same guard plus a session hook that tells the agent what its worktree holds and what to run next. Install them from this repository's marketplace:

```sh
# Claude Code
claude plugin marketplace add isnakolah/quipu
claude plugin install quipu@quipu

# Codex (append @v0.2.0 to the source to pin a tag)
codex plugin marketplace add isnakolah/quipu
codex plugin add quipu@quipu
```

Then approve their hooks the same way. After an upgrade, refresh them with `claude plugin marketplace update quipu && claude plugin update quipu@quipu`, and `codex plugin marketplace upgrade quipu && codex plugin add quipu@quipu`.

There is nothing to export. The lease lives in each worktree's git directory, written by `quipu start`; the agent identity is created once per worktree. Unset any old `QUIPU_LEASE_*` or `QUIPU_BOOTSTRAP_ADMIN` variables: `quipu doctor` fails while they are set.

## Upgrade

```sh
cd quipu && git pull
GOBIN="$HOME/.local/bin" go install ./cmd/quipu
quipu doctor
```

Update the plugins after an upgrade that changes `plugins/` (commands above), and approve the changed hooks.

## Self-hosted runners

A repository whose workflows run on self-hosted runners (`workflows.runs_on` in the config) needs, on each runner: `git`, and either Go 1.26.4 or `quipu` on the `PATH` at the pinned `workflows.quipu_version`. With `quipu` present, the workflows skip installing Go and Quipu. Register the runner under the repository's **Settings → Actions → Runners** with the labels the config lists. See the [owner guide](owner-guide.md#runners-and-workflows).

## Recovery

GitHub is the only record, so local state is disposable:

- The metadata cache can be deleted at any time.
- A request that needs correcting gets a new request; never edit request or receipt comments (edited requests are rejected).
- A worktree's lease file is rewritten by `quipu start N`.

See the [owner guide](owner-guide.md#recovery) for stuck work.

## Proof status

Unit, vet, plugin-hook, and guide-sync checks run in CI. The `Source install proof` workflow installs from source on hosted macOS and Linux runners. Trusted-plugin behaviour and the disposable-repository Writer proof (`scripts/live-proof.sh`) remain release-Gate requirements.
