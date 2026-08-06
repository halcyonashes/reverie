package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"reverie/internal/memloc"
	"reverie/internal/store"
)

// newLocationTestSet builds a Set with two independent real stores, the local one
// fenced exactly as memloc.Open would fence it.
func newLocationTestSet(t *testing.T) *memloc.Set {
	t.Helper()
	global := newMCPTestStore(t)
	local := newMCPTestStore(t)
	if _, err := local.ApplyLocalIDFence(); err != nil {
		t.Fatalf("ApplyLocalIDFence: %v", err)
	}
	return &memloc.Set{Global: global, Local: local, LocalDir: "/repo/.claude/engram"}
}

// echoStore returns a handler that reports which store it was bound to, wrapped
// in the same envelope shape the real handlers produce.
func echoStore(label string) storeHandler {
	return func(s *store.Store) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
			envelope := map[string]any{"project": "p", "result": label}
			out, _ := jsonMarshal(envelope)
			return mcppkg.NewToolResultText(string(out)), nil
		}
	}
}

// storeLabeller labels each store distinctly so dispatch can be observed.
func storeLabeller(set *memloc.Set) storeHandler {
	return func(s *store.Store) server.ToolHandlerFunc {
		label := "GLOBAL-STORE"
		if s == set.Local {
			label = "LOCAL-STORE"
		}
		return echoStore(label)(s)
	}
}

func request(args map[string]any) mcppkg.CallToolRequest {
	return mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: args}}
}

func envelopeOf(t *testing.T, res *mcppkg.CallToolResult) map[string]any {
	t.Helper()
	text := callResultText(t, res)
	var envelope map[string]any
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("result is not a JSON envelope: %v\n%s", err, text)
	}
	return envelope
}

// ─── merged reads ────────────────────────────────────────────────────────────

