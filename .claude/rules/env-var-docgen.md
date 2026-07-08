---
paths:
  - "internal/app/**"
  - "internal/cli/**"
  - "internal/config/**"
  - "internal/dial/**"
  - "internal/update/**"
  - "pkg/ibkr/**"
  - "scripts/docgen/config-ref/**"
---

# Adding or removing IBKR_* env vars

Every read of an `IBKR_*` environment variable must be flagged with a `// docgen:env NAME | description` comment next to the `os.Getenv` call. `scripts/docgen/config-ref` walks the tree for these and emits `docs/reference/config.md`; `make check` fails when the generated file and source disagree. New env var → add the read, add the comment, run `make docs-regen`, commit all three together.
