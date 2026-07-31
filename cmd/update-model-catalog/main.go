package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	defaultSourceBase = "https://raw.githubusercontent.com/openai/codex"
	modelCatalogPath  = "codex-rs/models-manager/models.json"
	maxCatalogBytes   = 4 << 20
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("update-model-catalog", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	ref := flags.String("ref", "", "explicit openai/codex Git ref")
	output := flags.String("output", "internal/codexonly/codex_client_models.json", "output path")
	sourceBase := flags.String("source-base", defaultSourceBase, "source base URL")
	check := flags.Bool("check", false, "verify the output already matches")
	if err := flags.Parse(args); err != nil {
		return err
	}
	*ref = strings.TrimSpace(*ref)
	*output = strings.TrimSpace(*output)
	*sourceBase = strings.TrimRight(strings.TrimSpace(*sourceBase), "/")
	if *ref == "" || *output == "" || *sourceBase == "" {
		return errors.New("--ref, --output, and --source-base must be non-empty")
	}

	sourceURL := *sourceBase + "/" + path.Clean(*ref) + "/" + modelCatalogPath
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(sourceURL)
	if err != nil {
		return fmt.Errorf("download model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download model catalog: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return fmt.Errorf("read model catalog: %w", err)
	}
	if len(data) > maxCatalogBytes {
		return fmt.Errorf("model catalog exceeds %d bytes", maxCatalogBytes)
	}
	if err = validateCatalog(data); err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}

	if *check {
		current, errRead := os.ReadFile(*output)
		if errRead != nil {
			return fmt.Errorf("read current model catalog: %w", errRead)
		}
		if !bytes.Equal(current, data) {
			return errors.New("embedded model catalog is out of date")
		}
		return nil
	}
	if err = writeAtomic(*output, data); err != nil {
		return err
	}
	return nil
}

func validateCatalog(data []byte) error {
	var payload struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("validate model catalog JSON: %w", err)
	}
	if len(payload.Models) == 0 {
		return errors.New("model catalog contains no models")
	}
	seen := make(map[string]struct{}, len(payload.Models))
	for _, model := range payload.Models {
		slug := strings.TrimSpace(model.Slug)
		if slug == "" || len(slug) > 128 {
			return errors.New("model catalog contains an invalid slug")
		}
		for _, char := range slug {
			if unicode.IsControl(char) {
				return errors.New("model catalog contains an invalid slug")
			}
		}
		if _, exists := seen[slug]; exists {
			return fmt.Errorf("model catalog contains duplicate slug %q", slug)
		}
		seen[slug] = struct{}{}
	}
	return nil
}

func writeAtomic(output string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create model catalog directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(output), "."+filepath.Base(output)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create model catalog temp file: %w", err)
	}
	tempPath := temp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set model catalog permissions: %w", err)
	}
	if _, err = temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write model catalog: %w", err)
	}
	if err = temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync model catalog: %w", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close model catalog: %w", err)
	}
	if err = os.Rename(tempPath, output); err != nil {
		return fmt.Errorf("replace model catalog: %w", err)
	}
	renamed = true
	return nil
}
