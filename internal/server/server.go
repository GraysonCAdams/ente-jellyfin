// Package server exposes the decrypted Ente library over HTTP for a local
// media server. Video is served as HLS: the gateway decrypts Ente's playlist
// (inline AES key), rewrites the segment reference to a local proxy, and
// streams the encrypted output.ts bytes from Wasabi with Range support.
// Nothing is persisted; only a small presigned-URL cache is kept in memory.
package server

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GraysonCAdams/ente-jellyfin/internal/ente"
)

type Server struct {
	client    *ente.Client
	addr      string
	publicURL string // base URL embedded in playlists (reachable by Jellyfin)
	token     string // shared secret required on every request when set
	cacheDir  string
	cacheCap  int64 // max bytes of decrypted originals kept on disk

	mu        sync.RWMutex
	lib       *ente.Library
	tsURLs    map[int64]cachedURL   // presigned output.ts URL cache
	playlists map[int64]*hlsInfo    // parsed Ente playlist (key + segment ranges)
	fileLocks map[int64]*sync.Mutex // per-file decrypt locks
}

// hlsInfo is Ente's playlist parsed into what we need to serve standard HLS:
// the AES-128 key/IV and each segment's byte range + duration in output.ts.
type hlsInfo struct {
	key  []byte
	iv   []byte
	segs []hlsSeg
}

type hlsSeg struct {
	offset int64
	length int64
	dur    string
}

type cachedURL struct {
	url    string
	expiry time.Time
}

func New(client *ente.Client, addr, publicURL, cacheDir string, cacheCapBytes int64, lib *ente.Library) *Server {
	os.MkdirAll(cacheDir, 0o755)
	if publicURL == "" {
		publicURL = "http://" + addr
	}
	return &Server{
		client:    client,
		addr:      addr,
		publicURL: strings.TrimRight(publicURL, "/"),
		token:     os.Getenv("GATEWAY_TOKEN"),
		cacheDir:  cacheDir,
		cacheCap:  cacheCapBytes,
		lib:       lib,
		tsURLs:    map[int64]cachedURL{},
		playlists: map[int64]*hlsInfo{},
		fileLocks: map[int64]*sync.Mutex{},
	}
}

// SelfURL is the base the gateway is reachable at (used inside playlists).
func (s *Server) SelfURL() string { return s.publicURL }

func (s *Server) item(id int64) (*ente.MediaItem, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.lib.ByID[id]
	return it, ok
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/hls/", s.handleHLS)
	mux.HandleFunc("/stream/", s.handleStream)
	mux.HandleFunc("/media/", s.handleMedia)
	return logging(s.auth(mux))
}

// auth rejects any request lacking the shared token (once one is configured).
// /healthz stays open for container health checks.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.URL.Path != "/healthz" && r.URL.Query().Get("t") != s.token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenQuery is the "?t=..." to append to generated URLs so HLS players carry
// the token to segment/key/media requests.
func (s *Server) tokenQuery() string {
	if s.token == "" {
		return ""
	}
	return "?t=" + s.token
}

func (s *Server) ListenAndServe() error {
	log.Printf("gateway listening on %s (%d items)", s.SelfURL(), len(s.lib.ByID))
	return http.ListenAndServe(s.addr, s.Handler())
}

// handleHLS routes:
//   /hls/{id}.m3u8      -> standard HLS playlist (plaintext segments)
//   /hls/{id}/seg{n}.ts -> one decrypted MPEG-TS segment
func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/hls/")
	switch {
	case strings.HasSuffix(path, ".m3u8"):
		id, err := strconv.ParseInt(strings.TrimSuffix(path, ".m3u8"), 10, 64)
		if err != nil {
			http.Error(w, "bad file id", http.StatusBadRequest)
			return
		}
		s.servePlaylist(w, r, id)
	case strings.Contains(path, "/seg") && strings.HasSuffix(path, ".ts"):
		slash := strings.Index(path, "/seg")
		id, err1 := strconv.ParseInt(path[:slash], 10, 64)
		n, err2 := strconv.Atoi(strings.TrimSuffix(path[slash+len("/seg"):], ".ts"))
		if err1 != nil || err2 != nil {
			http.Error(w, "bad segment", http.StatusBadRequest)
			return
		}
		s.serveStdSegment(w, r, id, n)
	default:
		http.NotFound(w, r)
	}
}

var errNoPreview = fmt.Errorf("no streamable preview")

