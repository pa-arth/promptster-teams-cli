# .agents — shared agent tooling

`skills/` holds the real content. `.claude/skills/`, `.cursor/skills/` and
`.codex/skills/` are symlinks into it, so Claude Code, Cursor and Codex all read
one source of truth and a fix lands in all three at once.

`.claude/` is gitignored in this repo (it also holds worktrees), so the Claude
symlink is tracked via `git add -f`. Re-run that if you recreate it.

Verified: `verify-teams-cli` was driven end to end through the Claude Code
symlink — a fresh agent session found the skill, built the binary, drove the
sandboxed CLI and returned a correct verdict on a planted defect. The Cursor and
Codex paths are their documented project-skill locations but were not exercised
here.

Add a skill: put it in `.agents/skills/<name>/`, then

```bash
for t in claude cursor codex; do
  ln -sfn ../../.agents/skills/<name> .$t/skills/<name>
done
```
