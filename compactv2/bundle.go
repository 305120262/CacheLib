package compactv2

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	_bundleTileCount  = _BUNDLX_MAXIDX * _BUNDLX_MAXIDX
	_bundleIndexBytes = _bundleTileCount * 8
	_bundleDataStart  = _COMPACT_CACHE_HEADER_LENGTH + _bundleIndexBytes
)

// TileCoordinate identifies a tile within a compact v2 cache.
type TileCoordinate struct {
	Level int
	Row   int
	Col   int
}

// BundleUpdate replaces selected tiles in one bundle. Row and Col may identify
// any tile in the target bundle; all coordinates in Tiles must belong to it.
// A nil or empty tile removes the tile from the rewritten bundle.
type BundleUpdate struct {
	Level int
	Row   int
	Col   int
	Tiles map[TileCoordinate][]byte
}

type compactBundle struct {
	header [64]byte
	tiles  [_bundleTileCount][]byte
}

// UpdateBundle rewrites one bundle after applying every update in the batch.
func (t *Tiles) UpdateBundle(ctx context.Context, update BundleUpdate) error {
	return t.UpdateBundles(ctx, []BundleUpdate{update}, 1)
}

// UpdateBundles applies tile changes in batches. Each affected bundle is
// encoded once, and different bundles are processed concurrently.
func (t *Tiles) UpdateBundles(ctx context.Context, updates []BundleUpdate, workers int) error {
	if len(updates) == 0 {
		return nil
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	seen := make(map[string]struct{}, len(updates))
	for i := range updates {
		path, err := bundlePathForUpdate(updates[i])
		if err != nil {
			return err
		}
		key := strings.ToLower(path)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("bundle %s appears more than once", path)
		}
		seen[key] = struct{}{}
	}

	type result struct {
		path string
		data []byte
	}
	jobs := make(chan BundleUpdate)
	results := make(chan result, len(updates))
	errs := make(chan error, 1)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for update := range jobs {
			path, _ := bundlePathForUpdate(update)
			data, found, err := t.readBundle(path)
			if err != nil {
				sendError(errs, err)
				continue
			}
			bundle := newCompactBundle()
			if found {
				bundle, err = parseCompactBundle(data)
				if err != nil {
					sendError(errs, fmt.Errorf("parse bundle %s: %w", path, err))
					continue
				}
			}
			if err := applyBundleUpdate(bundle, update); err != nil {
				sendError(errs, err)
				continue
			}
			data = bundle.encode()
			if t.isTPKX {
				results <- result{path: path, data: data}
				continue
			}
			fullPath := filepath.Join(t.RootPath, "_alllayers", filepath.FromSlash(path))
			if err := writeFileAtomic(fullPath, data, 0o644); err != nil {
				sendError(errs, err)
			}
		}
	}

	wg.Add(workers)
	for range workers {
		go worker()
	}
	go func() {
		defer close(jobs)
		for _, update := range updates {
			select {
			case jobs <- update:
			case <-ctx.Done():
				sendError(errs, ctx.Err())
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	rewritten := make(map[string][]byte, len(updates))
	for result := range results {
		rewritten[result.path] = result.data
	}
	select {
	case err := <-errs:
		return err
	default:
	}

	if t.isTPKX {
		canonical := make(map[string][]byte, len(rewritten))
		for path, data := range rewritten {
			canonical[strings.ToLower(path)] = data
		}
		return t.rewriteTPKX(canonical)
	}
	return nil
}

func bundlePathForUpdate(update BundleUpdate) (string, error) {
	if update.Level < 0 || update.Row < 0 || update.Col < 0 {
		return "", fmt.Errorf("bundle coordinates must be non-negative")
	}
	baseRow := update.Row / _BUNDLX_MAXIDX * _BUNDLX_MAXIDX
	baseCol := update.Col / _BUNDLX_MAXIDX * _BUNDLX_MAXIDX
	zoom := fmt.Sprintf("%02d", update.Level)
	return fmt.Sprintf("L%s/R%04xC%04x%s", zoom, baseRow, baseCol, _BUNDLE_EXT), nil
}

func applyBundleUpdate(bundle *compactBundle, update BundleUpdate) error {
	path, _ := bundlePathForUpdate(update)
	for coordinate, data := range update.Tiles {
		if coordinate.Level != update.Level ||
			coordinate.Row/_BUNDLX_MAXIDX != update.Row/_BUNDLX_MAXIDX ||
			coordinate.Col/_BUNDLX_MAXIDX != update.Col/_BUNDLX_MAXIDX {
			return fmt.Errorf("tile L%d R%d C%d does not belong to bundle %s", coordinate.Level, coordinate.Row, coordinate.Col, path)
		}
		index := tileIndex(coordinate.Row, coordinate.Col)
		bundle.tiles[index] = append([]byte(nil), data...)
	}
	return nil
}

func tileIndex(row, col int) int {
	return _BUNDLX_MAXIDX*(row%_BUNDLX_MAXIDX) + col%_BUNDLX_MAXIDX
}

func newCompactBundle() *compactBundle {
	bundle := &compactBundle{}
	putUint32(bundle.header[0:], 3)
	putUint32(bundle.header[8:], _bundleDataStart-44)
	putUint32(bundle.header[12:], 5)
	putUint64(bundle.header[32:], 40)
	putUint32(bundle.header[40:], _bundleDataStart-44)
	putUint32(bundle.header[44:], 3)
	putUint32(bundle.header[52:], _bundleTileCount)
	putUint32(bundle.header[56:], 5)
	putUint32(bundle.header[60:], _bundleIndexBytes)
	return bundle
}

func parseCompactBundle(data []byte) (*compactBundle, error) {
	if len(data) < _bundleDataStart {
		return nil, fmt.Errorf("bundle is %d bytes; need at least %d", len(data), _bundleDataStart)
	}

	bundle := &compactBundle{}
	copy(bundle.header[:], data[:_COMPACT_CACHE_HEADER_LENGTH])
	for index := range _bundleTileCount {
		position := _COMPACT_CACHE_HEADER_LENGTH + index*8
		record := data[position : position+8]
		offset := uint64(record[0]) |
			uint64(record[1])<<8 |
			uint64(record[2])<<16 |
			uint64(record[3])<<24 |
			uint64(record[4])<<32
		size := int(uint32(record[5]) | uint32(record[6])<<8 | uint32(record[7])<<16)
		if size == 0 {
			continue
		}
		if offset > uint64(len(data)) || uint64(size) > uint64(len(data))-offset {
			return nil, fmt.Errorf("tile %d has invalid offset/size %d/%d", index, offset, size)
		}
		bundle.tiles[index] = append([]byte(nil), data[offset:offset+uint64(size)]...)
	}
	return bundle, nil
}

func (b *compactBundle) encode() []byte {
	data := make([]byte, _bundleDataStart, _bundleDataStart+_bundleIndexBytes)
	copy(data, b.header[:])

	for index, tile := range b.tiles {
		if len(tile) == 0 {
			continue
		}
		offset := len(data) + 4
		if offset >= 1<<40 || len(tile) >= 1<<24 {
			panic("compact v2 tile offset or size exceeds on-disk limits")
		}
		record := data[_COMPACT_CACHE_HEADER_LENGTH+index*8 : _COMPACT_CACHE_HEADER_LENGTH+(index+1)*8]
		putUint40(record[:5], uint64(offset))
		record[5] = byte(len(tile))
		record[6] = byte(len(tile) >> 8)
		record[7] = byte(len(tile) >> 16)

		var size [4]byte
		putUint32(size[:], uint32(len(tile)))
		data = append(data, size[:]...)
		data = append(data, tile...)
	}
	putUint64(data[24:32], uint64(len(data)))
	return data
}

func (t *Tiles) rewriteTPKX(replacements map[string][]byte) error {
	reader := t.zipReader
	temporaryReader := false
	if reader == nil {
		var err error
		reader, err = zip.OpenReader(t.RootPath)
		if err != nil {
			return fmt.Errorf("open TPKX: %w", err)
		}
		temporaryReader = true
	}
	if temporaryReader {
		defer reader.Close()
	}

	temp, err := os.CreateTemp(filepath.Dir(t.RootPath), "."+filepath.Base(t.RootPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary TPKX: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		temp.Close()
		if cleanup {
			os.Remove(tempPath)
		}
	}()

	writer := zip.NewWriter(temp)
	replaced := make(map[string]struct{}, len(replacements))
	for _, file := range reader.File {
		var replacement []byte
		if strings.HasPrefix(file.Name, "tile/") {
			logicalPath := strings.TrimPrefix(file.Name, "tile/")
			key := strings.ToLower(logicalPath)
			replacement = replacements[key]
			if replacement != nil {
				replaced[key] = struct{}{}
			}
		}
		if replacement == nil {
			if err := writer.Copy(file); err != nil {
				writer.Close()
				return fmt.Errorf("copy %s: %w", file.Name, err)
			}
			continue
		}
		header := file.FileHeader
		header.CRC32 = 0
		header.CompressedSize = 0
		header.UncompressedSize = 0
		entry, err := writer.CreateHeader(&header)
		if err != nil {
			writer.Close()
			return fmt.Errorf("replace %s: %w", file.Name, err)
		}
		if _, err := entry.Write(replacement); err != nil {
			writer.Close()
			return fmt.Errorf("write %s: %w", file.Name, err)
		}
	}
	for path := range replacements {
		if _, ok := replaced[path]; ok {
			continue
		}
		if err := addZipFile(writer, "tile/"+filepath.ToSlash(path), replacements[path]); err != nil {
			writer.Close()
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close temporary TPKX: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary TPKX file: %w", err)
	}
	if temporaryReader {
		if err := reader.Close(); err != nil {
			return fmt.Errorf("close source TPKX: %w", err)
		}
	} else {
		if err := t.Close(); err != nil {
			return fmt.Errorf("close source TPKX: %w", err)
		}
	}
	if err := os.Rename(tempPath, t.RootPath); err != nil {
		return fmt.Errorf("replace TPKX: %w", err)
	}
	cleanup = false
	return t.loadTPKX()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func sendError(channel chan<- error, err error) {
	select {
	case channel <- err:
	default:
	}
}

func putUint32(dst []byte, value uint32) {
	binary.LittleEndian.PutUint32(dst, value)
}

func putUint64(dst []byte, value uint64) {
	binary.LittleEndian.PutUint64(dst, value)
}

func putUint40(dst []byte, value uint64) {
	for i := range 5 {
		dst[i] = byte(value >> (8 * i))
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func addZipFile(writer *zip.Writer, name string, data []byte) error {
	entry, err := writer.Create(name)
	if err != nil {
		return fmt.Errorf("create %s in TPKX: %w", name, err)
	}
	if _, err := entry.Write(data); err != nil {
		return fmt.Errorf("write %s to TPKX: %w", name, err)
	}
	return nil
}
