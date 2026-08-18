# Entity Service baseline findings

Measured against the live GoodMarket deployment on 2026-08-18:

- Public service: <https://goodmarket-k4u67azzg5.app.omnira.dev/>
- Entity Service: <https://entityservice-k4u67azzg5.app.omnira.dev/>
- Product ID: `5767359467261049418`
- Product owner: `5695892345266999354`
- Target device: owned M4 device `5752229357024231935`

## Results

| Probe | Observed result |
|---|---|
| Basic Entity read | roughly 280–310 ms in the observed live samples |
| Single listing write | roughly 590 ms in the initial live sample |
| Eight simultaneous creates | 8/8 succeeded; 1.02–5.84 s; 28 CAS conflicts |
| Eight simultaneous deletes | 8/8 succeeded; 0.99–5.99 s; 28 more CAS conflicts |
| Restart recovery | new process restored generation 4 / revision 4 and the proof listing |
| Invalid mutation | HTTP 400 without an Entity write |
| Six-listing state | 1,886 bytes at generation/revision 20 after the probe |
| Durable application disk | none |
| External database | none |

The concurrency result is the important boundary. All writes were correct, but
one state block turned eight parallel requests into a serialized CAS retry
queue. This model is suitable for a tiny catalog and unsuitable as a general
OLTP model for a Paperclip-scale application.

## Platform constraints discovered while publishing

1. The standard payload publisher's 3 MiB raw chunks timed out through the live
   tunnel. Publishing the same binaries in 512 KiB Entity blocks completed and
   passed round-trip SHA verification for all four architectures.
2. A native Go payload inside the M4 runner could not use the host macOS
   certificate verifier (`SecPolicyCreateSSL`). Embedding Mozilla's CA pool
   restored normal certificate validation; TLS verification was never disabled.
3. Product Service showed the new binary SHA before the runner-facing catalog
   adopted it. The first forced rotate fetched the previous SHA; the following
   refresh window fetched the correct SHA and started successfully.
4. Entity Service is a block store, not a query engine. It supplies keyed blobs
   and generation CAS, but no relational joins, secondary indexes, or arbitrary
   listing queries.
5. Public CRUD without application authentication is acceptable only for this
   capped experiment. A real marketplace needs authenticated ownership,
   authorization, moderation, and abuse controls before accepting public writes.

## What this means for heavier applications

Do not translate every SQL row into a shared JSON block. The safer pattern is:

- keep the application's transactional engine in memory;
- snapshot that engine into SHA-verified 512 KiB Entity chunks;
- store large attachments as independent chunked objects;
- protect a writable instance with a generation-backed lease;
- use small sharded indexes when direct entity access is required;
- retry transient `429`, `502`, `503`, and `504` responses;
- design for crash-consistent snapshots rather than claiming zero-RPO writes.

That is the architecture used by the strict Entity-only Paperclip wrapper. The
small marketplace experiment validates why that heavier adapter needs an
in-memory database, chunked snapshots, attachment blocks, and a single-writer
lease instead of using one Entity block as its live transactional database.
