package capabilities

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func TestAggregator_Get_BeforeDetect(t *testing.T) {
	a := NewAggregator(Defaults(), testLogger())
	caps := a.Get()
	if caps.GPU.Model != "" {
		t.Fatalf("expected zero-value capabilities before Detect, got %+v", caps)
	}
}

func TestAggregator_Get_AfterDetect(t *testing.T) {
	a := NewAggregator(Defaults(), testLogger())
	caps, err := a.Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	got := a.Get()
	if got.GPU.Model != caps.GPU.Model {
		t.Fatalf("Get().GPU.Model = %q, want %q", got.GPU.Model, caps.GPU.Model)
	}
	if time.Now().Before(a.GetLastUpdate()) {
		t.Fatal("GetLastUpdate returned a future timestamp")
	}
}

func TestAggregator_StartRefresh_StopsOnCancel(t *testing.T) {
	a := NewAggregator(Defaults(), testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.StartRefresh(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartRefresh did not return after ctx cancel")
	}
}

func TestAggregator_StartRefresh_PollInterval(t *testing.T) {
	a := NewAggregator(Defaults(), testLogger())
	cfg := Defaults()
	cfg.RefreshInterval = 20 * time.Millisecond
	a2 := NewAggregator(cfg, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a2.StartRefresh(ctx)
		close(done)
	}()
	waitForUpdate(t, 2*time.Second, func() bool {
		return !a2.GetLastUpdate().IsZero()
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartRefresh did not return after ctx cancel")
	}
	_ = a
}

func TestAggregator_Detect_ModelsFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := Defaults()
	cfg.ModelConfigPath = filepath.Join(dir, "models.yaml")
	if err := writeFileErr(cfg.ModelConfigPath, "models:\n  - name: test-model\n    backend: mock\n    loaded: true\n"); err != nil {
		t.Fatalf("write models.yaml: %v", err)
	}
	a := NewAggregator(cfg, testLogger())
	caps, err := a.Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(caps.Models) != 1 || caps.Models[0].Name != "test-model" || !caps.Models[0].Loaded {
		t.Fatalf("expected test-model in capabilities, got %+v", caps.Models)
	}
}

func writeFileErr(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
