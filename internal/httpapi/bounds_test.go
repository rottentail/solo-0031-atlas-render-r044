package httpapi

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// legalScene is a small valid document used as a mutation base. All bound tests
// change exactly one property so a rejection names the culprit.
func legalScene() map[string]any {
	return map[string]any{
		"width": 64, "height": 48, "background": "#000000",
		"layers": []any{map[string]any{
			"id": "plate", "kind": "rectangle", "width": 32, "height": 24,
			"transform": map[string]any{"x": 4, "y": 6, "scaleX": 1, "scaleY": 1, "degrees": 0},
			"opacity":   1, "fill": map[string]any{"kind": "solid", "color": "#FFFFFF"},
		}},
	}
}

func cloneScene(scene map[string]any) map[string]any {
	layers := scene["layers"].([]any)
	copied := make([]any, len(layers))
	for i, layer := range layers {
		original := layer.(map[string]any)
		duplicate := map[string]any{}
		for key, value := range original {
			duplicate[key] = value
		}
		copied[i] = duplicate
	}
	return map[string]any{"width": scene["width"], "height": scene["height"],
		"background": scene["background"], "layers": copied}
}

func firstLayer(scene map[string]any) map[string]any {
	return scene["layers"].([]any)[0].(map[string]any)
}

func firstTransform(scene map[string]any) map[string]any {
	return firstLayer(scene)["transform"].(map[string]any)
}

// TestLegalGeometryIsAcceptedBeforeLimits guards the other direction of the
// contract: tightening must not reject documents that sit exactly on a legal
// boundary. Each case is the extreme legal value of one bound.
func TestLegalGeometryIsAcceptedBeforeLimits(t *testing.T) {
	cases := map[string]func(map[string]any){
		"canvas-max": func(s map[string]any) { s["width"], s["height"] = 1536, 1536 },
		"canvas-min": func(s map[string]any) {
			s["width"], s["height"] = 1, 1
			firstLayer(s)["width"] = 1
			firstLayer(s)["height"] = 1
		},
		"scale-min":      func(s map[string]any) { firstTransform(s)["scaleX"], firstTransform(s)["scaleY"] = 0.001, 0.001 },
		"scale-max":      func(s map[string]any) { firstTransform(s)["scaleX"], firstTransform(s)["scaleY"] = 100.0, 100.0 },
		"negative-scale": func(s map[string]any) { firstTransform(s)["scaleX"] = -0.001 },
		"opacity-zero":   func(s map[string]any) { firstLayer(s)["opacity"] = 0 },
		"opacity-one":    func(s map[string]any) { firstLayer(s)["opacity"] = 1 },
		"translate-bound": func(s map[string]any) {
			firstTransform(s)["x"], firstTransform(s)["y"] = 100000.0, -100000.0
		},
		"degrees-bound": func(s map[string]any) { firstTransform(s)["degrees"] = 100000.0 },
		"layer-max":     func(s map[string]any) { firstLayer(s)["width"], firstLayer(s)["height"] = 4096.0, 4096.0 },
		"clip-min": func(s map[string]any) {
			firstLayer(s)["clip"] = map[string]any{"x": 0, "y": 0, "width": 0.0001, "height": 0.0001}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			scene := legalScene()
			mutate(scene)
			h.json("PUT", "/v1/scenes/legal", map[string]any{
				"name": "legal", "expectedRevision": 0, "document": scene}, 200, nil)
		})
	}
}

