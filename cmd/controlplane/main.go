// snapvol-controlplane: REST API for volumes, snapshots and schedules.
// Usage: snapvol-controlplane -addr :8080 -data-dir /var/lib/snapvol -quota-bytes 1099511627776
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
	"github.com/ayushgawai/snapvol-csi/pkg/controlplane"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data-dir", "/var/lib/snapvol", "backing directory (local backend)")
	backendKind := flag.String("backend", "local", "local|fsx|azurefiles|gcpfilestore")
	quotaBytes := flag.Int64("quota-bytes", 1<<40, "total provisioned byte quota")
	quotaVols := flag.Int("quota-volumes", 1000, "max volumes")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	b, err := backend.New(*backendKind, *dataDir)
	if err != nil {
		slog.Error("backend", "err", err)
		os.Exit(1)
	}
	slog.Info("controlplane listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, controlplane.New(b, *quotaBytes, *quotaVols).Handler()); err != nil {
		slog.Error("http", "err", err)
		os.Exit(1)
	}
}
