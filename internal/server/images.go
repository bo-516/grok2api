package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/shaoboli/agent-mock/internal/media"
	"github.com/shaoboli/agent-mock/internal/openai"
)

// imageGenerate is POST /v1/images/generations. It runs image_gen and returns
// an OpenAI ImagesResponse. n outside 1..4 never starts grok.
func (s *Server) imageGenerate(w http.ResponseWriter, r *http.Request) {
	rec := recFrom(r.Context())
	rec.media = true
	rec.kind = "image"
	rec.mediaTools = "image_gen"
	if s.mediaOff(w, rec) {
		return
	}
	body, ok := s.readMedia(w, r, rec)
	if !ok {
		return
	}
	gen, err := openai.ParseImageGen(body)
	if err != nil {
		s.writeMediaParse(w, rec, err)
		return
	}
	rec.n = gen.N
	if s.Config.LogPrompts {
		rec.prompt = gen.Prompt
	}
	if !s.toolReady(w, rec, "image_gen") {
		return
	}
	plan, err := media.PlanImage(media.ImageJob{
		Prompt: gen.Prompt, N: gen.N, Aspect: gen.Aspect, MCWD: s.mcwd(), Reasoning: s.Config.ReasoningEffort,
	})
	if err != nil {
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", "grok_media_failed", "could not plan the image run")
		return
	}
	s.runImage(w, r, rec, plan, gen.Format, gen.Ignored)
}

// imageEdit is POST /v1/images/edits. Multipart is the OpenAI shape.
// JSON is the xAI shape. Inputs are staged for the life of the request only.
func (s *Server) imageEdit(w http.ResponseWriter, r *http.Request) {
	rec := recFrom(r.Context())
	rec.media = true
	rec.kind = "image"
	rec.mediaTools = "image_edit"
	if s.mediaOff(w, rec) {
		return
	}
	body, ok := s.readMedia(w, r, rec)
	if !ok {
		return
	}
	var edit openai.ImageEdit
	var err error
	if openai.IsMultipart(r.Header.Get("Content-Type")) {
		edit, err = openai.ParseImageEditMultipart(body, r.Header.Get("Content-Type"))
	} else {
		edit, err = openai.ParseImageEditJSON(body)
	}
	if err != nil {
		s.writeMediaParse(w, rec, err)
		return
	}
	rec.n = edit.N
	if s.Config.LogPrompts {
		rec.prompt = edit.Prompt
	}
	if !s.toolReady(w, rec, "image_edit") {
		return
	}
	if s.Stager == nil {
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", "grok_media_failed", "image staging is not configured")
		return
	}
	rid, err := media.NewID()
	if err != nil {
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", "grok_media_failed", "could not stage the image")
		return
	}
	var srcs []media.Source
	for _, src := range edit.Sources {
		srcs = append(srcs, media.Source{Body: src.Body, Name: src.Name, URL: src.URL})
	}
	paths, cleanup, err := s.Stager.Stage(r.Context(), rid, srcs)
	if err != nil {
		s.writeStage(w, rec, err)
		return
	}
	defer cleanup()
	if s.Config.LogPrompts {
		rec.note = strings.Join(paths, ",")
	}
	plan, err := media.PlanImage(media.ImageJob{
		Prompt: edit.Prompt, N: edit.N, Aspect: edit.Aspect, Edit: true, Inputs: paths,
		PassAspect: edit.PassAspect, MCWD: s.mcwd(), Reasoning: s.Config.ReasoningEffort,
	})
	if err != nil {
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", "grok_media_failed", "could not plan the image edit")
		return
	}
	s.runImage(w, r, rec, plan, edit.Format, edit.Ignored)
}

// runImage acquires a chat slot and runs the plan. The client context cancels
// the grok process. Staged inputs are removed by the caller's defer.
func (s *Server) runImage(w http.ResponseWriter, r *http.Request, rec *logRec, plan media.Plan, format string, ignored []string) {
	ctx, cancel := s.mediaCtx(r)
	defer cancel()
	if err := s.lim.Acquire(ctx); err != nil {
		if errors.Is(err, errBusy) {
			rec.status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", "5")
			writeAPI(w, http.StatusTooManyRequests, "rate_limit_error", "agent_mock_busy", err.Error())
			return
		}
		return
	}
	defer s.lim.Release()
	if s.Store == nil || s.Runner == nil {
		rec.status = http.StatusBadGateway
		writeAPI(w, http.StatusBadGateway, "api_error", "grok_media_failed", "media store is not configured")
		return
	}
	g := media.NewGuard(plan, s.Store, s.sessionsRoot())
	final, err := s.runPlan(ctx, plan, g, nil)
	s.finishImage(w, r, rec, plan, g, final, err, format, ignored)
}

// writeMediaParse writes a RequestError from a media parser.
func (s *Server) writeMediaParse(w http.ResponseWriter, rec *logRec, err error) {
	var re *openai.RequestError
	if errors.As(err, &re) {
		rec.status = re.Status
		writeRequestError(w, re)
		return
	}
	rec.status = http.StatusBadRequest
	writeAPI(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
}

// writeStage writes an input-staging failure as 400 invalid_image.
func (s *Server) writeStage(w http.ResponseWriter, rec *logRec, err error) {
	var ie *media.InputError
	if errors.As(err, &ie) {
		rec.status = http.StatusBadRequest
		writeAPI(w, http.StatusBadRequest, "invalid_request_error", "invalid_image", ie.Msg)
		return
	}
	var re *openai.RequestError
	if errors.As(err, &re) {
		rec.status = re.Status
		writeRequestError(w, re)
		return
	}
	rec.status = http.StatusBadRequest
	writeAPI(w, http.StatusBadRequest, "invalid_request_error", "invalid_image", err.Error())
}
