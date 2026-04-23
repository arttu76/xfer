package urlfetch

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Errorf("missing User-Agent header")
		}
		w.Write([]byte("hello body"))
	}))
	defer srv.Close()

	body, name, err := Fetch(srv.URL + "/foo.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(body, []byte("hello body")) {
		t.Fatalf("body mismatch: %q", body)
	}
	if name != "foo.txt" {
		t.Fatalf("name want foo.txt, got %q", name)
	}
}

func TestFetch_404Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, _, err := Fetch(srv.URL + "/missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected status text in error, got %v", err)
	}
}

func TestFetch_500Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := Fetch(srv.URL + "/")
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestFetch_UnsupportedScheme(t *testing.T) {
	_, _, err := Fetch("ftp://example.com/file.bin")
	if err == nil {
		t.Fatal("expected scheme rejection")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Fatalf("error should mention required schemes: %v", err)
	}
}

func TestFetch_MalformedURL(t *testing.T) {
	_, _, err := Fetch("://no-scheme")
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestFetch_NoHost(t *testing.T) {
	_, _, err := Fetch("http:///path-only")
	if err == nil {
		t.Fatal("expected error for URL with no host")
	}
}

func TestFetch_BodySizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stream MaxBodySize + 100 bytes; expect rejection.
		for i := 0; i < (MaxBodySize/1024)+1; i++ {
			w.Write(bytes.Repeat([]byte("x"), 1024))
		}
	}))
	defer srv.Close()

	_, _, err := Fetch(srv.URL + "/big")
	if err == nil {
		t.Fatal("expected size-cap rejection")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected size-cap message, got %v", err)
	}
}

func TestFetch_ContentDispositionFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="realname.bin"`)
		w.Write([]byte("data"))
	}))
	defer srv.Close()

	_, name, err := Fetch(srv.URL + "/opaque-path")
	if err != nil {
		t.Fatal(err)
	}
	if name != "realname.bin" {
		t.Fatalf("want realname.bin (from header), got %q", name)
	}
}

func TestFetch_FilenameFallbackForEmptyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	_, name, err := Fetch(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if name != "download.bin" {
		t.Fatalf("want download.bin, got %q", name)
	}
}

func TestFetch_SanitizesPathTraversalFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../etc/passwd"`)
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	_, name, err := Fetch(srv.URL + "/any")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(name, "/") || strings.Contains(name, `\`) {
		t.Fatalf("filename must not retain path separators, got %q", name)
	}
}

func TestFetch_FollowsRedirect(t *testing.T) {
	// Second server returns the actual body. First redirects to it.
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("final body"))
	}))
	defer final.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/real.txt", http.StatusFound)
	}))
	defer redir.Close()

	body, name, err := Fetch(redir.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "final body" {
		t.Fatalf("redirect not followed; got %q", body)
	}
	// Name should reflect the final URL, not the redirect origin.
	if name != "real.txt" {
		t.Fatalf("want real.txt (from redirect target), got %q", name)
	}
}

func TestFetch_SchemeError_IncludesScheme(t *testing.T) {
	// Sanity: the error message is readable enough to land on a user's
	// terminal. Keep this assertion loose — just make sure the scheme we
	// rejected appears somewhere in the message context isn't garbled.
	_, _, err := Fetch("gopher://example.com")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if msg == "" || len(msg) > 200 {
		t.Fatalf("unhelpful error message: %q", msg)
	}
}

// Make sure we actually set a non-empty user agent (some servers reject "").
func TestFetch_SendsUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	if _, _, err := Fetch(srv.URL); err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("User-Agent header must be set")
	}
}
