# GoodMarket on Omnira Entity Service

GoodMarket is a deliberately small marketplace used to measure Omnira Entity
Service before adapting heavier applications. It uses one generation-protected
JSON block as its **only durable state**.

Live: <https://goodmarket-k4u67azzg5.app.omnira.dev/>

The measured concurrency, restart, publishing, and security findings are in
[LIMITATIONS.md](./LIMITATIONS.md).

## What it proves

- listing create, read, update, and delete through Entity Service
- optimistic concurrency with generation-based CAS retries
- restart recovery without a local database or durable application files
- live storage and latency evidence at `/_omnira/storage`
- a self-contained Go binary with embedded HTML, CSS, and JavaScript
- an embedded Mozilla CA pool for verified TLS inside Omnira's native sandbox

## Deliberate baseline limits

- one block means every mutation rewrites the full marketplace state
- concurrent writers contend on one generation
- the application caps state at 512 KiB and 250 listings
- block storage does not provide relational queries or secondary indexes

These are measurement boundaries, not hidden implementation details. A second
iteration can partition listings by ID and maintain sharded indexes after the
baseline results are recorded.

## Configuration

```text
OMNIRA_ENTITY_URL=https://entityservice-k4u67azzg5.app.omnira.dev
OMNIRA_ENTITY_API_KEY=<service principal key>
OMNIRA_ENTITY_OWNER_ID=5695892345266999354
OMNIRA_ENTITY_NAMESPACE=entity-marketplace
PORT=3100
```

The deployed Mac runner can also read the existing owner-only credential file.
That file contains authentication material only; marketplace data is never
written to local disk.

## Run

```sh
go test ./...
go run . --port 3100
```

Open `http://127.0.0.1:3100` and `http://127.0.0.1:3100/_omnira/storage`.
