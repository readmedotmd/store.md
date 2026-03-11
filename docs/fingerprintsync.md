# Fingerprint Sync Store

The `fingerprintsync` package is a standalone `SyncStore` implementation that adds XOR fingerprint reconciliation on top of the standard queue-based sync. It can be used as a drop-in replacement for `sync.StoreSync` when you need stronger consistency guarantees in mesh topologies.

## How It Works

The store uses two layers:

1. **Queue-based sync (fast path)** — same as `sync.StoreSync`. Writes go to a time-ordered queue, and peers pull incrementally using per-peer cursors.
2. **Fingerprint reconciliation (consistency safety net)** — periodically computes XOR fingerprints over 256 buckets and exchanges them with peers. Differing buckets trigger targeted item exchange.

The fingerprint layer catches anything the queue might miss — network partitions, out-of-order delivery, or items lost during reconnection.

## Quick Start

```go
import (
    "github.com/readmedotmd/store.md/bbolt"
    "github.com/readmedotmd/store.md/fingerprintsync"
)

store, _ := bbolt.New("data.db")
defer store.Close()

ss := fingerprintsync.New(store)

// Standard Store interface
ss.Set("greeting", "hello world")
val, _ := ss.Get("greeting")

// Full SyncStore interface
ss.SetItem("myapp", "config", `{"theme":"dark"}`)
item, _ := ss.GetItem("config")
```

## SyncStore Interface

`StoreFingerprint` implements the full `sync.SyncStore` interface:

```go
var _ sync.SyncStore = (*fingerprintsync.StoreFingerprint)(nil)
```

This means it works with the existing server and client infrastructure, or with the fingerprint-specific client and server provided in the same package.

## Fingerprint Computation

Fingerprints are computed on demand by scanning all view keys:

```go
fp, err := ss.ComputeFingerprints()
// fp.Buckets is [256]uint64
```

Each key is hashed (FNV-64a) to one of 256 buckets. The bucket value is the XOR of `hash(key, valueID)` for all keys in that bucket. This means:
- Identical data produces identical fingerprints
- Adding, removing, or updating a key changes the affected bucket
- Computation is O(n) where n is the number of keys

## Reconciliation Protocol

The fingerprint client uses a 3-message protocol:

| Step | Message | Content |
|------|---------|---------|
| 1 | `fingerprint_request` | Sender's 256 bucket fingerprints |
| 2 | `fingerprint_response` | Responder's fingerprints + items for differing buckets |
| 3 | `fingerprint_diff` | Requester's items for still-differing buckets (after applying step 2) |

After all three messages, both sides have exchanged items for every differing bucket.

## Diffing and Item Exchange

```go
// Compare two fingerprints
diff := fingerprintsync.DiffBuckets(localFP, remoteFP)
// diff = []int{5, 42, 200} — bucket indices that differ

// Get all items that hash to those buckets
items, err := ss.ItemsForBuckets(diff)
```

## SyncOut with Fingerprints

`SyncOut` automatically includes fingerprints in the payload:

```go
payload, _ := ss.SyncOut("peer1", 100)
// payload.Items — queue-based incremental items
// payload.Fingerprints — current 256-bucket fingerprints
```

Peers can use the fingerprints to detect if a full reconciliation is needed.

## Client Adapter

The package includes its own client adapter (`FingerprintClient`) that handles both queue-based sync and fingerprint reconciliation:

```go
import "github.com/readmedotmd/store.md/fingerprintsync"

ss := fingerprintsync.New(store)
client := fingerprintsync.NewClient(ss,
    fingerprintsync.WithInterval(5*time.Second),        // queue pull interval
    fingerprintsync.WithFingerprintInterval(30*time.Second), // reconciliation interval
)
defer client.Close()

header := http.Header{}
header.Set("Authorization", "Bearer my-token")
client.Connect("peer-id", "ws://server:8080", header)
```

### Options

```go
fingerprintsync.WithInterval(d)            // Queue sync polling interval (default: 5s)
fingerprintsync.WithFingerprintInterval(d)  // Fingerprint reconciliation interval (default: 30s)
fingerprintsync.WithLimit(n)                // Max items per sync request
fingerprintsync.WithPageSize(n)             // Items per SyncOut page (default: 100)
```

### Active vs Passive Connections

Same as the standard client adapter:
- **Active** connections (via `Connect`) run pull loops, fingerprint reconciliation loops, and push local changes
- **Passive** connections (via `AddConnection`) only respond to incoming messages

## Server

The package includes its own server that uses the fingerprint client internally:

```go
ss := fingerprintsync.New(store)

// Single-store server
server := fingerprintsync.NewServer(ss, fingerprintsync.TokenAuth(map[string]string{
    "secret-token": "peer-1",
}))
server.SetAllowedOrigins([]string{"*"})
server.ListenAndServe(":8080")
```

```go
// Multi-store server
server := fingerprintsync.NewMultiServer(func(storeID string) (*fingerprintsync.StoreFingerprint, error) {
    // resolve storeID to a StoreFingerprint
    return stores[storeID], nil
}, auth)
```

## When to Use

Use `fingerprintsync` instead of `sync.StoreSync` when:
- You have a **mesh topology** (multiple servers syncing with each other)
- **Network reliability** is uncertain and items might be missed
- You need **consistency verification** — fingerprints detect drift without exchanging all data
- You want a **self-healing** sync layer that recovers from any inconsistency

Use `sync.StoreSync` when:
- You have a simple **client-server** topology
- Network is reliable and reconnection is fast
- You don't need the overhead of periodic fingerprint computation
