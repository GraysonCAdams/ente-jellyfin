package ente

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/GraysonCAdams/ente-jellyfin/internal/crypto"
	"github.com/GraysonCAdams/ente-jellyfin/internal/encoding"
)

const thumbHost = "https://thumbnails.ente.com/?fileID="

// DownloadThumbnail fetches a file's encrypted thumbnail blob.
func (c *Client) DownloadThumbnail(fileID int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, thumbHost+strconv.FormatInt(fileID, 10), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(tokenHeader, c.sess.Token)
	req.Header.Set(clientPkgHeader, clientPkg)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("thumbnail %d -> %d: %s", fileID, resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

// ThumbnailJPEG returns the decrypted thumbnail image bytes for a clip.
func (c *Client) ThumbnailJPEG(item *MediaItem) ([]byte, error) {
	enc, err := c.DownloadThumbnail(item.ID)
	if err != nil {
		return nil, err
	}
	return decryptSecretStream(enc, item.ThumbKey, encoding.DecodeBase64(item.ThumbNonce))
}

// decryptSecretStream decrypts an in-memory XChaCha20-Poly1305 secretstream
// blob (chunked at 4 MiB + 17 bytes overhead), used for small blobs like
// thumbnails.
func decryptSecretStream(data, key, nonce []byte) ([]byte, error) {
	dec, err := crypto.NewDecryptor(key, nonce)
	if err != nil {
		return nil, err
	}
	const chunk = 4*1024*1024 + crypto.XChaCha20Poly1305IetfABYTES
	var out []byte
	for off := 0; off < len(data); {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		plain, tag, err := dec.Pull(data[off:end])
		if err != nil {
			return nil, err
		}
		out = append(out, plain...)
		off = end
		if tag == crypto.TagFinal {
			break
		}
	}
	return out, nil
}
