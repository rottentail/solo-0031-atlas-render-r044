package raster

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
)

// TestReferenceSamplesReplay renders every frozen scene through the production
// path and compares it to the independent reference PNG pixel for pixel, then
// checks that the measured mask still satisfies the geometry-only relations in
// the manifest. A failure names the scene, the scale and the derived quantity,
// so the report distinguishes a coordinate bug from a stale golden.
func TestReferenceSamplesReplay(t *testing.T) {
	dir := filepath.Join("testdata", "reference")
	for _, sample := range loadReferenceSamples(t) {
		for _, scale := range []int{1, 2} {
			t.Run(sample.Name+"/scale"+scaleKey(scale), func(t *testing.T) {
				document := loadSampleScene(t, dir, sample.Scene)
				golden := decodeGolden(t, filepath.Join(dir, sample.Golden[scaleKey(scale)]))
				got := renderDocument(t, document, scale)
				if got.Bounds() != golden.Bounds() {
					t.Fatalf("output size %v, golden %v", got.Bounds(), golden.Bounds())
				}
				differing, maxDelta := compareImages(got, golden)
				if differing != 0 {
					t.Fatalf("%d of %d pixels differ from the independent reference (max channel delta %d) at scale %d\n%s",
						differing, got.Bounds().Dx()*got.Bounds().Dy(), maxDelta, scale, sample.Derivation)
				}
				checkRelations(t, sample, scale, measure(got, background(document)))
			})
		}
	}
}

func checkRelations(t *testing.T, sample referenceSample, scale int, stats pixelStats) {
	t.Helper()
	raw, ok := sample.Relations[scaleKey(scale)]
	if !ok {
		t.Fatalf("manifest has no relations for scale %d", scale)
	}
	var relation referenceRelation
	if err := json.Unmarshal(raw, &relation); err != nil {
		t.Fatalf("relations invalid: %v", err)
	}
	allowed := sample.Tolerance*relation.Coverage + 2
	if math.Abs(float64(stats.Covered)-relation.Coverage) > allowed {
		t.Fatalf("coverage %d, geometry derives %.2f (allowed ±%.2f): %s",
			stats.Covered, relation.Coverage, allowed, sample.Derivation)
	}
	if len(relation.Centroid) == 2 && stats.Covered > 0 {
		drift := math.Hypot(stats.Centroid[0]-relation.Centroid[0], stats.Centroid[1]-relation.Centroid[1])
		// A shape's sampled centroid can trail its mapped center by at most
		// roughly one boundary pixel per covered pixel.
		bound := 1.5 + relation.Boundary/float64(stats.Covered)
		if drift > bound {
			t.Fatalf("centroid (%.3f,%.3f) is %.3f away from the mapped center (%.3f,%.3f), bound %.3f: %s",
				stats.Centroid[0], stats.Centroid[1], drift,
				relation.Centroid[0], relation.Centroid[1], bound, sample.Derivation)
		}
	}
	if len(relation.BBox) == 4 && stats.Covered > 0 {
		got := []float64{float64(stats.MinX), float64(stats.MinY), float64(stats.MaxX + 1), float64(stats.MaxY + 1)}
		for i := range got {
			if math.Abs(got[i]-relation.BBox[i]) > 1 {
				t.Fatalf("bounding box %v, geometry derives %v: %s", got, relation.BBox, sample.Derivation)
			}
		}
	}
	for hex, want := range relation.Colors {
		wantColor, err := parseHex(hex)
		if err != nil {
			t.Fatalf("manifest color %q invalid: %v", hex, err)
		}
		if got := float64(stats.byColor[wantColor]); math.Abs(got-want) > allowed {
			t.Fatalf("color %s covers %.0f pixels, geometry derives %.0f: %s", hex, got, want, sample.Derivation)
		}
	}
}

func TestReferenceManifestIsPinned(t *testing.T) {
	// The manifest must come from the checked-in generator and every file it
	// names must exist; otherwise a silent regeneration could hide a change.
	samples := loadReferenceSamples(t)
	if len(samples) < 8 {
		t.Fatalf("only %d reference samples", len(samples))
	}
	seen := map[string]bool{}
	for _, sample := range samples {
		if sample.Scene == "" || sample.Derivation == "" || len(sample.Golden) != 2 {
			t.Fatalf("sample %q is incomplete: %+v", sample.Name, sample)
		}
		if seen[sample.Name] {
			t.Fatalf("duplicate sample %q", sample.Name)
		}
		seen[sample.Name] = true
		for _, scale := range []string{"1", "2"} {
			if _, ok := sample.Relations[scale]; !ok {
				t.Fatalf("sample %q lacks relations for scale %s", sample.Name, scale)
			}
		}
	}
}
