package config

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func unmarshalScalar(t *testing.T, src string, u yaml.Unmarshaler) error {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return u.UnmarshalYAML(n.Content[0])
}

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		src     string
		want    time.Duration
		wantErr string
	}{
		{`60s`, 60 * time.Second, ""},
		{`2m30s`, 150 * time.Second, ""},
		{`"150ms"`, 150 * time.Millisecond, ""},
		{`0s`, 0, ""},
		{`~`, 0, ""},
		{`60`, 0, `expected a duration such as "60s"`},
		{`0`, 0, `expected a duration such as "60s"`},
		{`"60"`, 0, `expected a duration such as "60s"`},
		{`1.5`, 0, `expected a duration such as "60s"`},
		{`soon`, 0, `expected a duration such as "60s"`},
		{`-5s`, 0, "must not be negative"},
		{`[1s]`, 0, `expected a duration such as "60s"`},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			var d Duration
			err := unmarshalScalar(t, tt.src, &d)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Std() != tt.want {
				t.Errorf("got %v, want %v", d.Std(), tt.want)
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	for _, d := range []Duration{0, Duration(time.Second), Duration(150 * time.Millisecond), Duration(2*time.Hour + 3*time.Second)} {
		t.Run(d.String(), func(t *testing.T) {
			out, err := yaml.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			var back Duration
			if err := yaml.Unmarshal(out, &back); err != nil {
				t.Fatalf("unmarshal %q: %v", out, err)
			}
			if back != d {
				t.Errorf("round trip %v -> %q -> %v", d, out, back)
			}
		})
	}
}

func TestByteSizeUnmarshal(t *testing.T) {
	tests := []struct {
		src     string
		want    int64
		wantErr string
	}{
		{`0`, 0, ""},
		{`1500`, 1500, ""},
		{`10MiB`, 10 << 20, ""},
		{`64 KiB`, 64 << 10, ""},
		{`1GB`, 1_000_000_000, ""},
		{`"2 MB"`, 2_000_000, ""},
		{`5KB`, 5000, ""},
		{`1GiB`, 1 << 30, ""},
		{`7B`, 7, ""},
		{`~`, 0, ""},
		{strconv.FormatInt(math.MaxInt64, 10), math.MaxInt64, ""},
		{`1.5MiB`, 0, "expected a byte size"},
		{`-1`, 0, "expected a byte size"},
		{`-1KB`, 0, "expected a byte size"},
		{`10  MiB`, 0, "expected a byte size"},
		{`10mib`, 0, "unknown byte size unit"},
		{`10TB`, 0, "unknown byte size unit"},
		{`10kb`, 0, "unknown byte size unit"},
		{`MiB`, 0, "expected a byte size"},
		{`0x10`, 0, "expected a byte size"},
		{`9223372036854775808`, 0, "too large"},
		{`9000000000GiB`, 0, "too large"},
		{`{a: 1}`, 0, "expected a byte size"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			var b ByteSize
			err := unmarshalScalar(t, tt.src, &b)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.Int64() != tt.want {
				t.Errorf("got %d, want %d", b.Int64(), tt.want)
			}
		})
	}
}

func TestByteSizeString(t *testing.T) {
	tests := []struct {
		in   ByteSize
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{1500, "1500B"},
		{1000, "1KB"},
		{1024, "1KiB"},
		{10485760, "10MiB"},
		{1024000, "1000KiB"},
		{1_000_000, "1MB"},
		{1 << 30, "1GiB"},
		{1_000_000_000, "1GB"},
		{3 << 30, "3GiB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Errorf("String(%d) = %q, want %q", int64(tt.in), got, tt.want)
			}
			out, err := yaml.Marshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			var back ByteSize
			if err := yaml.Unmarshal(out, &back); err != nil {
				t.Fatalf("unmarshal %q: %v", out, err)
			}
			if back != tt.in {
				t.Errorf("round trip %d -> %q -> %d", int64(tt.in), out, int64(back))
			}
		})
	}
}
