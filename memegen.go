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
)

func loadImpactFont() (*truetype.Font, error) {
	fontBytes, err := os.ReadFile("/usr/share/fonts/truetype/msttcorefonts/impact.ttf")
	if err != nil {
		return nil, err
	}
	return truetype.Parse(fontBytes)
}

func createMeme(im image.Image, textTop string, textBottom string) image.Image {
	bounds := im.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	max := math.Round(float64(width) / 10)

	dc := gg.NewContextForImage(im)
	dc.SetFontFace(truetype.NewFace(impactFont, &truetype.Options{Size: max}))

	positionX := float64(width / 2)
	positionTopY := float64(height / 6)
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
		ybottom := positionBottomY + dy
		dc.DrawStringAnchored(strings.ToUpper(textTop), x, ytop, 0.5, 0)
		dc.DrawStringAnchored(strings.ToUpper(textBottom), x, ybottom, 0.5, 1)
	}

	dc.SetRGB(1, 1, 1)
	dc.DrawStringAnchored(strings.ToUpper(textTop), positionX, positionTopY, 0.5, 0)
	dc.DrawStringAnchored(strings.ToUpper(textBottom), positionX, positionBottomY, 0.5, 1)

	return dc.Image()
}

func handler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	log.Print("New meme ", q)

	textTop := q.Get("top")
	textBottom := q.Get("bottom")

	// Download image
	imgURL := q.Get("image")
	if imgURL == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "Generate meme by providing an image URL, top and bottom text using query parameters. See <a href=\"/?top=I'm in ur cloud&bottom=creating ur memes&image=https://upload.wikimedia.org/wikipedia/commons/f/ff/Cat_on_laptop_-_Just_Browsing.jpg\">example</a>")
		return
	}
	memeKey := imgURL + "\x00" + textTop + "\x00" + textBottom
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

	meme := createMeme(im, textTop, textBottom)

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

	http.HandleFunc("/", handler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Print("Starting memegen.")
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", port), nil))
}
