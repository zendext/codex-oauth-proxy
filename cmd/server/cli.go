package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type cli struct {
	Serve serveCommand `cmd:"" default:"withargs" help:"Run the proxy server."`
	Admin adminCommand `cmd:"" help:"Manage users and usage on a running proxy server."`
}

type serveCommand struct {
	ConfigPath string `name:"config" default:"${default_config}" help:"Configuration file path."`
	LocalModel bool   `name:"local-model" help:"Accepted for compatibility; codex-oauth-proxy uses embedded models."`
}

type commandRuntime struct {
	ctx    context.Context
	stdout io.Writer
}

type parsedCLI struct {
	app           cli
	context       *kong.Context
	helpRequested bool
}

func parseCLI(args []string, stdout io.Writer, stderr io.Writer) (*parsedCLI, error) {
	parsed := &parsedCLI{}
	exitCode := -1
	parser, err := kong.New(
		&parsed.app,
		kong.Name("codex-oauth-proxy"),
		kong.Description("Proxy Codex CLI traffic using Codex OAuth credentials. Omit a command to run the proxy server."),
		kong.Vars{
			"default_admin_url": defaultAdminBaseURL,
			"default_config":    DefaultConfigPath,
		},
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) {
			exitCode = code
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("build CLI parser: %w", err)
	}
	parsed.context, err = parser.Parse(args)
	if exitCode == 0 {
		parsed.helpRequested = true
		return parsed, nil
	}
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	parsed, err := parseCLI(args, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if parsed.helpRequested {
		return 0
	}
	if err = parsed.context.Run(&commandRuntime{ctx: ctx, stdout: stdout}); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func (c *serveCommand) Run(runtime *commandRuntime) error {
	_ = c.LocalModel
	fmt.Fprintf(runtime.stdout, "codex-oauth-proxy Version: %s, Commit: %s, BuiltAt: %s\n", Version, Commit, BuildDate)
	configPath := c.ConfigPath
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
	handler, err := codexonly.NewHandler(runtime.ctx, cfg)
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
		fmt.Fprintf(runtime.stdout, "codex-oauth-proxy listening on %s\n", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		fmt.Fprintf(runtime.stdout, "received %s, shutting down\n", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown failed: %w", err)
		}
	case err = <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server failed: %w", err)
		}
	case <-runtime.ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown failed: %w", err)
		}
	}
	return nil
}
