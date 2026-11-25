package main

import (
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/image/font/gofont/goregular"

	"github.com/golang/freetype/truetype"
)

// setup installs a test font (Impact is not available outside the Docker
// image) and resets global state, restoring it when the test ends.
func setup(t *testing.T) {
	t.Helper()
	f, err := truetype.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	oldFont, oldDomains, oldDefault := impactFont, allowedImageDomains, defaultImageDomain
	oldImg, oldMeme := imageCache, memeCache
	impactFont = f
	allowedImageDomains = nil
	defaultImageDomain = ""
	imageCache, memeCache = newLRUCache(10), newLRUCache(10)
	t.Cleanup(func() {
		impactFont, allowedImageDomains, defaultImageDomain = oldFont, oldDomains, oldDefault
		imageCache, memeCache = oldImg, oldMeme
	})
}

func TestLRUCacheEviction(t *testing.T) {
	c := newLRUCache(2)
	c.Set("a", 1)
	c.Set("b", 2)
	if _, ok := c.Get("a"); !ok { // makes "b" the oldest
		t.Fatal("expected a")
	}
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Error("b should have been evicted")
	}
	if v, ok := c.Get("a"); !ok || v.(int) != 1 {
		t.Error("a should still be cached")
	}
	if v, ok := c.Get("c"); !ok || v.(int) != 3 {
		t.Error("c should be cached")
	}
}

func TestLRUCacheOverwrite(t *testing.T) {
	c := newLRUCache(2)
	c.Set("a", 1)
	c.Set("a", 2)
	if v, _ := c.Get("a"); v.(int) != 2 {
		t.Errorf("got %v, want 2", v)
	}
	if c.order.Len() != 1 {
		t.Errorf("len = %d, want 1", c.order.Len())
	}
}

func TestIsAllowedImageDomain(t *testing.T) {
	allowedImageDomains = []string{"example.com"}
	defer func() { allowedImageDomains = nil }()

	tests := map[string]bool{
		"example.com":          true,
		"EXAMPLE.com":          true,
		"img.example.com":      true,
		"notexample.com":       false,
		"example.com.evil.org": false,
		"other.org":            false,
		"":                     false,
	}
	for host, want := range tests {
		if got := isAllowedImageDomain(host); got != want {
			t.Errorf("isAllowedImageDomain(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCreateMemeKeepsBounds(t *testing.T) {
	setup(t)
	src := image.NewRGBA(image.Rect(0, 0, 200, 100))
	out := createMeme(src, "top", "middle", "bottom")
	if out.Bounds() != src.Bounds() {
		t.Errorf("bounds = %v, want %v", out.Bounds(), src.Bounds())
	}
}

func TestCreateMemeDrawsText(t *testing.T) {
	setup(t)
	src := image.NewRGBA(image.Rect(0, 0, 300, 150))
	out := createMeme(src, "hello", "", "")
	white := 0
	for y := 0; y < 150; y++ {
		for x := 0; x < 300; x++ {
			if r, g, b, _ := out.At(x, y).RGBA(); r == 0xffff && g == 0xffff && b == 0xffff {
				white++
			}
		}
	}
	if white == 0 {
		t.Error("expected white text pixels")
	}
}

func get(h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestHandlerNoImageShowsHelp(t *testing.T) {
	setup(t)
	rec := get(handler, "/")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html" {
		t.Errorf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestHandlerDisallowedDomain(t *testing.T) {
	setup(t)
	allowedImageDomains = []string{"example.com"}
	rec := get(handler, "/?image="+url.QueryEscape("http://evil.org/a.png"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
}

func imageServer(t *testing.T, status int, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		img := image.NewRGBA(image.Rect(0, 0, 100, 60))
		for i := range img.Pix {
			img.Pix[i] = 0x80
		}
		w.Header().Set("Content-Type", "image/png")
		png.Encode(w, img)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHandlerGeneratesMemeAndCaches(t *testing.T) {
	setup(t)
	var hits int
	srv := imageServer(t, http.StatusOK, &hits)
	u, _ := url.Parse(srv.URL)
	allowedImageDomains = []string{u.Hostname()}

	target := "/?top=hi&bottom=there&image=" + url.QueryEscape(srv.URL+"/a.png")
	rec := get(handler, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	im, err := jpeg.Decode(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds().Dx() != 100 || im.Bounds().Dy() != 60 {
		t.Errorf("bounds = %v", im.Bounds())
	}

	// Same meme again: served from meme cache.
	get(handler, target)
	// Different text, same image: served from image cache.
	get(handler, "/?top=other&image="+url.QueryEscape(srv.URL+"/a.png"))
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1", hits)
	}
}

func TestHandlerUpstreamError(t *testing.T) {
	setup(t)
	var hits int
	srv := imageServer(t, http.StatusNotFound, &hits)
	u, _ := url.Parse(srv.URL)
	allowedImageDomains = []string{u.Hostname()}

	rec := get(handler, "/?image="+url.QueryEscape(srv.URL+"/a.png"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", rec.Code)
	}
}

func TestHandlerDefaultImageDomain(t *testing.T) {
	setup(t)
	allowedImageDomains = []string{"example.com"}
	defaultImageDomain = "evil.org" // not allowed -> proves the prefix was applied
	rec := get(handler, "/?image=cat.png")
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403 (default domain should be applied and checked)", rec.Code)
	}
}
