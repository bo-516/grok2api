package media

import (
	"encoding/json"
	"fmt"

	"github.com/shaoboli/agent-mock/internal/grok"
)

// Preamble is the system prompt for every media run. It contains no caller text.
// The model is told to copy the job JSON byte for byte and to stop after the tools.
const Preamble = "You are a media job runner behind an API. The user message is a JSON job. Make exactly the tool calls listed in \"calls\", with exactly the arguments given, copying every string byte for byte. An argument \"$OUTPUT_<k>\" means the absolute path returned by call k. Put calls into one step when \"parallel\" is true. Never use any other file path, URL, tool, or argument. Do not explain or retry. After the last tool result, reply DONE."

// Preface is the first line of the prompt file. grok 1.0.41 rejects a prompt
// file whose whole body is a JSON object, so the job JSON follows this line.
const Preface = "The JSON job follows.\n"

// permBypass is the media permission mode. dontAsk cancels image_gen on grok 1.0.41.
const permBypass = "bypassPermissions"

// AnimatePrompt is the tool prompt when a video request sets an image and omits prompt.
const AnimatePrompt = "Animate this image with natural, subtle motion."

// ImageJob is one image generation or edit after the HTTP layer has mapped
// size and aspect ratio and staged the inputs. N is the number of tool calls.
type ImageJob struct {
	// Prompt is the tool prompt. Empty is rejected by the parser before planning.
	Prompt string
	// N is how many images to ask for, from 1 to 4.
	N int
	// Aspect is the image_gen aspect_ratio, already mapped. Empty means omit it
	// on an edit that must not send one.
	Aspect string
	// Edit selects image_edit. False selects image_gen.
	Edit bool
	// Inputs are absolute staged paths. Edit requires at least one.
	Inputs []string
	// PassAspect sends Aspect on an edit. Single-image edits leave this false
	// so a caller size is not forwarded.
	PassAspect bool
	// MCWD is the media cwd passed as --cwd.
	MCWD string
	// Reasoning is --reasoning-effort when the process flag is set. Empty omits it.
	Reasoning string
}

// Keyframe is one video frame pinned at Timestamp seconds.
type Keyframe struct {
	// Path is the staged image. It becomes keyframes[].image.
	Path string
	// Timestamp is timestamp_s. The parser already checked 0 < Timestamp < duration.
	Timestamp float64
}

// VideoJob is one reference_to_video plan. TextToVideo adds a leading image_gen
// whose output is first_frame via $OUTPUT_1.
type VideoJob struct {
	// Prompt is the tool prompt, already defaulted when the caller omitted it.
	Prompt string
	// Duration is 1..15. Zero is not used; the parser defaults it to 8.
	Duration int
	// Aspect is one of the seven video ratios.
	Aspect string
	// Resolution is 480p or 720p. 1080p is rewritten by the parser.
	Resolution string
	// First is the staged first_frame path. Empty for text-to-video.
	First string
	// Last is an optional staged last_frame.
	Last string
	// Images are staged reference images, at most 4.
	Images []string
	// Voices are preset voice ids, at most 3.
	Voices []string
	// Frames are staged keyframes, at most 4.
	Frames []Keyframe
	// TextToVideo is true when the caller gave no image, reference, keyframe, or voice.
	TextToVideo bool
	// MCWD is the media cwd.
	MCWD string
	// Reasoning is --reasoning-effort. Empty omits the flag.
	Reasoning string
}

// Call is one planned tool invocation. Args is the object the model must send,
// except prompt, which may differ. Index in the slice is 0-based; $OUTPUT_k is 1-based.
type Call struct {
	// Tool is image_gen, image_edit, or reference_to_video.
	Tool string
	// Args is the argument object, including $OUTPUT_k placeholders.
	Args map[string]any
}

// Plan is a grok run plus the calls the guard matches against.
type Plan struct {
	// Spec is the grok invocation. Tools is the allowlist.
	Spec grok.RunSpec
	// Calls is the ordered list. A shortfall is len(outputs) < len(Calls).
	Calls []Call
	// Inputs are staged paths the guard accepts as path arguments.
	Inputs []string
	// Kind is "image" or "video" for the access log.
	Kind string
	// Noun is "images" or "videos" in the shortfall message.
	Noun string
}

// jobFile is the prompt-file JSON. Field order is the struct order.
type jobFile struct {
	Calls    []jobCall `json:"calls"`
	Parallel bool      `json:"parallel"`
}

// jobCall is one entry in jobFile. Arguments is the raw object.
type jobCall struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// genArgs is one image_gen object. Field order is stable in the prompt file.
type genArgs struct {
	Prompt string `json:"prompt"`
	Aspect string `json:"aspect_ratio,omitempty"`
}

// editArgs is one image_edit object. Aspect is omitted for a single-image edit.
type editArgs struct {
	Prompt string   `json:"prompt"`
	Image  []string `json:"image"`
	Aspect string   `json:"aspect_ratio,omitempty"`
}

// vidArgs is one reference_to_video object. Empty slices are omitted.
type vidArgs struct {
	Prompt     string   `json:"prompt"`
	Aspect     string   `json:"aspect_ratio"`
	Duration   int      `json:"duration"`
	Resolution string   `json:"resolution_name"`
	First      string   `json:"first_frame,omitempty"`
	Last       string   `json:"last_frame,omitempty"`
	Images     []string `json:"images,omitempty"`
	Voices     []string `json:"voices,omitempty"`
	Frames     []kfArgs `json:"keyframes,omitempty"`
}

