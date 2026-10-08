// Command ss33 runs the S3-compatible server, or — when invoked as `mc` or `ss33 mc` — the bootstrap CLI.
//
// The server also accepts MinIO's invocation (`server <dir> [--address :9000] [--console-address ...]`,
// MINIO_ROOT_USER / MINIO_ROOT_PASSWORD) so an existing compose service can switch images without
// changing its command or environment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/midagedev/ss33/internal/mc"
	"github.com/midagedev/ss33/internal/server"
	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

func main() {
	if filepath.Base(os.Args[0]) == "mc" {
		os.Exit(mc.Main(os.Args[1:], os.Stdout, os.Stderr))
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve", "server":
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ss33:", err)
			os.Exit(1)
		}
	case "mc":
		os.Exit(mc.Main(os.Args[2:], os.Stdout, os.Stderr))
	case "healthcheck":
		addr := "http://127.0.0.1:9000"
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		resp, err := http.Get(strings.TrimSuffix(addr, "/") + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  ss33 serve [--addr :9000] [--data ./data] [--access-key K] [--secret-key S] [--region us-east-1] [--durable]
  ss33 server <data-dir> [--address :9000]      (MinIO-compatible invocation)
  ss33 mc <alias|mb|rb|ls|cp|anonymous|ready> ...  (also: run the binary as "mc")
  ss33 healthcheck [http://127.0.0.1:9000]`)
}

func boolFlag(a string) bool {
	name := strings.TrimLeft(a, "-")
	return name == "quiet" || name == "durable"
}

func envOr(keys []string, def string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", envOr([]string{"SS33_ADDR"}, ":9000"), "listen address")
	fs.StringVar(addr, "address", *addr, "listen address (MinIO spelling)")
	fs.String("console-address", "", "accepted for MinIO compatibility; ignored")
	data := fs.String("data", envOr([]string{"SS33_DATA"}, "./data"), "data directory")
	access := fs.String("access-key", envOr([]string{"SS33_ACCESS_KEY", "MINIO_ROOT_USER", "MINIO_ACCESS_KEY"}, "minioadmin"), "access key")
	secret := fs.String("secret-key", envOr([]string{"SS33_SECRET_KEY", "MINIO_ROOT_PASSWORD", "MINIO_SECRET_KEY"}, "minioadmin"), "secret key")
	region := fs.String("region", envOr([]string{"SS33_REGION", "MINIO_REGION"}, "us-east-1"), "region reported to clients")
	quiet := fs.Bool("quiet", os.Getenv("SS33_QUIET") != "", "do not log requests")
	durable := fs.Bool("durable", os.Getenv("SS33_DURABLE") != "", "fsync every write before acknowledging it (slower; off by default)")

	// MinIO style: `server /data --console-address :9001` — the first positional argument is the data dir.
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			*data = a
			continue
		}
		flagArgs = append(flagArgs, a)
		if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !boolFlag(a) {
			flagArgs = append(flagArgs, args[i+1])
			i++
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	st, err := store.Open(*data)
	if err != nil {
		return err
	}
	st.Durable = *durable
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: *access, SecretKey: *secret}, Region: *region}
	if !*quiet {
		srv.Log = logger
	}
	httpSrv := &http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()
	logger.Info("ss33 listening", "addr", *addr, "data", *data, "region", *region, "durable", *durable)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
