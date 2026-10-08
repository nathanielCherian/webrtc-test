package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Library is the directory of videos a downlink session can choose from.
// Files are probed on first use and cached by name, size and modification time.
type Library struct {
	Dir         string
	Default     *MediaInfo // --video; used when a session doesn't pick one
	AllowUpload bool
	MaxUpload   int64 // bytes

	mu    sync.Mutex
	cache map[string]probeEntry
}

type probeEntry struct {
	size  int64
	mtime time.Time
	info  *MediaInfo
	err   error
}

// VideoEntry is one row of the library listing.
type VideoEntry struct {
	Name    string     `json:"name"` // "" is the server default
	Default bool       `json:"default"`
	Info    *MediaInfo `json:"info"`
	SizeMB  float64    `json:"sizeMB"`
}

// ErrVideoExists is returned by Save when the name is taken.
var ErrVideoExists = errors.New("a video with that name already exists")

// uploadPrefix marks in-progress uploads; List skips dotfiles.
const uploadPrefix = ".upload-"

// NewLibrary creates a library over dir. def is the probed --video file.
func NewLibrary(dir string, def *MediaInfo, allowUpload bool, maxUpload int64) *Library {
	return &Library{Dir: dir, Default: def, AllowUpload: allowUpload, MaxUpload: maxUpload, cache: map[string]probeEntry{}}
}

// cleanName accepts a bare file name inside the library directory.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") ||
		strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("invalid video name %q", name)
	}
	return name, nil
}

// isDefault reports whether name is the --video file itself.
func (l *Library) isDefault(name string) bool {
	a, err1 := filepath.Abs(filepath.Join(l.Dir, name))
	b, err2 := filepath.Abs(l.Default.Path)
	return err1 == nil && err2 == nil && a == b
}

// probe returns the cached probe of name, probing again if the file changed.
func (l *Library) probe(ctx context.Context, name string, fi os.FileInfo) (*MediaInfo, error) {
	l.mu.Lock()
	e, ok := l.cache[name]
	l.mu.Unlock()
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return e.info, e.err
	}
	info, err := Probe(ctx, filepath.Join(l.Dir, name))
	if ctx.Err() != nil {
		return nil, ctx.Err() // don't cache a cancelled probe
	}
	l.mu.Lock()
	l.cache[name] = probeEntry{size: fi.Size(), mtime: fi.ModTime(), info: info, err: err}
	l.mu.Unlock()
	return info, err
}

// List returns the default video first, then every playable file in Dir by name.
func (l *Library) List(ctx context.Context) []VideoEntry {
	out := []VideoEntry{{Name: "", Default: true, Info: l.Default}}
	if fi, err := os.Stat(l.Default.Path); err == nil {
		out[0].SizeMB = float64(fi.Size()) / 1e6
	}
	ents, err := os.ReadDir(l.Dir)
	if err != nil {
		return out
	}
	for _, de := range ents {
		name := de.Name()
		if !de.Type().IsRegular() || strings.HasPrefix(name, ".") || l.isDefault(name) {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		info, err := l.probe(ctx, name, fi)
		if err != nil {
			continue // not a video ffmpeg can read
		}
		out = append(out, VideoEntry{Name: name, Info: info, SizeMB: float64(fi.Size()) / 1e6})
	}
	sort.SliceStable(out[1:], func(i, j int) bool { return out[1+i].Name < out[1+j].Name })
	return out
}

// Get resolves a session's video choice; "" is the default.
func (l *Library) Get(ctx context.Context, name string) (*MediaInfo, error) {
	if name == "" {
		return l.Default, nil
	}
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	if l.isDefault(name) {
		return l.Default, nil
	}
	fi, err := os.Stat(filepath.Join(l.Dir, name))
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("video %q not found", name)
	}
	return l.probe(ctx, name, fi)
}

// Save stores an uploaded file as name after checking that ffmpeg can play it.
func (l *Library) Save(ctx context.Context, name string, r io.Reader) (*MediaInfo, error) {
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(l.Dir, name)
	if _, err := os.Stat(final); err == nil {
		return nil, ErrVideoExists
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(l.Dir, uploadPrefix+"*"+filepath.Ext(name))
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	_, err = io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	if _, err := Probe(ctx, tmp.Name()); err != nil {
		log.Printf("upload %q rejected: %v", name, err)
		return nil, errors.New("not a playable video: ffmpeg found no readable video stream")
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // CreateTemp makes it 0600
		return nil, err
	}
	// Link instead of rename so a file that appeared meanwhile isn't overwritten.
	if err := os.Link(tmp.Name(), final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrVideoExists
		}
		return nil, err
	}
	return l.Get(ctx, name)
}
