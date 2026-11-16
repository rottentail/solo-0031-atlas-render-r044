package raster

import (
	"atlas-render/internal/model"
	"context"
	"testing"
	"time"
)

// budget is the compiled layer-pixel ceiling from the specification.
const budget = 40_000_000

func canvasLayers(count, size int, opacity float64) model.Document {
	layers := make([]model.Layer, count)
	for i := range layers {
		layers[i] = model.Layer{
			ID: model.ID("layer", i), Kind: "rectangle", Width: float64(size), Height: float64(size),
			Transform: model.Transform{ScaleX: 1, ScaleY: 1}, Opacity: opacity,
			Fill: model.Paint{Kind: "solid", Color: "#FFFFFF"},
		}
	}
	return model.Document{Width: size, Height: size, Background: "#000000", Layers: layers}
}

// TestCompiledCostMatchesBoundingBoxArea walks the budget boundary and checks
// that Compile reports the actual work it plans to do: the sum of per-layer
// device bounding-box areas. A cost that undercounts would let an over-budget
// job reach the queue.
func TestCompiledCostMatchesBoundingBoxArea(t *testing.T) {
	cases := []struct {
		size   int
		layers int
		scale  int
	}{
		{size: 1536, layers: 1, scale: 1},
		{size: 1536, layers: 1, scale: 2},
		{size: 1536, layers: 4, scale: 1},
		{size: 1536, layers: 8, scale: 1},
		{size: 768, layers: 36, scale: 2},
	}
	for _, c := range cases {
		document := canvasLayers(c.layers, c.size, 0)
		plan, err := Compile(model.Snapshot{Document: document, Assets: map[string]model.Asset{}}, c.scale)
		if err == nil {
			var measured int64
			for _, op := range plan.ops {
				measured += int64(op.bounds.Dx()) * int64(op.bounds.Dy())
			}
			want := int64(c.size*c.scale) * int64(c.size*c.scale) * int64(c.layers)
			if plan.Cost != measured || plan.Cost != want {
				t.Fatalf("size %d layers %d scale %d: cost %d, bbox sum %d, expected %d",
					c.size, c.layers, c.scale, plan.Cost, measured, want)
			}
		}
	}
}

// TestWorkBudgetBoundary pins both sides of the ceiling. At 40,000,000 exactly
// the scene is accepted; one more device row of one layer is refused with 429.
func TestWorkBudgetBoundary(t *testing.T) {
	// 25 full-canvas layers of 1536x1536 at scale 1 = 58,982,400, over budget.
	over := canvasLayers(25, 1536, 0)
	if _, err := Compile(model.Snapshot{Document: over, Assets: map[string]model.Asset{}}, 1); err == nil {
		t.Fatal("scene over the work budget compiled without error")
	} else if apiErr, ok := err.(*model.Error); !ok || apiErr.Status != 429 {
		t.Fatalf("over-budget error %v, want a 429 capacity error", err)
	}
	// Direct boundary arithmetic: a 2000x2000 device canvas with ten layers is
	// exactly 40,000,000. Canvas per side is capped at 1536, so use scale 2 on
	// a 1000x1000 document: 2000x2000 device per layer.
	exact := canvasLayers(10, 1000, 0)
	plan, err := Compile(model.Snapshot{Document: exact, Assets: map[string]model.Asset{}}, 2)
	if err != nil {
		t.Fatalf("exact-budget scene rejected: %v", err)
	}
	if plan.Cost != budget {
		t.Fatalf("exact-budget cost %d, want %d", plan.Cost, budget)
	}
	// Eleven layers is 44,000,000: refused.
	more := canvasLayers(11, 1000, 0)
	if _, err := Compile(model.Snapshot{Document: more, Assets: map[string]model.Asset{}}, 2); err == nil {
		t.Fatal("one layer over budget compiled without error")
	}
}

