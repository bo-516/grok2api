package media

import (
	"encoding/json"
	"sync"

	"github.com/shaoboli/agent-mock/internal/grok"
)

// Guard checks each tool call against a Plan and copies accepted outputs into Store.
// sessionsRoot is <GROK_HOME>/sessions. A call that fails the path rule returns
// unsafe_grok_tool_input and drops anything already stored for this request.
type Guard struct {
	// plan is the calls and staged inputs. It is not mutated.
	plan Plan
	// store receives accepted files. Nil means a copy fails with grok_media_failed.
	store *Store
	// sessionsRoot is the directory that output paths must live under.
	sessionsRoot string

	mu        sync.Mutex
	sessionID string
	used      []bool
	pending   map[string]pend
	produced  map[int]string
	items     []Item
	prompts   []string
	rewritten bool
}

// pend is one accepted tool_use waiting for its tool_result.
type pend struct {
	// idx is the planned call index.
	idx int
	// prompt is the prompt argument the tool actually received.
	prompt string
}

// NewGuard binds a plan to a store. sessionsRoot comes from the child environment's
// GROK_HOME. A wrong root makes every file output grok_bad_output, so the caller
// must pass the same home the grok child uses.
func NewGuard(p Plan, store *Store, sessionsRoot string) *Guard {
	return &Guard{
		plan:         p,
		store:        store,
		sessionsRoot: sessionsRoot,
		used:         make([]bool, len(p.Calls)),
		pending:      map[string]pend{},
		produced:     map[int]string{},
	}
}

// SetSession records the init session id. Output paths must contain it.
// An empty id makes file outputs fail closed.
func (g *Guard) SetSession(id string) { g.mu.Lock(); g.sessionID = id; g.mu.Unlock() }

// Rewritten reports whether any tool prompt differed from the plan.
// The access log adds prompt_rewritten=1 when this is true.
func (g *Guard) Rewritten() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.rewritten }

// Outputs returns stored items and the prompt each tool received, in call order.
// The slices are copies. A failed guard returns whatever was not discarded.
func (g *Guard) Outputs() (items []Item, prompts []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Item(nil), g.items...), append([]string(nil), g.prompts...)
}

// Discard deletes every file this guard stored. It is safe after success is
// abandoned, including when the client cancels. A second call deletes nothing.
func (g *Guard) Discard() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.discardLocked()
}

// OnToolUse checks one tool call. A tool outside the allowlist is
// unsafe_grok_toolset. A path that is not staged or produced by this run is
// unsafe_grok_tool_input. Too many calls, a video call before its image exists,
// or a non-prompt argument that differs is grok_bad_output. A different prompt
// is allowed. A nil error means the call may proceed.
func (g *Guard) OnToolUse(tu grok.ToolUse) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !toolAllowed(tu.Name, g.plan.Spec.Tools) {
		g.discardLocked()
		return &grok.Error{Code: grok.CodeUnsafe, Message: "grok called " + tu.Name + ", which is outside the media allowlist. The process was killed."}
	}
	var in map[string]any
	if json.Unmarshal(tu.Input, &in) != nil {
		g.discardLocked()
		return &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool input was not a JSON object"}
	}
	idx, err := g.pick(tu.Name, in)
	if err != nil {
		g.discardLocked()
		return err
	}
	g.used[idx] = true
	prompt, _ := in["prompt"].(string)
	planned, _ := g.plan.Calls[idx].Args["prompt"].(string)
	if prompt != planned {
		g.rewritten = true
	}
	g.pending[tu.ID] = pend{idx: idx, prompt: prompt}
	return nil
}

// OnToolResult stores the file for a previously accepted call.
// An error result is classified and every file from this request is removed.
// When every planned call has a file, it returns grok.ErrStop so the runner
// kills grok and treats the run as success.
func (g *Guard) OnToolResult(tr grok.ToolResult) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.pending[tr.ToolUseID]
	if !ok {
		g.discardLocked()
		return &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result did not match a tool call"}
	}
	delete(g.pending, tr.ToolUseID)
	if tr.IsError {
		g.discardLocked()
		return grok.ClassifyMedia(tr.Text)
	}
	got, err := takeOutput(tr, g.sessionsRoot, g.sessionID)
	if err != nil {
		g.discardLocked()
		return err
	}
	var item Item
	if got.Path != "" {
		item, err = g.store.PutFile(got.Path, got.Limit)
	} else {
		item, err = g.store.PutBytes(got.Inline)
	}
	if err != nil {
		g.discardLocked()
		return &grok.Error{Code: grok.CodeMediaFailed, Message: "could not store the media file", Err: err}
	}
	g.items = append(g.items, item)
	g.prompts = append(g.prompts, p.prompt)
	if got.Path != "" {
		g.produced[p.idx+1] = got.Path
	}
	if len(g.items) == len(g.plan.Calls) {
		return grok.ErrStop
	}
	return nil
}

// discardLocked removes stored files. The caller holds g.mu.
func (g *Guard) discardLocked() {
	for _, it := range g.items {
		if g.store != nil {
			g.store.Delete(it.Name)
		}
	}
	g.items = nil
	g.prompts = nil
}

// pick finds an unused planned call whose non-prompt arguments match in.
// A hard path error is returned immediately. If every unused call of this tool
// mismatches, the error is grok_bad_output. If none remain, the call is extra.
func (g *Guard) pick(name string, in map[string]any) (int, error) {
	var soft error
	saw := false
	for i, c := range g.plan.Calls {
		if g.used[i] || c.Tool != name {
			continue
		}
		saw = true
		err := g.matchArgs(c.Args, in)
		if err == nil {
			return i, nil
		}
		if ge, ok := grok.AsError(err); ok && (ge.Code == grok.CodeUnsafeInput || ge.Code == grok.CodeUnsafe) {
			return 0, err
		}
		soft = err
	}
	if soft != nil {
		return 0, soft
	}
	if !saw {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok made more tool calls than the plan"}
	}
	return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool arguments did not match the plan"}
}

// matchArgs compares planned and actual arguments. prompt may differ.
// Path arguments must be staged inputs or files this run already produced.
// A $OUTPUT_k placeholder requires that call's output to exist already.
func (g *Guard) matchArgs(planned, actual map[string]any) error {
	for k, pv := range planned {
		if k == "prompt" {
			continue
		}
		av, ok := actual[k]
		if !ok {
			return &grok.Error{Code: grok.CodeBadOutput, Message: "grok omitted " + k}
		}
		if err := g.matchValue(k, pv, av); err != nil {
			return err
		}
	}
	for k := range actual {
		if k == "prompt" {
			continue
		}
		if _, ok := planned[k]; !ok {
			if isPathKey(k) {
				if err := g.matchValue(k, nil, actual[k]); err != nil {
					return err
				}
			}
			return &grok.Error{Code: grok.CodeBadOutput, Message: "grok sent unexpected " + k}
		}
	}
	return nil
}

// matchValue compares one argument. Path keys use the path rules. Other keys
// must be equal as JSON, with numbers compared numerically.
func (g *Guard) matchValue(key string, planned, actual any) error {
	switch key {
	case "image", "images", "first_frame", "last_frame":
		return g.matchPaths(planned, actual)
	case "keyframes":
		return g.matchFrames(planned, actual)
	default:
		if !sameJSON(planned, actual) {
			return &grok.Error{Code: grok.CodeBadOutput, Message: "grok " + key + " did not match the plan"}
		}
		return nil
	}
}
