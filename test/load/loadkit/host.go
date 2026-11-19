package loadkit

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Host describes the machine a measurement ran on.
type Host struct {
	CPU         string // the chip or CPU model
	Cores       int    // logical CPUs
	PerfCores   int    // performance cores, on Apple silicon; 0 if unknown
	EffCores    int    // efficiency cores, on Apple silicon; 0 if unknown
	MemoryBytes int64  // physical memory; 0 if unknown
	OS          string // GOOS, with the OS version when known
	GoVersion   string
}

// DescribeHost reads the host's hardware with sysctl on macOS and from
// /proc on Linux. Fields it cannot read are left empty.
func DescribeHost(ctx context.Context) Host {
	h := Host{Cores: runtime.NumCPU(), OS: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version()}
	switch runtime.GOOS {
	case "darwin":
		h.CPU = sysctl(ctx, "machdep.cpu.brand_string")
		h.PerfCores, _ = strconv.Atoi(sysctl(ctx, "hw.perflevel0.physicalcpu"))
		h.EffCores, _ = strconv.Atoi(sysctl(ctx, "hw.perflevel1.physicalcpu"))
		h.MemoryBytes, _ = strconv.ParseInt(sysctl(ctx, "hw.memsize"), 10, 64)
		if v := sysctl(ctx, "kern.osproductversion"); v != "" {
			h.OS = "macOS " + v + " (" + runtime.GOARCH + ")"
		}
	case "linux":
		h.CPU = procField("/proc/cpuinfo", "model name")
		if kb, err := strconv.ParseInt(strings.TrimSuffix(procField("/proc/meminfo", "MemTotal"), " kB"), 10, 64); err == nil {
			h.MemoryBytes = kb << 10
		}
	}
	return h
}

// String renders h on one line, for reports.
func (h Host) String() string {
	cpu := h.CPU
	if cpu == "" {
		cpu = "unknown CPU"
	}
	cores := fmt.Sprintf("%d cores", h.Cores)
	if h.PerfCores > 0 {
		cores = fmt.Sprintf("%d cores (%d performance, %d efficiency)", h.Cores, h.PerfCores, h.EffCores)
	}
	mem := "unknown memory"
	if h.MemoryBytes > 0 {
		mem = fmt.Sprintf("%.0f GiB RAM", float64(h.MemoryBytes)/(1<<30))
	}
	return fmt.Sprintf("%s, %s, %s, %s, %s", cpu, cores, mem, h.OS, h.GoVersion)
}

// LoadAverage returns the 1-, 5- and 15-minute load averages as text, or
// "unknown". On a shared machine they say how much else was running.
func LoadAverage(ctx context.Context) string {
	switch runtime.GOOS {
	case "darwin":
		if v := strings.Trim(sysctl(ctx, "vm.loadavg"), "{} "); v != "" {
			return v
		}
	case "linux":
		if b, err := os.ReadFile("/proc/loadavg"); err == nil {
			if f := strings.Fields(string(b)); len(f) >= 3 {
				return strings.Join(f[:3], " ")
			}
		}
	}
	return "unknown"
}

// sysctl returns the value of a sysctl name, or "" if it cannot be read.
func sysctl(ctx context.Context, name string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", name).Output() // #nosec G204 -- fixed names
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// procField returns the value of the first "name: value" line of a /proc
// file, or "".
func procField(path, name string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Percentile returns the nearest-rank p-th percentile (0 < p <= 1) of the
// sorted values, or 0 for none.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// Ms formats d in milliseconds with microsecond precision.
func Ms(d time.Duration) string {
	return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond))
}
