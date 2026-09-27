package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// mediaAllow is the tool set the startup media probe asks grok to offer.
// A route whose tool is missing returns 403 and does not start grok.
var mediaAllow = []string{"image_gen", "image_edit", "reference_to_video"}

// MediaProbe is the result of asking grok which media tools it will offer.
// Offered is what the init line listed. Missing is the allowlist gap.
// Err is set when the probe itself failed; Missing is still filled when the
// init line was seen.
type MediaProbe struct {
	// Offered is the tools array from system/init. Nil if init never arrived.
	Offered []string
	// Missing is mediaAllow entries that were not offered.
	Missing []string
	// Err is the probe failure text. Empty on a clean stop-after-init.
	Err string
}

// ProbeMedia runs grok with the three media tools and kills it at init.
// ctx bounds the probe. The chat toolset probe is separate and stays empty.
// A missing tool is reported here; the process still listens.
func ProbeMedia(ctx context.Context, runner *Runner) MediaProbe {
	if ctx == nil {
		ctx = context.Background()
	}
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var raw json.RawMessage
	_, err := runner.Run(pctx, RunSpec{
		Prompt:         ProbePrompt,
		StopAfterInit:  true,
		Tools:          append([]string(nil), mediaAllow...),
		PermissionMode: "bypassPermissions",
	}, Events{OnInit: func(info Init) error {
		raw = append(json.RawMessage(nil), info.ToolsRaw...)
		return nil
	}})
	var offered []string
	_ = json.Unmarshal(raw, &offered)
	seen := map[string]bool{}
	for _, name := range offered {
		seen[name] = true
	}
	var missing []string
	for _, name := range mediaAllow {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	out := MediaProbe{Offered: offered, Missing: missing}
	if err != nil {
		out.Err = err.Error()
	}
	return out
}

// mediaBanner is the startup media line, including its trailing newline.
// An unchecked probe returns "" so older banners stay the same.
func mediaBanner(p ProbeResult) string {
	if !p.MediaChecked {
		return ""
	}
	if p.MediaOff {
		return "media     off (-media=false)\n"
	}
	keep := p.MediaKeep
	if keep == "" {
		keep = "1h"
	}
	if len(p.MediaMissing) == 0 {
		list := strings.Join(p.MediaOffered, ", ")
		if list == "" {
			list = "image_gen, image_edit, reference_to_video"
		}
		return fmt.Sprintf("media     %s (exact allowlist per run, tool calls checked); files kept %s\n", list, keep)
	}
	ok := strings.Join(p.MediaOffered, ", ")
	if ok == "" {
		ok = "none"
	}
	miss := strings.Join(p.MediaMissing, ", ")
	note := "unavailable"
	for _, name := range p.MediaMissing {
		if name == "reference_to_video" {
			note = "video unavailable (plan or features.video_gen?)"
		}
	}
	return fmt.Sprintf("media     %s ok; %s missing - %s\n", ok, miss, note)
}
