package main

import (
	"context"
	"flag"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/agent"
)

func main() {
	var (
		addr            string
		sandboxID       api.SandboxID
		controlPlanePub string
		installTo       string
	)
	flag.StringVar(&addr, "listen", ":9090", "HTTP listen address")
	flag.TextVar(&sandboxID, "sandbox-id", sandboxID, "this sandbox's id; JWT sub must match")
	flag.StringVar(&controlPlanePub, "controlplane-public-key", "", "base64 SPKI RSA public key of the control plane (empty = no auth)")
	flag.StringVar(&installTo, "install-to", "", "copy own executable to this path and exit (for init-container injection into scratch)")
	flag.Parse()

	// init-container mode: copy this static binary to a shared volume and exit.
	// This lets a scratch image inject the agent without needing `cp` in the image.
	if installTo != "" {
		if err := installSelf(installTo); err != nil {
			log.Fatalf("install: %v", err)
		}
		return
	}

	agentSrv, err := agent.NewServer(sandboxID, controlPlanePub)
	if err != nil {
		log.Fatalf("agent server: %v", err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           agentSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("lean-sandbox agent listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("agent server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
}

// installSelf copies the currently running executable to dst, creating parent
// dirs as needed. Used by the sandbox pod's init container to place the agent
// binary into a shared emptyDir volume from a scratch image.
func installSelf(dst string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
