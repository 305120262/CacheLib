package compactv2

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"sean.lo/cachelib/common"
)

// MergeOptions controls composite cache generation.
type MergeOptions struct {
	// Output receives a new cache when Overwrite is false.
	Output string
	// Overwrite replaces the origin cache.
	Overwrite bool
	// Workers limits concurrent bundle and tile processing.
	Workers int
}

// MergeResult summarizes a completed cache merge.
type MergeResult struct {
	Bundles int
	Tiles   int
}

// MergeCaches composites every non-empty overlay tile that also exists in the
// origin cache. Origin and overlay must use the same compact cache container.
func MergeCaches(ctx context.Context, originPath, overlayPath string, options MergeOptions) (MergeResult, error) {
	var result MergeResult
	if options.Workers <= 0 {
		options.Workers = runtime.GOMAXPROCS(0)
	}

	origin, err := Open(originPath)
	if err != nil {
		return result, err
	}
	defer origin.Close()

	overlay, err := Open(overlayPath)
	if err != nil {
		return result, err
	}
	defer overlay.Close()

	if err := validateCacheCompatibility(origin, overlay); err != nil {
		return result, err
	}

	target, err := mergeTarget(origin, options)
	if err != nil {
		return result, err
	}

	overlayBundles, err := overlay.bundlePaths()
	if err != nil {
		return result, err
	}
	originBundles, err := origin.bundlePaths()
	if err != nil {
		return result, err
	}

	outputRoot := target.path
	var stage string
	if !origin.isTPKX && !options.Overwrite {
		stage, err = os.MkdirTemp(filepath.Dir(target.path), "."+filepath.Base(target.path)+".tmp-*")
		if err != nil {
			return result, fmt.Errorf("create merge staging directory: %w", err)
		}
		moved := false
		defer func() {
			if !moved {
				os.RemoveAll(stage)
			}
		}()
		if err := copyDirectory(origin.RootPath, stage); err != nil {
			return result, err
		}
		outputRoot = stage
		target.commit = func() error {
			if err := os.Rename(stage, target.path); err != nil {
				return fmt.Errorf("commit merged cache: %w", err)
			}
			moved = true
			return nil
		}
	}

	replacements := make(map[string][]byte)
	var replacementsMu sync.Mutex
	emit := func(path string, data []byte) error {
		if origin.isTPKX {
			replacementsMu.Lock()
			replacements[strings.ToLower(path)] = data
			replacementsMu.Unlock()
			return nil
		}
		fullPath := filepath.Join(outputRoot, "_alllayers", filepath.FromSlash(path))
		return writeFileAtomic(fullPath, data, 0o644)
	}

	result, err = composeBundles(ctx, origin, overlay, originBundles, overlayBundles, options.Workers, emit)
	if err != nil {
		return result, err
	}

	if origin.isTPKX {
		if err := writeTPKX(target.path, origin, replacements, options.Overwrite); err != nil {
			return result, err
		}
	} else if target.commit != nil {
		if err := target.commit(); err != nil {
			return result, err
		}
	}
	return result, nil
}

type mergeDestination struct {
	path   string
	commit func() error
}

