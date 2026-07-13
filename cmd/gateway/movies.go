package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GraysonCAdams/ente-jellyfin/internal/ente"
	"github.com/GraysonCAdams/ente-jellyfin/internal/session"
)

// namedEvents maps an Ente full-tape name to a human label (no trailing year).
// Date-range tapes are auto-formatted instead (see formatDateRange); only
// genuinely-named events need an entry here. New named tapes: add one line.
var namedEvents = map[string]string{
	"1985BrawleyCircle":       "Brawley Circle",
	"1986CatsMercedes":        "Cats & the Mercedes",
	"1986Emily1stBaseball":    "Emily's First Baseball",
	"1986dogs":                "The Dogs",
	"1986jul04":               "Fourth of July",
	"1986aug":                 "August",
	"1986aug31":               "August 31",
	"1987Baseball_Snow_Pokey": "Baseball, Snow & Pokey",
	"1987GrandmaHouseIntro":   "Grandma's House",
	"1987spring":              "Spring",
	"1987april":               "April",
	"1989Christmas":           "Christmas",
	"1991apr21":               "April 21",
	"1991xmas":                "Christmas",
	"1999MayGrayOy":           "Gray, Oy!",
	"1999mayGrayOy_b":         "Gray, Oy! (Part 2)",
	"2007-Paris":              "Paris",
}

var months = map[string]string{
	"jan": "Jan", "feb": "Feb", "mar": "Mar", "apr": "Apr", "may": "May",
	"jun": "Jun", "june": "Jun", "ju": "Jun", "jul": "Jul", "july": "Jul", "aug": "Aug",
	"sep": "Sep", "sept": "Sep", "oct": "Oct", "nov": "Nov", "dec": "Dec",
}

var rangeRe = regexp.MustCompile(`^(\d{4})([a-z]+?)(\d*)-(\d{4})?([a-z]+?)(\d*)$`)

// formatDateRange turns "2003jun04-2005jun04" into "Jun 2003 – Jun 2005" and
// "1989jun-1989aug" into "June–August 1989". Returns "" if it doesn't parse.
func formatDateRange(name string) string {
	m := rangeRe.FindStringSubmatch(strings.ToLower(name))
	if m == nil {
		return ""
	}
	y1, mo1, d1, y2s, mo2, d2 := m[1], m[2], m[3], m[4], m[5], m[6]
	M1, ok1 := months[mo1]
	M2, ok2 := months[mo2]
	if !ok1 || !ok2 {
		return ""
	}
	y2 := y2s
	if y2 == "" {
		y2 = y1
	}
	dd := func(s string) string {
		if s == "" {
			return ""
		}
		n, _ := strconv.Atoi(s)
		return " " + strconv.Itoa(n)
	}
	if y1 == y2 {
		// same year: "Jun 4 – Aug 26, 2002"  (or "June–August 1989" if no days)
		if d1 == "" && d2 == "" {
			return fmt.Sprintf("%s–%s %s", monthLong(mo1), monthLong(mo2), y1)
		}
		return fmt.Sprintf("%s%s – %s%s, %s", M1, dd(d1), M2, dd(d2), y1)
	}
	// spans years: "Jun 2003 – Jun 2005"
	return fmt.Sprintf("%s %s – %s %s", M1, y1, M2, y2)
}

func monthLong(mo string) string {
	long := map[string]string{"jan": "January", "feb": "February", "mar": "March",
		"apr": "April", "may": "May", "jun": "June", "june": "June", "jul": "July",
		"july": "July", "aug": "August", "sep": "September", "sept": "September",
		"oct": "October", "nov": "November", "dec": "December"}
	if v, ok := long[mo]; ok {
		return v
	}
	return mo
}

// friendlyLabel returns the display title (no year) for a full-tape name.
func friendlyLabel(name string) string {
	if v, ok := namedEvents[name]; ok {
		return v
	}
	if f := formatDateRange(name); f != "" {
		return f
	}
	return name
}

var clipSuffix = regexp.MustCompile(`_\d+$`)

