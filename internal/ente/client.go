package ente

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/GraysonCAdams/ente-jellyfin/internal/crypto"
	"github.com/GraysonCAdams/ente-jellyfin/internal/encoding"
	"github.com/GraysonCAdams/ente-jellyfin/internal/session"
)

const (
	apiHost         = "https://api.ente.com"
	downloadHost    = "https://files.ente.com/?fileID="
	clientPkgHeader = "X-Client-Package"
	clientPkg       = "io.ente.photos"
	tokenHeader     = "X-Auth-Token"
)

// Client is a thin, read-only client for the official Ente cloud, authorized
// with the token recovered from the CLI session.
type Client struct {
	sess *session.Session
	http *http.Client // short calls: API/JSON, thumbnails
	dl   *http.Client // large blob downloads: no total cap (files reach 1GB+)
}

func New(sess *session.Session) *Client {
	return &Client{
		sess: sess,
		http: &http.Client{Timeout: 60 * time.Second},
		// A total Client.Timeout also caps body streaming, so a 60s cap can
		// never finish a 1GB original download. This client has no overall
		// deadline; ResponseHeaderTimeout still guards against a dead connect
		// hanging forever (the failure mode that once wedged the batch).
		dl: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

func (c *Client) Session() *session.Session { return c.sess }

// get issues an authenticated GET to the API and decodes JSON into out.
func (c *Client) get(path string, query url.Values, out interface{}) (int, error) {
	u := apiHost + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set(tokenHeader, c.sess.Token)
	req.Header.Set(clientPkgHeader, clientPkg)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("GET %s -> %d: %s", path, resp.StatusCode, string(body))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// GetCollections returns albums changed since sinceTime (0 for all).
func (c *Client) GetCollections(sinceTime int64) ([]Collection, error) {
	var res struct {
		Collections []Collection `json:"collections"`
	}
	q := url.Values{"sinceTime": {strconv.FormatInt(sinceTime, 10)}}
	if _, err := c.get("/collections/v2", q, &res); err != nil {
		return nil, err
	}
	return res.Collections, nil
}

// GetFilesDiff returns files in a collection changed since sinceTime, plus
// whether more pages remain.
func (c *Client) GetFilesDiff(collectionID, sinceTime int64) ([]File, bool, error) {
	var res struct {
		Diff    []File `json:"diff"`
		HasMore bool   `json:"hasMore"`
	}
	q := url.Values{
		"collectionID": {strconv.FormatInt(collectionID, 10)},
		"sinceTime":    {strconv.FormatInt(sinceTime, 10)},
	}
	if _, err := c.get("/collections/v2/diff", q, &res); err != nil {
		return nil, false, err
	}
	return res.Diff, res.HasMore, nil
}

// DownloadEncrypted opens the raw encrypted blob for a file (the original).
// Caller decrypts the stream with the file key + nonce.
func (c *Client) DownloadEncrypted(fileID int64) (io.ReadCloser, int64, error) {
	req, err := http.NewRequest(http.MethodGet, downloadHost+strconv.FormatInt(fileID, 10), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set(tokenHeader, c.sess.Token)
	req.Header.Set(clientPkgHeader, clientPkg)
	resp, err := c.dl.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, 0, fmt.Errorf("download file %d -> %d: %s", fileID, resp.StatusCode, string(body))
	}
	return resp.Body, resp.ContentLength, nil
}

// --- Key hierarchy -------------------------------------------------------

// CollectionKey unwraps an album's key: secretbox under the master key for
// owned albums, sealed box under our keypair for shared albums.
func (c *Client) CollectionKey(col Collection) ([]byte, error) {
	if col.Owner.ID == c.sess.UserID {
		return crypto.SecretBoxOpen(
			encoding.DecodeBase64(col.EncryptedKey),
			encoding.DecodeBase64(col.KeyDecryptionNonce),
			c.sess.MasterKey,
		)
	}
	return crypto.SealedBoxOpen(
		encoding.DecodeBase64(col.EncryptedKey),
		c.sess.PublicKey,
		c.sess.SecretKey,
	)
}

// FileKey unwraps a file's content key using its album's key.
func FileKey(f File, collectionKey []byte) ([]byte, error) {
	return crypto.SecretBoxOpen(
		encoding.DecodeBase64(f.EncryptedKey),
		encoding.DecodeBase64(f.KeyDecryptionNonce),
		collectionKey,
	)
}

// --- Video preview (HLS) -------------------------------------------------

// RemoteFileData is the encrypted "file data" payload (used for vid_preview).
type RemoteFileData struct {
	FileID           int64  `json:"fileID"`
	Type             string `json:"type"`
	EncryptedData    string `json:"encryptedData"`
	DecryptionHeader string `json:"decryptionHeader"`
}

// FetchVidPreviewPlaylist fetches the encrypted HLS playlist blob for a video,
// if one has been generated. ok=false means no streamable preview exists yet.
func (c *Client) FetchVidPreviewPlaylist(fileID int64) (data RemoteFileData, ok bool, err error) {
	var res struct {
		Data RemoteFileData `json:"data"`
	}
	q := url.Values{
		"type":            {"vid_preview"},
		"fileID":          {strconv.FormatInt(fileID, 10)},
		"preferNoContent": {"true"},
	}
	status, err := c.get("/files/data/fetch", q, &res)
	if err != nil {
		return RemoteFileData{}, false, err
	}
	if status == http.StatusNoContent || status == http.StatusNotFound {
		return RemoteFileData{}, false, nil
	}
	return res.Data, true, nil
}

// FetchVidPreviewURL returns a presigned URL for the encrypted HLS segment
// blob (output.ts) referenced by the playlist.
func (c *Client) FetchVidPreviewURL(fileID int64) (string, bool, error) {
	var res struct {
		URL string `json:"url"`
	}
	q := url.Values{
		"type":   {"vid_preview"},
		"fileID": {strconv.FormatInt(fileID, 10)},
	}
	status, err := c.get("/files/data/preview", q, &res)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	return res.URL, res.URL != "", nil
}
