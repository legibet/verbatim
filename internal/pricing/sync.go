package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxSourceBytes bounds a downloaded provider file.
const maxSourceBytes = 64 << 20

// Sync downloads every source into cacheDir. All downloads must succeed and
// parse as provider files before anything is written, so a failed sync leaves
// the previous cache intact; each file is then replaced atomically.
func Sync(ctx context.Context, sources []string, cacheDir string) error {
	names, err := cacheFiles(sources)
	if err != nil {
		return err
	}
	bodies := make([][]byte, len(sources))
	for i, src := range sources {
		if bodies[i], err = download(ctx, src); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("pricing.cache: %w", err)
	}
	for i, name := range names {
		if err := writeAtomic(filepath.Join(cacheDir, name), bodies[i]); err != nil {
			return fmt.Errorf("pricing.cache: %w", err)
		}
	}
	return nil
}

func download(ctx context.Context, src string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, fmt.Errorf("pricing source %s: %w", src, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing source %s: %w", src, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing source %s: HTTP %s", src, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("pricing source %s: %w", src, err)
	}
	if len(body) > maxSourceBytes {
		return nil, fmt.Errorf("pricing source %s: larger than %d bytes", src, maxSourceBytes)
	}
	var f providerFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("pricing source %s: %w", src, err)
	}
	if f.Models == nil {
		return nil, fmt.Errorf("pricing source %s: no \"models\" object", src)
	}
	return body, nil
}

func writeAtomic(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".sync-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Watch calls reload, which loads and installs the table, whenever a cached
// source file or the overrides file changes (modification time or size),
// checking every interval. A failed reload is logged and the caller keeps its
// current table. Watch returns when ctx is done.
func Watch(ctx context.Context, interval time.Duration, cacheDir string, sources []string, overridesPath string, reload func() (*Table, []string, error)) {
	names, err := cacheFiles(sources)
	if err != nil {
		slog.Warn("pricing reload disabled", "err", err)
		return
	}
	files := make([]string, 0, len(names)+1)
	for _, name := range names {
		files = append(files, filepath.Join(cacheDir, name))
	}
	if overridesPath != "" {
		files = append(files, overridesPath)
	}
	stamp := func() string {
		var b strings.Builder
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil {
				fmt.Fprintf(&b, "%s %d %d\n", f, fi.ModTime().UnixNano(), fi.Size())
			}
		}
		return b.String()
	}

	last := stamp()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cur := stamp()
		if cur == last {
			continue
		}
		last = cur
		t, warnings, err := reload()
		if err != nil {
			slog.Warn("pricing reload failed; keeping the previous table", "err", err)
			continue
		}
		for _, w := range warnings {
			slog.Warn("pricing: " + w)
		}
		slog.Info("pricing table reloaded", "models", len(t.prices))
	}
}
