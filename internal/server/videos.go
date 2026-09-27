package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/media"
	"github.com/shaoboli/agent-mock/internal/xai"
)

// videoGenerate is POST /v1/videos/generations. It validates, stages inputs,
// and returns request_id before the grok run finishes. The run uses its own
// context, so the client disconnecting does not cancel it.
func (s *Server) videoGenerate(w http.ResponseWriter, r *http.Request) {
	rec := recFrom(r.Context())
	rec.media = true
	rec.kind = "video"
	rec.n = 1
	if s.mediaOff(w, rec) {
		return
	}
	body, ok := s.readMedia(w, r, rec)
	if !ok {
		return
	}
	req, err := xai.Parse(body)
	if err != nil {
		s.writeMediaParse(w, rec, err)
		return
	}
	if s.Config.LogPrompts {
		rec.prompt = req.Prompt
	}
	if !s.toolReady(w, rec, "reference_to_video") {
		return
	}
	if req.TextToVideo && !s.toolReady(w, rec, "image_gen") {
		return
	}
	if s.Jobs == nil || s.Store == nil || s.Runner == nil {
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", grok.CodeMediaFailed, "video jobs are not configured")
		return
	}
	id, err := s.Jobs.Reserve(req.Model)
	if errors.Is(err, media.ErrJobsFull) {
		rec.status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", "5")
		writeAPI(w, http.StatusTooManyRequests, "rate_limit_error", "agent_mock_busy", "agent-mock is at its video job limit. Retry after a few seconds.")
		return
	}
	if err != nil {
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", grok.CodeMediaFailed, "could not reserve a video job")
		return
	}
	paths, cleanup, err := s.stageVideo(r.Context(), req)
	if err != nil {
		s.Jobs.Drop(id)
		s.writeStage(w, rec, err)
		return
	}
	job := s.videoJob(req, paths)
	plan, err := media.PlanVideo(job)
	if err != nil {
		cleanup()
		s.Jobs.Drop(id)
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", grok.CodeMediaFailed, "could not plan the video run")
		return
	}
	rec.mediaTools = media.ToolsCSV(plan.Spec.Tools)
	rec.job = id
	setRunHeaders(w.Header(), "", req.Ignored)
	writeJSON(w, http.StatusOK, map[string]string{"request_id": id})
	go s.videoWorker(id, plan, req.Duration, cleanup)
}

// videoStatus is GET /v1/videos/{request_id}. An unknown or expired id is 404
// video_not_found. pending progress is only 0, 10, or 50.
func (s *Server) videoStatus(w http.ResponseWriter, r *http.Request) {
	rec := recFrom(r.Context())
	rec.media = true
	rec.kind = "video"
	rec.n = 1
	if s.mediaOff(w, rec) {
		return
	}
	id := r.PathValue("request_id")
	rec.job = id
	if s.Jobs == nil {
		s.videoMissing(w, rec, id)
		return
	}
	job, ok := s.Jobs.Get(id)
	if !ok {
		s.videoMissing(w, rec, id)
		return
	}
	rec.mediaTools = "reference_to_video"
	body := map[string]any{"status": job.Status, "model": job.Model}
	switch job.Status {
	case "done":
		rec.files = 1
		body["progress"] = 100
		body["video"] = map[string]any{
			"url":                requestOrigin(r) + "/v1/media/" + job.File,
			"duration":           job.Duration,
			"respect_moderation": true,
		}
	case "failed":
		body["error"] = map[string]string{"code": job.ErrCode, "message": job.ErrMsg}
	default:
		body["progress"] = job.Progress
	}
	writeJSON(w, http.StatusOK, body)
}

// videoMissing writes 404 video_not_found. The message names the id and the TTL.
func (s *Server) videoMissing(w http.ResponseWriter, rec *logRec, id string) {
	rec.status = http.StatusNotFound
	ttl := s.Config.MediaTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	writeAPI(w, http.StatusNotFound, "invalid_request_error", "video_not_found",
		fmt.Sprintf("video request %s not found or expired (-media-ttl %s)", id, ttl))
}

// stageVideo stages every image URL on the request. No images returns a no-op cleanup.
func (s *Server) stageVideo(ctx context.Context, req xai.Request) (map[string]string, func(), error) {
	noop := func() {}
	var srcs []media.Source
	add := func(u string) {
		if u != "" {
			srcs = append(srcs, media.Source{URL: u})
		}
	}
	add(req.First)
	for _, u := range req.Refs {
		add(u)
	}
	for _, f := range req.Frames {
		add(f.URL)
	}
	if len(srcs) == 0 {
		return map[string]string{}, noop, nil
	}
	if s.Stager == nil {
		return nil, noop, &media.InputError{Msg: "image staging is not configured"}
	}
	rid, err := media.NewID()
	if err != nil {
		return nil, noop, err
	}
	paths, cleanup, err := s.Stager.Stage(ctx, rid, srcs)
	if err != nil {
		return nil, noop, err
	}
	out := map[string]string{}
	i := 0
	put := func(u string) {
		if u == "" {
			return
		}
		out[u] = paths[i]
		i++
	}
	put(req.First)
	for _, u := range req.Refs {
		put(u)
	}
	for _, f := range req.Frames {
		put(f.URL)
	}
	return out, cleanup, nil
}

