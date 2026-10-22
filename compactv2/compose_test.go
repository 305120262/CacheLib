package compactv2

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"path/filepath"
	"testing"
)

func TestMergeDirectoryFixtures(t *testing.T) {
	overlayRoot := copyDirectoryCacheFixture(t)
	overlay, err := Open(overlayRoot)
	if err != nil {
		t.Fatal(err)
	}

	origin, err := Open(filepath.Join("..", "data", "zhuhai"))
	if err != nil {
		overlay.Close()
		t.Fatal(err)
	}
	originTotal := countTiles(t, origin)
	origin.Close()

	bundlePath := "L12/R0680C0d00.bundle"
	bundle := readTestBundle(t, overlay, bundlePath)
	missingOriginIndex := firstEmptyIndex(bundle)
	if missingOriginIndex < 0 {
		overlay.Close()
		t.Fatal("fixture bundle has no empty tile slot")
	}
	coordinate := coordinateForIndex(bundlePath, missingOriginIndex)
	if err := overlay.UpdateBundle(context.Background(), BundleUpdate{
		Level: coordinate.Level,
		Row:   coordinate.Row,
		Col:   coordinate.Col,
		Tiles: map[TileCoordinate][]byte{coordinate: testPNG(t)},
	}); err != nil {
		overlay.Close()
		t.Fatal(err)
	}
	overlay.Close()

	output := filepath.Join(t.TempDir(), "merged")
	result, err := MergeCaches(
		context.Background(),
		filepath.Join("..", "data", "zhuhai"),
		overlayRoot,
		MergeOptions{Output: output, Workers: 4},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bundles != 5 {
		t.Fatalf("merged %d bundles, want 5", result.Bundles)
	}
	if result.Tiles != originTotal {
		t.Fatalf("merged %d tiles, want %d", result.Tiles, originTotal)
	}

	merged, err := Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer merged.Close()
	if got := countTiles(t, merged); got != originTotal {
		t.Fatalf("output has %d tiles, want %d", got, originTotal)
	}
	mergedBundle := readTestBundle(t, merged, bundlePath)
	if len(mergedBundle.tiles[missingOriginIndex]) != 0 {
		t.Fatal("overlay tile without an origin tile was written")
	}
}

func TestMergeDirectoryOverwrite(t *testing.T) {
	cacheRoot := copyDirectoryCacheFixture(t)
	result, err := MergeCaches(
		context.Background(),
		cacheRoot,
		filepath.Join("..", "data", "zhuhai"),
		MergeOptions{Overwrite: true, Workers: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tiles != 162 || result.Bundles != 5 {
		t.Fatalf("merged %d tiles across %d bundles", result.Tiles, result.Bundles)
	}

	cache, err := Open(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if got := countTiles(t, cache); got != 162 {
		t.Fatalf("overwritten cache has %d tiles, want 162", got)
	}
}

func TestMergeTPKXFixtures(t *testing.T) {
	overlayPath := copyTPKXFixture(t)
	overlay, err := Open(overlayPath)
	if err != nil {
		t.Fatal(err)
	}

	// The L00 and L01 fixture entries are not decodable PNG tiles, so remove
	// them from the overlay to exercise the valid TPKX merge path.
	invalidPaths := []string{"L00/R0000C0000.bundle", "L01/R0000C0000.bundle"}
	updates := make([]BundleUpdate, 0, len(invalidPaths))
	for _, path := range invalidPaths {
		bundle := readTestBundle(t, overlay, path)
		coordinate := coordinateForIndex(path, nonEmptyTileIndices(bundle)[0])
		updates = append(updates, BundleUpdate{
			Level: coordinate.Level,
			Row:   coordinate.Row,
			Col:   coordinate.Col,
			Tiles: map[TileCoordinate][]byte{coordinate: nil},
		})
	}
	if err := overlay.UpdateBundles(context.Background(), updates, 2); err != nil {
		overlay.Close()
		t.Fatal(err)
	}
	overlay.Close()

	originPath := filepath.Join("..", "data", "Usa.tpkx")
	output := filepath.Join(t.TempDir(), "merged.tpkx")
	result, err := MergeCaches(
		context.Background(),
		originPath,
		overlayPath,
		MergeOptions{Output: output, Workers: 4},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bundles != 4 {
		t.Fatalf("merged %d bundles, want 4", result.Bundles)
	}
	if result.Tiles != 142 {
		t.Fatalf("merged %d tiles, want 142", result.Tiles)
	}

	merged, err := Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer merged.Close()
	if got := countTiles(t, merged); got != 144 {
		t.Fatalf("output has %d tiles, want 144", got)
	}
}

func TestCompositeTileKeepsOriginFormat(t *testing.T) {
	origin := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	origin.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	overlay := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	overlay.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 100, B: 50, A: 128})

	var originJPEG bytes.Buffer
	if err := jpeg.Encode(&originJPEG, origin, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	var overlayPNG bytes.Buffer
	if err := png.Encode(&overlayPNG, overlay); err != nil {
		t.Fatal(err)
	}

	composed, err := compositeTile(originJPEG.Bytes(), overlayPNG.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(composed, []byte{0xff, 0xd8}) {
		t.Fatal("composed tile did not retain the origin JPEG format")
	}
	decoded, _, err := decodeTileImage(composed)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 2 || decoded.Bounds().Dy() != 2 {
		t.Fatalf("composed tile has unexpected dimensions %v", decoded.Bounds())
	}
}

func countTiles(t *testing.T, cache *Tiles) int {
	t.Helper()
	paths, err := cache.bundlePaths()
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, path := range paths {
		total += len(nonEmptyTileIndices(readTestBundle(t, cache, path)))
	}
	return total
}

func firstEmptyIndex(bundle *compactBundle) int {
	for index, tile := range bundle.tiles {
		if len(tile) == 0 {
			return index
		}
	}
	return -1
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 64, B: 32, A: 255})
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
