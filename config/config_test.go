package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/config"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
)

type visualConfig struct {
	Theme theme.Settings `toml:"theme"`
}

func visualLoader() config.Loader[visualConfig] {
	return config.Loader[visualConfig]{
		Defaults: func() visualConfig { return visualConfig{Theme: theme.DefaultSettings()} },
		Validate: func(value visualConfig) error { return value.Theme.Validate() },
	}
}

func TestLoaderAppliesDefaultsAndRejectsUnknown(t *testing.T) {
	value, err := visualLoader().Decode([]byte("[theme]\nauto_switch = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if value.Theme.Name != "catppuccin" || value.Theme.LightName != "catppuccin-latte" || !value.Theme.AutoSwitch {
		t.Fatalf("defaults = %+v", value)
	}
	if _, err := visualLoader().Decode([]byte("surprise = true\n")); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := visualLoader().Decode([]byte("[theme]\nname = 'missing'\n")); err == nil || !strings.Contains(err.Error(), "unknown theme") {
		t.Fatalf("validation error = %v", err)
	}
	for name, fixture := range map[string]string{
		"unknown token": "[theme.custom]\nfuture='#ffffff'\n",
		"invalid color": "[theme.custom]\naccent='#xyzxyz'\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := visualLoader().Decode([]byte(fixture))
			if err == nil || !strings.Contains(err.Error(), "theme.custom") {
				t.Fatalf("contextual error = %v", err)
			}
		})
	}
}

func TestLoaderLoadReadsFileAndWrapsReadFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(path, []byte("[theme]\nname='dracula'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := visualLoader().Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if value.Theme.Name != "dracula" {
		t.Fatalf("theme = %q", value.Theme.Name)
	}

	_, err = visualLoader().Load(filepath.Join(directory, "missing.toml"))
	if err == nil || !strings.Contains(err.Error(), "read config:") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v", err)
	}
}

func TestWatchOptionBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[theme]\nname='catppuccin'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{Debounce: -time.Nanosecond}, func(config.Update[visualConfig]) {}); err == nil || watcher != nil || !strings.Contains(err.Error(), "debounce is negative") {
		t.Fatalf("negative debounce result = (%v, %v)", watcher, err)
	}

	for name, pollInterval := range map[string]time.Duration{
		"zero poll interval":     0,
		"negative poll interval": -time.Nanosecond,
	} {
		t.Run(name, func(t *testing.T) {
			updates := make(chan config.Update[visualConfig], 1)
			watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: pollInterval, Debounce: time.Millisecond}, func(update config.Update[visualConfig]) { updates <- update })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { watcher.Stop(); <-watcher.Done() })
			if err := os.WriteFile(path, []byte("[theme]\nname='dracula'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case update := <-updates:
				if update.Err != nil || update.Value.Theme.Name != "dracula" {
					t.Fatalf("update = %+v", update)
				}
			case <-time.After(time.Second):
				t.Fatal("normalized poll interval did not produce an update")
			}
			if err := os.WriteFile(path, []byte("[theme]\nname='catppuccin'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}

	updates := make(chan config.Update[visualConfig], 1)
	watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: time.Millisecond}, func(update config.Update[visualConfig]) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watcher.Stop(); <-watcher.Done() })
	if err := os.WriteFile(path, []byte("[theme]\nname='rose-pine'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.Err != nil || update.Value.Theme.Name != "rose-pine" {
			t.Fatalf("update = %+v", update)
		}
		if elapsed := update.AppliedAt.Sub(update.DetectedAt); elapsed < 80*time.Millisecond {
			t.Fatalf("zero debounce default applied after only %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("zero debounce default did not produce an update")
	}
}

func TestWatchDetectsSameSizeAtomicReplacementAndPropagates(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	initial := []byte("[theme]\nname='catppuccin' \n")
	replacement := []byte("[theme]\nname='tokyo-night'\n")
	if len(initial) != len(replacement) {
		t.Fatalf("fixture sizes differ: %d != %d", len(initial), len(replacement))
	}
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan config.Update[visualConfig], 2)
	watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: 10 * time.Millisecond, Debounce: 30 * time.Millisecond}, func(update config.Update[visualConfig]) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watcher.Stop(); <-watcher.Done() })
	temporary := filepath.Join(directory, "replacement")
	if err := os.WriteFile(temporary, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temporary, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.Err != nil {
			t.Fatal(update.Err)
		}
		if update.Value.Theme.Name != "tokyo-night" {
			t.Fatalf("theme = %q", update.Value.Theme.Name)
		}
		if update.DetectedAt.Before(started) || update.DetectedAt.Sub(started) >= time.Second {
			t.Fatalf("detection took %s", update.DetectedAt.Sub(started))
		}
		if update.AppliedAt.Sub(update.DetectedAt) < 30*time.Millisecond {
			t.Fatalf("debounce lasted %s", update.AppliedAt.Sub(update.DetectedAt))
		}
		if elapsed := time.Since(started); elapsed >= 2*time.Second {
			t.Fatalf("propagation took %s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement was not propagated within 2s")
	}
	select {
	case duplicate := <-updates:
		t.Fatalf("duplicate update: %+v", duplicate)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestWatchReportsInvalidReloadAndStopsWithoutLateCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[theme]\nname='catppuccin'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updates := make(chan config.Update[visualConfig], 4)
	watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: 5 * time.Millisecond, Debounce: 10 * time.Millisecond}, func(update config.Update[visualConfig]) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[theme]\nname='missing'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.Err == nil {
			t.Fatal("invalid reload had no error")
		}
	case <-time.After(time.Second):
		t.Fatal("invalid reload not reported")
	}

	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() { defer group.Done(); watcher.Stop() }()
	}
	group.Wait()
	select {
	case <-watcher.Done():
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop within 1s")
	}
	if err := os.WriteFile(path, []byte("[theme]\nname='dracula'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		t.Fatalf("late callback: %+v", update)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchReportsRemovalAndRecoversWhenFileReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	initial := []byte("[theme]\nname='catppuccin'\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	updates := make(chan config.Update[visualConfig], 2)
	watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: 5 * time.Millisecond, Debounce: 10 * time.Millisecond}, func(update config.Update[visualConfig]) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watcher.Stop(); <-watcher.Done() })

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.Err == nil || !strings.Contains(update.Err.Error(), "reload config:") || !errors.Is(update.Err, os.ErrNotExist) {
			t.Fatalf("removal update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("file removal was not reported")
	}

	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.Err != nil || update.Value.Theme.Name != "catppuccin" {
			t.Fatalf("recovery update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("restored file was not loaded")
	}
}

func TestWatchCallbackCanStopWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[theme]\nname='catppuccin'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{})
	ready := make(chan struct{})
	var watcher *config.Watcher
	var err error
	watcher, err = visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: 5 * time.Millisecond, Debounce: 5 * time.Millisecond}, func(config.Update[visualConfig]) {
		<-ready
		watcher.Stop()
		close(called)
	})
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	if err := os.WriteFile(path, []byte("[theme]\nname='rose-pine'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("callback-initiated stop deadlocked")
	}
	select {
	case <-watcher.Done():
	case <-time.After(time.Second):
		t.Fatal("watcher did not finish after callback stop")
	}
}