// runMovies builds a Jellyfin Movies library from the FULL tapes only (clips
// skipped): one folder per movie, "<Label> (<Year>)/", with .strm + poster +
// .nfo. Usage: gateway movies <outdir> [--dry]
func runMovies(args []string) error {
	dry := false
	var outDir string
	for _, a := range args {
		if a == "--dry" {
			dry = true
		} else if outDir == "" {
			outDir = a
		}
	}
	if outDir == "" && !dry {
		return fmt.Errorf("usage: gateway movies <outdir> [--dry]")
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
	fmt.Fprintln(os.Stderr, "Building library index...")
	lib, err := client.BuildLibrary()
	if err != nil {
		return err
	}

	// Collect the full tapes (skip clips), de-duped by name.
	type tape struct {
		item  *ente.MediaItem
		label string
		year  int
	}
	var tapes []tape
	seen := map[string]bool{}
	for _, a := range lib.Albums {
		for _, it := range a.Items {
			if it.Type != ente.Video {
				continue
			}
			title := strings.TrimSuffix(it.Title, filepath.Ext(it.Title))
			if clipSuffix.MatchString(title) || seen[title] {
				continue
			}
			seen[title] = true
			yr := 0
			if !it.Created.IsZero() {
				yr = it.Created.Year()
			}
			tapes = append(tapes, tape{it, friendlyLabel(title), yr})
		}
	}

	if dry {
		fmt.Printf("%d full tapes:\n", len(tapes))
		for _, t := range tapes {
			fmt.Printf("  %-34s -> %s (%d)\n", t.item.Title, t.label, t.year)
		}
		return nil
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	var written int
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, tp := range tapes {
		t := tp
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			folder := sanitize(fmt.Sprintf("%s (%d)", t.label, t.year))
			dir := filepath.Join(outDir, folder)
			os.MkdirAll(dir, 0o755)

			// stream URL: HLS-preview -> /stream/.mp4, else original -> /media
			_, hasPreview, _ := client.FetchVidPreviewPlaylist(t.item.ID)
			var url string
			if hasPreview {
				url = fmt.Sprintf("%s/stream/%d.mp4%s", base, t.item.ID, tokenQ)
			} else {
				ext := filepath.Ext(t.item.Title)
				if ext == "" {
					ext = ".mp4"
				}
				url = fmt.Sprintf("%s/media/%d%s%s", base, t.item.ID, ext, tokenQ)
			}
			os.WriteFile(filepath.Join(dir, folder+".strm"), []byte(url+"\n"), 0o644)
			if jpg, err := client.ThumbnailJPEG(t.item); err == nil {
				os.WriteFile(filepath.Join(dir, "poster.jpg"), jpg, 0o644)
			}
			nfo := buildMovieNFO(t.label, t.item.Created, t.year, readSummary(t.item.ID))
			os.WriteFile(filepath.Join(dir, folder+".nfo"), []byte(nfo), 0o644)
			mu.Lock()
			written++
			mu.Unlock()
		}()
	}
	wg.Wait()
	fmt.Printf("Wrote %d movies to %s\n", written, outDir)
	return nil
}

// readSummary returns the AI-generated plot summary for a file id, or "" if
// none exists. Summaries live in their own id-keyed store (like subtitles) so
// they survive every `gateway movies` regeneration.
func readSummary(id int64) string {
	dir := os.Getenv("GATEWAY_SUMS")
	if dir == "" {
		dir = "/summaries"
	}
	b, err := os.ReadFile(filepath.Join(dir, strconv.FormatInt(id, 10)+".txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func buildMovieNFO(title string, created time.Time, year int, plot string) string {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "&", "&amp;")
		s = strings.ReplaceAll(s, "<", "&lt;")
		return strings.ReplaceAll(s, ">", "&gt;")
	}
	premiered := ""
	if year > 0 {
		premiered = created.Format("2006-01-02")
	}
	plotXML := ""
	if plot != "" {
		plotXML = fmt.Sprintf("\n  <plot>%s</plot>", esc(plot))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<movie>
  <title>%s</title>
  <sorttitle>%s</sorttitle>
  <premiered>%s</premiered>
  <year>%d</year>%s
</movie>
`, esc(title), premiered, premiered, year, plotXML)
}
