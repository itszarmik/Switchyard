package semantic

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// maxPartitionBytes bounds a partition; the gateway's are 64 hex digits.
const maxPartitionBytes = 256

// encodeVector encodes vec as little-endian float32s, the layout of a
// FLOAT32 vector field.
func encodeVector(vec []float32) []byte {
	buf := make([]byte, 0, 4*len(vec))
	for _, f := range vec {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
	}
	return buf
}

// checkVector rejects a vector the index cannot compare: one whose length
// is not dims (when dims > 0), and one that is empty, has a non-finite
// value or has zero norm, whose cosine similarity is undefined. It returns
// the vector's norm.
func checkVector(vec []float32, dims int) (float64, error) {
	if dims > 0 && len(vec) != dims {
		return 0, fmt.Errorf("semantic: vector has %d dimensions, the index holds %d", len(vec), dims)
	}
	var sum float64
	for _, f := range vec {
		v := float64(f)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, errors.New("semantic: vector has a non-finite value")
		}
		sum += v * v
	}
	if sum == 0 {
		return 0, errors.New("semantic: vector is empty or zero")
	}
	return math.Sqrt(sum), nil
}

// checkPartition rejects a partition that a Redis TAG field cannot hold
// exactly: an empty or overlong one, and one with a byte outside printable
// ASCII or a comma (the tag separator). The gateway's partitions are hex.
func checkPartition(p string) error {
	if p == "" || len(p) > maxPartitionBytes {
		return fmt.Errorf("semantic: partition must have 1 to %d bytes", maxPartitionBytes)
	}
	for i := range len(p) {
		if c := p[i]; c <= ' ' || c > '~' || c == ',' {
			return errors.New("semantic: partition must be printable ASCII without spaces or commas")
		}
	}
	return nil
}
