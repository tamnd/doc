package sys

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync/atomic"
)

// ObjectID is the 12-byte MongoDB ObjectId: a 4-byte big-endian seconds-since-
// epoch timestamp, a 5-byte per-process random value, and a 3-byte big-endian
// counter that increments per generated id. The layout matches the MongoDB
// ObjectId specification so that ids minted by doc are interchangeable with ids
// minted by a Mongo driver.
type ObjectID [12]byte

// Hex returns the 24-character lowercase hexadecimal form of the id, the
// canonical textual representation used by drivers and the shell.
func (o ObjectID) Hex() string { return hex.EncodeToString(o[:]) }

// Timestamp returns the seconds-since-epoch encoded in the first four bytes.
func (o ObjectID) Timestamp() uint32 { return binary.BigEndian.Uint32(o[0:4]) }

// IsZero reports whether the id is the all-zero value.
func (o ObjectID) IsZero() bool { return o == ObjectID{} }

// IDGenerator mints ObjectIds. It is a seam so that tests can produce
// deterministic ids (fixed timestamp, fixed random prefix, predictable counter)
// while production uses real time and a cryptographically random process prefix.
// Implementations must be safe for concurrent use.
type IDGenerator interface {
	// NewID returns a fresh, unique ObjectId. Successive calls within the same
	// second differ only in the counter; the counter must not repeat within a
	// process for a given random prefix.
	NewID() ObjectID
}

// ObjectIDGenerator is the production IDGenerator. It draws its 5-byte process
// prefix once from crypto/rand, stamps each id with the supplied Clock, and
// advances an atomic counter seeded randomly to avoid collisions across short-
// lived processes that share a wall-clock second.
type ObjectIDGenerator struct {
	clock   Clock
	prefix  [5]byte
	counter atomic.Uint32
}

// NewObjectIDGenerator returns a generator stamping ids with clock. If clock is
// nil it falls back to SystemClock. It panics only if the system CSPRNG is
// unavailable, which is treated as a non-recoverable environment failure.
func NewObjectIDGenerator(clock Clock) *ObjectIDGenerator {
	if clock == nil {
		clock = SystemClock{}
	}
	g := &ObjectIDGenerator{clock: clock}
	var seed [9]byte // 5 prefix bytes + 4 counter-seed bytes
	if _, err := rand.Read(seed[:]); err != nil {
		panic("doc/sys: cannot read system CSPRNG: " + err.Error())
	}
	copy(g.prefix[:], seed[0:5])
	g.counter.Store(binary.BigEndian.Uint32(seed[5:9]))
	return g
}

// NewID returns a fresh ObjectId.
func (g *ObjectIDGenerator) NewID() ObjectID {
	var id ObjectID
	binary.BigEndian.PutUint32(id[0:4], uint32(g.clock.Now().Unix()))
	copy(id[4:9], g.prefix[:])
	// The 3-byte counter is the low 24 bits of an atomically incremented u32.
	c := g.counter.Add(1)
	id[9] = byte(c >> 16)
	id[10] = byte(c >> 8)
	id[11] = byte(c)
	return id
}

// FixedIDGenerator is a deterministic IDGenerator for tests. It stamps every id
// with the same timestamp and prefix and a counter that starts at zero and
// increments by one per call, so a test can predict the exact sequence of ids it
// will observe.
type FixedIDGenerator struct {
	Timestamp uint32
	Prefix    [5]byte
	counter   atomic.Uint32
}

// NewID returns the next deterministic id.
func (g *FixedIDGenerator) NewID() ObjectID {
	var id ObjectID
	binary.BigEndian.PutUint32(id[0:4], g.Timestamp)
	copy(id[4:9], g.Prefix[:])
	c := g.counter.Add(1)
	id[9] = byte(c >> 16)
	id[10] = byte(c >> 8)
	id[11] = byte(c)
	return id
}
