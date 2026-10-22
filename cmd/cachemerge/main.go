package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"

	"sean.lo/cachelib/compactv2"
)

func main() {
	origin := flag.String("origin", "", "origin compact v2 cache directory or .tpkx file")
	overlay := flag.String("overlay", "", "overlay compact v2 cache directory or .tpkx file")
	output := flag.String("output", "", "new cache directory or .tpkx file; required unless -overwrite is set")
	overwrite := flag.Bool("overwrite", false, "replace the origin cache instead of creating a new cache")
	workers := flag.Int("workers", runtime.GOMAXPROCS(0), "maximum concurrent bundle and tile workers")
	flag.Parse()

	if *origin == "" || *overlay == "" {
		fmt.Fprintln(os.Stderr, "-origin and -overlay are required")
		flag.Usage()
		os.Exit(2)
	}
	if *workers < 1 {
		fmt.Fprintln(os.Stderr, "-workers must be at least 1")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	result, err := compactv2.MergeCaches(ctx, *origin, *overlay, compactv2.MergeOptions{
		Output:    *output,
		Overwrite: *overwrite,
		Workers:   *workers,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("merged %d tiles across %d bundles", result.Tiles, result.Bundles)
}
