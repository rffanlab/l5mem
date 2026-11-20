# L5 Memory (the "fifth layer" of memory)

[中文](README.md)

A **shared long-term memory store** for a team of AI agents and business systems: the facts and judgments that don't fit in a
context window, must not be forgotten, or will go stale. Every entry can be found quickly, corrected safely, and traced back.

One static binary, `l5` (Go + SQLite FTS5, ~9 MB, no runtime dependencies). It is both the server and the CLI; symlink it as
`memo` if you like.

- **Fast**: ~3 ms p50 for a high-frequency two-character query over 50,000 entries; a full CLI round trip (process start +
  socket) is also ~3 ms
- **Built for Chinese**: overlapping character bigrams + trigrams, no word segmenter needed; alias groups let "英伟达" also find
  "NVIDIA"
- **Knows that facts expire**: each entry has a lifecycle (stable / slow / volatile) and lands in a re-check queue when due.
  When the world changes, a new value **supersedes** the old one; the old value is kept and always viewable
- **Auditable**: who wrote it and when, who verified it, what replaced it, why it was retracted. Daily automatic backups
- **Write once, use everywhere**: Unix socket locally (no token), HTTP remotely (token required for writes), read-only web UI

Design notes: [docs/DESIGN.md](docs/DESIGN.md) (Chinese). Benchmarks: [BENCH.md](BENCH.md) (Chinese).
A model-agnostic method for "thinking over new information together with long-term memory":
[docs/REFLECT_METHOD.md](docs/REFLECT_METHOD.md) (Chinese).

## What an entry looks like

| Field | Meaning |
|---|---|
| kind | principle / decision / judgment / lesson / fact / memo (Chinese names also accepted) |
| topic | One-line subject. A new value under the same subject supersedes the old one, forming a chain |
| body | The memory itself |
| tags | Retrieval shortcuts; a query that exactly matches a tag jumps to the top (over-broad "hub" tags are ignored automatically) |
| source | Link or note, so it can be re-checked later |
| volatility | stable (never expires) / slow (re-check every 180 days) / volatile (30 days); classified automatically if omitted |
| verified | Entries without evidence are written as unverified and come due in 7 days |
| recheck_days | Optional per-entry due date (e.g. 60 for a hypothesis that should resolve within 60 days) |

Writing the same content twice stores it once (content-hash dedup), so writes are safe to retry.

## Quick start

```bash
# start the server (default data dir is /srv/e5-data/l5mem; use ./data here)
l5 serve --data ./data --socket ./data/l5.sock --http 127.0.0.1:19110

# in another terminal: write, search, correct
export L5_SOCKET=./data/l5.sock
l5 add "GPU rental price" "H100 cloud rental is about $2/hour" -t compute,GPU -s https://example.com/report
l5 search "rental price" --explain     # why each hit ranks where it does (per-route ranks; lexical/proximity/recency/importance)
l5 add "GPU rental price" "H100 cloud rental fell to about $1.6/hour" --supersedes 1   # new value supersedes the old
l5 history 1                           # the supersede chain; old values are kept
l5 due                                 # entries due for re-checking
l5 verify 2 --note "checked three cloud providers' pricing pages"
l5 relate 2                            # association: other entries sharing rare tags (1–2 hops)
```

### Alias groups

```bash
l5 alias add 英伟达 NVIDIA NVDA         # querying any spelling now searches all three
l5 search 英伟达财报                     # aliases inside a Chinese run are expanded too (also searches "NVIDIA 财报")
l5 alias load deploy/aliases.txt       # bulk load (ships with 40 common AI / semiconductor groups)
l5 alias ls / l5 alias rm NVDA
```

Latin words the user types are prefix-matched; Latin spellings that come from alias expansion are matched as whole words, so
`Arm` does not drag in `army`.

### More search options

```bash
l5 search compute --since 7d           # only entries written in the last 7 days (also 2026-11-01 / "2026-11-01 08:00:00" / 24h)
l5 search compute --until 2026-06-01   # entries written before a date ("what did we think back then")
l5 search compute -k judgment --tag hypothesis
l5 search compute --topic focus/       # by topic prefix
l5 search compute --history            # include superseded / retracted entries
l5 search compute --no-fold            # turn off near-duplicate folding
```

