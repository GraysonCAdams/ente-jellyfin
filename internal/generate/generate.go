// Package generate replicates what Ente's own apps do to make a video
// streamable: download the original, transcode to a 720p single-file HLS with
// ffmpeg (Ente's exact recipe), then ADD the encrypted playlist + segments to
// the file as vid_preview data. It never modifies or deletes the original.
package generate

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GraysonCAdams/ente-jellyfin/internal/crypto"
	"github.com/GraysonCAdams/ente-jellyfin/internal/encoding"
	"github.com/GraysonCAdams/ente-jellyfin/internal/ente"
)

type Result struct {
	FileID       int64
	Width        int
	Height       int
	SegmentBytes int64
	Uploaded     bool
}

// Options tunes the transcode.
type Options struct {
	Upload bool // if false, dry run (no account writes)
	HWAccel bool // use Apple Silicon h264_videotoolbox instead of libx264
}

// Generate transcodes a video to HLS. If Upload is false it's a dry run:
// everything runs except the two writes to Ente (nothing touches the account).
func Generate(client *ente.Client, item *ente.MediaItem, opt Options) (*Result, error) {
	upload := opt.Upload
	work, err := os.MkdirTemp("", fmt.Sprintf("ente-gen-%d-", item.ID))
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	// 1. Download the encrypted original and decrypt it locally (read-only op).
	encPath := filepath.Join(work, "orig.enc")
	if err := client.DownloadToFile(item.ID, encPath); err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	inPath := filepath.Join(work, "input.bin")
	if err := crypto.DecryptFile(encPath, inPath, item.FileKey, encoding.DecodeBase64(item.FileNonce)); err != nil {
		return nil, fmt.Errorf("decrypt original: %w", err)
	}

	// 2. Random AES-128 key + ffmpeg keyinfo (embeds key as a data: URI, exactly
	//    like the Ente apps, so the official clients also recognize the preview).
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(work, "keyfile.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, err
	}
	keyinfoPath := filepath.Join(work, "mykey.keyinfo")
	keyinfo := fmt.Sprintf("data:text/plain;base64,%s\n%s\n", base64.StdEncoding.EncodeToString(key), keyPath)
	if err := os.WriteFile(keyinfoPath, []byte(keyinfo), 0o600); err != nil {
		return nil, err
	}

	// 3. Transcode: Ente's recipe (720p, libx264 maxrate 2000k, aac, single-file HLS).
	m3u8Path := filepath.Join(work, "output.m3u8")
	tsPath := filepath.Join(work, "output.ts")
	vcodec := []string{"-c:v", "libx264", "-maxrate", "2000k", "-bufsize", "4000k"}
	if opt.HWAccel {
		// Apple Silicon hardware H.264 encoder: much faster, cooler.
		vcodec = []string{"-c:v", "h264_videotoolbox", "-b:v", "2000k"}
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", inPath,
		"-vf", "scale='if(lt(iw,ih),min(720,iw),-2)':'if(lt(iw,ih),-2,min(720,ih))',format=yuv420p",
	}
	args = append(args, vcodec...)
	args = append(args,
		"-c:a", "aac", "-b:a", "128k",
		"-f", "hls", "-hls_flags", "single_file", "-hls_list_size", "0",
		"-hls_key_info_file", keyinfoPath,
		m3u8Path,
	)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	}

	tsInfo, err := os.Stat(tsPath)
	if err != nil {
		return nil, fmt.Errorf("no output.ts produced: %w", err)
	}
	// Report the transcoded resolution by probing the decrypted input and
	// applying the same scale rule ffmpeg used (the output blob is encrypted).
	iw, ih := probeFileDims(inPath)
	w, h := scaledDims(iw, ih)
	res := &Result{FileID: item.ID, Width: w, Height: h, SegmentBytes: tsInfo.Size()}

	if !upload {
		return res, nil // dry run: stop before any account write
	}

	// 4. Upload the encrypted segments blob to a presigned URL.
	objectID, upURL, err := client.PreviewUploadURL(item.ID)
	if err != nil {
		return nil, fmt.Errorf("preview upload url: %w", err)
	}
	if err := client.UploadObject(upURL, tsPath); err != nil {
		return nil, fmt.Errorf("upload segments: %w", err)
	}

	// 5. Encrypt the playlist with the file key (gzip -> secretstream) and register.
	m3u8, err := os.ReadFile(m3u8Path)
	if err != nil {
		return nil, err
	}
	playlistJSON, _ := json.Marshal(map[string]interface{}{
		"playlist": string(m3u8),
		"type":     "hls_video",
		"width":    w,
		"height":   h,
		"size":     tsInfo.Size(),
	})
	cipher, header, err := crypto.EncryptChaCha20poly1305(gzipBytes(playlistJSON), item.FileKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt playlist: %w", err)
	}
	if err := client.PutVideoData(item.ID, objectID, tsInfo.Size(),
		encoding.EncodeBase64(cipher), encoding.EncodeBase64(header)); err != nil {
		return nil, fmt.Errorf("put video-data: %w", err)
	}
	res.Uploaded = true
	return res, nil
}

// probeFileDims returns the width/height of a plain (decrypted) media file.
func probeFileDims(path string) (int, int) {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", path).Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "x")
	if len(parts) != 2 {
		return 0, 0
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	return w, h
}

// scaledDims replicates ffmpeg's scale expression: cap the shorter side at 720,
// scale the other proportionally to the nearest even number.
func scaledDims(iw, ih int) (int, int) {
	if iw == 0 || ih == 0 {
		return iw, ih
	}
	if iw < ih { // portrait: cap width
		w := iw
		if w > 720 {
			w = 720
		}
		return w, roundEven(float64(ih) * float64(w) / float64(iw))
	}
	// landscape/square: cap height
	h := ih
	if h > 720 {
		h = 720
	}
	return roundEven(float64(iw) * float64(h) / float64(ih)), h
}

func roundEven(v float64) int {
	n := int(v + 0.5)
	if n%2 != 0 {
		n++
	}
	return n
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}
