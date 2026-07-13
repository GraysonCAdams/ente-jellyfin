package ente

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// These methods mirror the writes the official Ente apps make when generating
// a streamable video preview. They ADD a vid_preview artifact to an existing
// file; they never modify or delete the original.

// PreviewUploadURL requests a presigned URL (and object ID) to upload a video's
// encrypted HLS segment blob (output.ts).
func (c *Client) PreviewUploadURL(fileID int64) (objectID, uploadURL string, err error) {
	var res struct {
		ObjectID string `json:"objectID"`
		URL      string `json:"url"`
	}
	q := url.Values{
		"fileID": {strconv.FormatInt(fileID, 10)},
		"type":   {"vid_preview"},
	}
	if _, err := c.get("/files/data/preview-upload-url", q, &res); err != nil {
		return "", "", err
	}
	if res.URL == "" || res.ObjectID == "" {
		return "", "", fmt.Errorf("empty preview upload url/objectID for file %d", fileID)
	}
	return res.ObjectID, res.URL, nil
}

// UploadObject PUTs a local file to a presigned S3 URL (the output.ts blob).
func (c *Client) UploadObject(presignedURL, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, presignedURL, f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload object -> %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// PutVideoData registers a generated HLS preview against a file: the object ID
// of the uploaded segments plus the encrypted playlist. This is the only write
// to the Ente account, and it is purely additive.
func (c *Client) PutVideoData(fileID int64, objectID string, objectSize int64, playlistB64, playlistHeaderB64 string) error {
	body := map[string]interface{}{
		"fileID":         fileID,
		"objectID":       objectID,
		"objectSize":     objectSize,
		"playlist":       playlistB64,
		"playlistHeader": playlistHeaderB64,
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, apiHost+"/files/video-data", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set(tokenHeader, c.sess.Token)
	req.Header.Set(clientPkgHeader, clientPkg)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("put video-data -> %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// DownloadToFile streams a file's encrypted blob to a local path.
func (c *Client) DownloadToFile(fileID int64, path string) error {
	rc, _, err := c.DownloadEncrypted(fileID)
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
