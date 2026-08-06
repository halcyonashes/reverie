package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"reverie/internal/memloc"
	"reverie/internal/store"
)

// storeHandler builds a tool handler bound to one store. Every handler
// constructor in this package already has this shape once its cfg/activity
// arguments are closed over, which lets the wrappers below add local/global
// dispatch without touching a single handler body.
type storeHandler func(*store.Store) server.ToolHandlerFunc

// locationArg is the MCP argument name callers use to target a store.
const locationArg = "location"

// locationInstructions is appended to the server instructions when a repository
// has local memory. It is the mapping from what the user says to the `location`
// argument — without it the parameter exists but nothing connects it to the
// phrase "reverie local".
const locationInstructions = `

## MEMORY LOCATIONS — this repository has local memory

Two stores are active:
  local  — ` + "`<repo>/.claude/engram`" + `, visible only inside this repository
  global — ` + "`~/.engram`" + `, shared across every project on this machine

READS ARE MERGED across both by default. Output is split under
"local memory (this repo)" and "global memory" headers.

WRITES go to this repository's default target (global unless configured
otherwise) — check default_write in the mem_current_project response.

MAPPING WHAT THE USER SAYS ONTO location:
  "reverie local", "recall local", "save local", "only this repo",
    "memoria local", "solo este repo"          → location: "local"
  "reverie global", "recall global", "save global", "everywhere",
    "memoria global"                            → location: "global"
  no qualifier                                  → omit location (merged read /
                                                  default write)

Applies to: mem_save, mem_search, mem_context, mem_save_prompt,
mem_session_summary, mem_capture_passive, mem_session_start, mem_session_end,
mem_stats, mem_doctor.

DO NOT pass location to id-based tools — mem_get_observation, mem_update,
mem_delete, mem_timeline, mem_pin, mem_unpin, mem_review and mem_compare route
automatically from the id. Local ids are always greater than 1000000000. Pass
back the exact #id you saw and it reaches the right store. Never offset or edit
an id. mem_compare cannot relate a local memory to a global one.

If the user asks for local memory and the tool reports no_local_store, tell them
to run ` + "`reverie local init`" + ` in the repository root. Do NOT create it
silently, and do NOT fall back to global for an explicit "local" request.`

// section headers used when merging output from both stores. The model reads
// these to know which store an observation #id came from — ids are per-database
// AUTOINCREMENT values, so #7 can exist in both.
const (
	localHeader  = "─── local memory (this repo) ───"
	globalHeader = "─── global memory (~/.engram) ───"
)

// locationParam is the shared `location` argument declaration.
func locationParam() mcp.ToolOption {
	return mcp.WithString(locationArg, mcp.Description(memloc.ParamDescription))
}

// requestedLocation reads the raw `location` argument off a request.
func requestedLocation(req mcp.CallToolRequest) string {
	v, _ := req.GetArguments()[locationArg].(string)
	return v
}

// locationError converts a memloc resolution failure into a structured tool
// error. A caller who asked for local memory in a repo without it gets told how
// to create it rather than having the write silently redirected to global.
func locationError(set *memloc.Set, err error) *mcp.CallToolResult {
	envelope := map[string]any{
		"error_code": "invalid_location",
		"message":    err.Error(),
	}
	if errors.Is(err, memloc.ErrNoLocalStore) {
		envelope["error_code"] = "no_local_store"
		envelope["local_path"] = set.LocalDir
		envelope["hint"] = fmt.Sprintf(
			"This repository has no local memory store. Run `reverie local init` to create %s, or retry with location=global.",
			set.LocalDir,
		)
	}
	out, _ := jsonMarshal(envelope)
	result := mcp.NewToolResultText(string(out))
	result.IsError = true
	return result
}

// dispatcher pre-builds one handler per available store. Handlers are built at
// registration time, not per request, so dispatch costs nothing at call time.
type dispatcher struct {
	set    *memloc.Set
	global server.ToolHandlerFunc
	local  server.ToolHandlerFunc
}