// TestIllegalGeometryIsRejectedBeforeAcceptance enumerates the boundary
// violations the contract forbids. Each must produce 400 and must not leave a
// scene record, which is what "restricted before acceptance" means.
func TestIllegalGeometryIsRejectedBeforeAcceptance(t *testing.T) {
	cases := map[string]func(map[string]any){
		"canvas-too-wide":   func(s map[string]any) { s["width"] = 1537 },
		"canvas-zero":       func(s map[string]any) { s["height"] = 0 },
		"zero-scale-x":      func(s map[string]any) { firstTransform(s)["scaleX"] = 0 },
		"zero-scale-y":      func(s map[string]any) { firstTransform(s)["scaleY"] = 0 },
		"tiny-scale":        func(s map[string]any) { firstTransform(s)["scaleX"] = 0.0009 },
		"huge-scale":        func(s map[string]any) { firstTransform(s)["scaleY"] = 100.5 },
		"negative-opacity":  func(s map[string]any) { firstLayer(s)["opacity"] = -0.01 },
		"opacity-above-one": func(s map[string]any) { firstLayer(s)["opacity"] = 1.01 },
		"nan-translate":     func(s map[string]any) { firstTransform(s)["x"] = math.NaN() },
		"unbounded-degrees": func(s map[string]any) { firstTransform(s)["degrees"] = 100001.0 },
		"zero-layer-width":  func(s map[string]any) { firstLayer(s)["width"] = 0.0 },
		"oversize-layer":    func(s map[string]any) { firstLayer(s)["width"] = 4097.0 },
		"zero-clip-width": func(s map[string]any) {
			firstLayer(s)["clip"] = map[string]any{"x": 0, "y": 0, "width": 0, "height": 5}
		},
		"unknown-kind": func(s map[string]any) { firstLayer(s)["kind"] = "sprite" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			scene := legalScene()
			mutate(scene)
			body := h.json("PUT", "/v1/scenes/rejected", map[string]any{
				"name": "rejected", "expectedRevision": 0, "document": scene}, 400, nil)
			if body["code"] != "invalid" {
				t.Fatalf("expected invalid code, got %q (%v)", body["code"], body)
			}
			h.json("GET", "/v1/scenes/rejected", nil, 404, nil)
		})
	}
}

// TestScaleAndJobGeometryAreBoundedAtSubmission checks that output-scale and
// per-job geometry limits are enforced before a job is queued.
func TestScaleAndJobGeometryAreBoundedAtSubmission(t *testing.T) {
	h := newHarness(t)
	scene := legalScene()
	h.json("PUT", "/v1/scenes/base", map[string]any{"name": "base", "expectedRevision": 0, "document": scene}, 200, nil)
	snapshot := h.json("POST", "/v1/scenes/base/snapshots", map[string]any{"expectedRevision": 1}, 201, nil)
	id := snapshot["id"].(string)
	for _, scale := range []float64{0, 3, -1, 1.5} {
		body := h.json("POST", "/v1/jobs", map[string]any{"snapshotId": id, "scale": scale}, 400,
			map[string]string{"Idempotency-Key": fmt.Sprintf("bad-scale-%g", scale)})
		if body["code"] != "invalid" && body["code"] != "capacity" {
			t.Fatalf("scale %g: code %q", scale, body["code"])
		}
	}
	// No rejected submission may consume a queue slot or create a job.
	status := h.json("GET", "/v1/status", nil, 200, nil)
	jobs, _ := status["jobs"].(map[string]any)
	if len(jobs) != 0 {
		t.Fatalf("rejected submissions left jobs behind: %v", jobs)
	}
}

// TestWorkBudgetRejectsBeforeQueueing proves an over-budget scene is refused
// with 429 at submission and leaves no job, while a scene just under budget is
// accepted. The budget is 40,000,000 layer pixels.
func TestWorkBudgetRejectsBeforeQueueing(t *testing.T) {
	h := newHarness(t)
	// 1536x1536 at scale 2 is 4x the area: one full-canvas layer costs
	// 3072*3072 = 9,437,184; five such layers exceed the budget.
	layers := make([]any, 0, 5)
	for i := 0; i < 5; i++ {
		layers = append(layers, map[string]any{
			"id": fmt.Sprintf("sheet-%d", i), "kind": "rectangle", "width": 1536, "height": 1536,
			"transform": map[string]any{"x": 0, "y": 0, "scaleX": 1, "scaleY": 1, "degrees": 0},
			"opacity":   0, "fill": map[string]any{"kind": "solid", "color": "#FFFFFF"},
		})
	}
	heavy := map[string]any{"width": 1536, "height": 1536, "background": "#000000", "layers": layers}
	h.json("PUT", "/v1/scenes/heavy", map[string]any{"name": "heavy", "expectedRevision": 0, "document": heavy}, 200, nil)
	snapshot := h.json("POST", "/v1/scenes/heavy/snapshots", map[string]any{"expectedRevision": 1}, 201, nil)
	id := snapshot["id"].(string)
	body := h.json("POST", "/v1/jobs", map[string]any{"snapshotId": id, "scale": 2}, 429,
		map[string]string{"Idempotency-Key": "over-budget"})
	if body["code"] != "capacity" {
		t.Fatalf("over-budget code %q, want capacity", body["code"])
	}
	if jobs, _ := h.json("GET", "/v1/status", nil, 200, nil)["jobs"].(map[string]any); len(jobs) != 0 {
		t.Fatalf("over-budget submission queued a job: %v", jobs)
	}
	// The same scene at scale 1 costs 1536*1536*5 = 11,796,480 and is legal.
	h.json("POST", "/v1/jobs", map[string]any{"snapshotId": id, "scale": 1}, 202,
		map[string]string{"Idempotency-Key": "under-budget"})
}

