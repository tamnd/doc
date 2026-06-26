package engine

import (
	"fmt"
	"testing"

	"github.com/tamnd/doc/bson"
)

// This file extends the §17.2 crash harness to a mixed insert/update/delete
// workload. The insert-only test in crash_test.go proves the durable prefix is
// gap-free; this one proves the stronger property for arbitrary mutations: the
// recovered state at any fsync boundary is exactly the committed state after some
// whole number of transactions, and that number is at least the count that had
// been acknowledged durable. There is no half-applied update, no resurrected
// delete, and no torn document.

// mixedRNG is a small deterministic xorshift so the workload is reproducible from
// a seed; the suite never depends on the test framework's randomness.
type mixedRNG uint64

func (r *mixedRNG) next() uint64 {
	x := uint64(*r)
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	*r = mixedRNG(x)
	return x
}

func (r *mixedRNG) intn(n int) int { return int(r.next() % uint64(n)) }

// TestCrashRecoveryMixedWorkload runs inserts, updates, and deletes, snapshots the
// expected document set after each commit, and checks that recovery at every fsync
// boundary lands on one of the committed states in the window between the last
// acknowledged commit and the last issued commit.
func TestCrashRecoveryMixedWorkload(t *testing.T) {
	const dbName, collName = "ledger", "entries"
	const path = "crashmix.doc"
	ops := crashScale(t, 150)

	cfs := newCrashFS(path)
	e, err := Open(cfs, path, crashOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c, err := e.CreateCollection(dbName, collName)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// state tracks the live documents by _id; states[k] is a copy of state after
	// k committed transactions, and commitAck[k] is the durable-image count when
	// commit k returned.
	state := map[int]string{}
	states := []map[int]string{cloneState(state)}
	commitAck := []int{cfs.syncCount()}

	rng := mixedRNG(0x9e3779b97f4a7c15)
	nextID := 1
	var live []int

	record := func() {
		states = append(states, cloneState(state))
		commitAck = append(commitAck, cfs.syncCount())
	}

	for i := 0; i < ops; i++ {
		switch {
		case len(live) == 0 || rng.intn(100) < 50:
			// Insert a fresh document.
			id := nextID
			nextID++
			val := mixedValue(id, 0)
			if _, err := c.InsertOne(mixedDoc(id, val)); err != nil {
				t.Fatalf("insert %d: %v", id, err)
			}
			state[id] = val
			live = append(live, id)
		case rng.intn(100) < 70:
			// Update an existing document to a new generation value.
			id := live[rng.intn(len(live))]
			val := mixedValue(id, int(rng.next()%1000)+1)
			if _, err := c.UpdateOne(
				bson.NewBuilder().AppendInt32("_id", int32(id)).Build(),
				bson.NewBuilder().AppendDocument("$set",
					bson.NewBuilder().AppendString("v", val).Build()).Build(),
			); err != nil {
				t.Fatalf("update %d: %v", id, err)
			}
			state[id] = val
		default:
			// Delete an existing document.
			pos := rng.intn(len(live))
			id := live[pos]
			if _, err := c.DeleteOne(bson.NewBuilder().AppendInt32("_id", int32(id)).Build()); err != nil {
				t.Fatalf("delete %d: %v", id, err)
			}
			delete(state, id)
			live = append(live[:pos], live[pos+1:]...)
		}
		record()
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	images := cfs.images()
	for idx, img := range images {
		verifyMixedImage(t, img, path, dbName, collName, states, commitAck, idx)
	}
	t.Logf("verified %d fsync boundaries across %d mixed ops", len(images), ops)
}

// verifyMixedImage reopens one image and asserts the recovered document set equals
// some committed snapshot states[k] with k at least the acknowledged-durable count.
func verifyMixedImage(t *testing.T, img crashImage, path, db, coll string, states []map[int]string, commitAck []int, idx int) {
	t.Helper()
	fs := loadCrashFS(img, path)
	e, err := Open(fs, path, crashOptions())
	if err != nil {
		t.Fatalf("image %d: reopen: %v", idx, err)
	}
	defer e.Close()

	recovered := map[int]string{}
	if c := e.GetCollection(db, coll); c != nil {
		docs, err := c.Find(bson.NewBuilder().Build())
		if err != nil {
			t.Fatalf("image %d: find: %v", idx, err)
		}
		for _, d := range docs {
			id, _ := d.Lookup("_id")
			v, _ := d.Lookup("v")
			recovered[int(id.Int32())] = v.StringValue()
		}
	}

	// ackedK is the highest commit index known durable by this boundary.
	ackedK := 0
	for k, boundary := range commitAck {
		if boundary <= idx+1 {
			ackedK = k
		}
	}

	matched := -1
	for k := len(states) - 1; k >= ackedK; k-- {
		if sameState(recovered, states[k]) {
			matched = k
			break
		}
	}
	if matched < 0 {
		t.Fatalf("image %d: recovered state (%d docs) matches no committed snapshot at or beyond the acknowledged commit %d; recovery produced a state no prefix of the log ever held", idx, len(recovered), ackedK)
	}
	if rep := e.Check(true); !rep.Valid {
		t.Fatalf("image %d: doc check failed after recovery: %+v", idx, rep)
	}
}

func cloneState(s map[int]string) map[int]string {
	out := make(map[int]string, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

func sameState(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func mixedDoc(id int, val string) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", int32(id)).AppendString("v", val).Build()
}

func mixedValue(id, gen int) string { return fmt.Sprintf("d%d-g%d", id, gen) }
