package main

import (
	"bytes"
	"container/list"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
)

// lruCache is a simple thread-safe, bounded, in-memory LRU cache.
type lruCache struct {
	mu    sync.Mutex
	max   int
	items map[string]*list.Element
	order *list.List
}

type lruEntry struct {
	key   string
	value interface{}
}

func newLRUCache(max int) *lruCache {
	return &lruCache{
		max:   max,
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

func (c *lruCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry).value, true
}

func (c *lruCache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		el.Value.(*lruEntry).value = value
		c.order.MoveToFront(el)
		return
	}

	el := c.order.PushFront(&lruEntry{key: key, value: value})
	c.items[key] = el

	if c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry).key)
		}
	}
}

var (
	// imageCache holds decoded source images keyed by image URL, so
	// requesting different meme text over the same image skips the
	// download + decode.
	imageCache = newLRUCache(100)
	// memeCache holds fully rendered JPEG output keyed by URL+top+bottom,
	// so requesting the exact same meme again skips rendering entirely.
	memeCache = newLRUCache(100)

	// impactFont is the parsed Impact font, loaded once at startup so that
	// each request only needs to build a sized face from it rather than
	// re-reading and re-parsing the TTF file from disk every time.
	impactFont *truetype.Font

	// allowedImageDomains is the whitelist of hosts images may be fetched from,
	// populated from the required ALLOWED_IMAGE_DOMAINS env var (comma-separated).
	allowedImageDomains []string
)

// isAllowedImageDomain reports whether host is allowed to be fetched from,
// matching either exactly or as a subdomain of an entry in allowedImageDomains.
func isAllowedImageDomain(host string) bool {
	host = strings.ToLower(host)
	for _, domain := range allowedImageDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func loadImpactFont() (*truetype.Font, error) {
	fontBytes, err := os.ReadFile("/usr/share/fonts/truetype/msttcorefonts/impact.ttf")
	if err != nil {
		return nil, err
	}
	return truetype.Parse(fontBytes)
}

func createMeme(im image.Image, textTop string, textMiddle string, textBottom string) image.Image {
	bounds := im.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	max := math.Round(float64(width) / 10)

	dc := gg.NewContextForImage(im)
	dc.SetFontFace(truetype.NewFace(impactFont, &truetype.Options{Size: max}))

	positionX := float64(width / 2)
	positionTopY := float64(height / 6)
	positionMiddleY := float64(height / 2)
	positionBottomY := float64(5 * height / 6)

	dc.SetRGB(0, 0, 0)
	const n = 6.0    // "stroke" size
	const steps = 24 // points sampled around the stroke's circumference
	for i := 0; i < steps; i++ {
		angle := 2 * math.Pi * float64(i) / steps
		dx := n * math.Cos(angle)
		dy := n * math.Sin(angle)
		x := positionX + dx
		ytop := positionTopY + dy
		ymiddle := positionMiddleY + dy
		ybottom := positionBottomY + dy
		dc.DrawStringAnchored(strings.ToUpper(textTop), x, ytop, 0.5, 0)
		dc.DrawStringAnchored(strings.ToUpper(textMiddle), x, ymiddle, 0.5, 0.5)
		dc.DrawStringAnchored(strings.ToUpper(textBottom), x, ybottom, 0.5, 1)
	}

	dc.SetRGB(1, 1, 1)
	dc.DrawStringAnchored(strings.ToUpper(textTop), positionX, positionTopY, 0.5, 0)
	dc.DrawStringAnchored(strings.ToUpper(textMiddle), positionX, positionMiddleY, 0.5, 0.5)
	dc.DrawStringAnchored(strings.ToUpper(textBottom), positionX, positionBottomY, 0.5, 1)

	return dc.Image()
}

func handler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	log.Print("New meme ", q)

	textTop := q.Get("top")
	textMiddle := q.Get("middle")
	textBottom := q.Get("bottom")

	// Download image
	imgURL := q.Get("image")
	if imgURL == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "Generate meme by providing an image URL, top, middle, and bottom text using query parameters. See <a href=\"/?top=I'm in ur cloud&bottom=creating ur memes&image=https://upload.wikimedia.org/wikipedia/commons/f/ff/Cat_on_laptop_-_Just_Browsing.jpg\">example</a>")
		return
	}
	parsedURL, err := url.Parse(imgURL)
	if err != nil {
		http.Error(w, "Invalid image URL", http.StatusBadRequest)
		return
	}
	if !isAllowedImageDomain(parsedURL.Hostname()) {
		log.Print("Rejected image from disallowed domain: ", imgURL)
		http.Error(w, "Image domain not allowed", http.StatusForbidden)
		return
	}

	memeKey := imgURL + "\x00" + textTop + "\x00" + textMiddle + "\x00" + textBottom
	if cached, ok := memeCache.Get(memeKey); ok {
		log.Print("Meme cache hit for ", imgURL)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(cached.([]byte))
		return
	}

	var im image.Image
	if cached, ok := imageCache.Get(imgURL); ok {
		log.Print("Image cache hit for ", imgURL)
		im = cached.(image.Image)
	} else {
		req, err := http.NewRequest(http.MethodGet, imgURL, nil)
		if err != nil {
			http.Error(w, "Invalid image URL", http.StatusBadRequest)
			return
		}
		req.Header.Set("User-Agent", "memegen/1.0 (+https://github.com/steren/memegen)")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Print("Error fetching image: ", err)
			http.Error(w, "Failed to fetch image", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Image fetch returned status %d for %s", resp.StatusCode, imgURL)
			http.Error(w, "Failed to fetch image", http.StatusBadGateway)
			return
		}

		im, _, err = image.Decode(resp.Body)
		if err != nil {
			log.Print("Error decoding image: ", err)
			http.Error(w, "Failed to decode image", http.StatusBadRequest)
			return
		}

		imageCache.Set(imgURL, im)
	}

	meme := createMeme(im, textTop, textMiddle, textBottom)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, meme, nil); err != nil {
		log.Print("Error encoding meme: ", err)
		http.Error(w, "Failed to encode meme", http.StatusInternalServerError)
		return
	}

	memeCache.Set(memeKey, buf.Bytes())

	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(buf.Bytes())
}

func main() {
	var err error
	impactFont, err = loadImpactFont()
	if err != nil {
		log.Fatal("Failed to load font: ", err)
	}

	domains := os.Getenv("ALLOWED_IMAGE_DOMAINS")
	if domains == "" {
		log.Fatal("ALLOWED_IMAGE_DOMAINS must be set to a comma-separated whitelist of allowed image domains")
	}
	for _, domain := range strings.Split(domains, ",") {
		if domain = strings.ToLower(strings.TrimSpace(domain)); domain != "" {
			allowedImageDomains = append(allowedImageDomains, domain)
		}
	}
	if len(allowedImageDomains) == 0 {
		log.Fatal("ALLOWED_IMAGE_DOMAINS must contain at least one domain")
	}
	log.Print("Restricting image fetches to domains: ", allowedImageDomains)

	http.HandleFunc("/", handler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Print("Starting memegen.")
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", port), nil))
}
