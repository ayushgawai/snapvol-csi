// snapvol-csi: CSI driver (Identity/Controller/Node) + /metrics + probes.
// Usage: snapvol-csi -endpoint unix:///tmp/csi.sock -data-dir /var/lib/snapvol -backend local
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
	driver "github.com/ayushgawai/snapvol-csi/pkg/csi"
)

func main() {
	endpoint := flag.String("endpoint", "unix:///var/lib/kubelet/plugins/csi.snapvol.dev/csi.sock", "CSI endpoint")
	dataDir := flag.String("data-dir", "/var/lib/snapvol", "backing directory (local backend)")
	backendKind := flag.String("backend", "local", "local|fsx|azurefiles|gcpfilestore")
	nodeID := flag.String("node-id", "", "node id (default hostname)")
	httpAddr := flag.String("http-addr", ":9808", "metrics/probe address (empty disables)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	b, err := backend.New(*backendKind, *dataDir)
	if err != nil {
		slog.Error("backend", "err", err)
		os.Exit(1)
	}
	if *httpAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ready")) })
		go func() { _ = http.ListenAndServe(*httpAddr, mux) }()
	}
	if err := driver.New(b, *nodeID).Run(*endpoint); err != nil {
		slog.Error("csi", "err", err)
		os.Exit(1)
	}
}
