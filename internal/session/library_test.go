package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanName(t *testing.T) {
	for _, bad := range []string{"", "../x.mp4", "a/b.mp4", `a\b.mp4`, ".hidden.mp4", "..", "/etc/passwd"} {
		if _, err := cleanName(bad); err == nil {
			t.Errorf("cleanName(%q) accepted", bad)
		}
	}
	if n, err := cleanName(" clip one.mp4 "); err != nil || n != "clip one.mp4" {
		t.Errorf("cleanName: %q, %v", n, err)
	}
}

func TestLibrary(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "testsrc2=size=320x240:rate=25:duration=1", "-pix_fmt", "yuv420p", clip).CombinedOutput()
	if err != nil {
		t.Fatalf("make clip: %v %s", err, out)
	}
	def, err := Probe(ctx, clip)
	if err != nil {
		t.Fatal(err)
	}
	lib := NewLibrary(dir, def, true, 1<<30)

	if got, err := lib.Get(ctx, ""); err != nil || got != def {
		t.Fatalf("default: %v %v", got, err)
	}
	if _, err := lib.Get(ctx, "../clip.mp4"); err == nil {
		t.Error("path traversal accepted")
	}
	if _, err := lib.Get(ctx, "missing.mp4"); err == nil {
		t.Error("missing file accepted")
	}

	if _, err := lib.Save(ctx, "notes.mp4", strings.NewReader("not a video")); err == nil {
		t.Error("non-video upload accepted")
	}
	data, _ := os.ReadFile(clip)
	info, err := lib.Save(ctx, "uploaded.mp4", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if info.Width != 320 || info.Height != 240 || info.FPSNum/info.FPSDen != 25 {
		t.Errorf("uploaded info: %+v", info)
	}
	if _, err := lib.Save(ctx, "uploaded.mp4", bytes.NewReader(data)); !errors.Is(err, ErrVideoExists) {
		t.Errorf("overwrite: got %v, want ErrVideoExists", err)
	}

	list := lib.List(ctx)
	if len(list) != 2 || !list[0].Default || list[1].Name != "uploaded.mp4" {
		t.Errorf("list: %+v", list)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), uploadPrefix) {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}
