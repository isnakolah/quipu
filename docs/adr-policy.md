# ADR policy

Use Decision Issue for question, options, consequences, and required approver. New Epic needs explicit owner approval recorded in GitHub. Agents can propose but cannot create or approve an Epic. Do not use GitHub Milestones or local planning ledgers.

## Guard modes

The guard's default is an allow-list: a command it cannot prove harmless counts as a change and needs a lease. On Calla this refused 226 commands in a week, about a third of them harmless reads, and its "rewrite it" advice collided with the rule against retrying a denial in another form. `guard: deny-list` reverses the default for repositories that choose it, and `quipu adopt` chooses it for new ones: only force pushes, pushes to the default branch, merges, approvals and closes outside Quipu, another agent's live work, Quipu's own files, declared `guard_deny` commands, and lease-less changes to a governed worktree's files are refused. Existing repositories keep the allow-list until their owner sets the flag, because it trades away a guarantee they may rely on. Protected files, `policy` and evidence rules are the same in both modes.
