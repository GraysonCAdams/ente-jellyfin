// Package session recovers the logged-in Ente session that the official
// `ente` CLI creates via `ente account add`. It reuses the CLI's battle-tested
// auth (SRP + OTP + TOTP/passkey) instead of reimplementing login: the CLI
// stores a 32-byte "device key" in the macOS Keychain (service "ente",
// account "ente-cli-user") and an encrypted account record (master key,
// secret key, API token) in ~/.ente/ente-cli.db (bbolt). We read both and
// recover the plaintext master key + token needed to talk to the Ente API.
package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/GraysonCAdams/ente-jellyfin/internal/crypto"
	"github.com/GraysonCAdams/ente-jellyfin/internal/encoding"
	keyring "github.com/zalando/go-keyring"
	bolt "go.etcd.io/bbolt"
)

const (
	keychainService = "ente"
	keychainUser    = "ente-cli-user"
	accountsBucket  = "accounts"
)

// encString mirrors the CLI's model.EncString: a device-key-wrapped secret,
// encrypted with XChaCha20-Poly1305 secretstream (single TagFinal chunk).
type encString struct {
	CipherText string `json:"cipherText"`
	Nonce      string `json:"nonce"`
}

func (e encString) decrypt(deviceKey []byte) ([]byte, error) {
	_, plain, err := crypto.DecryptChaChaBase64(e.CipherText, deviceKey, e.Nonce)
	return plain, err
}

// account mirrors the CLI's model.Account as persisted in the bbolt "accounts"
// bucket. JSON tags must match the CLI exactly.
type account struct {
	Email     string    `json:"email"`
	UserID    int64     `json:"userID"`
	App       string    `json:"app"`
	MasterKey encString `json:"masterKey"`
	SecretKey encString `json:"secretKey"`
	PublicKey string    `json:"publicKey"`
	Token     encString `json:"token"`
	ExportDir string    `json:"exportDir"`
}

// Session is the recovered, decrypted Ente session.
type Session struct {
	Email     string
	UserID    int64
	MasterKey []byte // unwraps collection keys (secretbox)
	SecretKey []byte // X25519 private key, for shared collections (sealed box)
	PublicKey []byte // X25519 public key
	Token     string // URL-safe base64, sent as the X-Auth-Token header
}

func deviceKey() ([]byte, error) {
	secret, err := keyring.Get(keychainService, keychainUser)
	if err != nil {
		return nil, fmt.Errorf("read device key from macOS Keychain (%s/%s): %w — run `ente account add` first", keychainService, keychainUser, err)
	}
	// The CLI stores the key as std-base64 of 32 raw bytes; older versions
	// stored the raw string. Match GetOrCreateClISecret's decoding.
	if decoded, derr := base64.StdEncoding.DecodeString(secret); derr == nil && len(decoded) == 32 {
		return decoded, nil
	}
	return []byte(secret), nil
}

func dbPath() string {
	if p := os.Getenv("ENTE_CLI_CONFIG_PATH"); p != "" {
		return filepath.Join(p, "ente-cli.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ente", "ente-cli.db")
}

// Load recovers the Ente session. On a server it reads injected env vars
// (ENTE_MASTER_KEY etc.); on the Mac it falls back to the CLI session in the
// Keychain + ente-cli.db.
func Load() (*Session, error) {
	if s := loadFromEnv(); s != nil {
		return s, nil
	}
	return loadFromCLI()
}

// loadFromEnv builds a session from secrets injected as environment variables
// (used on headless hosts where there is no macOS Keychain).
func loadFromEnv() *Session {
	token := os.Getenv("ENTE_TOKEN")
	masterKey := os.Getenv("ENTE_MASTER_KEY")
	if token == "" || masterKey == "" {
		return nil
	}
	uid, _ := strconv.ParseInt(os.Getenv("ENTE_USER_ID"), 10, 64)
	return &Session{
		Email:     os.Getenv("ENTE_EMAIL"),
		UserID:    uid,
		MasterKey: encoding.DecodeBase64(masterKey),
		SecretKey: encoding.DecodeBase64(os.Getenv("ENTE_SECRET_KEY")),
		PublicKey: encoding.DecodeBase64(os.Getenv("ENTE_PUBLIC_KEY")),
		Token:     token,
	}
}

// ExportEnv renders a session as the env-var lines a server needs. Values are
// secrets — handle accordingly.
func (s *Session) ExportEnv() string {
	return "ENTE_USER_ID=" + strconv.FormatInt(s.UserID, 10) + "\n" +
		"ENTE_EMAIL=" + s.Email + "\n" +
		"ENTE_MASTER_KEY=" + encoding.EncodeBase64(s.MasterKey) + "\n" +
		"ENTE_SECRET_KEY=" + encoding.EncodeBase64(s.SecretKey) + "\n" +
		"ENTE_PUBLIC_KEY=" + encoding.EncodeBase64(s.PublicKey) + "\n" +
		"ENTE_TOKEN=" + s.Token + "\n"
}

func loadFromCLI() (*Session, error) {
	dk, err := deviceKey()
	if err != nil {
		return nil, err
	}

	// Open read-only so we never disturb the CLI's own database.
	db, err := bolt.Open(dbPath(), 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath(), err)
	}
	defer db.Close()

	var chosen *account
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(accountsBucket))
		if b == nil {
			return fmt.Errorf("no accounts in ente-cli.db; run `ente account add` first")
		}
		return b.ForEach(func(_, v []byte) error {
			var a account
			if uerr := json.Unmarshal(v, &a); uerr != nil {
				return uerr
			}
			// Prefer the photos app; fall back to the first account found.
			if a.App == "photos" || chosen == nil {
				ac := a
				chosen = &ac
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if chosen == nil {
		return nil, fmt.Errorf("no ente account found; run `ente account add`")
	}

	master, err := chosen.MasterKey.decrypt(dk)
	if err != nil {
		return nil, fmt.Errorf("decrypt master key (device key mismatch?): %w", err)
	}
	secret, err := chosen.SecretKey.decrypt(dk)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret key: %w", err)
	}
	token, err := chosen.Token.decrypt(dk)
	if err != nil {
		return nil, fmt.Errorf("decrypt api token: %w", err)
	}

	return &Session{
		Email:     chosen.Email,
		UserID:    chosen.UserID,
		MasterKey: master,
		SecretKey: secret,
		PublicKey: encoding.DecodeBase64(chosen.PublicKey),
		Token:     base64.URLEncoding.EncodeToString(token),
	}, nil
}