func mergeTarget(origin *Tiles, options MergeOptions) (mergeDestination, error) {
	originPath, err := filepath.Abs(origin.RootPath)
	if err != nil {
		return mergeDestination{}, err
	}
	if options.Overwrite {
		if options.Output != "" {
			outputPath, absErr := filepath.Abs(options.Output)
			if absErr != nil {
				return mergeDestination{}, absErr
			}
			if !samePath(originPath, outputPath) {
				return mergeDestination{}, fmt.Errorf("-output cannot differ from -origin when -overwrite is set")
			}
		}
		return mergeDestination{path: originPath}, nil
	}
	if options.Output == "" {
		return mergeDestination{}, fmt.Errorf("-output is required unless -overwrite is set")
	}

	outputPath, err := filepath.Abs(options.Output)
	if err != nil {
		return mergeDestination{}, err
	}
	if samePath(originPath, outputPath) {
		return mergeDestination{}, fmt.Errorf("output equals origin; use -overwrite to modify the source")
	}
	if _, err := os.Stat(outputPath); err == nil {
		return mergeDestination{}, fmt.Errorf("output %q already exists", outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return mergeDestination{}, fmt.Errorf("stat output %q: %w", outputPath, err)
	}

	outputTPKX := strings.EqualFold(filepath.Ext(outputPath), ".tpkx")
	if outputTPKX != origin.isTPKX {
		return mergeDestination{}, fmt.Errorf("origin and output must both be directories or both be TPKX files")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return mergeDestination{}, fmt.Errorf("create output parent: %w", err)
	}
	return mergeDestination{path: outputPath}, nil
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

type tileComposition struct {
	bundle  *compactBundle
	updates map[int][]byte
	mu      sync.Mutex
	wg      sync.WaitGroup
	err     error
}

func (c *tileComposition) finish(index int, data []byte, err error) {
	c.mu.Lock()
	if err != nil {
		if c.err == nil {
			c.err = err
		}
	} else {
		c.updates[index] = data
	}
	c.mu.Unlock()
	c.wg.Done()
}

func (c *tileComposition) result() (map[int][]byte, error) {
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updates, c.err
}

func composeBundles(
	ctx context.Context,
	origin, overlay *Tiles,
	originBundles, overlayBundles map[string]string,
	workers int,
	emit func(string, []byte) error,
) (MergeResult, error) {
	var result MergeResult
	keys := sortedKeys(overlayBundles)
	if len(keys) == 0 {
		return result, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 1)
	report := func(err error) {
		if err != nil {
			sendError(errs, err)
			cancel()
		}
	}

	tileSlots := make(chan struct{}, workers)
	var next atomic.Int64
	var bundleCount atomic.Int64
	var tileCount atomic.Int64
	var bundleWG sync.WaitGroup
	bundleWG.Add(workers)
	for range workers {
		go func() {
			defer bundleWG.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= len(keys) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				key := keys[index]
				overlayPath := overlayBundles[key]
				originPath, ok := originBundles[key]
				if !ok {
					continue
				}
				bundles, tiles, err := composeOneBundle(
					ctx, origin, overlay, originPath, overlayPath, tileSlots, emit, report,
				)
				if err != nil {
					report(err)
					continue
				}
				if bundles == 0 {
					continue
				}
				bundleCount.Add(int64(bundles))
				tileCount.Add(int64(tiles))
			}
		}()
	}
	bundleWG.Wait()
	select {
	case err := <-errs:
		return result, err
	default:
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Bundles = int(bundleCount.Load())
	result.Tiles = int(tileCount.Load())
	return result, nil
}

func composeOneBundle(
	ctx context.Context,
	origin, overlay *Tiles,
	originPath, overlayPath string,
	tileSlots chan struct{},
	emit func(string, []byte) error,
	report func(error),
) (int, int, error) {
	originData, found, err := origin.readBundle(originPath)
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, nil
	}
	overlayData, found, err := overlay.readBundle(overlayPath)
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, nil
	}

	originBundle, err := parseCompactBundle(originData)
	if err != nil {
		return 0, 0, fmt.Errorf("parse origin bundle %s: %w", originPath, err)
	}
	overlayBundle, err := parseCompactBundle(overlayData)
	if err != nil {
		return 0, 0, fmt.Errorf("parse overlay bundle %s: %w", overlayPath, err)
	}

	state := &tileComposition{
		bundle:  originBundle,
		updates: make(map[int][]byte),
	}

	for index, overlayTile := range overlayBundle.tiles {
		if len(overlayTile) == 0 || len(originBundle.tiles[index]) == 0 {
			continue
		}
		select {
		case tileSlots <- struct{}{}:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
		state.wg.Add(1)
		originTile := originBundle.tiles[index]
		go func() {
			defer func() { <-tileSlots }()
			composed, err := compositeTile(originTile, overlayTile)
			state.finish(index, composed, err)
			if err != nil {
				report(err)
			}
		}()
	}

	updates, err := state.result()
	if err != nil {
		return 0, 0, err
	}
	if len(updates) == 0 {
		return 0, 0, nil
	}
	for index, data := range updates {
		state.bundle.tiles[index] = data
	}
	if err := emit(originPath, state.bundle.encode()); err != nil {
		return 0, 0, err
	}
	return 1, len(updates), nil
}

func compositeTile(originData, overlayData []byte) ([]byte, error) {
	originImage, format, err := decodeTileImage(originData)
	if err != nil {
		return nil, fmt.Errorf("decode origin tile: %w", err)
	}
	overlayImage, _, err := decodeTileImage(overlayData)
	if err != nil {
		return nil, fmt.Errorf("decode overlay tile: %w", err)
	}
	if originImage.Bounds().Dx() != overlayImage.Bounds().Dx() ||
		originImage.Bounds().Dy() != overlayImage.Bounds().Dy() {
		return nil, fmt.Errorf(
			"tile dimensions differ: origin %dx%d, overlay %dx%d",
			originImage.Bounds().Dx(), originImage.Bounds().Dy(),
			overlayImage.Bounds().Dx(), overlayImage.Bounds().Dy(),
		)
	}

	bounds := image.Rect(0, 0, originImage.Bounds().Dx(), originImage.Bounds().Dy())
	composed := image.NewNRGBA(bounds)
	draw.Draw(composed, bounds, originImage, originImage.Bounds().Min, draw.Src)
	draw.Draw(composed, bounds, overlayImage, overlayImage.Bounds().Min, draw.Over)

	var output bytes.Buffer
	switch format {
	case "png":
		err = png.Encode(&output, composed)
	case "jpeg":
		err = jpeg.Encode(&output, composed, &jpeg.Options{Quality: 90})
	default:
		return nil, fmt.Errorf("unsupported origin tile format %q", format)
	}
	if err != nil {
		return nil, fmt.Errorf("encode composed %s tile: %w", format, err)
	}
	return output.Bytes(), nil
}

func decodeTileImage(data []byte) (image.Image, string, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		img, err := png.Decode(bytes.NewReader(data))
		return img, "png", err
	case bytes.HasPrefix(data, []byte{0xff, 0xd8}):
		img, err := jpeg.Decode(bytes.NewReader(data))
		return img, "jpeg", err
	default:
		return nil, "", fmt.Errorf("only PNG and JPEG tiles are supported")
	}
}

