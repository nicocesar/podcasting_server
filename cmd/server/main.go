// The podcasting server: private podcast feeds for AntennaPod, published
// by an external Generator. See README.md and CONTEXT.md.
package main

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/nicocesar/podcasting_server/internal/httpapi"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
	"github.com/nicocesar/podcasting_server/internal/store/gcpstore"
)

// The Public Surface pages ship inside the binary (ADR 0003).
//
//go:embed templates static
var assetsFS embed.FS

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func credential(key string) (string, error) {
	v := os.Getenv(key)
	if !strings.Contains(v, ":") {
		return "", fmt.Errorf(`%s must be set to "user:password"`, key)
	}
	return v, nil
}

func run(log *slog.Logger) error {
	ctx := context.Background()

	reader, err := credential("READER_CREDENTIALS")
	if err != nil {
		return err
	}
	writer, err := credential("WRITER_CREDENTIALS")
	if err != nil {
		return err
	}

	var st store.Store
	backend := env("STORAGE", "fs")
	switch backend {
	case "fs":
		dataDir := env("DATA_DIR", "./data")
		st, err = fsstore.New(dataDir)
		if err != nil {
			return err
		}
		log.Info("storage: filesystem (dev only)", "dir", dataDir)
	case "gcp":
		bucket := os.Getenv("GCS_BUCKET")
		if bucket == "" {
			return fmt.Errorf("GCS_BUCKET must be set when STORAGE=gcp")
		}
		st, err = gcpstore.New(ctx, os.Getenv("GCP_PROJECT"), bucket)
		if err != nil {
			return err
		}
		log.Info("storage: datastore + gcs", "bucket", bucket)
	default:
		return fmt.Errorf("unknown STORAGE %q (want fs or gcp)", backend)
	}

	handler, err := httpapi.New(httpapi.Config{
		Store:       st,
		BaseURL:     os.Getenv("BASE_URL"),
		ReaderCreds: reader,
		WriterCreds: writer,
		Assets:      assetsFS,
		Logger:      log,
	})
	if err != nil {
		return err
	}

	addr := ":" + env("PORT", "8080")
	log.Info("listening", "addr", addr)
	return http.ListenAndServe(addr, handler)
}