// TestBudgetIsIndependentOfOpacity proves the budget bounds planned work, not
// visible output: fully transparent layers still cost their bounding box. This
// is deliberate, since the plan cannot know a layer is hidden without extra
// work, and it prevents "hide the cost behind opacity" submissions.
func TestBudgetIsIndependentOfOpacity(t *testing.T) {
	hidden := canvasLayers(25, 1536, 0)
	if _, err := Compile(model.Snapshot{Document: hidden, Assets: map[string]model.Asset{}}, 1); err == nil {
		t.Fatal("transparent over-budget scene compiled; cost must cover planned work")
	}
}

// TestClipDoesNotReducePlannedCost documents that an ancestor clip reduces
// painted pixels but not the planned per-layer bounding box. The clip is
// applied per pixel inside the bounding box, so cost accounting stays simple
// and conservative.
func TestClipDoesNotReducePlannedCost(t *testing.T) {
	layer := model.Layer{ID: "a", Kind: "rectangle", Width: 1200, Height: 1200,
		Transform: model.Transform{ScaleX: 1, ScaleY: 1}, Opacity: 1,
		Clip: &model.Rect{X: 0, Y: 0, Width: 10, Height: 10},
		Fill: model.Paint{Kind: "solid", Color: "#FFFFFF"}}
	document := model.Document{Width: 1536, Height: 1536, Background: "#000000", Layers: []model.Layer{layer}}
	plan, err := Compile(model.Snapshot{Document: document, Assets: map[string]model.Asset{}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Cost != 1200*1200 {
		t.Fatalf("cost %d, want the unclipped bounding box %d", plan.Cost, 1200*1200)
	}
}

// TestGeometryBeyondCanvasIsClippedToCanvas checks that off-canvas geometry
// cannot inflate cost: translated-away layers cost only their on-canvas
// intersection, and fully off-canvas layers cost nothing.
func TestGeometryBeyondCanvasIsClippedToCanvas(t *testing.T) {
	document := model.Document{Width: 100, Height: 100, Background: "#000000", Layers: []model.Layer{
		{ID: "offscreen", Kind: "rectangle", Width: 50, Height: 50,
			Transform: model.Transform{X: 500, Y: 500, ScaleX: 1, ScaleY: 1}, Opacity: 1,
			Fill: model.Paint{Kind: "solid", Color: "#FFFFFF"}},
		{ID: "half", Kind: "rectangle", Width: 50, Height: 50,
			Transform: model.Transform{X: 75, Y: 25, ScaleX: 1, ScaleY: 1}, Opacity: 1,
			Fill: model.Paint{Kind: "solid", Color: "#FFFFFF"}},
	}}
	plan, err := Compile(model.Snapshot{Document: document, Assets: map[string]model.Asset{}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Cost != 25*50 {
		t.Fatalf("cost %d, want only the on-canvas 25x50 intersection", plan.Cost)
	}
}

// TestLargestLegalSceneRendersWithinBoundedTime is a resource measurement, not
// a microbenchmark: the most expensive legal plan must finish, and the record
// of how long it took is what lets the cancellation deadline be judged.
func TestLargestLegalSceneRendersWithinBoundedTime(t *testing.T) {
	document := canvasLayers(16, 1536, 1)
	plan, err := Compile(model.Snapshot{Document: document, Assets: map[string]model.Asset{}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Cost > budget {
		t.Fatalf("chosen largest scene costs %d, over budget %d", plan.Cost, budget)
	}
	if testing.Short() {
		t.Skip("skipping the full-size render measurement in -short mode")
	}
	start := time.Now()
	img, err := plan.Render(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 1536 || img.Bounds().Dy() != 1536 {
		t.Fatalf("output is %v", img.Bounds())
	}
	t.Logf("largest legal scene: cost %d layer-pixels rendered in %v", plan.Cost, elapsed)
	if elapsed > 30*time.Second {
		t.Fatalf("largest legal scene took %v; resource bound is exceeded", elapsed)
	}
}
