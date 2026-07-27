// Package api exposes OpenAI Chat Completions/Responses, Anthropic Messages,
// and native Gemini-compatible HTTP endpoints.
package api

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/db"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type Server struct {
	chat      *ChatHandler
	responses *ResponsesHandler
	anthropic *AnthropicHandler
	image     *ImageHandler
	audio     *AudioHandler
	gemini    *GeminiHandler
	admin     *AdminHandler
	mw        *middleware
}

func NewServer(vc *vertex.VertexAIClient, keys *APIKeyManager, cfg config.ConfigProvider) *Server {
	h := handler{vc: vc, keys: keys, cfg: cfg}
	reqConv := transform.DefaultRequestConverter()
	respConv := transform.DefaultResponseConverter()
	return &Server{
		chat:      &ChatHandler{handler: h, reqConv: reqConv, respConv: respConv},
		responses: &ResponsesHandler{handler: h, reqConv: reqConv, respConv: respConv},
		anthropic: &AnthropicHandler{handler: h, reqConv: reqConv, respConv: respConv},
		image:     &ImageHandler{h},
		audio:     &AudioHandler{h},
		gemini:    &GeminiHandler{h},
		admin:     &AdminHandler{h},
		mw:        &middleware{cfg: cfg, keys: keys},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/v1/models", s.handleModelsOAI)
	mux.HandleFunc("/v1beta/models", s.handleModelsGemini)
	mux.HandleFunc("/v1/chat/completions", s.chat.handleChatCompletions)
	mux.HandleFunc("/v1/responses", s.responses.handleResponses)
	mux.HandleFunc("/v1/messages", s.anthropic.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.anthropic.handleCountTokens)
	mux.HandleFunc("/v1/images/generations", s.image.handleImageGenerations)
	mux.HandleFunc("/v1/images/edits", s.image.handleImageEdits)
	mux.HandleFunc("/v1/images/variations", s.image.handleImageVariations)
	mux.HandleFunc("/v1/audio/speech", s.audio.handleAudioSpeech)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/admin", s.admin.handleAdminPage)
	mux.HandleFunc("/admin/", s.admin.handleAdminPage)
	mux.HandleFunc("/api/admin/", s.admin.handleAdminAPI)
	mux.HandleFunc("/assets/", s.handleAssets)
	mux.HandleFunc("/v1beta/models/", s.gemini.handleModelsSubtree)
	mux.HandleFunc("/v1/models/", s.gemini.handleModelsSubtree)

	if s.mw.cfg.DebugPprof() {
		mux.HandleFunc("/debug/pprof/", pprofIndex)
		mux.HandleFunc("/debug/pprof/cmdline", pprintCmdline)
		mux.HandleFunc("/debug/pprof/profile", pprofProfile)
		mux.HandleFunc("/debug/pprof/symbol", pprofSymbol)
		mux.HandleFunc("/debug/pprof/trace", pprofTrace)
		mux.HandleFunc("/debug/pprof/goroutine", pprofGoroutine)
		mux.HandleFunc("/debug/pprof/heap", pprofHeap)
		mux.HandleFunc("/debug/pprof/threadcreate", pprofThreadcreate)
		mux.HandleFunc("/debug/pprof/block", pprofBlock)
		mux.HandleFunc("/debug/pprof/mutex", pprofMutex)
	}

	return s.mw.withRecover(
		s.mw.withSecurityHeaders(
			s.mw.withCORS(
				s.mw.withMetrics(
					s.mw.withAPIKey(
						s.mw.withConcurrencyLimit(s.mw.withBodyLimit(mux)),
					),
				),
			),
		),
	)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	assetsDir := filepath.Join(filepath.Dir(s.mw.cfg.ConfigDir()), "assets")
	fs := http.StripPrefix("/assets/", http.FileServer(http.Dir(assetsDir)))
	fs.ServeHTTP(w, r)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Vertex AI Proxy", "version": "2.0-go"})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "healthy",
		"timestamp":       time.Now().Unix(),
		"api_keys_loaded": s.mw.keys.Count(),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if s.mw.keys == nil || s.mw.keys.Count() == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready", "reason": "no_api_keys", "timestamp": time.Now().Unix(),
		})
		return
	}
	database := db.CurrentDB()
	if database == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready", "reason": "database_unavailable", "timestamp": time.Now().Unix(),
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready", "reason": "database_unavailable", "timestamp": time.Now().Unix(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready", "timestamp": time.Now().Unix(), "api_keys_loaded": s.mw.keys.Count(),
	})
}

func (s *Server) handleModelsOAI(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := s.mw.cfg.ModelsWithFakeVariants()
	log.Printf("[Server] [Models] 请求 OAI 模型列表，返回 %d 个模型", len(models))
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m, "object": "model", "created": now, "owned_by": "google", "permission": []any{},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleModelsGemini(w http.ResponseWriter, r *http.Request) {
	models := s.mw.cfg.ModelsWithFakeVariants()
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, geminiModelInfo(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": data})
}
