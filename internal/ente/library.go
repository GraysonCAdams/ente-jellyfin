package ente

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/GraysonCAdams/ente-jellyfin/internal/crypto"
)

// MediaItem is a decrypted, playable/viewable file with the keys needed to
// stream it on demand. Keys are held in memory only.
type MediaItem struct {
	ID        int64
	Title     string
	Type      FileType
	FileKey   []byte // content decryption key
	FileNonce string // secretstream header for the original blob (base64)
	ThumbKey  []byte // == FileKey; thumbnail shares the file key
	ThumbNonce string
	Size      int64
	ThumbSize int64
	Created   time.Time
	AlbumID   int64
	Lat       float64 // 0 if absent
	Long      float64
}

// Album is a decrypted collection with its media.
type Album struct {
	ID    int64
	Name  string
	Items []*MediaItem
}

// Library is the in-memory index the gateway serves from.
type Library struct {
	Albums    []*Album
	ByID      map[int64]*MediaItem
	BuiltAt   time.Time
}

// BuildLibrary syncs all collections and their files from the official cloud,
// decrypting album names, file keys, and per-file metadata. Nothing is written
// to disk; the returned index holds keys in memory for on-demand streaming.
func (c *Client) BuildLibrary() (*Library, error) {
	cols, err := c.GetCollections(0)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}

	lib := &Library{ByID: map[int64]*MediaItem{}, BuiltAt: time.Now()}
	// De-dup files that live in multiple albums; first album wins for display.
	seen := map[int64]bool{}

	for _, col := range cols {
		if col.IsDeleted {
			continue
		}
		colKey, err := c.CollectionKey(col)
		if err != nil {
			// Skip albums we can't unwrap rather than aborting the whole sync.
			fmt.Printf("warn: skip collection %d (key: %v)\n", col.ID, err)
			continue
		}
		name := decryptCollectionName(col, colKey)
		album := &Album{ID: col.ID, Name: name}

		var sinceTime int64
		for {
			files, hasMore, err := c.GetFilesDiff(col.ID, sinceTime)
			if err != nil {
				return nil, fmt.Errorf("diff collection %q (%d): %w", name, col.ID, err)
			}
			for _, f := range files {
				if f.UpdationTime > sinceTime {
					sinceTime = f.UpdationTime
				}
				if f.IsRemovedFromAlbum() || seen[f.ID] {
					continue
				}
				item, err := c.buildItem(col.ID, f, colKey)
				if err != nil {
					fmt.Printf("warn: skip file %d in %q: %v\n", f.ID, name, err)
					continue
				}
				seen[f.ID] = true
				lib.ByID[item.ID] = item
				album.Items = append(album.Items, item)
			}
			if !hasMore || len(files) == 0 {
				break
			}
		}

		if len(album.Items) > 0 {
			sort.Slice(album.Items, func(i, j int) bool {
				return album.Items[i].Created.Before(album.Items[j].Created)
			})
			lib.Albums = append(lib.Albums, album)
		}
	}

	sort.Slice(lib.Albums, func(i, j int) bool { return lib.Albums[i].Name < lib.Albums[j].Name })
	return lib, nil
}

func (c *Client) buildItem(albumID int64, f File, colKey []byte) (*MediaItem, error) {
	fileKey, err := FileKey(f, colKey)
	if err != nil {
		return nil, fmt.Errorf("file key: %w", err)
	}
	meta, err := decryptMetadata(f, fileKey)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}

	item := &MediaItem{
		ID:         f.ID,
		FileKey:    fileKey,
		FileNonce:  f.File.DecryptionHeader,
		ThumbKey:   fileKey,
		ThumbNonce: f.Thumbnail.DecryptionHeader,
		AlbumID:    albumID,
		Type:       fileTypeFromMeta(meta),
		Title:      stringField(meta, "title"),
		Created:    microTime(meta, "creationTime"),
	}
	if lat, ok := meta["latitude"].(float64); ok {
		if long, ok2 := meta["longitude"].(float64); ok2 && !(lat == 0 && long == 0) {
			item.Lat, item.Long = lat, long
		}
	}
	if f.Info != nil {
		item.Size = f.Info.FileSize
		item.ThumbSize = f.Info.ThumbnailSize
	}
	if item.Title == "" {
		item.Title = fmt.Sprintf("%d", f.ID)
	}
	return item, nil
}

func decryptCollectionName(col Collection, colKey []byte) string {
	if col.EncryptedName != "" {
		if name, err := crypto.SecretBoxOpenBase64(col.EncryptedName, col.NameDecryptionNonce, colKey); err == nil {
			return string(name)
		}
	}
	if col.Name != "" {
		return col.Name
	}
	return fmt.Sprintf("Album %d", col.ID)
}

func decryptMetadata(f File, fileKey []byte) (map[string]interface{}, error) {
	if f.Metadata.DecryptionHeader == "" || f.Metadata.EncryptedData == "" {
		return nil, fmt.Errorf("no metadata present")
	}
	_, jsonBytes, err := crypto.DecryptChaChaBase64(f.Metadata.EncryptedData, fileKey, f.Metadata.DecryptionHeader)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func fileTypeFromMeta(m map[string]interface{}) FileType {
	if v, ok := m["fileType"].(float64); ok {
		switch int(v) {
		case 1:
			return Video
		case 2:
			return LivePhoto
		}
	}
	return Image
}

func stringField(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func microTime(m map[string]interface{}, k string) time.Time {
	if v, ok := m[k].(float64); ok && v != 0 {
		return time.UnixMicro(int64(v))
	}
	return time.Time{}
}
