package httpapi

import (
	"container/list"
	"sync"
)

// QR codes cost about a thousand redirects of CPU each, so rendered PNGs
// are cached and cache misses are rate limited per client (see handleQR).
//
// A PNG depends only on the short URL and the size, never on the link's
// target, so a cached image never goes stale: handleQR still resolves the
// code first, so deleted and expired links get 404/410, and a code that is
// deleted and re-created encodes the same short URL as before.
const (
	qrCacheEntries = 256 // at 1024 px a PNG is ~10 KB, so a few MB at most
	qrRenderBurst  = 20
	qrRenderRate   = 5 // renders per second per client, after the burst
)

type qrKey struct {
	code string
	size int
}

type pngEntry struct {
	key qrKey
	png []byte
}

// pngCache is a fixed-size LRU cache of rendered QR codes.
type pngCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // front is most recently used
	items map[qrKey]*list.Element
}

func newPNGCache(max int) *pngCache {
	return &pngCache{max: max, order: list.New(), items: make(map[qrKey]*list.Element)}
}

func (c *pngCache) get(k qrKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*pngEntry).png, true
}

func (c *pngCache) put(k qrKey, png []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		el.Value.(*pngEntry).png = png
		c.order.MoveToFront(el)
		return
	}
	c.items[k] = c.order.PushFront(&pngEntry{key: k, png: png})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*pngEntry).key)
	}
}
