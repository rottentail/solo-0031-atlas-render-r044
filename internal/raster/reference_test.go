package raster

import (
	"atlas-render/internal/model"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// referenceSample is one entry of testdata/reference/manifest.json. The
// manifest and its PNGs are produced by scripts/reference_render.py, an
// independent Python implementation of the documented drawing semantics. The
// Go side never regenerates goldens: it renders the same scene through the
// production path and compares decoded pixels against both the golden and the
// geometry-only relations, so a regression is reported as a coordinate or
// coverage error rather than as an opaque hash change.
type referenceSample struct {
	Name       string                     `json:"name"`
	Scene      string                     `json:"scene"`
	Golden     map[string]string          `json:"golden"`
	Derivation string                     `json:"derivation"`
	Relations  map[string]json.RawMessage `json:"relations"`
	Tolerance  float64                    `json:"tolerance"`
}

type referenceRelation struct {
	Coverage float64            `json:"coverage"`
	Centroid []float64          `json:"centroid"`
	BBox     []float64          `json:"bbox"`
	Boundary float64            `json:"boundary"`
	Colors   map[string]float64 `json:"colors"`
}

func loadReferenceSamples(t *testing.T) []referenceSample {
	t.Helper()
	path := filepath.Join("testdata", "reference", "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reference manifest missing (%v); regenerate with python3 scripts/reference_render.py", err)
	}
	var manifest struct {
		Samples []referenceSample `json:"samples"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if len(manifest.Samples) == 0 {
		t.Fatal("reference manifest contains no samples")
	}
	return manifest.Samples
}

func loadSampleScene(t *testing.T, dir, name string) model.Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("scene %s missing: %v", name, err)
	}
	var document model.Document
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("scene %s invalid: %v", name, err)
	}
	return document
}

func decodeGolden(t *testing.T, path string) image.Image {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s missing: %v", path, err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("golden %s invalid: %v", path, err)
	}
	return img
}

func renderDocument(t *testing.T, document model.Document, scale int) *image.NRGBA {
	t.Helper()
	plan, err := Compile(model.Snapshot{Document: document, Assets: map[string]model.Asset{}}, scale)
	if err != nil {
		t.Fatalf("compile scale %d: %v", scale, err)
	}
	img, err := plan.Render(context.Background())
	if err != nil {
		t.Fatalf("render scale %d: %v", scale, err)
	}
	return img
}

func background(document model.Document) color.NRGBA {
	var out color.NRGBA
	fmt.Sscanf(document.Background, "#%02X%02X%02X", &out.R, &out.G, &out.B)
	out.A = 255
	return out
}

type pixelStats struct {
	Covered  int
	MinX     int
	MinY     int
	MaxX     int
	MaxY     int
	Centroid [2]float64
	byColor  map[color.NRGBA]int
}

func measure(img image.Image, bg color.NRGBA) pixelStats {
	stats := pixelStats{MinX: 1 << 30, MinY: 1 << 30, MaxX: -1, MaxY: -1, byColor: map[color.NRGBA]int{}}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if pixel == bg {
				continue
			}
			stats.Covered++
			stats.byColor[pixel]++
			stats.Centroid[0] += float64(x)
			stats.Centroid[1] += float64(y)
			if x < stats.MinX {
				stats.MinX = x
			}
			if x > stats.MaxX {
				stats.MaxX = x
			}
			if y < stats.MinY {
				stats.MinY = y
			}
			if y > stats.MaxY {
				stats.MaxY = y
			}
		}
	}
	if stats.Covered > 0 {
		stats.Centroid[0] /= float64(stats.Covered)
		stats.Centroid[1] /= float64(stats.Covered)
	}
	return stats
}

func parseHex(text string) (color.NRGBA, error) {
	var out color.NRGBA
	_, err := fmt.Sscanf(text, "#%02X%02X%02X", &out.R, &out.G, &out.B)
	out.A = 255
	return out, err
}

func compareImages(a, b image.Image) (int, int) {
	differing, maxDelta := 0, 0
	bounds := a.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			ca := color.NRGBAModel.Convert(a.At(x, y)).(color.NRGBA)
			cb := color.NRGBAModel.Convert(b.At(x, y)).(color.NRGBA)
			delta := 0
			for _, pair := range [][2]uint8{{ca.R, cb.R}, {ca.G, cb.G}, {ca.B, cb.B}, {ca.A, cb.A}} {
				if d := int(pair[0]) - int(pair[1]); abs(d) > delta {
					delta = abs(d)
				}
			}
			if delta > 0 {
				differing++
				if delta > maxDelta {
					maxDelta = delta
				}
			}
		}
	}
	return differing, maxDelta
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func scaleKey(scale int) string { return strconv.Itoa(scale) }
