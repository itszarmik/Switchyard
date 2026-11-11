package telemetry

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/version"
)

func TestNewRegistry(t *testing.T) {
	reg := NewRegistry()

	if prometheus.Gatherer(reg) == prometheus.DefaultGatherer || prometheus.Registerer(reg) == prometheus.DefaultRegisterer {
		t.Fatal("NewRegistry returned the global default registry")
	}
	if other := NewRegistry(); other == reg {
		t.Fatal("NewRegistry returned the same registry twice")
	}

	info := version.Get()
	want := fmt.Sprintf(`
# HELP switchyard_build_info Build metadata of the running binary; the value is always 1.
# TYPE switchyard_build_info gauge
switchyard_build_info{commit=%q,go_version=%q,version=%q} 1
`, info.Commit, info.GoVersion, info.Version)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "switchyard_build_info"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		metric string
	}{
		{"go_goroutines"},
		{"process_start_time_seconds"},
	}
	for _, tt := range tests {
		t.Run(tt.metric, func(t *testing.T) {
			n, err := testutil.GatherAndCount(reg, tt.metric)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatalf("%s not registered", tt.metric)
			}
		})
	}

	// Nothing leaked into the global registry.
	if n, err := testutil.GatherAndCount(prometheus.DefaultGatherer, "switchyard_build_info"); err != nil || n != 0 {
		t.Fatalf("global registry has %d switchyard_build_info series (err %v), want 0", n, err)
	}
}