func TestWatchPollingAndShutdownDoNotWaitForCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[theme]\nname='catppuccin'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	secondSeen := make(chan struct{})
	releaseFirst := make(chan struct{})
	watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: 5 * time.Millisecond, Debounce: 10 * time.Millisecond}, func(update config.Update[visualConfig]) {
		switch update.Value.Theme.Name {
		case "rose-pine":
			close(firstStarted)
			<-releaseFirst
		case "dracula":
			close(secondSeen)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[theme]\nname='rose-pine'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first callback did not start")
	}
	if err := os.WriteFile(path, []byte("[theme]\nname='dracula'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondSeen:
	case <-time.After(time.Second):
		t.Fatal("blocked callback stopped config polling")
	}
	watcher.Stop()
	select {
	case <-watcher.Done():
	case <-time.After(time.Second):
		t.Fatal("watcher shutdown waited for blocked callback")
	}
	close(releaseFirst)
}

func TestWatchDebounceCoalescesBurstToFinalValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[theme]\nname='catppuccin'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updates := make(chan config.Update[visualConfig], 4)
	const debounce = 500 * time.Millisecond
	watcher, err := visualLoader().Watch(context.Background(), path, config.WatchOptions{PollInterval: 5 * time.Millisecond, Debounce: debounce}, func(update config.Update[visualConfig]) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watcher.Stop(); <-watcher.Done() })
	for _, name := range []string{"rose-pine", "tokyo-night", "dracula"} {
		if err := os.WriteFile(path, []byte("[theme]\nname='"+name+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	select {
	case update := <-updates:
		if update.Err != nil || update.Value.Theme.Name != "dracula" {
			t.Fatalf("coalesced update = %+v", update)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("debounced update was not delivered")
	}
	select {
	case update := <-updates:
		t.Fatalf("burst produced duplicate update: %+v", update)
	case <-time.After(600 * time.Millisecond):
	}
}