// TestMalformedInputIsBounded checks the JSON envelope limits: unknown fields,
// trailing values, and oversized bodies are refused with 400.
func TestMalformedInputIsBounded(t *testing.T) {
	h := newHarness(t)
	valid := map[string]any{"name": "x", "expectedRevision": 0, "document": legalScene()}
	encoded := mustJSON(t, valid)
	h.do("PUT", "/v1/scenes/ok", encoded, nil)
	h.do("PUT", "/v1/scenes/ok", mustJSON(t, map[string]any{
		"name": "x", "expectedRevision": 1, "document": legalScene(), "extra": true}), nil)

	cases := map[string][]byte{
		"unknown-field": append(mustJSON(t, map[string]any{
			"name": "x", "expectedRevision": 0, "document": legalScene(), "unexpected": 1}), ""...),
		"trailing-value": append(append(mustJSON(t, map[string]any{
			"name": "x", "expectedRevision": 0, "document": legalScene()}), ' '), mustJSON(t, map[string]any{"a": 1})...),
		"not-object":          []byte(`[1,2,3]`),
		"truncated":           []byte(`{"name":`),
		"incomplete-document": []byte(`{"name":"x","expectedRevision":0,"document":{"width":1}}`),
	}
	for name, payload := range cases {
		if payload == nil {
			continue
		}
		status, body := h.do("PUT", "/v1/scenes/bad", payload, nil)
		if status != 400 && status != 413 {
			t.Fatalf("%s: status %d, want 400/413: %s", name, status, body)
		}
		if status, body := h.do("GET", "/v1/scenes/bad", nil, nil); status != 404 {
			t.Fatalf("%s: rejected scene was stored (GET status %d: %s)", name, status, body)
		}
	}
	// A scene that references an asset that is not available is refused before
	// acceptance too, but as a state conflict (409) rather than malformed input.
	t.Run("missing-asset", func(t *testing.T) {
		h := newHarness(t)
		scene := legalScene()
		firstLayer(scene)["kind"], firstLayer(scene)["assetId"] = "image", "absent"
		body := h.json("PUT", "/v1/scenes/rejected", map[string]any{
			"name": "rejected", "expectedRevision": 0, "document": scene}, 409, nil)
		if body["code"] != "conflict" {
			t.Fatalf("expected conflict code, got %q", body["code"])
		}
		h.json("GET", "/v1/scenes/rejected", nil, 404, nil)
	})

}

// TestBodyCapsAreEnforcedBeforeParsing checks both size caps: the 12 MiB JSON
// envelope and the 2 MiB asset upload. A cap violation must be a clean 400/413
// rather than memory growth or a panic.
func TestBodyCapsAreEnforcedBeforeParsing(t *testing.T) {
	h := newHarness(t)
	oversizedJSON := append([]byte(`{"name":"`), append(bytesOf('a', 13<<20), []byte(`"}`)...)...)
	status, body := h.do("PUT", "/v1/scenes/huge", oversizedJSON, nil)
	if status != 400 && status != 413 {
		t.Fatalf("oversized JSON status %d, want 400/413: %.200s", status, body)
	}

	oversizedAsset := bytesOf(0, 2<<20+1)
	status, body = h.do("PUT", "/v1/assets/huge", oversizedAsset, nil)
	if status != 400 && status != 413 {
		t.Fatalf("oversized asset status %d, want 400/413: %.200s", status, body)
	}
	if strings.Contains(string(body), "panic") {
		t.Fatalf("asset rejection leaked a panic: %s", body)
	}
	// The rejected asset must not exist as a usable record.
	h.json("POST", "/v1/assets/huge/retire", nil, 404, nil)
}