**Near-duplicate folding**: several versions of the same story, or many similar back-filled facts, crowd out everything else.
When two hits share ≥ 60% of the character pairs in their first 300 characters, the lower-ranked one is moved to the end,
so the limited slots go to different information. Nothing is dropped; it is still returned if there is room.

### Top of mind, and newly formed links

```bash
l5 mind              # topics most recently written, cited and recalled (recent use weighs more; over-broad tags listed
                     # separately as "background"; the last ~20% of slots go to the fastest-rising topics)
l5 links             # pairs of topics that co-occurred in the last 7 days but never before (ranked by PMI, so hub tags don't win)
l5 links --days 14 --prior 1
l5 add "inference" "…" -k judgment --cites 12,15   # derived from #12 and #15: their topics count as recalled once
```

Activation follows ACT-R base-level learning: every use is a trace that decays with age,
`A = ln Σ uses × (days + 1)^-0.5`, so three uses yesterday outweigh thirty half a year ago. Only real uses count: a write
(2), being cited as the basis of an inference (3), being verified (2), appearing in the top 3 of an agent's search (1).
Searches from the web UI and the patrol don't count, so recommendations can't feed themselves. Hub tags (on more than 8% of
entries) and system tags (每日沉思, ic-event-123-style ids) are excluded; add more with `L5_MIND_STOP`.

The daily patrol report includes the top of mind and new links. InfoCollector's daily reflection judges each new link as a
coincidence, the same thing, or a new relationship; a new relationship becomes a conclusion that goes through self-check and
is written back with `cites`.

## Integration

### CLI (local or remote)

Locally the CLI talks to the Unix socket (`/run/l5mem/l5.sock`, no token). On another machine, copy `l5` and set:

```bash
export L5_URL=http://<server>:9110 L5_TOKEN=<write token> L5_CALLER=researcher
```

`L5_CALLER` names the caller so the usage report can tell agents apart. Reads need no token; writes and maintenance do.

### Python

`clients/python/l5client.py` uses only the standard library; copy it into your project:

```python
from l5client import L5
m = L5(caller="infocollector")                       # local socket; pass url= and token= for remote
m.add("GPU rental price", "H100 about $1.6/hour", tags=["compute"], source="https://…")
for h in m.search("rental price", since="30d")["hits"]:
    print(h["id"], h["topic"], h["body"])
m.add_aliases("英伟达", "NVIDIA", "NVDA")
m.add("hypothesis/acme", "Acme will cut guidance next quarter", kind="judgment", unverified=True, recheck_days=60)
m.add("inference/power", "…", kind="judgment", cites=[12, 15])
for t in m.mind(limit=10)["tags"]:
    print(t["tag"], t["trend"], t["explore"])
for l in m.links(days=7):
    print(l["a"], "⟷", l["b"], l["count"])
```

### HTTP API

`/api/v1/<op>`, GET query string or POST JSON, JSON responses. Identify the caller with the `X-L5-Caller` header or the
`caller` parameter; writes need `X-L5-Token` (or `Authorization: Bearer …`).

| op | parameters | notes |
|---|---|---|
| add | topic, body, kind, tags[], source, volatility, unverified, supersedes[], meta{}, created_at, recheck_days, cites[] | content-hash dedup: a repeat returns `existed: true` |
| search | q, limit, kind, tag, topic_prefix, since, until, history, explain, no_fold | four-route RRF + scoring; returns `expanded` (alias spellings searched) and `folded` |
| mind | limit | top of mind: `tags` (activation, trend new/up/flat/down, uses in last 7 days / previous 4 weeks, explore, 3 recent entries), `background` (hub tags) |
| links | days, min, prior, limit | new links: `links` (a, b, count, prior, pmi, score, entries) |
| aliases | GET lists; POST `{"names": [...]}` creates / merges a group; POST `{"name": x}` removes one spelling | alias groups |
| get / history / relate | id (history also accepts topic) | |
| supersede / retract / verify / volatility | old,new / id,reason / id,note / id,volatility | |
| due / tags / stats / metrics | days / limit / – / since_last, since_hours | |
| consolidate / doctor / repair / reclassify / watch | | maintenance (watch = daily patrol; silent when nothing is found) |
| export / import | – / JSONL body | lossless migration with fingerprint check and read-back self-test; alias groups included |

Web UI at the server root (search, due list, top of mind, tags, status; read-only). `?id=123` opens one entry, `?q=term` searches, `?tab=mind` opens top of mind.

