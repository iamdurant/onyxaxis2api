package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"onyxaxis2api/onyx"
)

const (
	maxImageBytes = 20 << 20 // per reference image
	maxRefImages  = 5        // upstream accepts at most 5 reference images
)

// imageRequest covers the OpenAI images payload plus the non-standard fields
// the proxy passes through to the site (style) and accepts for edits sent as
// JSON instead of multipart (image / images).
type imageRequest struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	N              int      `json:"n"`
	Size           string   `json:"size"`
	Style          string   `json:"style"`
	ResponseFormat string   `json:"response_format"`
	Image          string   `json:"image"`
	Images         []string `json:"images"`
}

type imageData struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type imagesResponse struct {
	Created int64       `json:"created"`
	Data    []imageData `json:"data"`
}

// ImageGenerations serves POST /v1/images/generations (text-to-image).
func (h *Handler) ImageGenerations(w http.ResponseWriter, r *http.Request) {
	var req imageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	h.generate(w, r, &req, nil)
}

// ImageEdits serves POST /v1/images/edits (image-to-image). It accepts the
// standard multipart form (image / image[] files) and, for convenience, a
// JSON body whose image(s) are data URLs, raw base64 or http(s) URLs.
func (h *Handler) ImageEdits(w http.ResponseWriter, r *http.Request) {
	req := &imageRequest{}
	var refs []string
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "bad multipart body: "+err.Error())
			return
		}
		req.Prompt = r.FormValue("prompt")
		req.Model = r.FormValue("model")
		req.Size = r.FormValue("size")
		req.Style = r.FormValue("style")
		req.ResponseFormat = r.FormValue("response_format")
		req.N = atoiDefault(r.FormValue("n"), 1)
		files := r.MultipartForm.File
		for _, field := range []string{"image", "image[]", "images"} {
			for _, fh := range files[field] {
				f, err := fh.Open()
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_request_error", "open upload: "+err.Error())
					return
				}
				data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
				f.Close()
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_request_error", "read upload: "+err.Error())
					return
				}
				if len(data) > maxImageBytes {
					writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("image %s larger than %d MB", fh.Filename, maxImageBytes>>20))
					return
				}
				refs = append(refs, base64.StdEncoding.EncodeToString(data))
			}
		}
	default:
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 48<<20)).Decode(req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "bad JSON body: "+err.Error())
			return
		}
		all := req.Images
		if req.Image != "" {
			all = append([]string{req.Image}, all...)
		}
		for i, ref := range all {
			b64, err := imageToBase64(r.Context(), ref)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("image %d: %v", i+1, err))
				return
			}
			refs = append(refs, b64)
		}
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	if len(refs) > maxRefImages {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("too many reference images: %d (max %d)", len(refs), maxRefImages))
		return
	}
	h.generate(w, r, req, refs)
}

// generate runs the upstream call and shapes the reply.
func (h *Handler) generate(w http.ResponseWriter, r *http.Request, req *imageRequest, refs []string) {
	start := time.Now()
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		modelName = h.cfg.DefaultModel
	}
	m, err := h.models.Resolve(r.Context(), modelName, h.cfg.DefaultModel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "b64_json"
	}
	if format != "b64_json" && format != "url" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("unsupported response_format %q (b64_json or url)", req.ResponseFormat))
		return
	}

	client := h.pool.Pick()
	up := onyx.GenerateRequest{
		ModelID: m.ID,
		Prompt:  req.Prompt,
		Style:   req.Style,
		Size:    req.Size,
		N:       req.N,
	}
	if len(refs) > 0 {
		up.Images = refs
		up.Image = refs[0]
	}
	// The upstream provider intermittently returns output the site refuses
	// to accept ("The provider returned N image(s) this server could not
	// accept") — the web UI hits it too and users just click again. Retry
	// those, plus plain network failures, a couple of times before giving up.
	var resp *onyx.GenerateResponse
	for attempt := 0; ; attempt++ {
		resp, err = client.Generate(r.Context(), up)
		if err == nil || attempt >= 2 || !transientGenerateError(err) || r.Context().Err() != nil {
			break
		}
		log.Printf("images: transient upstream failure (attempt %d), retrying: %v", attempt+1, err)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
		}
	}
	if err != nil {
		log.Printf("images: upstream generate failed: %v", err)
		writeUpstreamError(w, err)
		return
	}

	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := imagesResponse{Created: created, Data: make([]imageData, 0, len(resp.Images))}
	for i, img := range resp.Images {
		switch format {
		case "url":
			if img.AttachmentID == "" {
				log.Printf("images: image %d has no attachment id for url mode", i)
				writeError(w, http.StatusBadGateway, "api_error", "upstream returned no attachment reference")
				return
			}
			out.Data = append(out.Data, imageData{URL: h.fileURL(r, img.AttachmentID)})
		default:
			b64 := img.B64
			if b64 == "" {
				attID := img.AttachmentID
				if attID == "" {
					attID = strings.TrimPrefix(img.URL, "/api/attachments/")
				}
				if attID == "" {
					writeError(w, http.StatusBadGateway, "api_error", "upstream returned no image data")
					return
				}
				data, _, err := client.Attachment(r.Context(), attID)
				if err != nil {
					log.Printf("images: attachment %s fetch failed: %v", attID, err)
					writeUpstreamError(w, err)
					return
				}
				b64 = base64.StdEncoding.EncodeToString(data)
			}
			out.Data = append(out.Data, imageData{B64JSON: b64})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
	logDone("images", randHex(8), m.Slug, start, fmt.Sprintf("n=%d refs=%d fmt=%s", len(out.Data), len(refs), format))
}

// transientGenerateError reports whether an upstream failure is the known
// flaky provider behaviour worth retrying (as opposed to a real request or
// quota problem).
func transientGenerateError(err error) bool {
	ae, ok := err.(*onyx.APIError)
	if !ok {
		return true // network/timeout failures
	}
	return ae.HTTPStatus == http.StatusBadRequest && strings.Contains(ae.Msg, "could not accept")
}

// fileURL builds the proxy URL handed out for response_format=url.
func (h *Handler) fileURL(r *http.Request, attachmentID string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return fmt.Sprintf("%s://%s/v1/images/file/%s", scheme, r.Host, attachmentID)
}

// imageToBase64 materializes one reference image reference into raw base64.
// It accepts data URLs, raw base64 and http(s) URLs.
func imageToBase64(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty image reference")
	}
	if strings.HasPrefix(ref, "data:") {
		comma := strings.Index(ref, ",")
		if comma < 0 {
			return "", fmt.Errorf("malformed data url")
		}
		meta := strings.ToLower(ref[len("data:"):comma])
		payload := ref[comma+1:]
		if !strings.Contains(meta, "base64") {
			return "", fmt.Errorf("only base64 data urls are supported")
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", fmt.Errorf("bad base64 image data: %w", err)
		}
		if len(data) > maxImageBytes {
			return "", fmt.Errorf("image larger than %d MB", maxImageBytes>>20)
		}
		return base64.StdEncoding.EncodeToString(data), nil
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetch image: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("fetch image: http %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
		if err != nil {
			return "", fmt.Errorf("read image: %w", err)
		}
		if len(data) > maxImageBytes {
			return "", fmt.Errorf("image larger than %d MB", maxImageBytes>>20)
		}
		return base64.StdEncoding.EncodeToString(data), nil
	}
	// Raw base64 (with or without padding / whitespace).
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, ref)
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", fmt.Errorf("not a data url, url or base64 image")
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("image larger than %d MB", maxImageBytes>>20)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
