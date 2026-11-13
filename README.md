# SQL Review for DBX

A static SQL review workbench: hand it a statement or an entire migration
directory, and get back **per-finding evidence** (line number, snippet,
suggested fix) plus a **quality profile**.

**It never connects to a database and never executes the SQL you review.**

[中文](README.zh-CN.md) · English

## Why

There is usually a manual step between writing SQL and approving it: a reviewer
reads the script and points out "this locks the table", "this scans everything",
"this breaks on replay". That step has three problems:

- **It is not reproducible** — change the reviewer and the verdict changes;
- **It happens too late** — problems surface when the test environment fails;
- **It does not scale** — nobody reads all 181 migration scripts every time.

This plugin turns that step into a set of toggleable rules: the same input
always yields the same verdict, and directory-wide batch review means nobody
has to read every script by hand.

## Core guarantees

| Guarantee | How |
|---|---|
| Never connects to a database | The backend has no `database/sql` import, no driver, and no connection fields anywhere |
| Never executes reviewed SQL | Lexical analysis only (tokenize + rule matching); there is no code path that executes a statement |
| No network access | The sidecar makes no outbound request and requests no host permission |
| No credentials | No host, account, or password is stored — the config holds rule switches and thresholds only |
| Reproducible builds | The SDK ships inside the repository, so the same binary builds with `GOPROXY=off` |

## Rule set (61 rules)

| Category | Count | Examples |
|---|--:|---|
| Destructive | 12 | `SQL001` DROP table/database · `SQL003` UPDATE/DELETE without WHERE · `SQL073` dynamic EXEC · `SQL087` CASCADE |
| Migration hygiene | 17 | `SQL010` bad file naming · `SQL014` too many statements per file · `SQL070` unguarded SQL Server DDL · `SQL080` unguarded PostgreSQL DDL |
| Performance | 12 | `SQL022` `SELECT *` · `SQL023` unfiltered full scan · `SQL071` NOLOCK · `SQL081` index without CONCURRENTLY |
| Schema quality | 16 | `SQL030` table without primary key · `SQL032` float for money · `SQL075` non-Unicode types · `SQL084` SERIAL · `SQL085` JSON instead of JSONB |
| Sensitive data | 1 | `SQL040` sensitive column names (password/token/id card/phone/bank card…) |
| Directory consistency | 3 | `SQL050` duplicate version · `SQL051` version gap · `SQL052` identical content under different versions |

### Convention rules are off by default

`SQL012` (idempotent DDL guard), `SQL018` (requires `USE`), `SQL033` (requires
column `COMMENT`), `SQL034` (requires explicit `ENGINE`), `SQL070` (SQL Server
idempotent DDL guard) and `SQL080` (PostgreSQL idempotent DDL guard) are **team
conventions**, not general SQL quality problems. A general-purpose plugin should not
impose one repository's house rules on another, so they ship disabled — enable the
ones your repository actually follows on the Rules tab.

### Dialects

MySQL, SQLite, SQL Server/T-SQL and PostgreSQL are supported, with rules routed per
dialect. SQL Server support recognizes `GO`, bracketed identifiers, temporary tables,
`TOP`, `IDENTITY` and named constraints (`SQL070`–`SQL077`). PostgreSQL support
recognizes dollar-quoted function bodies, `::` casts, `ON CONFLICT`, `RETURNING`,
`JSONB`, identity columns and timezone-aware types (`SQL080`–`SQL087`), covering DDL
replay guards, `CONCURRENTLY`, column type rewrites, `SET NOT NULL`, `SERIAL`, `JSON`,
timezone-less timestamps and `CASCADE`. All four dialects remain purely static.

## Interface

Four tabs:

- **Review** — paste SQL or drop a `.sql` file; see per-finding evidence and the
  six-dimension quality profile.
- **Batch** — drop many scripts at once, review the directory, check consistency,
  export Markdown.
- **Rules** — browse all rules by category; toggle and re-level each one.
- **Records** — review history and hash-chain verification.

## Built-in AI / MCP tools

`external_tools` and `ai_tools` are enabled, exposing four read-only tools to DBX's
built-in agent and MCP clients:

- `sql_review` — review SQL and return findings plus a `reviewId`;
- `sql_review_result` — retrieve the latest or selected review, with severity/rule
  filters and pagination;
- `sql_optimize` — return structured optimization actions for Agent-generated,
  reviewable patches (`changed=false`; no unproven automatic rewrite);
- `sql_rules` — list the rule catalog, optionally filtered by dialect.

The last 32 single reviews are kept in sidecar memory only. The original SQL never
enters the review-result response and is not written to disk. All tools preserve
the no-database, no-network, no-SQL-execution guarantee.

## How the report reads

Alongside the individual findings you get a **quality profile**:

- `quality` = 100 − risk score. Bigger is better; this is the human-facing verdict.
- `score` (risk) = Σ weights (critical 40 / high 20 / medium 8 / low 3 / info 0), used for ranking.
- Six dimension health scores use a saturation curve (`100·e^(−penalty/55)`) so that
  twenty "missing comment" hits cannot flatten a dimension to zero.
- The **grade is capped by the worst finding**: any critical forces E, any high caps
  at C. Otherwise a pile of low-severity notes would inflate the score and hide a
  single `DROP TABLE`.

Batch aggregation uses a statement-count-weighted average rather than summing
penalties — otherwise one bad file would zero out the whole batch and hide the fact
that every other file passed.

## Install

Install the published version from the DBX Plugin Center, or a local unsigned
development package:

```bash
# package (needs Node 22+)
npx @dbx-app/plugin-cli package .

# installing an unsigned package locally requires enabling
# "Allow unsigned development packages" in the Plugin Center
```

Development:

```bash
cd backend && go build ./... && go test ./...
```

## Publishing to DBX Store

See [docs/store-submission.md](docs/store-submission.md).

## Known limits

- **A review bench, not a gate.** DBX has no pre-execution hook, so nothing here can
  stop a statement from running. It surfaces problems *before* commit instead of
  blocking them at execution time.
- **Lexical, not a syntax tree.** Dynamically built identifiers
  (`CONCAT('t_', @suffix)`) cannot be reconstructed; such statements are marked
  dynamic and only their literal branches are analyzed. PostgreSQL `DO` and
  `CREATE FUNCTION` bodies inside dollar quotes are kept opaque: the outer script
  is reviewed, but PL/pgSQL statements inside the body are not audited.
- **No real row counts.** Because nothing is connected, the plugin cannot know how
  large a table is. `SQL020`/`SQL021` therefore flag every structural ALTER rather
  than only "large table" ones — a deliberate trade: an extra prompt beats a
  conclusion based on a guess.
- **The audit trail is not authentication.** Records are kept in an append-only hash
  chain, which proves a record was not edited after the fact, but **not** who wrote
  it — the reviewer name is self-declared and unverified.
- **Consistency checks cover the directory only.** True drift ("the database has a
  version the repository does not") requires comparing against the target database.
  This plugin does not connect, so it only reports what is visible inside the
  directory: duplicate versions, version gaps, and identical content under
  different versions.

## License

Apache-2.0. See [LICENSE](LICENSE).
