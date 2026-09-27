package server

import (
	"net/http"

	"github.com/shaoboli/agent-mock/internal/media"
)

// mediaFile is GET /v1/media/{name}. The name is the credential: no bearer is
// required. A name that is not 32 hex plus a known extension is 404 and does
// not touch the filesystem. Range requests are answered by ServeContent.
func (s *Server) mediaFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !media.NameOK(name) {
		http.NotFound(w, r)
		return
	}
	if !s.Config.Media {
		writeAPI(w, http.StatusNotFound, "invalid_request_error", "media_disabled", "media routes are off (-media=false)")
		return
	}
	if s.Store == nil {
		http.NotFound(w, r)
		return
	}
	f, item, err := s.Store.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", item.Type)
	http.ServeContent(w, r, item.Name, item.Created, f)
}
