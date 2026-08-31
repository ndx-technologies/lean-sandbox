package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ndx-technologies/lean-sandbox/internal/controlplane"
)

func main() {
	var (
		addr      string
		apiKey    string
		options   controlplane.Options
		warmFlags flagArray
	)
	flag.StringVar(&addr, "listen", ":8080", "HTTP listen address")
	flag.StringVar(&apiKey, "api-key", "", "require this key in X-Api-Key header (empty = no auth)")
	flag.StringVar(&options.Namespace, "namespace", "opensandbox", "namespace for sandbox pods")
	flag.IntVar(&options.AgentPort, "agent-port", 9090, "agent container port")
	flag.StringVar(&options.AgentImage, "agent-image", "", "image carrying the agent binary (required)")
	flag.StringVar(&options.AccessToken, "access-token", "", "token handed to agents (also required by SDK)")
	flag.DurationVar(&options.LeaseTTL, "ttl", 15*time.Minute, "sandbox lease: reclaimed when no KeepAlive for this long")
	flag.Var(&warmFlags, "warm-image", "keep N ready pods warm for image, e.g. -warm-image ubuntu:22.04=2 (repeatable)")
	flag.Parse()

	options.WarmImages = parseWarmImages(warmFlags.values)

	cp, err := controlplane.New(options)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go cp.Run(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           controlplane.NewServer(cp, apiKey).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("lean-sandbox controlplane listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("controlplane server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatal(err)
	}
}

type flagArray struct{ values []string }

func (f *flagArray) String() string { return strings.Join(f.values, ",") }
func (f *flagArray) Set(v string) error {
	f.values = append(f.values, v)
	return nil
}

// parseWarmImages parses repeated -warm-image image=count flags.
func parseWarmImages(vals []string) []controlplane.WarmImage {
	var out []controlplane.WarmImage
	for _, v := range vals {
		img, count := v, 1
		if before, after, ok := strings.Cut(v, "="); ok {
			img = before
			if n, err := strconv.Atoi(after); err == nil && n > 0 {
				count = n
			}
		}
		out = append(out, controlplane.WarmImage{Image: img, Min: count})
	}
	return out
}
