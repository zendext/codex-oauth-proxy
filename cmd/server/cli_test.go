package main

import "testing"

func TestParseCLILegacyServeFlags(t *testing.T) {
	opts, err := parseCLI([]string{"--config", "config.yaml", "--local-model"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandServe {
		t.Fatalf("command = %s, want %s", opts.command, commandServe)
	}
	if opts.configPath != "config.yaml" {
		t.Fatalf("configPath = %q, want config.yaml", opts.configPath)
	}
	if !opts.localModel {
		t.Fatalf("localModel = false, want true")
	}
}

func TestParseCLIExplicitServe(t *testing.T) {
	opts, err := parseCLI([]string{"serve", "--config", "config.yaml"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandServe || opts.configPath != "config.yaml" {
		t.Fatalf("opts = %#v, want serve with config.yaml", opts)
	}
}

func TestParseCLIAdminKeepsRemainingArgs(t *testing.T) {
	opts, err := parseCLI([]string{"admin", "users", "list", "--url", "http://127.0.0.1:8318"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandAdmin {
		t.Fatalf("command = %s, want %s", opts.command, commandAdmin)
	}
	if got := len(opts.adminArgs); got != 4 {
		t.Fatalf("adminArgs length = %d, want 4: %#v", got, opts.adminArgs)
	}
}
