// Command gateway is a decrypting, streaming bridge between the official Ente
// cloud and a local media server (Jellyfin). It never stores the library: it
// reuses the `ente` CLI session, lists albums, and streams decrypted media on
// demand.
//
// Subcommands:
//
//	gateway list     Smoke test: recover the session and print the album tree.
//	gateway serve    (coming next) Run the HTTP + WebDAV gateway.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/GraysonCAdams/ente-jellyfin/internal/ente"
	"github.com/GraysonCAdams/ente-jellyfin/internal/generate"
	"github.com/GraysonCAdams/ente-jellyfin/internal/server"
	"github.com/GraysonCAdams/ente-jellyfin/internal/session"
)

func main() {
	cmd := "list"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "list":
		if err := runList(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "probe":
		if err := runProbe(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "serve":
		if err := runServe(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "strm":
		if err := runStrm(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "generate":
		if err := runGenerate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "thumbs":
		if err := runThumbs(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "flat":
		if err := runFlat(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "manifest":
		if err := runManifest(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "movies":
		if err := runMovies(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "export-secrets":
		sess, err := session.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Print(sess.ExportEnv())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want: list, probe, serve, strm, generate, thumbs)\n", cmd)
		os.Exit(2)
	}
}

func runList() error {
	sess, err := session.Load()
	if err != nil {
		return err
	}
	fmt.Printf("Signed in as %s (userID %d)\n\n", sess.Email, sess.UserID)

	client := ente.New(sess)
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}

	var totItems, totVideos, totImages int
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ALBUM\tITEMS\tVIDEOS\tPHOTOS")
	for _, a := range lib.Albums {
		var vids, imgs int
		for _, it := range a.Items {
			if it.Type == ente.Video {
				vids++
			} else {
				imgs++
			}
		}
		totItems += len(a.Items)
		totVideos += vids
		totImages += imgs
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\n", a.Name, len(a.Items), vids, imgs)
	}
	fmt.Fprintf(w, "TOTAL (%d albums)\t%d\t%d\t%d\n", len(lib.Albums), totItems, totVideos, totImages)
	w.Flush()
	return nil
}

// runProbe samples videos and reports how many have a streamable HLS preview,
// then dumps the first real playlist (.m3u8 + preview URL) for inspection.
func runProbe() error {
	sess, err := session.Load()
	if err != nil {
		return err
	}
	client := ente.New(sess)
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}

	// Collect videos in album order.
	var videos []*ente.MediaItem
	for _, a := range lib.Albums {
		for _, it := range a.Items {
			if it.Type == ente.Video {
				videos = append(videos, it)
			}
		}
	}

	const sample = 25
	n := sample
	if len(videos) < n {
		n = len(videos)
	}
	fmt.Printf("Probing %d of %d videos for HLS previews...\n\n", n, len(videos))

	var withPreview int
	var firstPlaylist *ente.PlaylistJSON
	var firstItem *ente.MediaItem
	for i := 0; i < n; i++ {
		it := videos[i]
		pj, ok, err := client.VidPreview(it)
		if err != nil {
			fmt.Printf("  [%d] %-28s ERROR: %v\n", it.ID, trunc(it.Title, 28), err)
			continue
		}
		if !ok {
			fmt.Printf("  [%d] %-28s no preview\n", it.ID, trunc(it.Title, 28))
			continue
		}
		withPreview++
		fmt.Printf("  [%d] %-28s HLS %dx%d (%d bytes)\n", it.ID, trunc(it.Title, 28), pj.Width, pj.Height, pj.Size)
		if firstPlaylist == nil {
			firstPlaylist = pj
			firstItem = it
		}
	}

	fmt.Printf("\n%d/%d sampled videos have a streamable HLS preview.\n", withPreview, n)

	if firstPlaylist != nil {
		url, ok, err := client.FetchVidPreviewURL(firstItem.ID)
		fmt.Printf("\n--- First playlist: file %d (%s) ---\n", firstItem.ID, firstItem.Title)
		fmt.Printf("type=%s  %dx%d\n", firstPlaylist.Type, firstPlaylist.Width, firstPlaylist.Height)
		if err == nil && ok {
			fmt.Printf("preview .ts URL host: %s\n", urlHost(url))
		}
		fmt.Println("---- .m3u8 ----")
		fmt.Println(firstPlaylist.Playlist)
	}
	return nil
}

func runServe() error {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8092"
	}
	sess, err := session.Load()
	if err != nil {
		return err
	}
	client := ente.New(sess)
	fmt.Printf("Signed in as %s; building library index...\n", sess.Email)
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}
	fmt.Printf("Indexed %d albums / %d items.\n", len(lib.Albums), len(lib.ByID))
	cacheDir := os.Getenv("GATEWAY_CACHE")
	if cacheDir == "" {
		cacheDir = filepath.Join(os.Getenv("HOME"), "ente-jellyfin", "cache")
	}
	var cacheCap int64 = 5 << 30 // 5 GiB of decrypted originals, LRU-evicted
	publicURL := os.Getenv("GATEWAY_PUBLIC_URL")
	srv := server.New(client, addr, publicURL, cacheDir, cacheCap, lib)
	return srv.ListenAndServe()
}

// runStrm writes Jellyfin-readable .strm stubs (one per streamable video) into
// an output dir, organized by album. Each stub contains the gateway HLS URL.
// Usage: gateway strm <outdir> [albumSubstring]
func runStrm(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway strm <outdir> [albumSubstring]")
	}
	outDir := args[0]
	var filter string
	if len(args) > 1 {
		filter = strings.ToLower(args[1])
	}
	base := os.Getenv("GATEWAY_URL")
	if base == "" {
		base = "http://127.0.0.1:8092"
	}
	tokenQ := ""
	if t := os.Getenv("GATEWAY_TOKEN"); t != "" {
		tokenQ = "?t=" + t
	}

	sess, err := session.Load()
	if err != nil {
		return err
	}
	client := ente.New(sess)
	fmt.Println("Building library index...")
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	var hlsCount, fallbackCount int
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, album := range lib.Albums {
		if filter != "" && !strings.Contains(strings.ToLower(album.Name), filter) {
			continue
		}
		albumDir := filepath.Join(outDir, sanitize(album.Name))
		if err := os.MkdirAll(albumDir, 0o755); err != nil {
			return err
		}
		for _, it := range album.Items {
			if it.Type != ente.Video {
				continue
			}
			item := it
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				// Prefer the HLS preview; fall back to the original blob endpoint
				// so every clip is playable, preview or not.
				_, hasPreview, _ := client.FetchVidPreviewPlaylist(item.ID)
				name := strings.TrimSuffix(item.Title, filepath.Ext(item.Title))
				stub := filepath.Join(albumDir, sanitize(name)+".strm")
				var url string
				mu.Lock()
				if hasPreview {
					url = fmt.Sprintf("%s/stream/%d.mp4%s", base, item.ID, tokenQ)
					hlsCount++
				} else {
					ext := filepath.Ext(item.Title)
					if ext == "" {
						ext = ".mp4"
					}
					url = fmt.Sprintf("%s/media/%d%s%s", base, item.ID, ext, tokenQ)
					fallbackCount++
				}
				mu.Unlock()
				_ = os.WriteFile(stub, []byte(url+"\n"), 0o644)
			}()
		}
	}
	wg.Wait()

	fmt.Printf("Wrote %d .strm stubs (%d HLS, %d original-fallback) to %s\n", hlsCount+fallbackCount, hlsCount, fallbackCount, outDir)
	return nil
}

