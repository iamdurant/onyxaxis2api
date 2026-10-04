package onyx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client talks to the ai.onyxaxis.org web backend on behalf of one logged-in
// browser session. The only credential is the session cookie header copied
// out of a logged-in browser (obsidian_session=...); the web API has no
// bearer-token alternative for the image endpoints.
type Client struct {
	BaseURL   string
	Cookie    string
	UserAgent string
	HTTP      *http.Client
}

func NewClient(baseURL, cookie string, timeout time.Duration) *Client {
	return &Client{
		BaseURL:   baseURL,
		Cookie:    cookie,
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
		HTTP: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 0, // image generation streams nothing until done; let the client timeout rule
			},
		},
	}
}

// APIError is a structured upstream failure.
type APIError struct {
	HTTPStatus int
	Code       string
	Msg        string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("onyxaxis api: http %d, code %s: %s", e.HTTPStatus, e.Code, e.Msg)
	}
	return fmt.Sprintf("onyxaxis api: http %d: %s", e.HTTPStatus, e.Msg)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("cookie", c.Cookie)
	req.Header.Set("origin", c.BaseURL)
	req.Header.Set("referer", c.BaseURL+"/image-lab")
	req.Header.Set("user-agent", c.UserAgent)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	return req, nil
}

// do sends the request and decodes a JSON response into out. Non-2xx replies
// are surfaced as *APIError using the site's {"error":{code,message}} shape.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{HTTPStatus: resp.StatusCode, Msg: truncate(string(data), 300)}
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && (e.Error.Code != "" || e.Error.Message != "") {
			apiErr.Code = e.Error.Code
			apiErr.Msg = e.Error.Message
		}
		return apiErr
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("onyxaxis: bad response from %s: %w", path, err)
		}
	}
	return nil
}

// Model is one entry of GET /api/models.
type Model struct {
	ID               string `json:"id"`
	DisplayName      string `json:"display_name"`
	Usable           bool   `json:"usable"`
	SupportsImageGen bool   `json:"supports_image_gen"`
}

// ImageModels lists the models this session may generate images with.
func (c *Client) ImageModels(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []Model `json:"models"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/models", nil, &out); err != nil {
		return nil, err
	}
	var keep []Model
	for _, m := range out.Models {
		if m.Usable && m.SupportsImageGen && m.ID != "" {
			keep = append(keep, m)
		}
	}
	return keep, nil
}

// Image is one generated image in a /api/images/generate reply.
type Image struct {
	ID           string `json:"id"`
	AttachmentID string `json:"attachment_id"`
	URL          string `json:"url"`
	B64          string `json:"b64_json"`
}

type GenerateRequest struct {
	ModelID string   `json:"model_id"`
	Prompt  string   `json:"prompt"`
	Style   string   `json:"style"`
	Size    string   `json:"size"`
	N       int      `json:"n"`
	Images  []string `json:"images,omitempty"` // raw base64, no data: prefix
	Image   string   `json:"image,omitempty"`  // the site repeats the first reference here
}

// GenerateResponse mirrors the reply of POST /api/images/generate. b64_json
// is only present when the upstream provider returned inline data; otherwise
// the image must be fetched from the attachment URL.
type GenerateResponse struct {
	Created int64   `json:"created"`
	Images  []Image `json:"images"`
}

// Generate submits one image generation (text-to-image, or image-to-image
// when Images is non-empty). It blocks until the upstream finishes, which
// routinely takes 30-90s.
func (c *Client) Generate(ctx context.Context, in GenerateRequest) (*GenerateResponse, error) {
	if in.N < 1 {
		in.N = 1
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out GenerateResponse
	if err := c.do(ctx, http.MethodPost, "/api/images/generate", raw, &out); err != nil {
		return nil, err
	}
	if len(out.Images) == 0 {
		return nil, fmt.Errorf("onyxaxis: generate returned no images")
	}
	return &out, nil
}

// Attachment downloads a generated image by attachment id (the site serves
// them from /api/attachments/{id} behind the session cookie).
func (c *Client) Attachment(ctx context.Context, attachmentID string) ([]byte, string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/attachments/"+attachmentID, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", &APIError{HTTPStatus: resp.StatusCode, Msg: truncate(string(data), 300)}
	}
	ct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	return data, ct, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
