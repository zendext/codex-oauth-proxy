package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

var (
	Version           = "dev"
	Commit            = "none"
	BuildDate         = "unknown"
	DefaultConfigPath = ""
)

func main() {
	fmt.Printf("codex-oauth-proxy Version: %s, Commit: %s, BuiltAt: %s\n", Version, Commit, BuildDate)

	configPath := ""
	localModel := false
	flag.StringVar(&configPath, "config", DefaultConfigPath, "Configuration file path")
	flag.BoolVar(&localModel, "local-model", false, "Accepted for compatibility; codex-oauth-proxy uses embedded models")
	flag.Parse()
	_ = localModel

	if configPath == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get working directory: %v\n", err)
			os.Exit(1)
		}
		configPath = filepath.Join(wd, "config.yaml")
	}

	cfg, err := codexonly.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	handler, err := codexonly.NewHandler(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              codexonly.ListenAddr(cfg),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("codex-oauth-proxy listening on %s\n", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		fmt.Printf("received %s, shutting down\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "shutdown failed: %v\n", err)
			os.Exit(1)
		}
	case err = <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "server failed: %v\n", err)
			os.Exit(1)
		}
	}
}