func newDispatcher(set *memloc.Set, build storeHandler) *dispatcher {
	d := &dispatcher{set: set, global: build(set.Global)}
	if set.HasLocalStore() {
		d.local = build(set.Local)
	}
	return d
}

// handlerFor returns the pre-built handler for a single-target location.
func (d *dispatcher) handlerFor(loc memloc.Location) (server.ToolHandlerFunc, error) {
	switch loc {
	case memloc.Local:
		if d.local == nil {
			return nil, memloc.ErrNoLocalStore
		}
		return d.local, nil
	case memloc.Global:
		return d.global, nil
	default:
		return nil, fmt.Errorf("memloc: %q is not a single-target location", loc)
	}
}

// withWriteLocation dispatches a write tool to exactly one store. When the
// caller does not specify, the repo's configured default applies (global unless
// .claude/engram/config.json says otherwise).
func withWriteLocation(set *memloc.Set, build storeHandler) server.ToolHandlerFunc {
	return withSingleLocation(set, build, set.DefaultWrite())
}

// routeByID dispatches a tool that addresses a row by numeric id to whichever
// store minted that id.
//
// Local stores are fenced so their ids start above store.LocalIDFence (see
// internal/store/idfence.go), which makes an id self-describing: there is no
// `location` argument to declare, nothing for the model to remember between a
// search and a follow-up, and no way for a forgotten argument to silently act on
// the wrong store's row. An id from the wrong store simply is not found.
//
// idArgs lists the arguments carrying an id, in precedence order. When several
// are present they must all resolve to the same store — relating a local memory
// to a global one is not representable, since relations live inside one database.
func routeByID(set *memloc.Set, build storeHandler, idArgs ...string) server.ToolHandlerFunc {
	d := newDispatcher(set, build)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		loc, err := locationFromIDs(req, idArgs)
		if err != nil {
			return crossStoreError(err), nil
		}
		if loc == memloc.Local && !set.HasLocalStore() {
			return locationError(set, memloc.ErrNoLocalStore), nil
		}
		h, err := d.handlerFor(loc)
		if err != nil {
			return locationError(set, err), nil
		}
		return d.run(ctx, req, h, loc)
	}
}

// locationFromIDs derives the target store from the id arguments present on a
// request. Absent ids mean global, matching pre-fork behavior.
func locationFromIDs(req mcp.CallToolRequest, idArgs []string) (memloc.Location, error) {
	resolved := memloc.Global
	seen := false
	for _, name := range idArgs {
		raw, ok := req.GetArguments()[name]
		if !ok {
			continue
		}
		num, ok := toInt64(raw)
		if !ok || num == 0 {
			continue
		}
		loc := memloc.Global
		if store.IsLocalID(num) {
			loc = memloc.Local
		}
		if seen && loc != resolved {
			return "", fmt.Errorf("id %d belongs to the %s store while another id in this call belongs to the %s store", num, loc, resolved)
		}
		resolved = loc
		seen = true
	}
	return resolved, nil
}

// toInt64 coerces a JSON-decoded argument to an integer id.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		parsed, err := n.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// crossStoreError reports an operation spanning both stores, which relations
// cannot express.
func crossStoreError(err error) *mcp.CallToolResult {
	envelope := map[string]any{
		"error_code": "cross_store_relation",
		"message":    err.Error(),
		"hint": "Memory relations live inside a single store and cannot span local and global memory. " +
			"Pick two ids from the same store, or re-save one of the memories into the other store.",
	}
	out, _ := jsonMarshal(envelope)
	result := mcp.NewToolResultText(string(out))
	result.IsError = true
	return result
}

