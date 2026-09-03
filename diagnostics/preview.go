package diagnostics

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const previewRegistryName = "registry.json"

type PreviewLimits struct {
	MaxCount int
	MaxBytes int64
}

func DefaultPreviewLimits() PreviewLimits {
	return PreviewLimits{MaxCount: 10_000, MaxBytes: 512 << 20}
}

type PreviewEntry struct {
	Sequence     uint64 `json:"sequence"`
	VisualDigest string `json:"visual_digest"`
	RelativePath string `json:"relative_path"`
	PNGSHA256    string `json:"png_sha256"`
	Bytes        int64  `json:"bytes"`
}

type PreviewStore struct {
	mu      sync.Mutex
	root    string
	rootFS  *os.Root
	limits  PreviewLimits
	entries map[uint64]PreviewEntry
	bytes   int64
	closed  bool
}

func OpenPreviewStore(stateDirectory string, limits PreviewLimits) (*PreviewStore, error) {
	if stateDirectory == "" {
		return nil, errors.New("preview state directory is empty")
	}
	if limits.MaxCount <= 0 || limits.MaxBytes <= 0 {
		return nil, errors.New("preview limits must be positive")
	}
	abs, err := filepath.Abs(stateDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve preview state directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create preview state directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve preview state directory links: %w", err)
	}
	root := filepath.Join(resolved, "snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create preview directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure preview directory: %w", err)
	}
	if err := ensureDirectory(root); err != nil {
		return nil, err
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open preview directory: %w", err)
	}
	store := &PreviewStore{root: root, rootFS: rootFS, limits: limits, entries: make(map[uint64]PreviewEntry)}
	if err := store.secureRoot(); err != nil {
		_ = rootFS.Close()
		return nil, err
	}
	if err := store.loadRegistry(); err != nil {
		_ = rootFS.Close()
		return nil, err
	}
	return store, nil
}

func (store *PreviewStore) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	return store.rootFS.Close()
}

func ensureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect preview directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("preview directory must be a real directory")
	}
	return nil
}

func (store *PreviewStore) Put(sequence uint64, state VisualState) (PreviewEntry, error) {
	if sequence == 0 {
		return PreviewEntry{}, errors.New("preview sequence must be positive")
	}
	if err := validateVisualState(state); err != nil {
		return PreviewEntry{}, err
	}
	digest, err := visualDigest(state)
	if err != nil {
		return PreviewEntry{}, err
	}
	data, err := renderSemanticPreview(state)
	if err != nil {
		return PreviewEntry{}, err
	}
	entry := PreviewEntry{
		Sequence: sequence, VisualDigest: digest,
		RelativePath: canonicalPreviewPath(sequence, digest),
		PNGSHA256:    hashBytes(data), Bytes: int64(len(data)),
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return PreviewEntry{}, ErrClosed
	}
	if existing, ok := store.entries[sequence]; ok {
		if existing == entry {
			if _, err := store.readLocked(entry, state); err == nil {
				return entry, nil
			}
		}
		return PreviewEntry{}, errors.New("preview sequence is already registered")
	}
	if len(store.entries) >= store.limits.MaxCount {
		return PreviewEntry{}, errors.New("preview count limit reached")
	}
	if entry.Bytes > store.limits.MaxBytes-store.bytes {
		return PreviewEntry{}, errors.New("preview byte limit reached")
	}
	if err := store.secureRoot(); err != nil {
		return PreviewEntry{}, err
	}
	name := filepath.Base(filepath.FromSlash(entry.RelativePath))
	if err := writeAtomic(store.rootFS, name, data, 0o600); err != nil {
		return PreviewEntry{}, fmt.Errorf("store preview: %w", err)
	}
	store.entries[sequence] = entry
	store.bytes += entry.Bytes
	if err := store.writeRegistryLocked(); err != nil {
		delete(store.entries, sequence)
		store.bytes -= entry.Bytes
		_ = store.rootFS.Remove(name)
		return PreviewEntry{}, err
	}
	return entry, nil
}

func (store *PreviewStore) Entry(sequence uint64, state VisualState) (PreviewEntry, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return PreviewEntry{}, false
	}
	entry, ok := store.entries[sequence]
	if !ok {
		return PreviewEntry{}, false
	}
	if _, err := store.readLocked(entry, state); err != nil {
		return PreviewEntry{}, false
	}
	return entry, true
}

func (store *PreviewStore) Read(entry PreviewEntry, state VisualState) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil, ErrClosed
	}
	registered, ok := store.entries[entry.Sequence]
	if !ok || registered != entry {
		return nil, errors.New("preview is not registered")
	}
	return store.readLocked(entry, state)
}