## How ranking works

1. **Four retrieval routes** in parallel: Chinese bigrams / Latin prefixes (FTS5; spellings of one term are OR-ed through alias
   groups, terms are AND-ed), trigram substrings (terms ≥ 3 characters), topic-prefix range scan, and exact tag lookup.
   With no query, the newest entries are listed
2. **RRF fusion** (k = 60), then `0.45 lexical + 0.15 proximity + 0.25 recency + 0.15 importance + tag boost`.
   Proximity (do the query words really appear together?) is computed only for the top 3n candidates
3. **Near-duplicate folding**, then the top n

What keeps it at a few milliseconds at 50k entries: bm25 is computed only for the part worth ranking; the bigram index uses
`detail=full`; lightweight candidate metadata lives in memory; the read path never writes (hit counts are buffered and flushed
periodically); results are LRU-cached and any write invalidates the cache. Measurements in [BENCH.md](BENCH.md).

Scaling design (what happens as the store grows) is in [docs/SCALING.md](docs/SCALING.md) (Chinese).

## Maintenance

Every day at `L5_WATCH_AT` (default 10:17) the server runs a patrol:

- flush hit counts; escalate entries overdue by 7+ days
- health check (index consistency etc.); incremental usage report (who uses it, p50/p95, tag hit rate)
- **automatic backup**: full export (including alias groups) to `<data dir>/backups/l5-DATE.jsonl.gz`, keeping the newest
  `L5_BACKUP_KEEP`
- the report is written to `reports/DATE.json`; if anything was found it is POSTed to `L5_NOTIFY_URL`, otherwise nothing is sent

Restore with `l5 import backup.jsonl` (gunzip first); import verifies the fingerprint and reads a sample back.

## Build and deploy

```bash
./build.sh                 # needs Go 1.22+ and gcc (mattn/go-sqlite3 uses cgo); runs tests, produces a static bin/l5
VER=2026.11.19.1 ./build.sh
```

`bin/l5` is a statically linked x86_64 Linux binary that runs on any Linux.

`deploy/` contains an example deployment to a server managed by "E5" (systemd + Nginx + registration with E5 Manager).
On Windows, run `deploy\deploy-158.bat`: it packs `bin/l5` and `deploy/`, uploads them and runs `deploy-l5mem.sh`
(idempotent, rolls back on failure, never touches data). The first deployment generates a write token and stores it in
`/etc/e5-runtime/l5mem.env` on the server.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| L5_TOKEN | empty (the deploy script generates one) | HTTP write token; not needed over the socket; empty = no check |
| L5_DATA / L5_DB | /srv/e5-data/l5mem / l5.db in the data dir | data dir (reports, backups, usage logs) and database |
| L5_SOCKET / L5_HTTP | /run/l5mem/l5.sock / 127.0.0.1:19110 | listen addresses (empty string = disabled) |
| L5_WATCH_AT | 10:17 | daily patrol time |
| L5_BACKUP_KEEP | 14 | daily backups to keep; 0 = no backups |
| L5_NOTIFY_URL | empty | POST JSON here when the patrol finds something |
| L5_W_LEX / L5_W_PROX / L5_W_REC / L5_W_IMP | 0.45 / 0.15 / 0.25 / 0.15 | ranking weights |
| L5_HALF_LIFE_DAYS | 30 | recency half-life |
| L5_STABLE_FLAT | 1 | stable memories do not decay with age; 0 = uniform decay |
| L5_MIND_STOP | empty | extra tags (comma-separated) excluded from mind and links, on top of the built-in system tags |
| L5_TAG_BOOST | 0.10 | bonus when the query is exactly a tag name; set 0 if the usage report flags sloppy tagging |
| client: L5_URL / L5_TOKEN / L5_CALLER | | remote address, write token, caller name |

## Layout

```
cmd/l5/            server + CLI
cmd/l5bench/       benchmark (go run -tags sqlite_fts5 ./cmd/l5bench -sizes 1000,10000,50000)
internal/store/    storage, indexes, search, lifecycle, aliases, export/import
internal/server/   HTTP / socket API, daily patrol and backup, web UI
internal/client/   Go client
clients/python/    Python client (standard library only)
deploy/            deploy scripts, systemd, Nginx, alias list
docs/              design notes, reflection method (Chinese)
```
