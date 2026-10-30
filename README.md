# Supplier Catalog Sync

A small Go service that keeps a shop's **local copy of a supplier's catalog** up to date.
Staff start an update, see whether it **completed**, **what changed**, and **which problems still need attention**.
An update can be interrupted (supplier outage, application crash, restart) and **resumed without losing finished work**.

- Go + `net/http` + SQLite. No web framework.
- The supplier in the demo is **fictional** (`cmd/fakesupplier`, data in `data/feeds/`). No live account needed.
- Everything is tested with isolated temporary databases and an in-memory fake supplier.

---

## Contents

1. [Quick start](#quick-start)
2. [How the requirements are met](#how-the-requirements-are-met)
3. [The rules](#the-rules)
4. [API](#api)
5. [Architecture and database design](#architecture-and-database-design)
6. [Recovery behaviour](#recovery-behaviour)
7. [Assumptions](#assumptions)
8. [Known limitations / unfinished work](#known-limitations--unfinished-work)
9. [Testing and verification](#testing-and-verification)
10. [Demo](#demo)
11. [AI usage](#ai-usage)

---

## Quick start

**Requirements:** Go 1.22 or newer, and a **C compiler** (`gcc`/`clang`), because the SQLite driver
(`github.com/mattn/go-sqlite3`) uses CGO.

| OS      | C compiler                                                                                                               |
| ------- | ------------------------------------------------------------------------------------------------------------------------ |
| Linux   | `sudo apt install build-essential`                                                                                       |
| macOS   | `xcode-select --install`                                                                                                 |
| Windows | a MinGW-w64 `gcc`, e.g. `winget install BrechtSanders.WinLibs.POSIX.UCRT` or [MSYS2](https://www.msys2.org/); or use WSL |

**Windows notes** (all of this was needed on a real Windows machine):

- Open a **new** terminal after installing `gcc`, then check `gcc --version` and `go env CGO_ENABLED` (must print `1`).
- If `go build` says `C compiler "gcc" not found` although `gcc --version` works in Git Bash, give Go the full path:
  `export CC="C:/Users/<you>/.../mingw64/bin/gcc.exe" CGO_ENABLED=1` (or `go env -w CC=...` to keep it).
- `scripts/demo.sh` is a bash script: run it in **Git Bash** or **WSL**, not in PowerShell.
- In PowerShell use `curl.exe` (plain `curl` is an alias of `Invoke-WebRequest`) and escape JSON quotes:
  `curl.exe -X POST localhost:8080/v1/updates -d "{\"feed\":\"day1-initial\"}"`.
- If the demo reports that a port is in use, free it or pick other ports: `APP_PORT=18080 SUP_PORT=19090 bash scripts/demo.sh`.

```bash
# 1. Terminal A: the fictional supplier (serves data/feeds/*.json on :9090)
go run ./cmd/fakesupplier

# 2. Terminal B: the application (API on :8080, database file ./catalog.db)
go run ./cmd/server

# 3. Terminal C: use it
curl -s -X POST localhost:8080/v1/updates -d '{"feed":"day1-initial"}'
curl -s localhost:8080/v1/updates/1
```

Run the automated end-to-end demo (builds the binaries, so it needs Go and the C compiler too; uses a throw-away database; needs `bash` and `curl`):

```bash
./scripts/demo.sh        # Linux / macOS / WSL / Git Bash
```

Run the tests:

```bash
go test ./...                 # everything
go test -race ./...           # with the race detector (recommended)
go vet ./... && gofmt -l .    # static checks (gofmt prints nothing when clean)
```

Server flags (`go run ./cmd/server -h`):

| Flag                      | Default                 | Meaning                                                                                |
| ------------------------- | ----------------------- | -------------------------------------------------------------------------------------- |
| `-addr`                   | `:8080`                 | listen address                                                                         |
| `-db`                     | `catalog.db`            | SQLite file (created if missing)                                                       |
| `-supplier`               | `http://localhost:9090` | supplier base URL                                                                      |
| `-fetch-attempts`         | `3`                     | tries per batch before the update is interrupted                                       |
| `-retry-backoff`          | `500ms`                 | pause before the first retry, doubles each time                                        |
| `-demo-crash-after-batch` | `0` (off)               | **demo only:** `os.Exit(137)` after N committed batches, to demonstrate crash recovery |

The fictional supplier can simulate an outage on demand:
`curl -X POST 'localhost:9090/admin/outage?from_batch=2'` (batches ≥ 2 return HTTP 503) and `curl -X DELETE localhost:9090/admin/outage`.

---

## How the requirements are met

| Requirement                                   | How                                                                                                                               | Where                                           | Tests                                                                                                                                                                            |
| --------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Staff start an update                         | `POST /v1/updates {"feed":…}` returns `202` and processes in the background                                                       | `internal/api`, `updater.Start`                 | `TestStartUpdateAndWatchItComplete`, `TestBackgroundStartResumeAndConcurrentRequests`                                                                                            |
| See whether it **completed**                  | `status`, `complete` (true only for `completed`), plain-language `summary`                                                        | `api.jobView`                                   | `TestInterruptedUpdateIsClearlyNotCompleteAndCanBeResumed`                                                                                                                       |
| See **what changed**                          | per-update and per-product change history with before/after and changed fields; counters for every record                         | `product_changes` table, `/changes`, `/history` | `TestDemoFeedsProduceTheDocumentedResults`, `TestProductsAndHistory`                                                                                                             |
| See **which problems need attention**         | `issues` (invalid records, version conflicts) + interrupted updates flagged `needs_attention`; staff can resolve issues           | `issues` table, `/issues`                       | `TestInvalidRecordsAreRecordedNotSilentlyDropped`, `TestIssuesCanBeResolved`                                                                                                     |
| New / changed / withdrawn products            | `upsert` and `withdraw` records, decided by one pure function                                                                     | `catalog.Decide`                                | `TestDecide` (17 cases)                                                                                                                                                          |
| Repeated information                          | same version + same content = no-op                                                                                               | `catalog.Decide`                                | `TestImportingTheSameUpdateTwiceCreatesNoDuplicatesAndNoChanges`                                                                                                                 |
| Out-of-order information                      | highest **version** wins whatever the arrival order                                                                               | `catalog.Decide`                                | `TestDecideIsOrderIndependent` (all 120 orders), `TestOutOfOrderDeliveryGivesTheSameResultAsInOrderDelivery`                                                                     |
| Same update twice → no duplicate products     | `sku` is the primary key (normalised, enforced by `CHECK`) and writes are upserts                                                 | `schema.sql`                                    | `TestProductIdentityIsEnforcedByTheDatabase`, import-twice test                                                                                                                  |
| Older info never replaces newer               | rule in `Decide` **and** a DB trigger that refuses to lower `version`                                                             | `products_no_downgrade`                         | `TestDatabaseRefusesToLowerAProductVersion`, `TestOlderInformationFromALaterUpdateNeverOverwritesNewerData`                                                                      |
| Older info never restores a withdrawn product | rule in `Decide`, withdrawal-before-creation stores a tombstone, **and** a DB trigger                                             | `products_no_stale_relist`                      | `TestWithdrawalArrivingBeforeOlderUpsertIsNotUndone`, `TestWithdrawnProductReturnsOnlyThroughAStrictlyNewerVersion`                                                              |
| Outage must not erase the catalog             | catalog is only ever changed by applying a fetched batch; failures change nothing                                                 | `updater.Run`                                   | `TestSupplierOutageNeverErasesTheExistingCatalog`                                                                                                                                |
| Resume after restart without losing work      | cursor + data committed **in one transaction**; startup marks orphaned `running` jobs `interrupted`                               | `AdvanceJob`, `Recover`                         | `TestApplicationCrashAtAnyPointThenRestartAndResume` (8 crash points), `TestShutdownInterruptsARunningUpdateAndItCanBeResumed`                                                   |
| Partial update never reported complete        | `completed` is set only in the transaction that applies the supplier's **final** batch; a `CHECK` makes any other path impossible | `schema.sql`, `AdvanceJob`                      | `TestJobCannotBeCompletedWithoutSupplierFinalBatch`, `TestPartialUpdateIsNeverReportedCompleteAtAnyFailurePoint`, `TestOutageMidwayKeepsCompletedBatchesAndNeverReportsComplete` |
| Fictional supplier data                       | `data/feeds/day1-initial.json` (22 records), `day2-changes.json` (12 records)                                                     | `data/feeds`, `cmd/fakesupplier`                | `TestDemoFeedsProduceTheDocumentedResults`                                                                                                                                       |

---

## The rules

The supplier sends records like:

```json
{"event_id":"d1-009","sku":"HP-1003","type":"upsert","version":2,"name":"Linen Apron",
 "description":"…","category":"Kitchen","price_cents":3000,"currency":"USD"}
{"event_id":"d1-010","sku":"HP-1008","type":"withdraw","version":3}
```

`version` is the supplier's revision counter **per product**. The shop stores the highest version it has applied.
For each valid record, `catalog.Decide(existingProduct, record)` returns exactly one outcome:

| Record vs. stored product           | Outcome     | Effect                                                                              |
| ----------------------------------- | ----------- | ----------------------------------------------------------------------------------- |
| product unknown, `upsert`           | `created`   | row inserted                                                                        |
| product unknown, `withdraw`         | `withdrawn` | **tombstone** row (withdrawn, no details) so a later-arriving older upsert is stale |
| newer version, details differ       | `updated`   | row updated, history entry with changed fields                                      |
| newer version, identical details    | `unchanged` | only the version number moves                                                       |
| newer `withdraw`, product active    | `withdrawn` | status → withdrawn                                                                  |
| newer `upsert`, product withdrawn   | `relisted`  | back on sale, **reported explicitly**                                               |
| **older** version                   | `stale`     | ignored (counted)                                                                   |
| same version, same content          | `duplicate` | ignored (counted)                                                                   |
| same version, **different** content | `conflict`  | ignored, **issue raised** (we cannot know which is right)                           |
| record fails validation             | `invalid`   | skipped, **issue raised** with the original record                                  |

Every record ends in exactly one counter, and `received = created + updated + relisted + withdrawn + unchanged + duplicates + stale + invalid + conflicts` is checked on every batch.

Normalisation: SKUs are trimmed and upper-cased (`"  hp-1011 "` → `HP-1011`), currency is upper-cased, text is trimmed. Nothing else is silently "fixed".

---

## API

All responses are JSON. Errors look like `{"error":"…","code":"…"}`. List endpoints return
`{"items":[…],"count":n,"limit":50,"offset":0}` and accept `?limit=` (max 500) and `?offset=`.

### Updates

| Method & path                                     | Purpose                                                                                                                                  |
| ------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /v1/updates` body `{"feed":"day1-initial"}` | start an update → `202` + `Location`. `409 update_already_open` (with `existing_update_id`) if the feed already has an unfinished update |
| `GET /v1/updates[?status=]`                       | list, newest first                                                                                                                       |
| `GET /v1/updates/{id}`                            | status, progress, counters, `open_issues`, `needs_attention`, `summary`, `next_action`                                                   |
| `POST /v1/updates/{id}/resume`                    | continue an `interrupted` update from its stored cursor → `202`                                                                          |
| `POST /v1/updates/{id}/cancel`                    | abandon an `interrupted` update; it stays `cancelled`, never `completed`                                                                 |
| `GET /v1/updates/{id}/changes`                    | what this update changed                                                                                                                 |
| `GET /v1/updates/{id}/issues[?status=open]`       | problems this update ran into                                                                                                            |

Update states: `running` → `completed` (supplier's final batch applied) · `running` → `interrupted` (supplier outage, error, shutdown, crash) → `running` (resume) · `interrupted` → `cancelled`.

```bash
$ curl -s localhost:8080/v1/updates/1
{
  "id": 1,
  "feed": "day1-initial",
  "status": "completed",
  "batches_done": 4,
  "supplier_done": true,
  "resumes": 1,
  "counters": { "received": 22, "created": 10, "updated": 1, "relisted": 0, "withdrawn": 2,
                "unchanged": 0, "duplicates": 2, "stale": 2, "invalid": 4, "conflicts": 1 },
  "complete": true,
  "open_issues": 5,
  "needs_attention": true,
  "summary": "Completed: 22 record(s) in 4 batch(es): 10 created, 1 updated, 2 withdrawn, … 5 open issue(s) need attention.",
  "next_action": "GET /v1/updates/1/issues?status=open"
}
```

An interrupted update looks like this (note `complete: false`):

```json
{
  "status": "interrupted",
  "complete": false,
  "batches_done": 2,
  "needs_attention": true,
  "summary": "NOT COMPLETE. Interrupted after 2 batch(es): fetching batch 2 from supplier: supplier unavailable after 3 attempts: supplier returned HTTP 503 …",
  "next_action": "POST /v1/updates/1/resume (or /cancel)"
}
```

### Issues

| Method & path                                                                  | Purpose                   |
| ------------------------------------------------------------------------------ | ------------------------- |
| `GET /v1/issues[?status=open\|resolved&kind=invalid_record\|version_conflict]` | all problems              |
| `POST /v1/issues/{id}/resolve` body `{"note":"…"}` (optional)                  | mark handled (idempotent) |

An open issue is **de-duplicated**: the same bad record seen again (e.g. a re-import) increments `occurrences` instead of creating a second issue. If staff resolve it and it comes back, it reopens as a new issue.

### Catalog

| Method & path                                      | Purpose                                                  |
| -------------------------------------------------- | -------------------------------------------------------- |
| `GET /v1/catalog/summary`                          | product counts by status, updates by status, open issues |
| `GET /v1/products[?status=active\|withdrawn\|all]` | default `active` (what is on sale)                       |
| `GET /v1/products/{sku}`                           | one product (SKU is normalised, `hp-1008` works)         |
| `GET /v1/products/{sku}/history`                   | every change to that product                             |
| `GET /healthz`                                     | liveness                                                 |

---

## Architecture and database design

```
cmd/server         application: wiring, flags, graceful shutdown, startup recovery
cmd/fakesupplier   fictional supplier HTTP server (demo only)
internal/catalog   PURE business rules: Record validation + Decide()  (no DB, no network, no clock)
internal/store     the only package that talks to SQLite (schema.sql is embedded)
internal/supplier  Source interface, wire format, HTTP client, in-memory fake for tests
internal/updater  job lifecycle: fetch → apply batch in one tx → advance cursor; retry; resume; recover
internal/api       HTTP handlers
```

Dependencies point inwards: `catalog` imports nothing from the project; `supplier` is a leaf; `store` → `catalog`; `updater` → `catalog`, `store`, `supplier`; `api` → all of them.

**Processing one batch** (`updater.applyBatch`) — a single SQLite transaction:

```
BEGIN IMMEDIATE
  for each record:  parse/validate → load product → Decide → save product + history / add issue
  check: every record is accounted for
  UPDATE update_jobs: next_cursor, batches_done+1, counters, and (only if the supplier said "done") status='completed'
COMMIT
```

Because progress and data share the transaction, a crash can only leave the database **before** the batch or **after** it, never in between. Resuming re-fetches the batch at the stored cursor, and the version rules make even an accidental replay harmless.

**Tables**

| Table             | Purpose                                                                                                                                                                                                        |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `products`        | the local catalog. `sku` primary key (`CHECK sku = upper(trim(sku))`), `version`, `status`. Triggers forbid lowering `version` and restoring a withdrawn product with an older/equal version                   |
| `update_jobs`     | one row per update: state, `next_cursor`, `batches_done`, `supplier_done`, counters, `last_error`. `CHECK (status <> 'completed' OR supplier_done = 1)`; partial unique index → one unfinished update per feed |
| `product_changes` | change history: type, versions, changed fields, before/after JSON, event id, job                                                                                                                               |
| `issues`          | problems for staff; open issues unique by fingerprint                                                                                                                                                          |
| `issue_sightings` | which update ran into which issue (unique per record position, so replays cannot double count)                                                                                                                 |

SQLite settings: WAL (readers never block the importer), `synchronous=FULL` (committed batches survive power loss), `busy_timeout`, `foreign_keys`, `BEGIN IMMEDIATE`. Schema version is kept in `PRAGMA user_version`.

**Why these choices**

- _SQLite_: one file, real transactions, constraints and triggers, trivial to run and inspect (`sqlite3 catalog.db`).
- _Rules in a pure function_: the hard part (ordering) is a table-driven test, not scattered `if`s.
- _Rules also in the database_: a bug in Go code still cannot move a version backwards or fake a completed update.
- _Version as the ordering key_: avoids clock problems; a timestamp would have ties and skew.
- _Cursor stored with the data_: the only way resume can be exactly right.

---

## Recovery behaviour

| Situation                                    | What happens                                                                                                                                                                               | What staff do                             |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------- |
| Supplier returns errors / is unreachable     | transient failures are retried (`-fetch-attempts`, exponential backoff); then the update becomes `interrupted` with the reason. **Catalog untouched** apart from batches already committed | `POST …/resume` when the supplier is back |
| Unknown feed (HTTP 404)                      | not retried; `interrupted` with a clear reason                                                                                                                                             | cancel and fix the feed name              |
| Supplier sends a truncated/garbled response  | JSON decode fails → treated as an outage; a half-delivered batch is never applied                                                                                                          | resume                                    |
| Supplier says "more" but gives no new cursor | `interrupted` (protects against an endless loop)                                                                                                                                           | investigate with the supplier             |
| `Ctrl-C` / `SIGTERM`                         | HTTP server stops; each worker finishes or rolls back its current batch, marks the update `interrupted` ("application was shutting down")                                                  | resume after restart                      |
| Crash / `kill -9` / power loss               | on next start, `Recover` marks every job still `running` as `interrupted`. Batches that committed are kept; the one in flight rolled back                                                  | resume                                    |
| Resume                                       | continues from `next_cursor`; already-finished batches are not fetched again                                                                                                               | —                                         |

Resuming never re-does finished work, and a resumed run produces the **same** catalog, counters and history as an uninterrupted one (tested at every batch boundary and at 8 crash points).

---

## Assumptions

1. **Per-product `version`** from the supplier is a positive integer that only increases. (If a supplier only gave timestamps, they could be mapped to a version such as Unix milliseconds.)
2. **Product identity = SKU**, compared case-insensitively and ignoring surrounding whitespace. Allowed: letters, digits, `.`, `_`, `-`, up to 64 characters.
3. **Withdrawal is just another versioned state.** The newest version wins: a _strictly newer_ upsert after a withdrawal **relists** the product (and is reported as `relisted`). Older or equal-version information never does. If the business wants withdrawal to be permanent, change the single `existing.Status == StatusWithdrawn` branch in `catalog.Decide`.
4. **Same version, different content is a conflict**, not a silent overwrite: we keep what we have and raise an issue.
5. A **batch is atomic**; the supplier's batches are deterministic for a given cursor (re-fetching a cursor returns the same batch).
6. The supplier tells us which batch is the last one (`"done": true`). That is the **only** completion criterion.
7. A **single application process** uses the database file (see limitations).
8. **Invalid records do not stop an update.** They are recorded as issues and the update still completes (it is then flagged `needs_attention`). A _run_ is "complete" when all the supplier's data has been processed, even if some records were rejected.
9. Prices are integer **cents** with a 3-letter currency code; no currency conversion.
10. No authentication: this is an internal staff tool for the assessment.

---

## Known limitations / unfinished work

- **Single instance only.** Startup recovery assumes any `running` job belongs to a dead process. Running two instances against one database would let one mark the other's job `interrupted` (data would still be safe because batches are guarded by the job's `batches_done`/`status`, but the job would stop). A lease/heartbeat column would fix this.
- **Resume is manual.** There is no automatic resume or scheduler; staff call `resume`. An `-auto-resume` flag on startup would be a small addition.
- **No authentication, authorisation or rate limiting.**
- **Notifications:** problems are visible through the API only (no email/Slack).
- **No web UI** (not required).
- **Changing a supplier's batch size mid-update** is outside the model: cursors are opaque and assumed stable.
- **Issue de-duplication** keys on the exact record text; the same problem in a slightly different record is a different issue.
- **Retention:** history, issues and finished jobs are never pruned.
- **Pure-Go driver:** `mattn/go-sqlite3` needs a C compiler. Swapping to `modernc.org/sqlite` (no CGO) only touches `internal/store/store.go` (import, driver name `"sqlite"`, DSN options `_pragma=…`); I could not test that swap in my build environment, so it is not done.
- **`go.sum`:** it was generated in an environment without access to `sum.golang.org`. On a second machine, with the normal Go proxy, the SQLite driver downloaded and built against this `go.sum` without any checksum error. If `go build` ever reports a checksum mismatch, delete `go.sum` and run `go mod tidy`.
- Server `main` and the demo scripts have no unit tests; they are exercised by `scripts/demo.sh`.

---

## Testing and verification

114 test cases (including sub-tests), all using isolated temporary databases and no network:

| Package             | What is covered                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/catalog`  | every outcome of `Decide`; **all 120 arrival orders** of one product's records give the same result; 15 validation failures; "all problems listed at once"                                                                                                                                                                                                                                                                                                                                        |
| `internal/store`    | triggers refuse stale writes/restores; identity `CHECK`; fake "completed" refused by the DB; one unfinished job per feed (also under 12 concurrent starts); state machine; rollback; `AdvanceJob` position guard                                                                                                                                                                                                                                                                                  |
| `internal/updater`  | the demo feeds produce the documented numbers; import-twice; out-of-order; withdrawal protection; conflicts; invalid records; outage at **every** batch; resume does not re-fetch; **interrupted+resumed ≡ uninterrupted**; atomic batch (fault just before commit); **crash inside / right after a commit at every batch, then restart + recover + resume**; graceful shutdown; retry/give-up; unknown feed; endless-cursor guard; cancel rules; concurrent start/resume with concurrent readers |
| `internal/api`      | start → poll → complete; interrupted → resume; cancel; products/history; issue resolve; 12 error cases                                                                                                                                                                                                                                                                                                                                                                                            |
| `internal/supplier` | HTTP client: success, 503, 404, garbage, **truncated body**, timeout, connection refused                                                                                                                                                                                                                                                                                                                                                                                                          |

Results of the commands I actually ran (Go 1.24.13, Ubuntu 24.04, SQLite 3.53 bundled with the driver):

```
gofmt -l .                      → (no output)
go vet ./...                    → ok
go test -race -count=1 ./...    → all packages ok
go test -v ./...                → 114 PASS, 0 FAIL, 0 SKIP
./scripts/demo.sh               → ran end to end (outage → resume → idempotent re-import → crash → recover → resume)
```

Package-local statement coverage: catalog 93%, updater 87%, api 77%, store 48%, supplier 19%. The store and supplier numbers are low because their code is mostly exercised by the _other_ packages' tests (the updater and API suites drive the real store), not by their own test files.

The automated tests were run on Linux. `scripts/demo.sh` was also run on Windows (Git Bash, WinLibs gcc) and printed the same numbers as on Linux. macOS was not tested.

---

Sample data (fictional supplier "Harbor & Pine Supply Co."):

| Feed           | Records         | What it demonstrates                                                                                                                                                                                                                     |
| -------------- | --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `day1-initial` | 22 in 4 batches | creations; **repeated** records; v2 arriving **before** v1; a **withdrawal arriving before an older upsert**; lower-case/padded SKU; 4 **invalid** records (missing name, negative price, price as text, no SKU); 1 **version conflict** |
| `day2-changes` | 12 in 3 batches | price change; version-only change (`unchanged`); new products; **relist by a newer version**; withdrawal; **stale replays** of old versions; exact repeats; 1 invalid currency                                                           |

Expected results (asserted by a test): day 1 → 10 created, 1 updated, 2 withdrawn, 2 repeated, 2 stale, 4 invalid, 1 conflict, 5 open issues, 9 active + 2 withdrawn products. After day 2 → 13 products (11 active, 2 withdrawn), 6 open issues.

---

## AI usage

This project was built with an AI assistant (Claude, Anthropic): it designed, coded and tested the solution, and helped me get it running on Windows. Testing found real defects in the AI's first versions, which were then fixed. Details are in [`AI_USAGE.md`](AI_USAGE.md).