// videoJob maps staged paths onto a media.VideoJob. An empty aspect with a
// png or jpeg first frame becomes the closest ratio; otherwise 16:9.
func (s *Server) videoJob(req xai.Request, paths map[string]string) media.VideoJob {
	job := media.VideoJob{
		Prompt: req.Prompt, Duration: req.Duration, Aspect: req.Aspect, Resolution: req.Resolution,
		TextToVideo: req.TextToVideo, MCWD: s.mcwd(), Reasoning: s.Config.ReasoningEffort,
		First: paths[req.First],
	}
	for _, u := range req.Refs {
		job.Images = append(job.Images, paths[u])
	}
	for _, f := range req.Frames {
		job.Frames = append(job.Frames, media.Keyframe{Path: paths[f.URL], Timestamp: f.Timestamp})
	}
	job.Voices = append([]string(nil), req.Voices...)
	if job.Aspect == "" {
		job.Aspect = "16:9"
		if job.First != "" {
			if w, h, ok := media.ImageSize(job.First); ok {
				job.Aspect = media.ClosestAspect(w, h)
			}
		}
	}
	return job
}

// videoWorker runs the plan until done, failure, or -video-timeout.
// cleanup removes staged inputs when the run ends. The request context is not used.
func (s *Server) videoWorker(id string, plan media.Plan, duration int, cleanup func()) {
	start := time.Now()
	defer cleanup()
	d := s.Config.VideoTimeout
	if d <= 0 {
		d = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	fail := func(code, msg string, final grok.Final) {
		s.Jobs.Fail(id, code, msg)
		s.logJob(id, media.ToolsCSV(plan.Spec.Tools), "failed", time.Since(start), final, 0)
	}
	if err := s.lim.AcquireUntil(ctx); err != nil {
		fail(xai.ErrorCode(grok.CodeTimeout), "video run exceeded -video-timeout", grok.Final{})
		return
	}
	defer s.lim.Release()
	s.Jobs.MarkRunning(id)
	g := media.NewGuard(plan, s.Store, s.sessionsRoot())
	final, err := s.runPlan(ctx, plan, g, func(tu grok.ToolUse) error {
		if tu.Name == "image_gen" && len(plan.Calls) > 1 {
			s.Jobs.SetProgress(id, 10)
		}
		if tu.Name == "reference_to_video" {
			s.Jobs.SetProgress(id, 50)
		}
		return nil
	})
	items, _ := g.Outputs()
	if ge, ok := grok.AsError(err); ok && ge.Code == grok.CodeMaxTurns && len(items) == len(plan.Calls) {
		err = nil
	}
	if err != nil || len(items) != len(plan.Calls) {
		g.Discard()
		if err == nil {
			fail("internal_error", fmt.Sprintf("got %d of %d %s", len(items), len(plan.Calls), plan.Noun), final)
			return
		}
		code, msg := videoErr(err)
		fail(code, msg, final)
		return
	}
	file := items[len(items)-1].Name
	for _, it := range items {
		if it.Type == "video/mp4" {
			file = it.Name
		}
	}
	s.Jobs.Finish(id, file, duration)
	s.logJob(id, media.ToolsCSV(plan.Spec.Tools), "done", time.Since(start), final, 1)
}

// videoErr maps a run error onto the xAI code and message.
// A deadline is service_unavailable. grok's message is kept for the other codes.
func videoErr(err error) (string, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "service_unavailable", "video run exceeded -video-timeout"
	}
	ge, ok := grok.AsError(err)
	if !ok {
		return "internal_error", err.Error()
	}
	if ge.Code == grok.CodeTimeout {
		return "service_unavailable", "video run exceeded -video-timeout"
	}
	return xai.ErrorCode(ge.Code), ge.Message
}

// logJob writes the single JOB line for a finished video.
// The prompt is not included. dur, in, out, and files match the access-log fields.
func (s *Server) logJob(id, tools, status string, dur time.Duration, final grok.Final, files int) {
	if s.Log == nil {
		return
	}
	fmt.Fprintf(s.Log, "%s JOB  video %s tools=%s status=%s dur=%.1fs in=%d out=%d files=%d\n",
		time.Now().Format("15:04:05"), id, tools, status, dur.Seconds(), final.Usage.PromptTokens(), final.Usage.CompletionTokens(), files)
}
