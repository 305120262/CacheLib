# Repository Guidelines

## Project Scope

CacheLib is a Go library for reading and writing Esri/ArcGIS tile caches(https://github.com/Esri/raster-tiles-compactcache/blob/master/CompactCacheV2.md). It supports
directory-based compact v2 caches and compressed `.tpkx` caches.

- `common/` contains shared XML/JSON metadata models and compression helpers.
- `compactv2/` loads cache metadata, maps tile coordinates to bundle files, and
  retrieves tile bytes or presence maps.
- `data/zhuhai/` is a compact v2 cache fixture; `data/Usa.tpkx` is a TPKX
  fixture.

## Development Constraints

- The repository currently has no `go.mod`. The source imports
  `sean.lo/tile-server/common`, so confirm or update that import path before
  adding a module file.
- Keep exported APIs compatible unless the task explicitly calls for a breaking
  change. Preserve ArcGIS cache semantics, bundle naming, byte offsets, and
  endianness when editing parsing code.
- Treat files under `data/` as binary fixtures. Do not rewrite or reformat them.
- Keep source changes focused and follow standard Go formatting.
- Add focused tests when changing bundle parsing, coordinate conversion,
  metadata decoding, or boundary handling. There is currently no test suite.

## Verification

After a Go module is available, run:

```powershell
gofmt -w <changed-go-files>
go test ./...
go vet ./...
```

If a change depends on cache-format behavior, exercise at least one path from
each supported source: the compact v2 fixture in `data/zhuhai/` and
`data/Usa.tpkx`.
