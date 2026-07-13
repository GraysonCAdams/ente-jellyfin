package ente

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GraysonCAdams/ente-jellyfin/internal/crypto"
)

// PlaylistJSON is the decrypted, gunzipped HLS descriptor Ente stores per video
// (file data of type "vid_preview"). Playlist is an HLS .m3u8 whose segments
// reference a single encrypted "output.ts" (fetched separately as preview data).
type PlaylistJSON struct {
	Type     string `json:"type"` // "hls_video"
	Playlist string `json:"playlist"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Size     int64  `json:"size"`
}

// VidPreview fetches and decrypts the HLS playlist for a video, if Ente has
// generated a streamable preview for it. ok=false means no preview exists yet
// (the video would have to be streamed from its original blob instead).
func (c *Client) VidPreview(item *MediaItem) (*PlaylistJSON, bool, error) {
	data, ok, err := c.FetchVidPreviewPlaylist(item.ID)
	if err != nil || !ok {
		return nil, ok, err
	}
	// The playlist blob is E2EE with the file key (secretstream), then gzipped.
	_, plain, err := crypto.DecryptChaChaBase64(data.EncryptedData, item.FileKey, data.DecryptionHeader)
	if err != nil {
		return nil, true, fmt.Errorf("decrypt playlist: %w", err)
	}
	unzipped, err := gunzip(plain)
	if err != nil {
		return nil, true, fmt.Errorf("gunzip playlist: %w", err)
	}
	var pj PlaylistJSON
	if err := json.Unmarshal(unzipped, &pj); err != nil {
		return nil, true, fmt.Errorf("parse playlist json: %w", err)
	}
	return &pj, true, nil
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
