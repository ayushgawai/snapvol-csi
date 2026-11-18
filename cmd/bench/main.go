// snapvol-bench measures CreateVolume / CreateSnapshot latency against the local backend.
//
//	snapvol-bench -ops 1000 -data-dir /tmp/bench
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
)

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[min(len(sorted)-1, int(p*float64(len(sorted))))]
}

func main() {
	ops := flag.Int("ops", 1000, "operations per benchmark")
	dir := flag.String("data-dir", "", "temp dir (default: os temp)")
	srcSize := flag.Int("src-bytes", 4096, "bytes written into the snapshot source volume")
	flag.Parse()
	if *dir == "" {
		d, err := os.MkdirTemp("", "snapvol-bench-")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(d)
		*dir = d
	}
	b, err := backend.NewLocal(*dir)
	if err != nil {
		panic(err)
	}

	var create []time.Duration
	for i := 0; i < *ops; i++ {
		start := time.Now()
		if _, err := b.CreateVolume(fmt.Sprintf("bench-vol-%d", i), 1<<20, ""); err != nil {
			panic(err)
		}
		create = append(create, time.Since(start))
	}

	// Snapshot source with a little real data in it.
	src, err := b.CreateVolume("bench-src", 1<<20, "")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(src.Path+"/data", make([]byte, *srcSize), 0o644); err != nil {
		panic(err)
	}
	var snap []time.Duration
	for i := 0; i < *ops; i++ {
		start := time.Now()
		if _, err := b.CreateSnapshot(fmt.Sprintf("bench-snap-%d", i), src.ID); err != nil {
			panic(err)
		}
		snap = append(snap, time.Since(start))
	}

	fmt.Printf("snapvol-bench ops=%d backend=local src_bytes=%d\n", *ops, *srcSize)
	fmt.Printf("CreateVolume  p50=%s p99=%s\n", pct(create, .50), pct(create, .99))
	fmt.Printf("CreateSnapshot p50=%s p99=%s\n", pct(snap, .50), pct(snap, .99))
}