// runGenerate transcodes videos that lack an HLS preview and (only with
// --upload) adds the preview to your Ente account. Default is a dry run.
// Usage: gateway generate [--upload] [--limit N] [--album substr]
func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	upload := fs.Bool("upload", false, "actually upload previews to Ente (default: dry run, no writes)")
	limit := fs.Int("limit", 2, "max videos to process this run")
	album := fs.String("album", "", "only videos whose album name contains this substring")
	hw := fs.Bool("hw", false, "use Apple Silicon hardware H.264 encoder (faster)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sess, err := session.Load()
	if err != nil {
		return err
	}
	client := ente.New(sess)
	fmt.Println("Building library index...")
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}

	if *upload {
		fmt.Println("\n*** UPLOAD MODE: previews WILL be added to your Ente account (originals untouched) ***")
	} else {
		fmt.Println("\n=== DRY RUN: transcoding only, nothing will be uploaded ===")
	}

	processed := 0
	for _, a := range lib.Albums {
		if *album != "" && !strings.Contains(strings.ToLower(a.Name), strings.ToLower(*album)) {
			continue
		}
		for _, it := range a.Items {
			if processed >= *limit {
				fmt.Printf("\nReached limit of %d. Re-run with a higher --limit to continue.\n", *limit)
				return nil
			}
			if it.Type != ente.Video {
				continue
			}
			// Skip videos that already have a preview.
			if _, has, err := client.FetchVidPreviewPlaylist(it.ID); err == nil && has {
				continue
			}
			processed++
			fmt.Printf("\n[%d] %s / %s ...\n", it.ID, a.Name, it.Title)
			res, gerr := generate.Generate(client, it, generate.Options{Upload: *upload, HWAccel: *hw})
			if gerr != nil {
				fmt.Printf("      FAILED: %v\n", gerr)
				continue
			}
			status := "transcoded (dry run)"
			if res.Uploaded {
				status = "UPLOADED preview"
			}
			fmt.Printf("      %s: %dx%d, %.1f MB segments\n", status, res.Width, res.Height, float64(res.SegmentBytes)/(1<<20))
		}
	}
	fmt.Printf("\nDone. Processed %d video(s).\n", processed)
	return nil
}

