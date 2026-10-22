package compactv2

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestCompactBundleWritesTilesInIndexOrder(t *testing.T) {
	bundle := newCompactBundle()
	bundle.tiles[0] = []byte("first")
	bundle.tiles[129] = []byte("middle")
	bundle.tiles[16383] = []byte("last")

	data := bundle.encode()
	if got, want := binary.LittleEndian.Uint64(data[24:32]), uint64(len(data)); got != want {
		t.Fatalf("bundle length header = %d, want %d", got, want)
	}

	decoded, err := parseCompactBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 129, 16383} {
		if string(decoded.tiles[index]) != string(bundle.tiles[index]) {
			t.Fatalf("tile %d = %q, want %q", index, decoded.tiles[index], bundle.tiles[index])
		}
	}

	previousOffset := 0
	for index := range _bundleTileCount {
		position := _COMPACT_CACHE_HEADER_LENGTH + index*8
		record := data[position : position+8]
		offset := int(uint64(record[0]) |
			uint64(record[1])<<8 |
			uint64(record[2])<<16 |
			uint64(record[3])<<24 |
			uint64(record[4])<<32)
		if offset == 0 {
			continue
		}
		if offset <= previousOffset {
			t.Fatalf("tile %d offset %d is not after %d", index, offset, previousOffset)
		}
		previousOffset = offset
	}
}

func TestUpdateBundlesPreservesUnchangedTiles(t *testing.T) {
	cacheRoot := copyDirectoryCacheFixture(t)
	cache, err := Open(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	firstPath := "L12/R0680C0d00.bundle"
	secondPath := "L13/R0d80C1a00.bundle"
	firstBefore := readTestBundle(t, cache, firstPath)
	secondBefore := readTestBundle(t, cache, secondPath)
	firstIndices := nonEmptyTileIndices(firstBefore)
	secondIndices := nonEmptyTileIndices(secondBefore)
	if len(firstIndices) < 2 || len(secondIndices) < 1 {
		t.Fatal("fixture does not contain enough tiles")
	}

	firstUpdate := BundleUpdate{
		Level: 12,
		Row:   0,
		Col:   0,
		Tiles: map[TileCoordinate][]byte{},
	}
	for _, index := range firstIndices[:2] {
		coordinate := coordinateForIndex(firstPath, index)
		firstUpdate.Row = coordinate.Row
		firstUpdate.Col = coordinate.Col
		firstUpdate.Tiles[coordinate] = []byte{byte(index), 1, 2, 3}
	}
	secondCoordinate := coordinateForIndex(secondPath, secondIndices[0])
	secondUpdate := BundleUpdate{
		Level: secondCoordinate.Level,
		Row:   secondCoordinate.Row,
		Col:   secondCoordinate.Col,
		Tiles: map[TileCoordinate][]byte{
			secondCoordinate: {9, 8, 7},
		},
	}

	if err := cache.UpdateBundles(
		context.Background(),
		[]BundleUpdate{firstUpdate, secondUpdate},
		2,
	); err != nil {
		t.Fatal(err)
	}

	firstAfter := readTestBundle(t, cache, firstPath)
	secondAfter := readTestBundle(t, cache, secondPath)
	for index, tile := range firstAfter.tiles {
		if updated, ok := firstUpdate.Tiles[coordinateForIndex(firstPath, index)]; ok {
			if string(tile) != string(updated) {
				t.Fatalf("updated tile %d = %v, want %v", index, tile, updated)
			}
			continue
		}
		if string(tile) != string(firstBefore.tiles[index]) {
			t.Fatalf("unchanged tile %d was modified", index)
		}
	}
	if got := secondAfter.tiles[tileIndex(secondCoordinate.Row, secondCoordinate.Col)]; string(got) != string([]byte{9, 8, 7}) {
		t.Fatalf("second bundle update = %v", got)
	}
}

func TestUpdateBundlesRejectsDuplicateBundles(t *testing.T) {
	cache := &Tiles{}
	err := cache.UpdateBundles(context.Background(), []BundleUpdate{
		{Level: 1, Row: 0, Col: 0},
		{Level: 1, Row: 1, Col: 2},
	}, 2)
	if err == nil {
		t.Fatal("expected duplicate bundle error")
	}
}

func TestUpdateTPKXBundle(t *testing.T) {
	cachePath := copyTPKXFixture(t)
	cache, err := Open(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	bundlePath := "L02/R0000C0000.bundle"
	before := readTestBundle(t, cache, bundlePath)
	coordinate := coordinateForIndex(bundlePath, nonEmptyTileIndices(before)[0])
	replacement := testPNG(t)
	if err := cache.UpdateBundle(context.Background(), BundleUpdate{
		Level: coordinate.Level,
		Row:   coordinate.Row,
		Col:   coordinate.Col,
		Tiles: map[TileCoordinate][]byte{coordinate: replacement},
	}); err != nil {
		cache.Close()
		t.Fatal(err)
	}
	defer cache.Close()

	after := readTestBundle(t, cache, bundlePath)
	if string(after.tiles[tileIndex(coordinate.Row, coordinate.Col)]) != string(replacement) {
		t.Fatal("updated TPKX tile was not written")
	}
	for index, tile := range after.tiles {
		if index == tileIndex(coordinate.Row, coordinate.Col) {
			continue
		}
		if string(tile) != string(before.tiles[index]) {
			t.Fatalf("unchanged TPKX tile %d was modified", index)
		}
	}
}

func TestUpdateTPKXBundleAfterLegacyLoad(t *testing.T) {
	cachePath := copyTPKXFixture(t)
	cache := &Tiles{}
	cache.Load(cachePath, nil, nil, true)

	bundlePath := "L02/R0000C0000.bundle"
	before := readTestBundle(t, cache, bundlePath)
	coordinate := coordinateForIndex(bundlePath, nonEmptyTileIndices(before)[0])
	replacement := testPNG(t)
	if err := cache.UpdateBundle(context.Background(), BundleUpdate{
		Level: coordinate.Level,
		Row:   coordinate.Row,
		Col:   coordinate.Col,
		Tiles: map[TileCoordinate][]byte{coordinate: replacement},
	}); err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	after := readTestBundle(t, cache, bundlePath)
	if string(after.tiles[tileIndex(coordinate.Row, coordinate.Col)]) != string(replacement) {
		t.Fatal("legacy-loaded TPKX tile was not updated")
	}
}

func copyDirectoryCacheFixture(t *testing.T) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "cache")
	if err := copyDirectory(filepath.Join("..", "data", "zhuhai"), destination); err != nil {
		t.Fatal(err)
	}
	return destination
}

func copyTPKXFixture(t *testing.T) string {
	t.Helper()
	source := filepath.Join("..", "data", "Usa.tpkx")
	destination := filepath.Join(t.TempDir(), "Usa.tpkx")
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile(source, destination, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	return destination
}

func readTestBundle(t *testing.T, cache *Tiles, path string) *compactBundle {
	t.Helper()
	data, found, err := cache.readBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("bundle %s not found", path)
	}
	bundle, err := parseCompactBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func nonEmptyTileIndices(bundle *compactBundle) []int {
	indices := make([]int, 0)
	for index, tile := range bundle.tiles {
		if len(tile) != 0 {
			indices = append(indices, index)
		}
	}
	return indices
}

func coordinateForIndex(path string, index int) TileCoordinate {
	level, baseRow, baseCol, _ := parseBundlePath(path)
	return TileCoordinate{
		Level: level,
		Row:   baseRow + index/_BUNDLX_MAXIDX,
		Col:   baseCol + index%_BUNDLX_MAXIDX,
	}
}