// servePlaylist emits a standard HLS playlist: one plaintext segment per entry,
// no EXT-X-KEY, no byte ranges. Every player (Infuse, Swiftfin, AVPlayer, ...)
// handles this; the gateway does the AES-128 decryption server-side.
func (s *Server) servePlaylist(w http.ResponseWriter, _ *http.Request, id int64) {
	info, err := s.hlsInfoFor(id)
	if err != nil {
		if err == errNoPreview {
			http.Error(w, "no streamable preview", http.StatusNotFound)
			return
		}
		http.Error(w, "playlist: "+err.Error(), http.StatusBadGateway)
		return
	}
	maxDur := 1
	for _, sg := range info.segs {
		if d := durCeil(sg.dur); d > maxDur {
			maxDur = d
		}
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", maxDur)
	for i, sg := range info.segs {
		fmt.Fprintf(&b, "#EXTINF:%s,\n%s/hls/%d/seg%d.ts%s\n", sg.dur, s.SelfURL(), id, i, s.tokenQuery())
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, b.String())
}

// serveStdSegment fetches segment n's encrypted byte range from output.ts,
// AES-128-CBC decrypts it, strips PKCS#7 padding, and serves plaintext MPEG-TS.
func (s *Server) serveStdSegment(w http.ResponseWriter, _ *http.Request, id int64, n int) {
	info, err := s.hlsInfoFor(id)
	if err != nil {
		http.Error(w, "segment: "+err.Error(), http.StatusBadGateway)
		return
	}
	if n < 0 || n >= len(info.segs) {
		http.Error(w, "no such segment", http.StatusNotFound)
		return
	}
	seg := info.segs[n]
	url, err := s.segmentURL(id)
	if err != nil {
		http.Error(w, "segment url: "+err.Error(), http.StatusBadGateway)
		return
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", seg.offset, seg.offset+seg.length-1))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ciphertext, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusBadGateway)
		return
	}
	plain, err := aesCBCUnpad(ciphertext, info.key, info.iv)
	if err != nil {
		http.Error(w, "decrypt: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(plain)))
	w.Write(plain)
}

// hlsInfoFor returns the parsed Ente playlist for a video, fetching + caching
// it on first use.
func (s *Server) hlsInfoFor(id int64) (*hlsInfo, error) {
	s.mu.RLock()
	info, ok := s.playlists[id]
	s.mu.RUnlock()
	if ok {
		return info, nil
	}
	it, ok := s.item(id)
	if !ok {
		return nil, fmt.Errorf("unknown file")
	}
	pj, present, err := s.client.VidPreview(it)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, errNoPreview
	}
	info, err = parseEntePlaylist(pj.Playlist)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.playlists[id] = info
	s.mu.Unlock()
	return info, nil
}

// parseEntePlaylist extracts the AES key/IV and per-segment byte ranges from
// Ente's single-file byte-range playlist.
func parseEntePlaylist(playlist string) (*hlsInfo, error) {
	info := &hlsInfo{iv: make([]byte, 16)}
	// Key from the EXT-X-KEY data: URI.
	const km = `URI="data:text/plain;base64,`
	if i := strings.Index(playlist, km); i >= 0 {
		start := i + len(km)
		if end := strings.IndexByte(playlist[start:], '"'); end >= 0 {
			if k, err := base64.StdEncoding.DecodeString(playlist[start : start+end]); err == nil {
				info.key = k
			}
		}
	}
	// IV (optional): IV=0x....
	if i := strings.Index(playlist, "IV=0x"); i >= 0 {
		hexStr := playlist[i+5:]
		if end := strings.IndexAny(hexStr, "\r\n,"); end >= 0 {
			hexStr = hexStr[:end]
		}
		if iv, err := hex.DecodeString(strings.TrimSpace(hexStr)); err == nil && len(iv) == 16 {
			info.iv = iv
		}
	}
	if len(info.key) != 16 {
		return nil, fmt.Errorf("no/invalid AES key in playlist")
	}
	// Segments: pair #EXTINF with the following #EXT-X-BYTERANGE.
	var dur string
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			dur = strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			v := strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
			parts := strings.SplitN(v, "@", 2)
			if len(parts) != 2 {
				continue
			}
			length, e1 := strconv.ParseInt(parts[0], 10, 64)
			offset, e2 := strconv.ParseInt(parts[1], 10, 64)
			if e1 == nil && e2 == nil {
				info.segs = append(info.segs, hlsSeg{offset: offset, length: length, dur: dur})
			}
		}
	}
	if len(info.segs) == 0 {
		return nil, fmt.Errorf("no segments in playlist")
	}
	return info, nil
}

