package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GraysonCAdams/ente-jellyfin/internal/crypto"
	"github.com/GraysonCAdams/ente-jellyfin/internal/encoding"
	"github.com/GraysonCAdams/ente-jellyfin/internal/ente"
)

// handleMedia streams a decrypted original with full Range/seek support. This
// is the fallback for videos that have no HLS preview (and works for any clip)
// — the same "download + decrypt + play" the Ente website does, but the
// decrypted copy lives only in a small capped cache, evicted over time.
//
//	/media/{id}.{ext}
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/media/")
	if i := strings.IndexByte(path, '.'); i >= 0 {
		path = path[:i]
	}
	id, err := strconv.ParseInt(path, 10, 64)
	if err != nil {
		http.Error(w, "bad file id", http.StatusBadRequest)
		return
	}
	it, ok := s.item(id)
	if !ok {
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}
	cachePath, err := s.ensureDecrypted(it)
	if err != nil {
		http.Error(w, "decrypt: "+err.Error(), http.StatusBadGateway)
		return
	}
	f, err := os.Open(cachePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// ServeContent handles Range requests, so the player can seek freely.
	http.ServeContent(w, r, it.Title, fi.ModTime(), f)
}

func (s *Server) fileLock(id int64) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lk, ok := s.fileLocks[id]
	if !ok {
		lk = &sync.Mutex{}
		s.fileLocks[id] = lk
	}
	return lk
}

// ensureDecrypted returns a path to the fully-decrypted original, downloading
// and decrypting it into the cache on first access.
func (s *Server) ensureDecrypted(it *ente.MediaItem) (string, error) {
	cachePath := filepath.Join(s.cacheDir, strconv.FormatInt(it.ID, 10))

	lk := s.fileLock(it.ID)
	lk.Lock()
	defer lk.Unlock()

	if fi, err := os.Stat(cachePath); err == nil && fi.Size() > 0 {
		now := time.Now()
		_ = os.Chtimes(cachePath, now, now) // mark recently used for LRU
		return cachePath, nil
	}

	encPath := cachePath + ".enc"
	if err := s.client.DownloadToFile(it.ID, encPath); err != nil {
		return "", err
	}
	defer os.Remove(encPath)

	if err := crypto.DecryptFile(encPath, cachePath, it.FileKey, encoding.DecodeBase64(it.FileNonce)); err != nil {
		os.Remove(cachePath)
		return "", err
	}
	s.evictCache()
	return cachePath, nil
}

// evictCache deletes the least-recently-used decrypted originals when the cache
// exceeds its size cap, so the library is never fully stored.
func (s *Server) evictCache() {
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return
	}
	type ent struct {
		path string
		size int64
		mod  time.Time
	}
	var files []ent
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".enc") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, ent{filepath.Join(s.cacheDir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= s.cacheCap {
		return
	}
	// Oldest first; delete until under cap.
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= s.cacheCap {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}
