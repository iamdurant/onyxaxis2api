package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"onyxaxis2api/config"
	"onyxaxis2api/onyx"
)

// AccountPool round-robins the configured session cookies.
type AccountPool struct {
	clients []*onyx.Client
	next    atomic.Uint64
}

func NewPool(cfg *config.Config) *AccountPool {
	p := &AccountPool{}
	for _, c := range cfg.Cookies {
		p.clients = append(p.clients, onyx.NewClient(cfg.BaseURL, c, cfg.UpstreamTimeout))
	}
	return p
}

func (p *AccountPool) Pick() *onyx.Client {
	return p.clients[p.next.Add(1)%uint64(len(p.clients))]
}

// modelRef is one upstream image model plus its proxy slug.
type modelRef struct {
	ID          string `json:"-"` // upstream model_id (ULID)
	Slug        string `json:"id"`
	DisplayName string `json:"display_name"`
	Created     int64  `json:"created"`
}

// ModelCache keeps the upstream image-model list fresh and resolves
// requested ids (slug or raw upstream id) against it.
type ModelCache struct {
	pool *AccountPool
	ttl  time.Duration

	mu      sync.RWMutex
	models  []modelRef
	fetched time.Time
}

func NewModelCache(pool *AccountPool, ttl time.Duration) *ModelCache {
	return &ModelCache{pool: pool, ttl: ttl}
}

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	dash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.'
		if ok {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// List returns the cached image models, refreshing them when stale.
func (c *ModelCache) List(ctx context.Context) ([]modelRef, error) {
	if models, ok := c.snapshot(); ok {
		return models, nil
	}
	if err := c.refresh(ctx); err != nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
		if len(c.models) > 0 {
			return c.models, nil // serve stale on upstream failure
		}
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.models, nil
}

func (c *ModelCache) snapshot() ([]modelRef, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.models, len(c.models) > 0 && time.Since(c.fetched) < c.ttl
}

func (c *ModelCache) refresh(ctx context.Context) error {
	models, err := c.pool.Pick().ImageModels(ctx)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	out := make([]modelRef, 0, len(models))
	for _, m := range models {
		out = append(out, modelRef{ID: m.ID, Slug: slugify(m.DisplayName), DisplayName: m.DisplayName, Created: now})
	}
	c.mu.Lock()
	c.models = out
	c.fetched = time.Now()
	c.mu.Unlock()
	return nil
}

// Resolve matches a requested model id: slug first, then the raw upstream
// id. Unknown ids error out, except image-ish names (clients hardcoding
// gpt-image-1 & co), which fall back to the default model.
func (c *ModelCache) Resolve(ctx context.Context, name, fallback string) (*modelRef, error) {
	models, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	for _, m := range models {
		if m.Slug == name || strings.EqualFold(m.ID, name) {
			return &m, nil
		}
	}
	if fallback != "" && (strings.Contains(name, "image") || strings.Contains(name, "dall-e")) {
		for _, m := range models {
			if m.Slug == fallback {
				return &m, nil
			}
		}
	}
	return nil, fmt.Errorf("unknown model %q; available: %s", name, joinSlugs(models))
}

func joinSlugs(models []modelRef) string {
	slugs := make([]string, len(models))
	for i, m := range models {
		slugs[i] = m.Slug
	}
	return strings.Join(slugs, ", ")
}

// Handler serves the OpenAI-compatible image API.
type Handler struct {
	cfg    *config.Config
	pool   *AccountPool
	models *ModelCache
}

func New(cfg *config.Config, pool *AccountPool, models *ModelCache) *Handler {
	return &Handler{cfg: cfg, pool: pool, models: models}
}

// ---- OpenAI wire types ----

type openAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	var e openAIError
	e.Error.Message = msg
	e.Error.Type = errType
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(e)
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	var ae *onyx.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.HTTPStatus == http.StatusUnauthorized || ae.HTTPStatus == http.StatusForbidden:
			writeError(w, http.StatusUnauthorized, "authentication_error",
				"upstream rejected the configured session cookie (expired or revoked); refresh accounts.txt")
		case ae.HTTPStatus == http.StatusTooManyRequests:
			writeError(w, http.StatusTooManyRequests, "rate_limit_error",
				"upstream quota exhausted; retry after the 5h window resets")
		default:
			writeError(w, http.StatusBadGateway, "api_error", "upstream error: "+ae.Msg)
		}
		return
	}
	writeError(w, http.StatusBadGateway, "api_error", "upstream error: "+err.Error())
}

// Auth guards /v1/* with the proxy API key.
func (h *Handler) Auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			key = strings.TrimPrefix(auth, "Bearer ")
		}
		if key == "" {
			key = r.Header.Get("X-Api-Key")
		}
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if key == "" || key != h.cfg.ProxyAPIKey {
			writeError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy API key")
			return
		}
		next(w, r)
	}
}

// ListModels serves GET /v1/models.
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.models.List(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	out := struct {
		Object string     `json:"object"`
		Data   []modelRef `json:"data"`
	}{Object: "list", Data: models}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// GetModel serves GET /v1/models/{id}.
func (h *Handler) GetModel(w http.ResponseWriter, r *http.Request) {
	m, err := h.models.Resolve(r.Context(), r.PathValue("id"), "")
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

// ImageFile streams a generated image attachment through the proxy session,
// so response_format=url works without handing the client our cookie.
func (h *Handler) ImageFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing attachment id")
		return
	}
	data, ct, err := h.pool.Pick().Attachment(r.Context(), id)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		writeError(w, http.StatusBadGateway, "api_error", "attachment is not an image")
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(data)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func logDone(kind, id, model string, start time.Time, extra string) {
	log.Printf("%s %s model=%s %s %s", kind, id, model, time.Since(start).Round(time.Millisecond), extra)
}
