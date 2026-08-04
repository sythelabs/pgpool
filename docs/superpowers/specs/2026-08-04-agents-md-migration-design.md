# AGENTS.md Migration Design

**Date:** 2026-08-04

## Goal

Make `AGENTS.md` the canonical agent-instruction file for pgpool itself and for projects initialized by `pgpoolcli`, while preserving compatibility with Claude Code and safely migrating integration blocks written by older CLI releases.

## Scope

This change covers:

1. Moving pgpool's root project guide from `CLAUDE.md` to `AGENTS.md`.
2. Replacing the root `CLAUDE.md` with the single directive `@AGENTS.md`.
3. Changing `pgpoolcli init` to add or update its integration block in `AGENTS.md` by default.
4. Migrating an integration block previously written to `CLAUDE.md` into `AGENTS.md` and removing the old copy.
5. Updating CLI messages, help text, embedded reference text, tests, and current user-facing README documentation.

Historical design and implementation documents remain historical records and will not be rewritten solely to replace old `CLAUDE.md` references.

## Non-goals

- Adding a flag to select a different agent-instruction filename.
- Writing the integration block to both `AGENTS.md` and `CLAUDE.md`.
- Creating `CLAUDE.md` with `@AGENTS.md` in consumer projects. That pointer convention is applied only to the pgpool repository.
- Changing the contents or version of the pgpool integration block when no documentation content has changed.
- Splitting `cmd/pgpoolcli/pgpoolcli.go` into additional packages or adding dependencies.

## Repository Documentation

The existing root `CLAUDE.md` content becomes the new root `AGENTS.md` without semantic changes. Root `CLAUDE.md` becomes one line:

```text
@AGENTS.md
```

The file retains a conventional trailing newline. README references to the canonical project guide and the CLI integration destination change to `AGENTS.md`.

## CLI Naming

Implementation names tied to the old destination will be renamed to describe agent instructions rather than Claude specifically. Examples include:

- `claudeSegment` to `agentSegment`
- marker constants with `claude` prefixes to `agent` prefixes
- `claudeMergeAction` to `agentMergeAction`
- `mergeClaudeBlock` to `mergeAgentBlock`

The marker text remains `PGPOOL INTEGRATION`; existing versioned blocks therefore remain discoverable without changing the embedded block version.

## `pgpoolcli init` Behavior

### Fresh project

If neither file contains a pgpool integration block, `init` creates `AGENTS.md` when absent or appends the block when the file already contains unrelated instructions. It does not create or modify `CLAUDE.md` unless that file contains an old pgpool integration block that must be removed.

### Current `AGENTS.md` block

If `AGENTS.md` already contains the current integration block, the block is left byte-for-byte unchanged. Migration cleanup still runs against `CLAUDE.md`; the command must not return early before checking the legacy file.

### Older `AGENTS.md` block

Any version of the integration block in `AGENTS.md` is replaced in place with the current block. Content before and after the marked block is preserved.

### Legacy `CLAUDE.md` block

If `CLAUDE.md` contains an integration block and `AGENTS.md` does not, the current block is added to `AGENTS.md`, then the marked block is removed from `CLAUDE.md`.

If both files contain integration blocks, `AGENTS.md` is made current and the `CLAUDE.md` copy is removed. This converges on exactly one managed block across the two files.

All bytes outside the marked block in `CLAUDE.md` are preserved. If removing the block leaves only whitespace, the empty `CLAUDE.md` file is deleted. Otherwise, the remaining content is written back without inventing an `@AGENTS.md` pointer.

### Idempotency

After a successful run:

- `AGENTS.md` contains exactly one current pgpool integration block.
- `CLAUDE.md` contains no pgpool integration block.
- A subsequent `init` run makes no file changes.

## Validation and Write Ordering

`init` reads and validates both files before writing either one. A begin marker without a following end marker in either file is an error naming the affected path. No write occurs after such a validation error.

Once both transformations are valid, writes happen in this order:

1. Create or update `AGENTS.md`.
2. Rewrite or remove `CLAUDE.md` only when legacy cleanup is needed.

This ordering prioritizes preservation of the integration instructions. If legacy cleanup fails after `AGENTS.md` is written, the command returns an error but leaves a harmless duplicate rather than losing the block.

File-operation errors include the affected path and operation. Existing file permissions behavior remains unchanged: newly created files use mode `0644`.

## Operator Output and Documentation

All active CLI surfaces describe `AGENTS.md` as the destination:

- `init` success and no-op messages
- command help
- `pgpoolcli prime`
- README setup and integration sections

Migration output identifies both actions when applicable: the block was added or updated in `AGENTS.md`, and the legacy copy was removed from `CLAUDE.md` (or the now-empty file was removed).

## Testing

Tests will drive the implementation and cover:

1. Creating `AGENTS.md` when absent.
2. Appending to an existing `AGENTS.md` without clobbering unrelated content.
3. Replacing an older block in `AGENTS.md`.
4. Leaving the current `AGENTS.md` block unchanged.
5. Migrating a legacy block from `CLAUDE.md` into a missing or existing `AGENTS.md`.
6. Removing a duplicate legacy block when both files contain blocks.
7. Preserving unrelated `CLAUDE.md` content during migration.
8. Removing `CLAUDE.md` when migration leaves only whitespace.
9. Rejecting malformed markers in either file before any write.
10. Producing no changes on a second run.
11. Reporting `AGENTS.md` in help, prime text, and operator messages.

The full Go test suite and formatting checks will run before completion.