// runThumbs writes each clip's decrypted Ente thumbnail as a sibling poster
// image (<basename>-poster.jpg) next to its .strm, so Jellyfin adopts it on
// scan without any frame extraction. Usage: gateway thumbs <libdir> [album]
func runThumbs(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway thumbs <libdir> [albumSubstring]")
	}
	outDir := args[0]
	var filter string
	if len(args) > 1 {
		filter = strings.ToLower(args[1])
	}

	sess, err := session.Load()
	if err != nil {
		return err
	}
	client := ente.New(sess)
	fmt.Println("Building library index...")
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}

	var written, failed int
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, album := range lib.Albums {
		if filter != "" && !strings.Contains(strings.ToLower(album.Name), filter) {
			continue
		}
		albumDir := filepath.Join(outDir, sanitize(album.Name))
		if err := os.MkdirAll(albumDir, 0o755); err != nil {
			return err
		}
		for _, it := range album.Items {
			if it.Type != ente.Video {
				continue
			}
			item := it
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				jpg, err := client.ThumbnailJPEG(item)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failed++
					return
				}
				name := strings.TrimSuffix(item.Title, filepath.Ext(item.Title))
				poster := filepath.Join(albumDir, sanitize(name)+"-poster.jpg")
				if os.WriteFile(poster, jpg, 0o644) == nil {
					written++
				} else {
					failed++
				}
			}()
		}
	}
	wg.Wait()
	fmt.Printf("Wrote %d poster images (%d failed) under %s\n", written, failed, outDir)
	return nil
}

