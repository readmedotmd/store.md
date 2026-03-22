# Implementations

All implementations satisfy the `storemd.Store` interface and pass the generic test suite. All Store methods accept a `context.Context` as the first parameter, enabling cancellation and timeout propagation.

Most backends also provide a `Clear(ctx context.Context) error` method that removes all key-value pairs. This is available on the concrete type (not the `Store` interface) since not all backends support it efficiently. Use `storemd.Clearable` for type assertions. S3 does not support `Clear`.

---

## BBolt — `backend/bbolt/`

Embedded key-value store using [bbolt](https://github.com/etcd-io/bbolt). Single-file, no server required.

```go
import "github.com/readmedotmd/store.md/backend/bbolt"

store, err := bbolt.New("/path/to/data.db")
defer store.Close()
```

**Best for:** CLI tools, desktop apps, single-process services.

**Clear:** `store.Clear(ctx)` deletes and recreates the internal bucket.

---

## Badger — `backend/badger/`

High-performance embedded store using [badger](https://github.com/dgraph-io/badger). LSM-tree based, optimized for SSDs.

```go
import "github.com/readmedotmd/store.md/backend/badger"

store, err := badger.New("/path/to/data-dir")
defer store.Close()
```

**Best for:** High-throughput local workloads, write-heavy applications.

**Clear:** `store.Clear(ctx)` calls `db.DropAll()`.

---

## SQL — `backend/sql/`

Uses `database/sql` with upsert support. Tested with SQLite via [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO). Compatible with any SQL database that supports `ON CONFLICT`.

```go
import (
    "database/sql"

    sqlstore "github.com/readmedotmd/store.md/backend/sql"
    _ "modernc.org/sqlite"
)

db, err := sql.Open("sqlite", "data.db")
store, err := sqlstore.New(db)
```

Creates a `kv_store` table automatically:

```sql
CREATE TABLE IF NOT EXISTS kv_store (
    key TEXT PRIMARY KEY,
    value TEXT
)
```

**Best for:** Projects already using SQL, relational database integration, SQLite deployments.

**Clear:** `store.Clear(ctx)` runs `DELETE FROM kv_store`.

---

## S3 — `backend/s3/`

Uses [MinIO Go client](https://pkg.go.dev/github.com/minio/minio-go/v7) with any S3-compatible backend (AWS S3, MinIO, R2, etc).

```go
import (
    "github.com/minio/minio-go/v7"
    "github.com/minio/minio-go/v7/pkg/credentials"

    s3store "github.com/readmedotmd/store.md/backend/s3"
)

client, _ := minio.New("s3.amazonaws.com", &minio.Options{
    Creds:  credentials.NewStaticV4("ACCESS_KEY", "SECRET_KEY", ""),
    Secure: true,
})
store := s3store.New(client, "my-bucket", "optional/prefix/")
```

Values are stored as object contents. `List` uses `ListObjects` and fetches each value individually.

**Best for:** Serverless, cloud-native, large values, cross-region storage.

**Note:** `Delete` performs a `StatObject` check first since S3's `RemoveObject` doesn't error on missing keys.

**Context:** S3 operations respect context deadlines and cancellation. The default timeout is 30 seconds when no deadline is set on the context.

---

## MongoDB — `backend/mongodb/`

Uses the official [MongoDB Go driver v2](https://pkg.go.dev/go.mongodb.org/mongo-driver/v2).

```go
import (
    "go.mongodb.org/mongo-driver/v2/mongo"
    "go.mongodb.org/mongo-driver/v2/mongo/options"

    "github.com/readmedotmd/store.md/backend/mongodb"
)

client, err := mongo.Connect(options.Client().ApplyURI("mongodb://localhost:27017"))
col := client.Database("mydb").Collection("kv")
store := mongodb.New(col)
```

Documents are stored as `{_id: key, value: value}`. List uses regex for prefix filtering and sorts by `_id`.

**Best for:** Document-oriented projects, existing MongoDB infrastructure.

**Clear:** `store.Clear(ctx)` runs `DeleteMany({})` on the collection.

**Context:** MongoDB operations respect context deadlines and cancellation. The default timeout is 30 seconds when no deadline is set on the context.

---

## Redis — `backend/redis/`

Uses [go-redis v9](https://github.com/redis/go-redis).

```go
import (
    "github.com/redis/go-redis/v9"

    redisstore "github.com/readmedotmd/store.md/backend/redis"
)

client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
store := redisstore.New(client, "myapp:") // key prefix for namespacing
```

`List` uses `SCAN` + sort + `MGET` since Redis doesn't have native ordered iteration.

**Best for:** Caching layers, shared state across services, pub/sub systems.

**Clear:** `store.Clear(ctx)` scans and deletes all keys matching the store's prefix.

**Note:** Key prefix is recommended to avoid collisions in shared Redis instances.

**Context:** Redis operations respect context deadlines and cancellation. The default timeout is 30 seconds when no deadline is set on the context.

---

## IndexedDB — `backend/indexeddb/`

Browser-native key-value store via IndexedDB, compiled to WebAssembly. Uses `syscall/js` to interact with the IndexedDB API.

```go
//go:build js && wasm

import "github.com/readmedotmd/store.md/backend/indexeddb"

store, err := indexeddb.New("my-database")
defer store.Close()
```

Compile with:

```bash
GOOS=js GOARCH=wasm go build -o app.wasm
```

Values are stored in a `kv` object store. Cursor iteration provides lexicographic key ordering for `List`.

**Best for:** Browser apps, PWAs, offline-first web applications.

**Clear:** `store.Clear(ctx)` calls `objectStore.clear()` on the IndexedDB object store.

**Note:** Requires `GOOS=js GOARCH=wasm` build target. Tests need a browser or JS runtime with IndexedDB support.

---

## Sync Implementation

The `sync/core/` package provides a `SyncStore` implementation wrapping any `Store`:

| Package | Description |
|---------|-------------|
| `sync/core/` | Queue-based sync with timestamp conflict resolution. See [sync.md](sync.md). |

It implements the `core.SyncStore` interface and works with the server and client packages.
