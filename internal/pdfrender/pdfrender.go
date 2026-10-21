// Package pdfrender turns HTML into PDF through the optional PDF renderer of
// a Core deployment, the pinned Gotenberg of components.yaml, at
// ZS_PDF_RENDERER_URL. Core sends the invoice HTML Kill Bill rendered as
// one file and validates the complete bounded PDF before returning it.
// The reference stack disables JavaScript and configures Chromium URL
// filters to reject HTTP, HTTPS, WS and WSS destinations resolving to public
// or non-public IP addresses. Local file access is restricted to the current
// request's temporary directory. These controls do not establish container-wide
// egress isolation.
package pdfrender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// convertPath is Gotenberg's route that converts an HTML file with
// Chromium. The file must be named index.html.
const convertPath = "/forms/chromium/convert/html"

// MaxPDFBytes caps the PDF Core streams back.
const MaxPDFBytes = 64 << 20

// ErrUnavailable means the renderer could not be reached or failed.
var ErrUnavailable = errors.New("the PDF renderer is unavailable")

// Client calls the PDF renderer.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for the renderer at base, such as
// http://gotenberg:3000. hc defaults to a client with a 60 second timeout.
func New(base string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("pdfrender: the renderer URL must be http or https with a host")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc}, nil
}

// PDF is a rendered document. Body starts with the %PDF header. Close it.
type PDF struct {
	Body io.ReadCloser
}

// Render converts one HTML document to PDF. It reads the complete answer
// within MaxPDFBytes and checks the PDF header before it returns, so a
// caller can still answer with an error for an oversized or failed response.
func (c *Client) Render(ctx context.Context, html []byte) (PDF, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="files"; filename="index.html"`)
	h.Set("Content-Type", "text/html; charset=utf-8")
	part, err := mw.CreatePart(h)
	if err != nil {
		return PDF{}, err
	}
	if _, err := part.Write(html); err != nil {
		return PDF{}, err
	}
	if err := mw.Close(); err != nil {
		return PDF{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+convertPath, &body)
	if err != nil {
		return PDF{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return PDF{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return PDF{}, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	if resp.ContentLength > MaxPDFBytes {
		return PDF{}, fmt.Errorf("%w: the PDF is larger than %d bytes", ErrUnavailable, MaxPDFBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxPDFBytes+1))
	if err != nil {
		return PDF{}, fmt.Errorf("%w: read PDF: %v", ErrUnavailable, err)
	}
	if len(data) > MaxPDFBytes {
		return PDF{}, fmt.Errorf("%w: the PDF is larger than %d bytes", ErrUnavailable, MaxPDFBytes)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return PDF{}, fmt.Errorf("%w: the answer is not a PDF", ErrUnavailable)
	}
	return PDF{Body: io.NopCloser(bytes.NewReader(data))}, nil
}

// Version reads the renderer's version from its /version route.
func (c *Client) Version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: GET /version returned HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	return strings.TrimSpace(string(data)), nil
}