func (store *PreviewStore) readLocked(entry PreviewEntry, state VisualState) ([]byte, error) {
	digest, err := visualDigest(state)
	if err != nil || digest != entry.VisualDigest {
		return nil, errors.New("preview visual state does not match")
	}
	if entry.RelativePath != canonicalPreviewPath(entry.Sequence, entry.VisualDigest) {
		return nil, errors.New("preview path is not canonical")
	}
	if err := store.secureRoot(); err != nil {
		return nil, err
	}
	name := filepath.Base(filepath.FromSlash(entry.RelativePath))
	info, err := store.rootFS.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect preview: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("preview must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("preview permissions are not owner-only")
	}
	if info.Size() != entry.Bytes || info.Size() < 0 || info.Size() > store.limits.MaxBytes {
		return nil, errors.New("preview size does not match")
	}
	data, err := store.rootFS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read preview: %w", err)
	}
	if hashBytes(data) != entry.PNGSHA256 {
		return nil, errors.New("preview hash does not match")
	}
	return data, nil
}

func (store *PreviewStore) secureRoot() error {
	if err := ensureDirectory(store.root); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(store.root)
	if err != nil || resolved != store.root {
		return errors.New("preview directory no longer resolves to its canonical root")
	}
	current, err := os.Stat(store.root)
	if err != nil {
		return fmt.Errorf("inspect current preview directory: %w", err)
	}
	anchored, err := store.rootFS.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect anchored preview directory: %w", err)
	}
	if !os.SameFile(current, anchored) {
		return errors.New("preview directory identity changed")
	}
	return nil
}

func (store *PreviewStore) loadRegistry() error {
	info, statErr := store.rootFS.Lstat(previewRegistryName)
	if statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0) {
		return errors.New("preview registry must be an owner-only regular file")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect preview registry: %w", statErr)
	}
	data, err := store.rootFS.ReadFile(previewRegistryName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read preview registry: %w", err)
	}
	var entries []PreviewEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("decode preview registry: %w", err)
	}
	if len(entries) > store.limits.MaxCount {
		return errors.New("preview registry exceeds count limit")
	}
	for _, entry := range entries {
		if entry.Sequence == 0 || entry.Bytes <= 0 || !validSHA256(entry.VisualDigest) || !validSHA256(entry.PNGSHA256) {
			return errors.New("preview registry contains an invalid entry")
		}
		if _, exists := store.entries[entry.Sequence]; exists {
			return errors.New("preview registry contains a duplicate sequence")
		}
		if entry.RelativePath != canonicalPreviewPath(entry.Sequence, entry.VisualDigest) {
			return errors.New("preview registry contains a noncanonical path")
		}
		if entry.Bytes > store.limits.MaxBytes-store.bytes {
			return errors.New("preview registry exceeds byte limit")
		}
		store.entries[entry.Sequence] = entry
		store.bytes += entry.Bytes
	}
	return nil
}

func (store *PreviewStore) writeRegistryLocked() error {
	if err := store.secureRoot(); err != nil {
		return err
	}
	entries := make([]PreviewEntry, 0, len(store.entries))
	for _, entry := range store.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode preview registry: %w", err)
	}
	if err := writeAtomic(store.rootFS, previewRegistryName, data, 0o600); err != nil {
		return fmt.Errorf("store preview registry: %w", err)
	}
	return nil
}

func writeAtomic(root *os.Root, name string, data []byte, mode os.FileMode) error {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temporaryPath := ".preview-" + hex.EncodeToString(random[:])
	temporary, err := root.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = root.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := root.Rename(temporaryPath, name); err != nil {
		return err
	}
	remove = false
	return nil
}

func visualDigest(state VisualState) (string, error) {
	if err := validateVisualState(state); err != nil {
		return "", err
	}
	data, err := encodeVisualState(state)
	if err != nil {
		return "", err
	}
	return hashBytes(data), nil
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func canonicalPreviewPath(sequence uint64, digest string) string {
	prefix := digest
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return filepath.ToSlash(filepath.Join("snapshots", fmt.Sprintf("%020d-%s.png", sequence, prefix)))
}

func renderSemanticPreview(state VisualState) ([]byte, error) {
	const width, height = 320, 180
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{R: 24, G: 24, B: 30, A: 255}), image.Point{}, draw.Src)
	draw.Draw(canvas, image.Rect(0, 0, width, 24), image.NewUniform(color.RGBA{R: 49, G: 50, B: 68, A: 255}), image.Point{}, draw.Src)
	accent := color.RGBA{R: 137, G: 180, B: 250, A: 255}
	if state.HasError {
		accent = color.RGBA{R: 243, G: 139, B: 168, A: 255}
	} else if state.Pending {
		accent = color.RGBA{R: 249, G: 226, B: 175, A: 255}
	}
	draw.Draw(canvas, image.Rect(12, 38, 16, 160), image.NewUniform(accent), image.Point{}, draw.Src)
	rows := min(max(state.ItemCount, 1), 6)
	for row := range rows {
		top := 40 + row*20
		shade := color.RGBA{R: 69, G: 71, B: 90, A: 255}
		if row == min(state.SelectedIndex, rows-1) {
			shade = color.RGBA{R: 88, G: 91, B: 112, A: 255}
		}
		draw.Draw(canvas, image.Rect(24, top, 206, top+14), image.NewUniform(shade), image.Point{}, draw.Src)
	}
	draw.Draw(canvas, image.Rect(218, 38, 308, 160), image.NewUniform(color.RGBA{R: 36, G: 39, B: 58, A: 255}), image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		return nil, fmt.Errorf("encode semantic preview: %w", err)
	}
	return encoded.Bytes(), nil
}