// runFlat builds a single flat, chronological library: one .strm per clip named
// by capture date (so it sorts as a timeline), a poster, and an .nfo carrying
// the real date + album-as-genre. Usage: gateway flat <outdir>
func runFlat(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway flat <outdir>")
	}
	outDir := args[0]
	base := os.Getenv("GATEWAY_URL")
	if base == "" {
		base = "http://127.0.0.1:8092"
	}
	tokenQ := ""
	if t := os.Getenv("GATEWAY_TOKEN"); t != "" {
		tokenQ = "?t=" + t
	}

	sess, err := session.Load()
	if err != nil {
		return err
	}
	client := ente.New(sess)
	fmt.Println("Building library index...")
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	var written int
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, album := range lib.Albums {
		for _, it := range album.Items {
			if it.Type != ente.Video {
				continue
			}
			item, albumName := it, album.Name
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				// Date-prefixed base name -> chronological even by filename sort.
				datekey := "00000000-000000"
				if !item.Created.IsZero() {
					datekey = item.Created.Format("20060102-150405")
				}
				stem := fmt.Sprintf("%s_%d", datekey, item.ID)
				title := strings.TrimSuffix(item.Title, filepath.Ext(item.Title))

				// URL: HLS if a preview exists, else original fallback.
				_, hasPreview, _ := client.FetchVidPreviewPlaylist(item.ID)
				var url string
				if hasPreview {
					url = fmt.Sprintf("%s/stream/%d.mp4%s", base, item.ID, tokenQ)
				} else {
					ext := filepath.Ext(item.Title)
					if ext == "" {
						ext = ".mp4"
					}
					url = fmt.Sprintf("%s/media/%d%s%s", base, item.ID, ext, tokenQ)
				}
				_ = os.WriteFile(filepath.Join(outDir, stem+".strm"), []byte(url+"\n"), 0o644)

				// Poster.
				if jpg, err := client.ThumbnailJPEG(item); err == nil {
					_ = os.WriteFile(filepath.Join(outDir, stem+"-poster.jpg"), jpg, 0o644)
				}

				// NFO: real date + album as genre/tag for filtering.
				nfo := buildNFO(title, item.Created, albumName)
				_ = os.WriteFile(filepath.Join(outDir, stem+".nfo"), []byte(nfo), 0o644)

				mu.Lock()
				written++
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	fmt.Printf("Wrote %d clips (flat, date-sorted) to %s\n", written, outDir)
	return nil
}

// runManifest prints a JSON array of every clip's metadata (id, date, album,
// location) so an external updater can push it into Jellyfin via the API.
func runManifest() error {
	sess, err := session.Load()
	if err != nil {
		return err
	}
	client := ente.New(sess)
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}
	type entry struct {
		ID    int64   `json:"id"`
		Title string  `json:"title"`
		Album string  `json:"album"`
		Date  string  `json:"date,omitempty"` // ISO8601, empty if unknown
		Year  int     `json:"year,omitempty"`
		Lat   float64 `json:"lat,omitempty"`
		Long  float64 `json:"long,omitempty"`
	}
	var out []entry
	for _, a := range lib.Albums {
		for _, it := range a.Items {
			if it.Type != ente.Video {
				continue
			}
			e := entry{ID: it.ID, Title: strings.TrimSuffix(it.Title, filepath.Ext(it.Title)), Album: a.Name, Lat: it.Lat, Long: it.Long}
			if !it.Created.IsZero() {
				e.Date = it.Created.Format("2006-01-02T15:04:05.000Z")
				e.Year = it.Created.Year()
			}
			out = append(out, e)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

func buildNFO(title string, created time.Time, album string) string {
	premiered, year := "", ""
	if !created.IsZero() {
		premiered = created.Format("2006-01-02")
		year = created.Format("2006")
	}
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "&", "&amp;")
		s = strings.ReplaceAll(s, "<", "&lt;")
		return strings.ReplaceAll(s, ">", "&gt;")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<movie>
  <title>%s</title>
  <sorttitle>%s</sorttitle>
  <premiered>%s</premiered>
  <year>%s</year>
  <genre>%s</genre>
  <tag>%s</tag>
</movie>
`, esc(title), premiered, premiered, year, esc(album), esc(album))
}

func sanitize(s string) string {
	repl := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "\x00", "")
	s = repl.Replace(strings.TrimSpace(s))
	if s == "" {
		return "untitled"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func urlHost(u string) string {
	if i := indexOf(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