func parseBundlePath(path string) (level, baseRow, baseCol int, err error) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "L") {
		return 0, 0, 0, fmt.Errorf("parse bundle path %s: unexpected layout", path)
	}
	level, err = strconv.Atoi(strings.TrimPrefix(parts[0], "L"))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse bundle path %s: %w", path, err)
	}
	name := strings.TrimSuffix(parts[1], filepath.Ext(parts[1]))
	if !strings.HasPrefix(name, "R") {
		return 0, 0, 0, fmt.Errorf("parse bundle path %s: missing row", path)
	}
	separator := strings.IndexByte(name[1:], 'C')
	if separator < 1 {
		return 0, 0, 0, fmt.Errorf("parse bundle path %s: missing column", path)
	}
	separator++
	rowHex := name[1:separator]
	colHex := name[separator+1:]
	row, err := strconv.ParseInt(rowHex, 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse bundle row in %s: %w", path, err)
	}
	col, err := strconv.ParseInt(colHex, 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse bundle column in %s: %w", path, err)
	}
	return level, int(row), int(col), nil
}

func validateCacheCompatibility(origin, overlay *Tiles) error {
	o := origin.TileCacheInfo
	l := overlay.TileCacheInfo
	if o.TileRows != 0 && l.TileRows != 0 && o.TileRows != l.TileRows {
		return fmt.Errorf("tile row counts differ: origin %d, overlay %d", o.TileRows, l.TileRows)
	}
	if o.TileColumns != 0 && l.TileColumns != 0 && o.TileColumns != l.TileColumns {
		return fmt.Errorf("tile column counts differ: origin %d, overlay %d", o.TileColumns, l.TileColumns)
	}
	if !nearlyEqual(o.OriginX, l.OriginX) || !nearlyEqual(o.OriginY, l.OriginY) {
		return fmt.Errorf("tile origins differ")
	}

	oSR, lSR := o.SpatialReference, l.SpatialReference
	if oSR.WKID != 0 && lSR.WKID != 0 && !equivalentWKID(oSR.WKID, lSR.WKID) {
		return fmt.Errorf("spatial references differ: origin WKID %d, overlay WKID %d", oSR.WKID, lSR.WKID)
	}
	if oSR.LatestWKID != 0 && lSR.LatestWKID != 0 && !equivalentWKID(oSR.LatestWKID, lSR.LatestWKID) {
		return fmt.Errorf("latest WKIDs differ: origin %d, overlay %d", oSR.LatestWKID, lSR.LatestWKID)
	}
	if oSR.WKID == 0 && lSR.WKID == 0 && oSR.WKT != "" && lSR.WKT != "" && oSR.WKT != lSR.WKT {
		return fmt.Errorf("spatial reference WKT differs")
	}

	originLODs := lodResolutions(o.LODS)
	overlayLODs := lodResolutions(l.LODS)
	for level, resolution := range originLODs {
		other, ok := overlayLODs[level]
		if ok && !nearlyEqual(resolution, other) {
			return fmt.Errorf("LOD %d resolutions differ: origin %g, overlay %g", level, resolution, other)
		}
	}
	return nil
}

func equivalentWKID(a, b int) bool {
	if a == b {
		return true
	}
	webMercator := func(wkid int) bool {
		return wkid == 3857 || wkid == 102100 || wkid == 102113
	}
	return webMercator(a) && webMercator(b)
}

func lodResolutions(lods []common.LOD) map[int]float64 {
	resolutions := make(map[int]float64, len(lods))
	for _, lod := range lods {
		resolutions[lod.LevelID] = lod.Resolution
	}
	return resolutions
}

func nearlyEqual(a, b float64) bool {
	scale := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b) <= math.Max(1e-9, scale*1e-12)
}

func copyDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s is not supported", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := copyFile(path, target, info.Mode().Perm()); err != nil {
			return err
		}
		return nil
	})
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", destination, err)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", destination, err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("copy %s: %w", source, err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close %s: %w", destination, err)
	}
	return nil
}

func writeTPKX(target string, origin *Tiles, replacements map[string][]byte, overwrite bool) error {
	if origin.zipReader == nil {
		return fmt.Errorf("origin TPKX is not open")
	}
	temp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
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
	for _, file := range origin.zipReader.File {
		var replacement []byte
		if strings.HasPrefix(file.Name, "tile/") {
			key := strings.ToLower(strings.TrimPrefix(file.Name, "tile/"))
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
	for key := range replacements {
		if _, ok := replaced[key]; ok {
			continue
		}
		if err := addZipFile(writer, "tile/"+filepath.ToSlash(key), replacements[key]); err != nil {
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
	if overwrite {
		if err := origin.Close(); err != nil {
			return fmt.Errorf("close origin TPKX: %w", err)
		}
	}
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("commit TPKX: %w", err)
	}
	cleanup = false
	return nil
}
