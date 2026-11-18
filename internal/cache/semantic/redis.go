package semantic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"switchyard/internal/cache"
)

// keyPrefix starts every semantic-cache index name and document key.
const keyPrefix = "sy:s:v1:"

// Document fields: the partition TAG, the FLOAT32 vector and the encoded
// entry. distField is the KNN distance's alias in queries.
const (
	partField = "p"
	vecField  = "v"
	entField  = "e"
	distField = "d"
)

// RedisIndex is a cache.VectorIndex on the Redis query engine: an HNSW
// index with cosine distance over HASH documents, filtered by an exact
// partition TAG. Every embedding model and dimension has its own index, so
// vector spaces never mix.
type RedisIndex struct {
	client *redis.Client
	name   string // the index name
	prefix string // the documents' key prefix
	dims   int
	ready  atomic.Bool // the index is known to exist
}

var _ cache.VectorIndex = (*RedisIndex)(nil)

// NewRedisIndex returns the index of embeddingModel (model@provider) and
// dims on client, normally the cache instance. It is named
// sy:s:v1:<slug>:<dims>, with documents under sy:s:v1:<slug>:<dims>:,
// where slug is embeddingModel made safe for key and index names. It is
// created on first use.
//
// The query engine indexes logical database 0 only, so client must use
// it. Every call honors ctx's deadline only when client was built with
// redis.Options.ContextTimeoutEnabled, which the gateway sets so that a
// hung Redis costs at most the lookup timeout. Both RESP2 and RESP3
// clients work.
func NewRedisIndex(client *redis.Client, embeddingModel string, dims int) *RedisIndex {
	name := keyPrefix + slug(embeddingModel) + ":" + strconv.Itoa(dims)
	return &RedisIndex{client: client, name: name, prefix: name + ":", dims: dims}
}

// Name returns the index name.
func (ix *RedisIndex) Name() string { return ix.name }

// slug maps a deployment ID to a string of [A-Za-z0-9.-] and escapes, one
// to one, so that two deployments never share an index: '_' is "__", ':'
// is "_c", '/' is "_s", '@' is "_a", and any other byte "_xHH".
func slug(s string) string {
	var b strings.Builder
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteByte(c)
		case c == '_':
			b.WriteString("__")
		case c == ':':
			b.WriteString("_c")
		case c == '/':
			b.WriteString("_s")
		case c == '@':
			b.WriteString("_a")
		default:
			fmt.Fprintf(&b, "_x%02x", c)
		}
	}
	return b.String()
}

// Search returns the entry of partition nearest to vec by cosine
// distance, or nil when the partition has none. If the index has gone
// missing (dropped, or lost with the Redis data), it is created again and
// the search retried once; documents still stored are re-indexed in the
// background, so that search may miss them.
func (ix *RedisIndex) Search(ctx context.Context, partition string, vec []float32) (*cache.Match, error) {
	if err := checkPartition(partition); err != nil {
		return nil, err
	}
	if _, err := checkVector(vec, ix.dims); err != nil {
		return nil, err
	}
	if err := ix.ensure(ctx); err != nil {
		return nil, err
	}
	m, err := ix.search(ctx, partition, vec)
	if err != nil && isMissingIndex(err) {
		ix.ready.Store(false)
		if err := ix.ensure(ctx); err != nil {
			return nil, err
		}
		m, err = ix.search(ctx, partition, vec)
	}
	return m, err
}

// Add stores e with vec in partition for ttl, as a HASH under a random key
// that expires with the entry. The HSET and PEXPIRE run in one MULTI, so a
// document never lives without its TTL.
func (ix *RedisIndex) Add(ctx context.Context, partition string, vec []float32, e *cache.Entry, ttl time.Duration) error {
	if err := checkPartition(partition); err != nil {
		return err
	}
	if _, err := checkVector(vec, ix.dims); err != nil {
		return err
	}
	if e == nil {
		return errors.New("semantic: nil entry")
	}
	if ttl < time.Millisecond {
		return errors.New("semantic: ttl must be at least 1ms")
	}
	if err := ix.ensure(ctx); err != nil {
		return err
	}
	var id [16]byte
	_, _ = rand.Read(id[:]) // never fails (crypto/rand)
	key := ix.prefix + hex.EncodeToString(id[:])
	_, err := ix.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, key, partField, partition, vecField, encodeVector(vec), entField, cache.MarshalEntry(e))
		p.PExpire(ctx, key, ttl)
		return nil
	})
	if err != nil {
		return fmt.Errorf("semantic: adding to %s: %w", ix.name, err)
	}
	return nil
}

