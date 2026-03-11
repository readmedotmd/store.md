// Command syncanalysis exercises the sync store through realistic sync
// scenarios and dumps all underlying keys for analysis.
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/backend/memory"
	storesync "github.com/readmedotmd/store.md/sync/core"
)

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("SYNC STORE ANALYSIS — underlying key audit")
	fmt.Println("=" + strings.Repeat("=", 79))

	analyzeQueueSync()
}

func analyzeQueueSync() {
	fmt.Println()
	fmt.Println(strings.Repeat("-", 80))
	fmt.Println("QUEUE-BASED SYNC (sync.StoreSync)")
	fmt.Println(strings.Repeat("-", 80))

	storeA := memory.New()
	storeB := memory.New()
	ssA := storesync.New(storeA, int64(100*time.Millisecond))
	ssB := storesync.New(storeB, int64(100*time.Millisecond))

	// Scenario 1: A writes 3 keys
	fmt.Println("\n--- Scenario 1: A writes 3 keys ---")
	ssA.SetItem("app", "color", "blue")
	ssA.SetItem("app", "size", "large")
	ssA.SetItem("app", "shape", "circle")

	fmt.Println("\nStore A after 3 writes:")
	dumpKeys(storeA)

	// Scenario 2: Sync A → B
	fmt.Println("\n--- Scenario 2: Sync A → B (A initiates) ---")
	p1, _ := ssA.Sync("peer-b", nil)
	fmt.Printf("A.Sync(nil) → %d items\n", len(p1.Items))
	p2, _ := ssB.Sync("peer-a", p1)
	if p2 != nil {
		fmt.Printf("B.Sync(payload) → %d items\n", len(p2.Items))
	} else {
		fmt.Println("B.Sync(payload) → nil (done)")
	}

	fmt.Println("\nStore A after sync:")
	dumpKeys(storeA)
	fmt.Println("\nStore B after sync:")
	dumpKeys(storeB)

	// Scenario 3: A updates an existing key, syncs again
	fmt.Println("\n--- Scenario 3: A updates 'color' to 'red', syncs again ---")
	time.Sleep(1 * time.Millisecond)
	ssA.SetItem("app", "color", "red")

	fmt.Println("\nStore A after update:")
	dumpKeys(storeA)

	p3, _ := ssA.Sync("peer-b", nil)
	fmt.Printf("A.Sync(nil) → %d items\n", len(p3.Items))
	p4, _ := ssB.Sync("peer-a", p3)
	if p4 != nil {
		fmt.Printf("B.Sync(payload) → %d items\n", len(p4.Items))
	} else {
		fmt.Println("B.Sync(payload) → nil (done)")
	}

	fmt.Println("\nStore A after second sync:")
	dumpKeys(storeA)
	fmt.Println("\nStore B after second sync:")
	dumpKeys(storeB)

	// Scenario 4: Sync A → B again (nothing changed)
	fmt.Println("\n--- Scenario 4: Sync A → B again (no changes) ---")
	time.Sleep(150 * time.Millisecond)
	p5, _ := ssA.Sync("peer-b", nil)
	if p5 != nil {
		fmt.Printf("A.Sync(nil) → %d items (UNEXPECTED - should be nil)\n", len(p5.Items))
	} else {
		fmt.Println("A.Sync(nil) → nil (nothing to sync)")
	}

	fmt.Println("\nStore A after no-op sync:")
	dumpKeys(storeA)

	// Scenario 5: B writes, syncs to A
	fmt.Println("\n--- Scenario 5: B writes 'weight'='heavy', syncs B → A ---")
	ssB.SetItem("app", "weight", "heavy")
	p6, _ := ssB.Sync("peer-a", nil)
	fmt.Printf("B.Sync(nil) → %d items\n", len(p6.Items))

	p7, _ := ssA.Sync("peer-b", p6)
	if p7 != nil {
		fmt.Printf("A.Sync(payload) → %d items\n", len(p7.Items))
		p8, _ := ssB.Sync("peer-a", p7)
		if p8 != nil {
			fmt.Printf("B.Sync(response) → %d items\n", len(p8.Items))
		} else {
			fmt.Println("B.Sync(response) → nil (done)")
		}
	} else {
		fmt.Println("A.Sync(payload) → nil (done)")
	}

	fmt.Println("\nStore A final:")
	dumpKeys(storeA)
	fmt.Println("\nStore B final:")
	dumpKeys(storeB)

	// Summary
	fmt.Println("\n--- Queue Sync Summary ---")
	summarize("A", storeA)
	summarize("B", storeB)
}

func dumpKeys(store *memory.StoreMemory) {
	all, _ := store.List(storemd.ListArgs{})
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })

	viewKeys := 0
	valueKeys := 0
	queueKeys := 0
	cursorKeys := 0
	otherKeys := 0

	for _, kv := range all {
		category := categorize(kv.Key)
		switch category {
		case "view":
			viewKeys++
		case "value":
			valueKeys++
		case "queue":
			queueKeys++
		case "cursor":
			cursorKeys++
		default:
			otherKeys++
		}

		val := kv.Value
		if len(val) > 80 {
			val = val[:77] + "..."
		}
		fmt.Printf("  [%-6s] %s = %s\n", category, kv.Key, val)
	}
	fmt.Printf("  --- total: %d keys (view:%d, value:%d, queue:%d, cursor:%d, other:%d)\n",
		len(all), viewKeys, valueKeys, queueKeys, cursorKeys, otherKeys)
}

func categorize(key string) string {
	switch {
	case strings.HasPrefix(key, "%sync%view%"):
		return "view"
	case strings.HasPrefix(key, "%sync%value%"):
		return "value"
	case strings.HasPrefix(key, "%sync%queue%"):
		return "queue"
	case strings.HasPrefix(key, "%sync%lastsync"):
		return "cursor"
	default:
		return "other"
	}
}

func summarize(name string, store *memory.StoreMemory) {
	all, _ := store.List(storemd.ListArgs{})

	viewKeys := 0
	valueKeys := 0
	queueKeys := 0
	cursorKeys := 0

	viewValueIDs := map[string]bool{}

	for _, kv := range all {
		switch {
		case strings.HasPrefix(kv.Key, "%sync%view%"):
			viewKeys++
			viewValueIDs[kv.Value] = true
		case strings.HasPrefix(kv.Key, "%sync%value%"):
			valueKeys++
		case strings.HasPrefix(kv.Key, "%sync%queue%"):
			queueKeys++
		case strings.HasPrefix(kv.Key, "%sync%lastsync"):
			cursorKeys++
		}
	}

	orphanedValues := 0
	for _, kv := range all {
		if strings.HasPrefix(kv.Key, "%sync%value%") {
			id := kv.Key[len("%sync%value%"):]
			if !viewValueIDs[id] {
				orphanedValues++
			}
		}
	}

	fmt.Printf("\n  Store %s: %d total keys\n", name, len(all))
	fmt.Printf("    view:   %d (unique user keys)\n", viewKeys)
	fmt.Printf("    value:  %d (%d orphaned — not referenced by any view key)\n", valueKeys, orphanedValues)
	fmt.Printf("    queue:  %d\n", queueKeys)
	fmt.Printf("    cursor: %d\n", cursorKeys)
	if orphanedValues > 0 {
		fmt.Printf("    ⚠  %d value keys are orphaned (old versions never cleaned up)\n", orphanedValues)
	}
	if queueKeys > viewKeys && queueKeys > 0 {
		fmt.Printf("    ⚠  %d queue entries vs %d unique keys — queue has stale entries from overwrites\n", queueKeys, viewKeys)
	}
}