// aesCBCUnpad decrypts an AES-128-CBC segment and removes PKCS#7 padding.
func aesCBCUnpad(ct, key, iv []byte) ([]byte, error) {
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not block-aligned (%d)", len(ct))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	pad := int(out[len(out)-1])
	if pad >= 1 && pad <= aes.BlockSize && pad <= len(out) {
		out = out[:len(out)-pad]
	}
	return out, nil
}

// durCeil rounds an EXTINF duration string up to the next whole second.
func durCeil(dur string) int {
	f, err := strconv.ParseFloat(dur, 64)
	if err != nil {
		return 1
	}
	if f == float64(int(f)) {
		return int(f)
	}
	return int(f) + 1
}

// segmentURL returns a cached-or-fresh presigned URL for a video's output.ts.
func (s *Server) segmentURL(id int64) (string, error) {
	s.mu.RLock()
	if c, ok := s.tsURLs[id]; ok && time.Now().Before(c.expiry) {
		s.mu.RUnlock()
		return c.url, nil
	}
	s.mu.RUnlock()

	url, ok, err := s.client.FetchVidPreviewURL(id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no preview data for file %d", id)
	}
	s.mu.Lock()
	s.tsURLs[id] = cachedURL{url: url, expiry: time.Now().Add(30 * time.Minute)}
	s.mu.Unlock()
	return url, nil
}

// handleStream serves /stream/{id}.mp4 — the decrypted 720p preview remuxed to
// MP4. MP4 is the one container every client direct-plays: AVPlayer (Swiftfin/
// tvOS), Infuse/Streamyfin (VLC engines), Tizen, and browsers.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/stream/"), ".mp4")
	id, err := strconv.ParseInt(path, 10, 64)
	if err != nil {
		http.Error(w, "bad file id", http.StatusBadRequest)
		return
	}
	cachePath := filepath.Join(s.cacheDir, fmt.Sprintf("prev-%d.mp4", id))
	lk := s.fileLock(id)
	lk.Lock()
	if fi, statErr := os.Stat(cachePath); statErr != nil || fi.Size() == 0 {
		if berr := s.buildProgressive(id, cachePath); berr != nil {
			lk.Unlock()
			http.Error(w, "stream: "+berr.Error(), http.StatusBadGateway)
			return
		}
	} else {
		now := time.Now()
		_ = os.Chtimes(cachePath, now, now)
	}
	lk.Unlock()

	f, err := os.Open(cachePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, fmt.Sprintf("%d.mp4", id), fi.ModTime(), f)
}

// buildProgressive downloads the encrypted output.ts, decrypts every segment to
// a temp MPEG-TS, then remuxes (stream copy, no re-encode) to a faststart MP4.
func (s *Server) buildProgressive(id int64, cachePath string) error {
	info, err := s.hlsInfoFor(id)
	if err != nil {
		return err
	}
	url, err := s.segmentURL(id)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	whole, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	tmpTS := cachePath + ".ts"
	out, err := os.Create(tmpTS)
	if err != nil {
		return err
	}
	for _, seg := range info.segs {
		if seg.offset+seg.length > int64(len(whole)) {
			out.Close()
			os.Remove(tmpTS)
			return fmt.Errorf("segment range beyond blob")
		}
		plain, derr := aesCBCUnpad(whole[seg.offset:seg.offset+seg.length], info.key, info.iv)
		if derr != nil {
			out.Close()
			os.Remove(tmpTS)
			return derr
		}
		if _, werr := out.Write(plain); werr != nil {
			out.Close()
			os.Remove(tmpTS)
			return werr
		}
	}
	out.Close()
	defer os.Remove(tmpTS)

	// Remux TS -> MP4 without re-encoding (fast). +faststart makes it seekable.
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-fflags", "+genpts", "-i", tmpTS,
		"-c", "copy", "-movflags", "+faststart", cachePath)
	if outErr, e := cmd.CombinedOutput(); e != nil {
		os.Remove(cachePath)
		return fmt.Errorf("remux: %v: %s", e, strings.TrimSpace(string(outErr)))
	}
	s.evictCache()
	return nil
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
