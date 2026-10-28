# Catalog Sync

Keeps a shop's local copy of a supplier catalog up to date. Staff start an
update, see whether it completed, what changed, and which problems still need
attention. The supplier ("Acme Garden Supply") and all its data are fictional
and simulated, so no supplier account is needed.

## Running the application

Requires **Go 1.26 or newer**. No C compiler or database server is needed:
SQLite is the pure-Go `modernc.org/sqlite`, and the database is a single file.

```sh
go run ./cmd/catalogsync
```

Open http://127.0.0.1:8080. Pages:

- **Updates**: start an update, see recent updates, resume incomplete ones, simulate a supplier outage,
  and **reset the demo data** (refused while an update is running)
- **Update #N**: status, progress, what changed, and the problems found in that update
- **Issues**: problems that still need attention; staff close them
- **Catalog**: the local copy of the supplier catalog

| Flag | Default | Purpose |
|---|---|---|
| `-db` | `catalog.db` | SQLite file; the catalog survives restarts |
| `-addr` | `127.0.0.1:8080` | listen address |
| `-batch-delay` | `1s` | pause between batches so progress is visible |
| `-outage-at N` | `0` | supplier fails from batch N onwards (also adjustable in the UI) |
| `-crash-at B:R` | — | kill the process in batch B after record R (demo only) |

To start from an empty catalog, press **Reset demo data** or delete `catalog.db*`.

### Demo walkthrough

1. Start `01-initial-catalog`, then `02-weekly-changes`, then `03-relist-and-late-data`.
   Each update page lists what changed. Rejected records show up as issues.
2. **Supplier outage**: set "Fail from batch" to 2 and start an update. It stops as
   `FAILED` and batch 1 is kept. Set the field back to 0 and press **Resume**.
3. **Crash**: run `go run ./cmd/catalogsync -crash-at 2:2` and start an update.
   The process dies in the middle of batch 2. Start the app again without the flag:
   the update shows `INTERRUPTED`, only batch 1 is in the catalog, and **Resume**
   finishes the update.
4. Start `01-initial-catalog` again. The catalog does not change, and the older
   data is reported as ignored.

### Docker

```sh
docker build -t catalogsync .
docker run -p 8080:8080 -v catalogsync-data:/data catalogsync
```

### Deploying to Fly.io

The included `fly.toml` runs **one** machine with a persistent volume for the
SQLite file. One machine is required, because SQLite cannot be shared between
machines and the app allows only one running update.

```sh
fly auth login
# Set a unique `app` name in fly.toml, then:
fly apps create <app-name>
fly volumes create catalog_data --region sin --size 1
fly deploy
fly scale count 1   # make sure exactly one machine runs
```

Notes:

- Idle machines are stopped automatically. If one stops during an update, the
  update is saved as `INTERRUPTED` and can be resumed after the next visit.
- Do not add `-crash-at` to the deployed command: the machine would crash at the
  same point on every update. Use the outage simulator instead.
- Times are shown in the server's time zone, which is UTC in the container.

## Running the tests

```sh
go test ./...
```

- `internal/domain`: table-driven tests for the ordering rules, record validation, and the update state machine.
- `tests/integration`: end-to-end tests on a real SQLite file and the simulated supplier.
  - Repeated and out-of-order data, withdrawals, re-listing, invalid records, and the issue lifecycle.
  - Supplier outage followed by a resume ends with the same catalog as an uninterrupted update.
  - Crash inside a batch transaction, simulated in-process.
  - **Real process kill**: `TestKilledProcessLosesNoFinishedWork` re-runs the
    test binary as a child process that calls `os.Exit` mid-batch. The parent
    then reopens the database the way a restarted application would.
  - Randomised delivery: for each of 40 seeds, a product history is delivered shuffled,
    with repeats, split into random batches, with a crash and an outage along the way. The
    catalog must end up at the newest valid supplier state.
  - Invariants checked after every scenario: no duplicate SKUs, counters match the
    change log, and an update has a finish time if and only if it is `COMPLETED`.

## How it works

