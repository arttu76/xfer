// Package urlfetch downloads a remote file over http/https so xfer can
// serve it to the old computer without writing it to the host's disk.
//
// The server performs the request from its own network, so operators who
// run xfer inside a private network should disable the feature via the
// --no-url CLI flag unless they trust the connecting client to make any
// outbound HTTP request the host itself could make (SSRF risk).
package urlfetch

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Defaults tuned for the same scale as loading a local file fully into
// memory before sending: 64 MB cap, 30 s total-request timeout.
const (
	MaxBodySize    = 64 * 1024 * 1024
	RequestTimeout = 30 * time.Second
	userAgent      = "xfer-urlfetch"
)

// Fetch downloads rawURL and returns the body plus a display filename.
// Errors are readable — they end up on the client's terminal verbatim.
func Fetch(rawURL string) ([]byte, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, "", fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", errors.New("URL must start with http:// or https://")
	}
	if u.Host == "" {
		return nil, "", errors.New("URL has no host")
	}

	client := &http.Client{Timeout: RequestTimeout}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %s", resp.Status)
	}

	// Read one byte past the cap so we can distinguish "exactly cap bytes"
	// from "cap exceeded".
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodySize+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > MaxBodySize {
		return nil, "", fmt.Errorf("file too large (> %d bytes)", MaxBodySize)
	}

	name := fileNameFromResponse(resp)
	return body, name, nil
}

// fileNameFromResponse derives a sensible display / ZFILE name. Preference
// order: Content-Disposition → last path segment of the *final* URL after
// redirects → "download.bin". Using resp.Request.URL (not the original
// input) means "https://example.org/dl/123" → 302 → "/pkg.tar.gz" picks
// up "pkg.tar.gz" rather than "123". The chosen name is stripped of path
// separators so a hostile response can't influence on-disk paths anywhere
// downstream.
func fileNameFromResponse(resp *http.Response) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if n := params["filename"]; n != "" {
				return sanitizeFilename(n)
			}
		}
	}
	var p string
	if resp.Request != nil && resp.Request.URL != nil {
		p = resp.Request.URL.Path
	}
	base := path.Base(p)
	if base == "." || base == "/" || base == "" {
		return "download.bin"
	}
	return sanitizeFilename(base)
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, `\`, "_")
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "download.bin"
	}
	return name
}
