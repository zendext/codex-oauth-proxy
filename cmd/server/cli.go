package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type commandKind string

const (
	commandServe commandKind = "serve"
	commandAdmin commandKind = "admin"
)

type cliOptions struct {
	command    commandKind
	configPath string
	localModel bool
	adminArgs  []string
}

func parseCLI(args []string) (cliOptions, error) {
	if len(args) > 0 && args[0] == "admin" {
		return cliOptions{command: commandAdmin, adminArgs: append([]string(nil), args[1:]...)}, nil
	}
	if len(args) > 0 && args[0] == "serve" {
		return parseServeFlags(args[1:])
	}
	return parseServeFlags(args)
}

func parseServeFlags(args []string) (cliOptions, error) {
	fs := flag.NewFlagSet("codex-oauth-proxy serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := cliOptions{command: commandServe}
	fs.StringVar(&opts.configPath, "config", DefaultConfigPath, "Configuration file path")
	fs.BoolVar(&opts.localModel, "local-model", false, "Accepted for compatibility; codex-oauth-proxy uses embedded models")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}
	return opts, nil
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fmt.Fprintf(stdout, "codex-oauth-proxy Version: %s, Commit: %s, BuiltAt: %s\n", Version, Commit, BuildDate)
	opts, err := parseCLI(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	switch opts.command {
	case commandAdmin:
		if err = runAdmin(ctx, opts.adminArgs, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		return 0
	default:
		if err = runServe(ctx, opts, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		return 0
	}
}

func runServe(ctx context.Context, opts cliOptions, stdout io.Writer, stderr io.Writer) error {
	_ = opts.localModel
	_ = stderr
	configPath := opts.configPath
	if configPath == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		configPath = filepath.Join(wd, "config.yaml")
	}

	cfg, err := codexonly.LoadConfig(configPath)
	if err != nil {
		return err
	}
	handler, err := codexonly.NewHandler(ctx, cfg)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              codexonly.ListenAddr(cfg),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "codex-oauth-proxy listening on %s\n", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		fmt.Fprintf(stdout, "received %s, shutting down\n", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown failed: %w", err)
		}
	case err = <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server failed: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown failed: %w", err)
		}
	}
	return nil
}

func runAdmin(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	_ = stderr
	opts, rest, err := splitAdminFlags(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("admin requires a resource: users or usage")
	}
	client, err := newAdminClient(opts.baseURL)
	if err != nil {
		return err
	}
	switch rest[0] {
	case "users":
		return runAdminUsers(ctx, client, opts, rest[1:], stdout)
	case "usage":
		return runAdminUsage(ctx, client, opts, rest[1:], stdout)
	default:
		return fmt.Errorf("unknown admin resource %q", rest[0])
	}
}

func runAdminUsers(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	_ = ctx
	_ = client
	_ = opts
	_ = stdout
	return fmt.Errorf("admin users command requires user command handlers: %v", args)
}

func runAdminUsage(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	_ = ctx
	_ = client
	_ = opts
	_ = stdout
	return fmt.Errorf("admin usage command requires usage command handlers: %v", args)
}
