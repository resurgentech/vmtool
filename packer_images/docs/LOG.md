# Work log (packer_images)

Append-only history of what shipped from `packer_images/ROADMAP.md`.
FOREMAN appends an entry per landed step or tranche. Newest at the
bottom. Corrections are appended, never edited in; a hook refuses
rewrites.

```
## YYYY-MM-DD — <step id and title>

- What shipped
- Commits: <shas>
```

---

## 2026-11-14 — T1 Local orch scaffolding + `validate` / `check` recipes

- `.gitignore`: `/ROADMAP.md`, `/docs/reports/`, `/tmp/` ignored.
- `docs/LOG.md` (this file) created.
- `justfile`: `validate <template>` (render user-data, `packer init`,
  `packer validate` with a non-existent `output_directory`; no build,
  no sudo) and `check` (`bash -n run` + `validate` on every dir in
  `qemu_images/`, fails if any fails).
- Commits: the commit that adds this entry ("packer_images T1: ...").