// kfArgs is one keyframe. Timestamp is timestamp_s.
type kfArgs struct {
	Image     string  `json:"image"`
	Timestamp float64 `json:"timestamp_s"`
}

// PlanImage builds a one-tool image run. job.N < 1 is treated as 1.
// Edit with PassAspect false omits aspect_ratio. The child env sets
// GROK_MAX_PARALLEL_IMAGE_GEN_CALLS to N. MaxTurns is 2.
func PlanImage(job ImageJob) (Plan, error) {
	n := job.N
	if n < 1 {
		n = 1
	}
	tool := "image_gen"
	if job.Edit {
		tool = "image_edit"
	}
	var calls []Call
	var raws []jobCall
	for i := 0; i < n; i++ {
		var v any
		if job.Edit {
			ea := editArgs{Prompt: job.Prompt, Image: append([]string(nil), job.Inputs...)}
			if job.PassAspect {
				ea.Aspect = job.Aspect
			}
			v = ea
		} else {
			v = genArgs{Prompt: job.Prompt, Aspect: job.Aspect}
		}
		c, raw, err := callFrom(tool, v)
		if err != nil {
			return Plan{}, err
		}
		calls = append(calls, c)
		raws = append(raws, raw)
	}
	spec, err := specOf([]string{tool}, 2, job.MCWD, job.Reasoning, []string{fmt.Sprintf("GROK_MAX_PARALLEL_IMAGE_GEN_CALLS=%d", n)}, raws, !job.Edit && n > 1)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Spec: spec, Calls: calls, Inputs: append([]string(nil), job.Inputs...), Kind: "image", Noun: "images"}, nil
}

// PlanVideo builds a reference_to_video run, or image_gen then reference_to_video
// when TextToVideo is set. The video env caps parallel video calls at 1.
// MaxTurns is 2, or 3 when an image must be generated first. first_frame of the
// video call is $OUTPUT_1 in the text-to-video case.
func PlanVideo(job VideoJob) (Plan, error) {
	var calls []Call
	var raws []jobCall
	tools := []string{"reference_to_video"}
	turns := 2
	env := []string{"GROK_MAX_PARALLEL_VIDEO_GEN_CALLS=1"}
	if job.TextToVideo {
		tools = []string{"image_gen", "reference_to_video"}
		turns = 3
		env = append(env, "GROK_MAX_PARALLEL_IMAGE_GEN_CALLS=1")
		c, raw, err := callFrom("image_gen", genArgs{Prompt: job.Prompt, Aspect: job.Aspect})
		if err != nil {
			return Plan{}, err
		}
		calls = append(calls, c)
		raws = append(raws, raw)
	}
	vid := vidArgs{Prompt: job.Prompt, Aspect: job.Aspect, Duration: job.Duration, Resolution: job.Resolution}
	if job.TextToVideo {
		vid.First = "$OUTPUT_1"
	} else {
		vid.First = job.First
	}
	vid.Last = job.Last
	vid.Images = append([]string(nil), job.Images...)
	vid.Voices = append([]string(nil), job.Voices...)
	for _, f := range job.Frames {
		vid.Frames = append(vid.Frames, kfArgs{Image: f.Path, Timestamp: f.Timestamp})
	}
	c, raw, err := callFrom("reference_to_video", vid)
	if err != nil {
		return Plan{}, err
	}
	calls = append(calls, c)
	raws = append(raws, raw)
	spec, err := specOf(tools, turns, job.MCWD, job.Reasoning, env, raws, false)
	if err != nil {
		return Plan{}, err
	}
	inputs := append([]string{}, job.Images...)
	if job.First != "" {
		inputs = append(inputs, job.First)
	}
	if job.Last != "" {
		inputs = append(inputs, job.Last)
	}
	for _, f := range job.Frames {
		inputs = append(inputs, f.Path)
	}
	noun := "videos"
	if job.TextToVideo {
		noun = "images"
	}
	return Plan{Spec: spec, Calls: calls, Inputs: inputs, Kind: "video", Noun: noun}, nil
}

// callFrom marshals v with struct field order, then unmarshals it into a map
// so the guard and the prompt file describe the same values. A marshal error
// is returned and the caller does not start grok.
func callFrom(tool string, v any) (Call, jobCall, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Call{}, jobCall{}, err
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return Call{}, jobCall{}, err
	}
	return Call{Tool: tool, Args: args}, jobCall{Tool: tool, Arguments: raw}, nil
}

// specOf fills a media RunSpec. tools is the allowlist and is also written into
// the job JSON. parallel is true only for n>1 image generation. A marshal error
// is returned and the caller does not start grok.
func specOf(tools []string, turns int, mcwd, reasoning string, env []string, calls []jobCall, parallel bool) (grok.RunSpec, error) {
	body, err := json.Marshal(jobFile{Calls: calls, Parallel: parallel})
	if err != nil {
		return grok.RunSpec{}, err
	}
	return grok.RunSpec{
		Prompt:          Preface + string(body) + "\n",
		SystemOverride:  Preamble,
		Tools:           tools,
		MaxTurns:        turns,
		Cwd:             mcwd,
		ExtraEnv:        env,
		PermissionMode:  permBypass,
		ReasoningEffort: reasoning,
	}, nil
}

// ToolsCSV joins the allowlist the way --tools and the access log show it.
// An empty plan returns an empty string.
func ToolsCSV(tools []string) string {
	out := ""
	for i, t := range tools {
		if i > 0 {
			out += ","
		}
		out += t
	}
	return out
}
