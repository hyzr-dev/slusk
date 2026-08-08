package soulseek

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul/peer"
)

var benchmarkShareSnapshotMatches []peer.File

func benchmarkShareSearch(size int) []*indexedFile {
	search := make([]*indexedFile, size)
	for i := range search {
		virtual := fmt.Sprintf(`Music\Artist %06d\Album\Track %06d.FLAC`, i, i)
		if i%100 == 0 {
			virtual = fmt.Sprintf(`Music\Artist %06d\Rare Needle\Track %06d.FLAC`, i, i)
		}
		search[i] = &indexedFile{
			virtual:      virtual,
			virtualLower: strings.ToLower(virtual),
			wire:         peer.File{Name: virtual},
		}
	}
	sort.Slice(search, func(i, j int) bool { return search[i].virtualLower < search[j].virtualLower })
	return search
}

func benchmarkShareSnapshot(b *testing.B, size int) *shareSnapshot {
	b.Helper()
	search := benchmarkShareSearch(size)
	trigrams, err := buildShareTrigramIndex(context.Background(), search)
	if err != nil {
		b.Fatal(err)
	}
	return &shareSnapshot{search: search, trigrams: trigrams}
}

func sharePostingPayloadBytes(index map[shareTrigram][]uint32) int64 {
	var bytes int64
	for _, posting := range index {
		bytes += int64(cap(posting)) * 4
	}
	return bytes
}

// benchmarkExcludedPhrases is the list the live server pushed on 2026-07-31 -
// a handful of specific takedowns, not a broad filter (#324). Its length is
// what the per-search cost scales with, so use the real one.
var benchmarkExcludedPhrases = []string{
	"bryan adams",
	"from zero",
	"housezzz",
	"look outside your window",
	"paper kingdom",
	"village people",
}

func BenchmarkShareSnapshotMatch(b *testing.B) {
	for _, size := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("files=%d", size), func(b *testing.B) {
			snapshot := benchmarkShareSnapshot(b, size)
			postingBytesPerFile := float64(sharePostingPayloadBytes(snapshot.trigrams)) / float64(size)
			repeatedCommonQuery := strings.Repeat("music ", 2_700) + "-flac"
			queries := []struct {
				name  string
				query string
			}{
				{name: "selective", query: fmt.Sprintf("rare needle %06d", size-100)},
				{name: "miss", query: "definitely-missing"},
				{name: "common", query: "music artist"},
				{name: "exclusion", query: "music -needle"},
				{name: "common-all-excluded", query: "music artist album track -flac"},
				{name: "repeated-common-all-excluded", query: repeatedCommonQuery},
				{name: "mixed-short-indexed", query: "mu track 000123"},
				{name: "short-fallback", query: "mu ar"},
			}
			for _, query := range queries {
				b.Run(query.name+"/indexed", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						benchmarkShareSnapshotMatches = snapshot.match(query.query, maxSharedSearchResults, nil)
					}
					b.ReportMetric(postingBytesPerFile, "posting-payload-bytes/file")
				})
				b.Run(query.name+"/linear", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						benchmarkShareSnapshotMatches = linearShareSnapshotMatch(snapshot, query.query, maxSharedSearchResults)
					}
				})
			}
			// The exclusion filter runs per candidate file, so measure it on
			// the query with the most candidates. Every case above passes nil,
			// which is the one input shape that skips the filter entirely.
			b.Run("common/indexed-excluded-phrases", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchmarkShareSnapshotMatches = snapshot.match("music artist", maxSharedSearchResults, &benchmarkExcludedPhrases)
				}
			})
		})
	}
}

func BenchmarkBuildShareTrigramIndex(b *testing.B) {
	for _, size := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("files=%d", size), func(b *testing.B) {
			search := benchmarkShareSearch(size)
			baseline, err := buildShareTrigramIndex(context.Background(), search)
			if err != nil {
				b.Fatal(err)
			}
			postingBytesPerFile := float64(sharePostingPayloadBytes(baseline)) / float64(size)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := buildShareTrigramIndex(context.Background(), search); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(postingBytesPerFile, "posting-payload-bytes/file")
		})
	}
}