- A product is identified by its supplier SKU and upserted by that key, so it cannot be duplicated.
- The supplier's `updated_at` decides ordering. A record is applied only if it is
  strictly newer than the stored version. An equal timestamp means a repeat; an older one is ignored.
- Withdrawn products are kept as tombstones. Older data therefore cannot bring
  them back; newer active data re-lists them.
- Each batch is applied in **one transaction together with the update's checkpoint**.
  A crash or outage keeps every finished batch, never keeps part of a batch, and
  never removes existing catalog data.
- An update is `COMPLETED` only after the supplier's last batch is committed.
  At startup, updates left `RUNNING` by a crash are marked `INTERRUPTED`.

### Architecture

Clean architecture: dependencies point inward only.

```
cmd/catalogsync/                 composition root: wires the adapters into the use cases
internal/domain/                 entities and rules (Decide, update states, issues); no dependencies
internal/usecase/                SyncService, QueryService, IssueService, and the ports (interfaces)
internal/adapter/sqlite/         storage implementation on SQLite
internal/adapter/supplier/mock/  fictional supplier with JSON feeds and outage simulation
internal/adapter/web/            staff UI (net/http + html/template)
tests/integration/               end-to-end tests
```

## Assumptions

- **The supplier's `updated_at` is the version.** It is set by the supplier and
  strictly increases for each product. The time the shop receives the data is never used.
- **Updates contain changes, not a full snapshot.** A product missing from an
  update is unchanged. It is withdrawn only by an explicit `"status": "withdrawn"` record.
- **A withdrawn product can be sold again** if the supplier later sends newer active data.
- **Two records with the same SKU and `updated_at` are the same version.** The first
  one applied wins, and later copies are treated as repeats.
- **An invalid record is skipped on its own** (for example: missing SKU, negative price,
  unknown status). The rest of its batch is applied, and the record becomes an issue.
- **Staff close issues manually.** The same bad record imported again does not
  create a duplicate issue or reopen a closed one.
- **Staff resume incomplete updates manually**, with the Resume button.
- **The supplier's cursor is replayable:** fetching the same cursor again returns
  the same batch. This lets an interrupted update continue from its checkpoint.
- **Only one update runs at a time.** This is enforced in code and by a unique index.
- Prices are stored in cents, in a single currency.
- In the demo, an "update" is one of the bundled supplier feeds, chosen by staff.

## Unfinished work and limitations

- **Supplier integration:** only the simulated supplier exists. A real supplier would be a new
  adapter implementing the `usecase.Supplier` port; nothing else would change.
- **Users:** there is no authentication, and no record of which staff member did what.
- **Single instance:** the application is designed to run as one process. Running
  several instances against the same database is not supported.
- **UI:** progress refreshes by reloading the page every 2 seconds, not by live push.
  The catalog and change lists are not paginated.
- **Race detector:** `go test -race` was not run, because on Windows it needs CGO and a C compiler.
- **Use case tests:** the use cases are covered by the integration tests only, with
  no separate unit tests using in-memory fakes.
- **Retention:** old updates and change logs are never archived.
- **Issue types:** issues cover rejected records only. Supplier outages and crashes
  are reported on the update itself (`FAILED` / `INTERRUPTED`), not as issues.

## Use of AI tools

<!-- Review and adjust this section so it accurately describes your own process. -->

This solution was built with **Claude Code** (Anthropic) as a pair programmer:

- **Analysis:** the AI broke the brief down into requirements and listed the
  ambiguities. I made the product decisions: no implicit withdrawals, `updated_at`
  as the version, re-listing allowed, per-record rejection, manual issue closing,
  manual resume, and Go as the language.
- **Test strategy:** the testing approach (unit, integration, crash and process-kill,
  randomised) was discussed and agreed before any code was written.
- **Implementation:** the AI generated the code, tests and fictional data following
  that design. It verified the tests by deliberately breaking the ordering rule and
  confirming that the tests failed, then smoke-tested the app (crash, restart, resume).
- **My role:** I reviewed the design and the code, ran the application and the tests,
  and remain responsible for the submission.
