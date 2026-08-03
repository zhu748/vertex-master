// Package api exposes OpenAI Chat Completions/Responses, Anthropic Messages,
// and native Gemini-compatible HTTP endpoints.
package api

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"strings"
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
	version   string
	commit    string
	buildTime string
}

func NewServer(vc *vertex.VertexAIClient, keys *APIKeyManager, cfg config.ConfigProvider) *Server {
	requests := &requestMetrics{}
	h := handler{vc: vc, keys: keys, cfg: cfg, requests: requests}
	reqConv := transform.DefaultRequestConverter()
	respConv := transform.DefaultResponseConverter()
	claudePrompts := &claudePromptStore{}
	return &Server{
		chat:      &ChatHandler{handler: h, reqConv: reqConv, respConv: respConv},
		responses: &ResponsesHandler{handler: h, reqConv: reqConv, respConv: respConv},
		anthropic: &AnthropicHandler{
			handler: h, reqConv: reqConv, respConv: respConv, claudePrompts: claudePrompts,
		},
		image:     &ImageHandler{h},
		audio:     &AudioHandler{h},
		gemini:    &GeminiHandler{h},
		admin:     &AdminHandler{handler: h, claudePrompts: claudePrompts}, //nolint:exhaustruct
		mw:        &middleware{cfg: cfg, keys: keys, requests: requests},
		version:   "dev",
		commit:    "unknown",
		buildTime: "unknown",
	}
}

// SetBuildInfo 注入构建脚本通过 ldflags 写入的版本信息，供公开健康检查和
// 根路径展示，便于确认线上实例是否已经完成升级。
func (s *Server) SetBuildInfo(version, commit, buildTime string) {
	if value := strings.TrimSpace(version); value != "" {
		s.version = value
	}
	if value := strings.TrimSpace(commit); value != "" {
		s.commit = value
	}
	if value := strings.TrimSpace(buildTime); value != "" {
		s.buildTime = value
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/api/hello", s.handleAnthropicHello)
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
					s.mw.withRecover(
						s.mw.withAPIKey(
							s.mw.withConcurrencyLimit(s.mw.withBodyLimit(mux)),
						),
					),
				),
			),
		),
	)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleAnthropicHello 兼容 Claude Code 启动时对自定义 LLM gateway 的轻量探测。
// 该端点不暴露配置或密钥，仅表明 HTTP 服务可达。
func (s *Server) handleAnthropicHello(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodHead && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "Vertex AI Proxy", "version": s.version,
		"build_commit": s.commit, "build_time": s.buildTime,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "healthy",
		"timestamp": time.Now().Unix(),
		"version":   s.version,
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
		"status": "ready", "timestamp": time.Now().Unix(),
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