// TestMergedLocation_DefaultShowsBothSections is the core read behavior: an
// unqualified read reaches both stores.
func TestMergedLocation_DefaultShowsBothSections(t *testing.T) {
	set := newLocationTestSet(t)
	h := mergedLocation(set, storeLabeller(set))

	res, err := h(context.Background(), request(map[string]any{}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	env := envelopeOf(t, res)

	body, _ := env["result"].(string)
	if !strings.Contains(body, "LOCAL-STORE") || !strings.Contains(body, "GLOBAL-STORE") {
		t.Fatalf("merged body missing a store:\n%s", body)
	}
	if !strings.Contains(body, localHeader) || !strings.Contains(body, globalHeader) {
		t.Fatalf("merged body missing section headers:\n%s", body)
	}
	if strings.Index(body, localHeader) > strings.Index(body, globalHeader) {
		t.Fatal("local section should come first")
	}
	if env[locationArg] != string(memloc.Both) {
		t.Fatalf("location = %v, want both", env[locationArg])
	}
	if env["id_scope_note"] == nil {
		t.Fatal("merged result should explain that ids are per-store")
	}
}

func TestMergedLocation_ExplicitLocalOnly(t *testing.T) {
	set := newLocationTestSet(t)
	h := mergedLocation(set, storeLabeller(set))

	res, err := h(context.Background(), request(map[string]any{"location": "local"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	env := envelopeOf(t, res)
	body, _ := env["result"].(string)

	if !strings.Contains(body, "LOCAL-STORE") {
		t.Fatalf("expected local result, got %q", body)
	}
	if strings.Contains(body, "GLOBAL-STORE") {
		t.Fatalf("location=local leaked global results: %q", body)
	}
	if env[locationArg] != string(memloc.Local) {
		t.Fatalf("location = %v, want local", env[locationArg])
	}
}

func TestMergedLocation_ExplicitGlobalOnly(t *testing.T) {
	set := newLocationTestSet(t)
	h := mergedLocation(set, storeLabeller(set))

	res, err := h(context.Background(), request(map[string]any{"location": "global"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body, _ := envelopeOf(t, res)["result"].(string)
	if !strings.Contains(body, "GLOBAL-STORE") || strings.Contains(body, "LOCAL-STORE") {
		t.Fatalf("location=global returned %q", body)
	}
}

// TestMergedLocation_NoLocalStoreIsUnchanged is the regression guarantee for
// every repository that never opted in: a plain passthrough, no headers.
func TestMergedLocation_NoLocalStoreIsUnchanged(t *testing.T) {
	set := memloc.GlobalOnly(newMCPTestStore(t))
	h := mergedLocation(set, echoStore("GLOBAL-STORE"))

	res, err := h(context.Background(), request(map[string]any{}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body, _ := envelopeOf(t, res)["result"].(string)
	if body != "GLOBAL-STORE" {
		t.Fatalf("expected untouched passthrough, got %q", body)
	}
}

// TestMergedLocation_LocalRequestedWithoutStore — an explicit local request must
// fail loudly rather than quietly returning global memories.
func TestMergedLocation_LocalRequestedWithoutStore(t *testing.T) {
	set := memloc.GlobalOnly(newMCPTestStore(t))
	h := mergedLocation(set, echoStore("GLOBAL-STORE"))

	res, err := h(context.Background(), request(map[string]any{"location": "local"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	env := envelopeOf(t, res)
	if env["error_code"] != "no_local_store" {
		t.Fatalf("error_code = %v, want no_local_store", env["error_code"])
	}
	if !strings.Contains(env["hint"].(string), "reverie local init") {
		t.Fatalf("hint should point at `reverie local init`, got %v", env["hint"])
	}
}

func TestMergedLocation_UnknownLocationErrors(t *testing.T) {
	set := newLocationTestSet(t)
	h := mergedLocation(set, storeLabeller(set))

	res, err := h(context.Background(), request(map[string]any{"location": "somewhere"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unknown location")
	}
}

// TestMergeResults_SurvivesOneSideErroring — one unhappy store must not sink a
// merged read.
func TestMergeResults_SurvivesOneSideErroring(t *testing.T) {
	good := mcppkg.NewToolResultText(`{"project":"p","result":"good side"}`)
	bad := mcppkg.NewToolResultText(`{"error_code":"unknown_project","message":"nope"}`)
	bad.IsError = true

	merged := mergeResults(bad, good)
	if merged.IsError {
		t.Fatal("merged result should not be an error when one side succeeded")
	}
	body, _ := envelopeOf(t, merged)["result"].(string)
	if !strings.Contains(body, "good side") || !strings.Contains(body, "nope") {
		t.Fatalf("merged body should carry both sides, got %q", body)
	}

	bothBad := mergeResults(bad, bad)
	if !bothBad.IsError {
		t.Fatal("merged result should be an error when both sides failed")
	}
}

// ─── writes ──────────────────────────────────────────────────────────────────

// TestWithWriteLocation_DefaultsToGlobal — unqualified saves keep their historical
// destination, so proactive auto-saves never get trapped in one repository.
func TestWithWriteLocation_DefaultsToGlobal(t *testing.T) {
	set := newLocationTestSet(t)
	h := withWriteLocation(set, storeLabeller(set))

	res, err := h(context.Background(), request(map[string]any{}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body, _ := envelopeOf(t, res)["result"].(string)
	if body != "GLOBAL-STORE" {
		t.Fatalf("unqualified write went to %q, want GLOBAL-STORE", body)
	}
}

func TestWithWriteLocation_ExplicitLocal(t *testing.T) {
	set := newLocationTestSet(t)
	h := withWriteLocation(set, storeLabeller(set))

	res, err := h(context.Background(), request(map[string]any{"location": "local"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body, _ := envelopeOf(t, res)["result"].(string)
	if body != "LOCAL-STORE" {
		t.Fatalf("location=local write went to %q", body)
	}
}

// ─── id routing ──────────────────────────────────────────────────────────────

// TestRouteByID_FencedIDGoesLocal is the payoff of the id fence: no `location`
// argument, and the id alone picks the right store.
func TestRouteByID_FencedIDGoesLocal(t *testing.T) {
	set := newLocationTestSet(t)
	h := routeByID(set, storeLabeller(set), "id")

	res, err := h(context.Background(), request(map[string]any{"id": float64(store.LocalIDFence + 5)}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	env := envelopeOf(t, res)
	if body, _ := env["result"].(string); body != "LOCAL-STORE" {
		t.Fatalf("fenced id routed to %q", body)
	}
	if env[locationArg] != string(memloc.Local) {
		t.Fatalf("location = %v, want local", env[locationArg])
	}
}

func TestRouteByID_SmallIDGoesGlobal(t *testing.T) {
	set := newLocationTestSet(t)
	h := routeByID(set, storeLabeller(set), "id")

	res, err := h(context.Background(), request(map[string]any{"id": float64(7)}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if body, _ := envelopeOf(t, res)["result"].(string); body != "GLOBAL-STORE" {
		t.Fatalf("global id routed to %q", body)
	}
}

// TestRouteByID_MissingIDGoesGlobal — absent ids keep pre-fork behavior.
func TestRouteByID_MissingIDGoesGlobal(t *testing.T) {
	set := newLocationTestSet(t)
	h := routeByID(set, storeLabeller(set), "id")

	res, err := h(context.Background(), request(map[string]any{}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if body, _ := envelopeOf(t, res)["result"].(string); body != "GLOBAL-STORE" {
		t.Fatalf("missing id routed to %q", body)
	}
}

// TestRouteByID_CrossStorePairIsRejected — relations live inside one database, so
// pairing a local id with a global one cannot be represented.
func TestRouteByID_CrossStorePairIsRejected(t *testing.T) {
	set := newLocationTestSet(t)
	h := routeByID(set, storeLabeller(set), "memory_id_a", "memory_id_b")

	res, err := h(context.Background(), request(map[string]any{
		"memory_id_a": float64(7),
		"memory_id_b": float64(store.LocalIDFence + 1),
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a cross-store error")
	}
	if code := envelopeOf(t, res)["error_code"]; code != "cross_store_relation" {
		t.Fatalf("error_code = %v, want cross_store_relation", code)
	}
}

func TestRouteByID_SameStorePairIsAccepted(t *testing.T) {
	set := newLocationTestSet(t)
	h := routeByID(set, storeLabeller(set), "memory_id_a", "memory_id_b")

	res, err := h(context.Background(), request(map[string]any{
		"memory_id_a": float64(store.LocalIDFence + 1),
		"memory_id_b": float64(store.LocalIDFence + 2),
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("same-store pair rejected: %s", callResultText(t, res))
	}
	if body, _ := envelopeOf(t, res)["result"].(string); body != "LOCAL-STORE" {
		t.Fatalf("local pair routed to %q", body)
	}
}

// TestRouteByID_FencedIDWithoutLocalStore — an id from a store that is not
// mounted must report that clearly, not silently hit global.
func TestRouteByID_FencedIDWithoutLocalStore(t *testing.T) {
	set := memloc.GlobalOnly(newMCPTestStore(t))
	h := routeByID(set, echoStore("GLOBAL-STORE"), "id")

	res, err := h(context.Background(), request(map[string]any{"id": float64(store.LocalIDFence + 1)}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected no_local_store error")
	}
	if code := envelopeOf(t, res)["error_code"]; code != "no_local_store" {
		t.Fatalf("error_code = %v, want no_local_store", code)
	}
}

// ─── fallback routing (opaque ids) ───────────────────────────────────────────

// TestWithFallbackLocation_TriesLocalThenGlobal covers mem_judge, whose id is an
// opaque hex string rather than a fenced integer.
func TestWithFallbackLocation_TriesLocalThenGlobal(t *testing.T) {
	set := newLocationTestSet(t)

	build := func(s *store.Store) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
			if s == set.Local {
				res := mcppkg.NewToolResultText(`{"message":"not found here"}`)
				res.IsError = true
				return res, nil
			}
			return mcppkg.NewToolResultText(`{"project":"p","result":"GLOBAL-STORE"}`), nil
		}
	}

	h := withFallbackLocation(set, build)
	res, err := h(context.Background(), request(map[string]any{}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("fallback should have reached global: %s", callResultText(t, res))
	}
	env := envelopeOf(t, res)
	if env[locationArg] != string(memloc.Global) {
		t.Fatalf("location = %v, want global", env[locationArg])
	}
}

// ─── discovery ───────────────────────────────────────────────────────────────

// TestWithLocalStoreInfo_AnnouncesLocalMemory — this is how the model learns a
// local store exists at all.
func TestWithLocalStoreInfo_AnnouncesLocalMemory(t *testing.T) {
	set := newLocationTestSet(t)
	h := withLocalStoreInfo(set, echoStore("x")(set.Global))

	env := envelopeOf(t, mustCall(t, h))
	if env["local_memory"] != true {
		t.Fatalf("local_memory = %v, want true", env["local_memory"])
	}
	if env["local_path"] != set.LocalDir {
		t.Fatalf("local_path = %v, want %q", env["local_path"], set.LocalDir)
	}
	if env["default_write"] != string(memloc.Global) {
		t.Fatalf("default_write = %v, want global", env["default_write"])
	}
}

func TestWithLocalStoreInfo_ReportsAbsentLocalMemory(t *testing.T) {
	set := memloc.GlobalOnly(newMCPTestStore(t))
	h := withLocalStoreInfo(set, echoStore("x")(set.Global))

	env := envelopeOf(t, mustCall(t, h))
	if env["local_memory"] != false {
		t.Fatalf("local_memory = %v, want false", env["local_memory"])
	}
}

func mustCall(t *testing.T, h server.ToolHandlerFunc) *mcppkg.CallToolResult {
	t.Helper()
	res, err := h(context.Background(), request(map[string]any{}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return res
}
