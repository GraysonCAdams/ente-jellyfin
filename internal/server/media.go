package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
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
	s.serveOriginal(w, r, id)
}

// serveOriginal decrypts (cached) and serves a file's original blob with Range
// support. handleStream falls back to this when a tape has no servable HLS
// preview (Ente generates previews asynchronously, so a freshly-added tape may
// briefly report a preview that isn't yet built).
func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request, id int64) {
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
	// Previewless tapes bypass buildProgressive, so this is the only place they
	// can pick up captions: if a subtitle exists, serve a cached copy of the
	// original with the sub embedded as a soft mov_text track. -c copy avoids
	// re-decoding (also dodges tapes whose source audio ffmpeg can't decode).
	servePath := cachePath
	if subPath, ok := s.subtitleFor(id); ok {
		capPath := filepath.Join(s.cacheDir, fmt.Sprintf("cap-%d.mp4", id))
		lk := s.fileLock(id)
		lk.Lock()
		if fi, statErr := os.Stat(capPath); statErr != nil || fi.Size() == 0 {
			args := []string{"-y", "-loglevel", "error", "-i", cachePath, "-i", subPath,
				"-map", "0:v:0", "-map", "0:a:0?", "-map", "1:0",
				"-c", "copy", "-c:s", "mov_text", "-metadata:s:s:0", "language=eng",
				"-movflags", "+faststart", capPath}
			if out, e := exec.Command("ffmpeg", args...).CombinedOutput(); e != nil {
				os.Remove(capPath)
				log.Printf("caption remux %d failed, serving uncaptioned: %v: %s",
					id, e, strings.TrimSpace(string(out)))
			}
		}
		lk.Unlock()
		if fi, statErr := os.Stat(capPath); statErr == nil && fi.Size() > 0 {
			servePath = capPath
			now := time.Now()
			_ = os.Chtimes(capPath, now, now)
			s.evictCache()
		}
	}
	f, err := os.Open(servePath)
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
	// Oldest first; delete until under cap. Never evict a file modified in the
	// last 2 minutes: it may be an in-flight decrypt another request is about to
	// serve, and a single original larger than the cap must not delete itself
	// immediately after creation.
	cutoff := time.Now().Add(-2 * time.Minute)
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= s.cacheCap {
			break
		}
		if f.mod.After(cutoff) {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}