// withSingleLocation dispatches a tool to exactly one store, applying def when
// the caller does not specify.
func withSingleLocation(set *memloc.Set, build storeHandler, def memloc.Location) server.ToolHandlerFunc {
	d := newDispatcher(set, build)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		loc, err := memloc.Normalize(requestedLocation(req), set.HasLocalStore(), def)
		if err != nil {
			return locationError(set, err), nil
		}
		// Both is not meaningful for a single-target tool; prefer local.
		if loc == memloc.Both {
			loc = memloc.Local
			if !set.HasLocalStore() {
				loc = memloc.Global
			}
		}
		h, err := d.handlerFor(loc)
		if err != nil {
			return locationError(set, err), nil
		}
		return d.run(ctx, req, h, loc)
	}
}

// mergedLocation runs a read tool against every targeted store and stitches the
// results into one response under per-store headers.
//
// With a single target the underlying result is passed through untouched apart
// from a `location` field, which keeps behavior identical to pre-fork engram in
// repositories that never opted in.
func mergedLocation(set *memloc.Set, build storeHandler) server.ToolHandlerFunc {
	d := newDispatcher(set, build)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		loc, err := memloc.Normalize(requestedLocation(req), set.HasLocalStore(), memloc.Both)
		if err != nil {
			return locationError(set, err), nil
		}
		// Merging needs two stores. With one, dispatch straight through so the
		// underlying result is returned exactly as it was pre-fork.
		if loc != memloc.Both || d.local == nil {
			if loc == memloc.Both {
				loc = memloc.Global
			}
			h, err := d.handlerFor(loc)
			if err != nil {
				return locationError(set, err), nil
			}
			return d.run(ctx, req, h, loc)
		}

		localRes, localErr := d.local(ctx, req)
		if localErr != nil {
			return nil, localErr
		}
		globalRes, globalErr := d.global(ctx, req)
		if globalErr != nil {
			return nil, globalErr
		}
		return mergeResults(localRes, globalRes), nil
	}
}

// withFallbackLocation is for get-by-id tools. An explicit location targets one
// store. Without one, local is tried first and global is used when the local
// store does not hold that id — the two databases assign ids independently, so
// a bare id is ambiguous and this ordering favors the repo's own memory.
func withFallbackLocation(set *memloc.Set, build storeHandler) server.ToolHandlerFunc {
	d := newDispatcher(set, build)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw := requestedLocation(req)
		loc, err := memloc.Normalize(raw, set.HasLocalStore(), memloc.Global)
		if err != nil {
			return locationError(set, err), nil
		}

		// Explicit single target: no fallback, so a wrong location reports
		// not-found instead of quietly returning the other store's row.
		if strings.TrimSpace(raw) != "" && loc != memloc.Both {
			h, err := d.handlerFor(loc)
			if err != nil {
				return locationError(set, err), nil
			}
			return d.run(ctx, req, h, loc)
		}

		if d.local != nil {
			res, err := d.local(ctx, req)
			if err != nil {
				return nil, err
			}
			if res != nil && !res.IsError {
				return d.annotate(res, memloc.Local)
			}
		}
		res, err := d.global(ctx, req)
		if err != nil {
			return nil, err
		}
		return d.annotate(res, memloc.Global)
	}
}

// withLocalStoreInfo adds local-memory facts to a tool's envelope. This is how
// the model discovers that a repository has local memory at all, and what
// happens to a save that does not name a location.
func withLocalStoreInfo(set *memloc.Set, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := h(ctx, req)
		if err != nil || res == nil {
			return res, err
		}
		text, ok := resultText(res)
		if !ok {
			return res, nil
		}
		var envelope map[string]any
		if jsonErr := json.Unmarshal([]byte(text), &envelope); jsonErr != nil {
			return res, nil
		}
		envelope["local_memory"] = set.HasLocalStore()
		envelope["local_path"] = set.LocalDir
		envelope["default_write"] = string(set.DefaultWrite())
		if set.Warning != "" {
			envelope["local_memory_warning"] = set.Warning
		}
		out, marshalErr := jsonMarshal(envelope)
		if marshalErr != nil {
			return res, nil
		}
		res.Content[0] = mcp.NewTextContent(string(out))
		return res, nil
	}
}

