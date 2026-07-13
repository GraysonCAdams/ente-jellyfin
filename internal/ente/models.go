package ente

// These mirror the Ente API response shapes (ported from the official CLI's
// internal/api types). Only the fields the gateway needs are retained.

// Collection is an Ente album.
type Collection struct {
	ID                  int64          `json:"id"`
	Owner               CollectionUser `json:"owner"`
	EncryptedKey        string         `json:"encryptedKey"`
	KeyDecryptionNonce  string         `json:"keyDecryptionNonce"`
	Name                string         `json:"name"`
	EncryptedName       string         `json:"encryptedName"`
	NameDecryptionNonce string         `json:"nameDecryptionNonce"`
	Type                string         `json:"type"`
	UpdationTime        int64          `json:"updationTime"`
	IsDeleted           bool           `json:"isDeleted,omitempty"`
	MagicMetadata       *MagicMetadata `json:"magicMetadata,omitempty"`
	PublicMagicMetadata *MagicMetadata `json:"pubMagicMetadata,omitempty"`
}

type CollectionUser struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type MagicMetadata struct {
	Version int    `json:"version,omitempty"`
	Count   int    `json:"count,omitempty"`
	Data    string `json:"data,omitempty"`
	Header  string `json:"header,omitempty"`
}

// File is an encrypted file within a collection.
type File struct {
	ID                 int64          `json:"id"`
	OwnerID            int64          `json:"ownerID"`
	CollectionID       int64          `json:"collectionID"`
	EncryptedKey       string         `json:"encryptedKey"`
	KeyDecryptionNonce string         `json:"keyDecryptionNonce"`
	File               FileAttributes `json:"file"`
	Thumbnail          FileAttributes `json:"thumbnail"`
	Metadata           FileAttributes `json:"metadata"`
	IsDeleted          bool           `json:"isDeleted"`
	UpdationTime       int64          `json:"updationTime"`
	MagicMetadata      *MagicMetadata `json:"magicMetadata,omitempty"`
	PubMagicMetadata   *MagicMetadata `json:"pubMagicMetadata,omitempty"`
	Info               *FileInfo      `json:"info,omitempty"`
}

// IsRemovedFromAlbum reports files that were deleted or tombstoned in a diff.
func (f File) IsRemovedFromAlbum() bool {
	return f.IsDeleted || f.File.EncryptedData == "-"
}

type FileInfo struct {
	FileSize      int64 `json:"fileSize,omitempty"`
	ThumbnailSize int64 `json:"thumbSize,omitempty"`
}

// FileAttributes carries the secretstream decryption header (nonce) for a
// blob; EncryptedData holds inline ciphertext for small blobs like metadata.
type FileAttributes struct {
	EncryptedData    string `json:"encryptedData,omitempty"`
	DecryptionHeader string `json:"decryptionHeader"`
}

// FileType enumerates Ente's media kinds (from decrypted metadata "fileType").
type FileType int

const (
	Image FileType = iota
	Video
	LivePhoto
)

func (t FileType) String() string {
	switch t {
	case Image:
		return "image"
	case Video:
		return "video"
	case LivePhoto:
		return "livephoto"
	default:
		return "unknown"
	}
}