// ensure creates the index unless it is known to exist. An index that
// already exists counts as created, so concurrent first calls are safe.
func (ix *RedisIndex) ensure(ctx context.Context) error {
	if ix.ready.Load() {
		return nil
	}
	err := ix.client.Do(ctx, "FT.CREATE", ix.name, "ON", "HASH", "PREFIX", 1, ix.prefix,
		"SCHEMA", partField, "TAG", "CASESENSITIVE",
		vecField, "VECTOR", "HNSW", 6, "TYPE", "FLOAT32", "DIM", ix.dims, "DISTANCE_METRIC", "COSINE",
	).Err()
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "index already exists") {
		return fmt.Errorf("semantic: creating index %s: %w", ix.name, err)
	}
	ix.ready.Store(true)
	return nil
}

// isMissingIndex reports whether err is the query engine's answer for an
// index that does not exist: "SEARCH_INDEX_NOT_FOUND Index not found" in
// Redis 8, "no such index" or "Unknown index name" before.
func isMissingIndex(err error) bool {
	var re redis.Error
	if !errors.As(err, &re) {
		return false
	}
	msg := strings.ToLower(re.Error())
	return strings.Contains(msg, "index not found") || strings.Contains(msg, "no such index") ||
		strings.Contains(msg, "unknown index name")
}

// search runs the KNN 1 query of vec within partition.
func (ix *RedisIndex) search(ctx context.Context, partition string, vec []float32) (*cache.Match, error) {
	query := "(@" + partField + ":{" + escapeTag(partition) + "})=>[KNN 1 @" + vecField + " $vec AS " + distField + "]"
	res, err := ix.client.Do(ctx, "FT.SEARCH", ix.name, query,
		"PARAMS", 2, "vec", encodeVector(vec),
		"SORTBY", distField, "RETURN", 2, entField, distField, "DIALECT", 2,
	).Result()
	if err != nil {
		return nil, fmt.Errorf("semantic: searching %s: %w", ix.name, err)
	}
	fields, ok, err := firstResult(res)
	if err != nil || !ok {
		return nil, err
	}
	enc, okE := fields[entField]
	dist, okD := fields[distField]
	if !okE || !okD {
		// The document expired between the index and the read.
		return nil, nil
	}
	d, err := strconv.ParseFloat(dist, 64)
	if err != nil {
		return nil, fmt.Errorf("semantic: searching %s: bad distance: %w", ix.name, err)
	}
	e, err := cache.UnmarshalEntry([]byte(enc))
	switch {
	case errors.Is(err, cache.ErrEntryVersion):
		// Stored before an upgrade: no match.
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &cache.Match{Entry: e, Similarity: 1 - d}, nil
}

// escapeTag escapes every byte of a (checkPartition-valid) partition that
// the TAG query syntax treats as special: all but letters, digits and '_'.
func escapeTag(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if !isWordByte(c) {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// isWordByte reports whether c is a letter, a digit or '_'.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// errBadReply reports an FT.SEARCH reply of an unexpected shape.
var errBadReply = errors.New("semantic: unexpected FT.SEARCH reply")

// firstResult returns the returned fields of the first document of an
// FT.SEARCH reply, and false when there is none. It reads both the RESP2
// reply, [total, id, [field, value, ...], ...], and the RESP3 one, a map
// whose "results" holds maps with an "extra_attributes" map.
func firstResult(res any) (map[string]string, bool, error) {
	switch r := res.(type) {
	case []any:
		if len(r) < 3 {
			return nil, false, nil
		}
		pairs, ok := r[2].([]any)
		if !ok {
			// A document read after it expired has nil fields.
			return nil, false, nil
		}
		fields := make(map[string]string, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			k, ok1 := pairs[i].(string)
			v, ok2 := pairs[i+1].(string)
			if !ok1 || !ok2 {
				return nil, false, errBadReply
			}
			fields[k] = v
		}
		return fields, true, nil
	case map[any]any:
		results, ok := r["results"].([]any)
		if !ok {
			return nil, false, errBadReply
		}
		if len(results) == 0 {
			return nil, false, nil
		}
		doc, ok := results[0].(map[any]any)
		if !ok {
			return nil, false, errBadReply
		}
		attrs, ok := doc["extra_attributes"].(map[any]any)
		if !ok {
			return nil, false, nil
		}
		fields := make(map[string]string, len(attrs))
		for k, v := range attrs {
			ks, ok1 := k.(string)
			vs, ok2 := v.(string)
			if !ok1 || !ok2 {
				return nil, false, errBadReply
			}
			fields[ks] = vs
		}
		return fields, true, nil
	}
	return nil, false, errBadReply
}
