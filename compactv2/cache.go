package compactv2

import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Open loads compact v2 cache metadata from a directory or a TPKX file.
// Callers should close caches opened with Open.
func Open(root string) (*Tiles, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat cache %q: %w", root, err)
	}

	t := &Tiles{RootPath: root}
	if info.IsDir() {
		if err := t.loadDirectory(); err != nil {
			return nil, err
		}
		return t, nil
	}

	if err := t.loadTPKX(); err != nil {
		return nil, err
	}
	return t, nil
}

// Close releases file handles held by a cache opened with Open.
func (t *Tiles) Close() error {
	if t == nil || t.zipReader == nil {
		return nil
	}
	err := t.zipReader.Close()
	t.zipReader = nil
	return err
}

func (t *Tiles) loadDirectory() error {
	confPath := ""
	for _, name := range []string{"Conf.xml", "conf.xml"} {
		path := filepath.Join(t.RootPath, name)
		if _, err := os.Stat(path); err == nil {
			confPath = path
			break
		}
	}
	if confPath == "" {
		return fmt.Errorf("cache %q has no Conf.xml or conf.xml", t.RootPath)
	}

	conf, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", confPath, err)
	}
	if err := xml.Unmarshal(conf, t); err != nil {
		return fmt.Errorf("parse %s: %w", confPath, err)
	}

	cdiPath := filepath.Join(t.RootPath, "conf.cdi")
	if cdi, err := os.ReadFile(cdiPath); err == nil {
		if err := xml.Unmarshal(cdi, &t.EnvelopeN); err != nil {
			return fmt.Errorf("parse %s: %w", cdiPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", cdiPath, err)
	}

	levels, err := t.directoryLevels()
	if err != nil {
		return err
	}
	if len(levels) > 0 {
		t.TileCacheInfo.MinLOD = levels[0]
		t.TileCacheInfo.MaxLOD = levels[len(levels)-1]
	}
	return nil
}

func (t *Tiles) directoryLevels() ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(t.RootPath, "_alllayers"))
	if err != nil {
		return nil, fmt.Errorf("read cache levels: %w", err)
	}

	levels := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) < 2 || entry.Name()[0] != 'L' {
			continue
		}
		level, err := strconv.Atoi(entry.Name()[1:])
		if err == nil {
			levels = append(levels, level)
		}
	}
	sort.Ints(levels)
	return levels, nil
}

func (t *Tiles) loadTPKX() error {
	r, err := zip.OpenReader(t.RootPath)
	if err != nil {
		return fmt.Errorf("open TPKX %q: %w", t.RootPath, err)
	}
	t.zipReader = r
	t.isTPKX = true
	t.fileIndex = make(map[string]int, len(r.File))
	for i, file := range r.File {
		t.fileIndex[file.Name] = i
	}

	root, ok := t.fileIndex["root.json"]
	if !ok {
		t.Close()
		return fmt.Errorf("TPKX %q has no root.json", t.RootPath)
	}
	reader, err := r.File[root].Open()
	if err != nil {
		t.Close()
		return fmt.Errorf("open root.json: %w", err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Close()
		return fmt.Errorf("read root.json: %w", readErr)
	}
	if closeErr != nil {
		t.Close()
		return fmt.Errorf("close root.json: %w", closeErr)
	}

	t.RootJSONFile = &RootJSONFile{}
	if err := json.Unmarshal(data, t.RootJSONFile); err != nil {
		t.Close()
		return fmt.Errorf("parse root.json: %w", err)
	}

	rootInfo := t.RootJSONFile
	t.TileCacheInfo = TileCacheInfo{
		OriginX:          rootInfo.TileInfo.Origin.X,
		OriginY:          rootInfo.TileInfo.Origin.Y,
		TileRows:         int16(rootInfo.TileInfo.Rows),
		TileColumns:      int16(rootInfo.TileInfo.Cols),
		DPI:              rootInfo.TileInfo.DPI,
		LODS:             rootInfo.TileInfo.LODs,
		SpatialReference: rootInfo.TileInfo.SpatialReference,
		MinLOD:           rootInfo.MinLOD,
		MaxLOD:           rootInfo.MaxLOD,
		MinScale:         rootInfo.MinScale,
		MaxScale:         rootInfo.MaxScale,
	}
	t.TileImageInfo = rootInfo.TileImageInfo
	return nil
}

func (t *Tiles) readBundle(path string) ([]byte, bool, error) {
	path = filepath.ToSlash(path)
	if t.isTPKX {
		reader := t.zipReader
		if reader == nil {
			var err error
			reader, err = zip.OpenReader(t.RootPath)
			if err != nil {
				return nil, false, fmt.Errorf("open TPKX %q: %w", t.RootPath, err)
			}
			defer reader.Close()
		}
		archivePath := "tile/" + path
		file := findZipFile(reader.File, archivePath)
		if file == nil {
			return nil, false, nil
		}
		entry, err := file.Open()
		if err != nil {
			return nil, false, fmt.Errorf("open %s: %w", archivePath, err)
		}
		data, readErr := io.ReadAll(entry)
		closeErr := entry.Close()
		if readErr != nil {
			return nil, false, fmt.Errorf("read %s: %w", archivePath, readErr)
		}
		if closeErr != nil {
			return nil, false, fmt.Errorf("close %s: %w", archivePath, closeErr)
		}
		return data, true, nil
	}

	fullPath := filepath.Join(t.RootPath, "_alllayers", filepath.FromSlash(path))
	data, err := os.ReadFile(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", fullPath, err)
	}
	return data, true, nil
}

func (t *Tiles) bundlePaths() (map[string]string, error) {
	paths := make(map[string]string)
	if t.isTPKX {
		reader := t.zipReader
		if reader == nil {
			var err error
			reader, err = zip.OpenReader(t.RootPath)
			if err != nil {
				return nil, fmt.Errorf("open TPKX %q: %w", t.RootPath, err)
			}
			defer reader.Close()
		}
		for _, file := range reader.File {
			if !strings.HasPrefix(file.Name, "tile/") || !strings.EqualFold(filepath.Ext(file.Name), _BUNDLE_EXT) {
				continue
			}
			path := strings.TrimPrefix(file.Name, "tile/")
			paths[strings.ToLower(path)] = path
		}
		return paths, nil
	}

	root := filepath.Join(t.RootPath, "_alllayers")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), _BUNDLE_EXT) {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		slashPath := filepath.ToSlash(relative)
		paths[strings.ToLower(slashPath)] = slashPath
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list cache bundles: %w", err)
	}
	return paths, nil
}

func findZipFile(files []*zip.File, name string) *zip.File {
	for _, file := range files {
		if strings.EqualFold(file.Name, name) {
			return file
		}
	}
	return nil
}