// resultText extracts the text payload of a tool result.
func resultText(res *mcp.CallToolResult) (string, bool) {
	if res == nil || len(res.Content) == 0 {
		return "", false
	}
	text, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		return "", false
	}
	return text.Text, true
}

// run executes a handler and stamps the answering store onto its envelope.
func (d *dispatcher) run(ctx context.Context, req mcp.CallToolRequest, h server.ToolHandlerFunc, loc memloc.Location) (*mcp.CallToolResult, error) {
	res, err := h(ctx, req)
	if err != nil {
		return res, err
	}
	return d.annotate(res, loc)
}

// annotate labels a result with its store, but only when there are two stores to
// tell apart. Without local memory the envelope is left byte-for-byte as
// upstream produced it.
func (d *dispatcher) annotate(res *mcp.CallToolResult, loc memloc.Location) (*mcp.CallToolResult, error) {
	if d.local == nil {
		return res, nil
	}
	return annotateLocation(res, nil, loc)
}

// annotateLocation stamps which store answered onto the result envelope, so the
// caller can see where a result came from. It takes the handler's (result, error)
// pair directly so dispatch sites stay one-liners.
func annotateLocation(res *mcp.CallToolResult, err error, loc memloc.Location) (*mcp.CallToolResult, error) {
	if err != nil || res == nil {
		return res, err
	}
	text, ok := resultText(res)
	if !ok {
		return res, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		// Not a JSON envelope (degraded plain-text path) — leave it alone.
		return res, nil
	}
	envelope[locationArg] = string(loc)
	out, err := jsonMarshal(envelope)
	if err != nil {
		return res, nil
	}
	res.Content[0] = mcp.NewTextContent(string(out))
	return res, nil
}

// mergeResults folds the local and global responses into one envelope.
//
// The global envelope supplies the project metadata (both stores resolve the
// same project from the same cwd) and the `result` field becomes the two bodies
// under headers. The merged call is only an error when BOTH stores errored.
func mergeResults(localRes, globalRes *mcp.CallToolResult) *mcp.CallToolResult {
	localBody, localEnv := splitResult(localRes)
	globalBody, globalEnv := splitResult(globalRes)

	envelope := globalEnv
	if envelope == nil {
		envelope = localEnv
	}
	if envelope == nil {
		envelope = map[string]any{}
	}

	var b strings.Builder
	b.WriteString(localHeader)
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(localBody, "\n"))
	b.WriteString("\n\n")
	b.WriteString(globalHeader)
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(globalBody, "\n"))

	envelope["result"] = b.String()
	envelope[locationArg] = string(memloc.Both)
	envelope["locations_searched"] = []string{string(memloc.Local), string(memloc.Global)}
	envelope["id_scope_note"] = "Observation ids are per-store. When following up with mem_get_observation, " +
		"mem_update, or mem_pin, pass the location of the section the id came from."

	out, err := jsonMarshal(envelope)
	if err != nil {
		return mcp.NewToolResultText(b.String())
	}
	result := mcp.NewToolResultText(string(out))
	bothFailed := localRes != nil && localRes.IsError && globalRes != nil && globalRes.IsError
	result.IsError = bothFailed
	return result
}

// splitResult returns a result's human-readable body and, when the payload is
// one of engram's JSON envelopes, the envelope itself.
func splitResult(res *mcp.CallToolResult) (string, map[string]any) {
	text, ok := resultText(res)
	if !ok {
		return "(no result)", nil
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return text, nil
	}
	body, _ := envelope["result"].(string)
	if body == "" {
		// Error envelopes carry `message` instead of `result`.
		if msg, ok := envelope["message"].(string); ok {
			return msg, nil
		}
		return text, envelope
	}
	return body, envelope
}
