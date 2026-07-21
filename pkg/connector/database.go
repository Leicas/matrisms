package connector

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
	"golang.org/x/crypto/pbkdf2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// SMSAccount represents a stored VoIP.ms account with credentials.
//
// One account = one VoIP.ms API user (api_username + api_password). The
// account may own several SMS-enabled DIDs; the user picks which ones to
// bridge at login time (all by default).
type SMSAccount struct {
	UserMXID     string    `json:"user_mxid"`
	APIUsername  string    `json:"api_username"` // VoIP.ms account email
	APIPassword  string    `json:"api_password"` // API password (NOT the portal password)
	DIDs         []string  `json:"dids"`         // monitored DIDs, E.164-ish digits (e.g. "5551234567")
	CreatedAt    time.Time `json:"created_at"`
	LastSyncTime time.Time `json:"last_sync_time"`
}

// GetDIDsJSON serializes DIDs to JSON for database storage.
func (a *SMSAccount) GetDIDsJSON() string {
	if len(a.DIDs) == 0 {
		return `[]`
	}
	data, err := json.Marshal(a.DIDs)
	if err != nil {
		return `[]`
	}
	return string(data)
}

// SetDIDsFromJSON deserializes DIDs from JSON database storage.
func (a *SMSAccount) SetDIDsFromJSON(jsonStr string) {
	if jsonStr == "" {
		a.DIDs = nil
		return
	}
	if err := json.Unmarshal([]byte(jsonStr), &a.DIDs); err != nil {
		a.DIDs = nil
	}
}

// SMSAccountQuery handles database operations for VoIP.ms accounts.
type SMSAccountQuery struct {
	DB *database.Database
}

// dialectQuery converts `?` placeholders to `$n` for Postgres; SQLite takes
// them as-is.
func dialectQuery(d dbutil.Dialect, q string) string {
	if d != dbutil.Postgres {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- Minimal AES-GCM helper and key management (self-contained) ---

const encPrefix = "v2:"

const (
	pbkdf2Iterations = 100000 // PBKDF2 iterations for key derivation
	saltSize         = 32     // Salt size in bytes
)

var (
	keyOnce sync.Once
	dbKey   []byte
	keyErr  error
)

func getDBKey() ([]byte, error) {
	keyOnce.Do(func() {
		// Step 1: Check environment variable (highest priority for production)
		passphrase := strings.TrimSpace(os.Getenv("MATRISMS_PASSPHRASE"))

		// Step 2: Check for passphrase file if env var not set
		if passphrase == "" {
			passphrase, _ = readPassphraseFile()
		}

		// Step 3: Auto-generate secure passphrase if neither exists
		if passphrase == "" {
			passphrase, keyErr = generateAndStorePassphrase()
			if keyErr != nil {
				return
			}
		}

		salt, err := getSalt()
		if err != nil {
			keyErr = fmt.Errorf("failed to get salt: %w", err)
			return
		}

		// Derive key using PBKDF2
		dbKey = pbkdf2.Key([]byte(passphrase), salt, pbkdf2Iterations, 32, sha256.New)
	})
	if keyErr != nil {
		return nil, keyErr
	}
	if len(dbKey) != 32 {
		return nil, fmt.Errorf("derived key must be 32 bytes, got %d", len(dbKey))
	}
	return dbKey, nil
}

// getPassphraseFilePath returns the canonical passphrase path:
// ./data/passphrase relative to the bridge's CWD. Co-located with the
// SQLite DB and matrisms.salt so a single volume mount persists everything
// the encryption layer needs across container restarts.
func getPassphraseFilePath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}
	return filepath.Join(cwd, "data", "passphrase"), nil
}

func readPassphraseFile() (string, error) {
	primary, err := getPassphraseFilePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(primary)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// generateAndStorePassphrase creates a new secure passphrase and writes it to
// ./data/passphrase.
func generateAndStorePassphrase() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random passphrase: %w", err)
	}
	passphrase := base64.StdEncoding.EncodeToString(randomBytes)

	passphrasePath, err := getPassphraseFilePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(passphrasePath), 0o700); err != nil {
		return "", fmt.Errorf("failed to create data directory: %w", err)
	}
	if err := os.WriteFile(passphrasePath, []byte(passphrase), 0o600); err != nil {
		return "", fmt.Errorf("failed to write passphrase file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Auto-generated secure passphrase stored at %s\n", passphrasePath)
	fmt.Fprintf(os.Stderr, "Set MATRISMS_PASSPHRASE in your environment to override this default.\n")

	return passphrase, nil
}

// getSalt returns the salt for PBKDF2, generating one if needed.
func getSalt() ([]byte, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}
	dataDir := filepath.Join(cwd, "data")
	saltPath := filepath.Join(dataDir, "matrisms.salt")

	// Try to read existing salt
	if data, err := os.ReadFile(saltPath); err == nil {
		salt, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err == nil && len(salt) == saltSize {
			return salt, nil
		}
	}

	// Generate new salt
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	saltB64 := base64.StdEncoding.EncodeToString(salt)
	if err := os.WriteFile(saltPath, []byte(saltB64), 0o600); err != nil {
		return nil, fmt.Errorf("failed to save salt: %w", err)
	}

	return salt, nil
}

func encryptString(plain string) (string, error) {
	key, err := getDBKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plain), nil)
	buf := append(nonce, ct...)
	return encPrefix + base64.StdEncoding.EncodeToString(buf), nil
}

func decryptString(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return "", errors.New("value is not encrypted with expected v2: prefix")
	}

	key, err := getDBKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce := raw[:gcm.NonceSize()]
	ct := raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func (saq *SMSAccountQuery) CreateTable(ctx context.Context) error {
	_, err := saq.DB.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sms_accounts (
			user_mxid TEXT NOT NULL,
			api_username TEXT NOT NULL,
			api_password TEXT NOT NULL,
			dids TEXT DEFAULT '[]',
			created_at TIMESTAMP NOT NULL,
			last_sync_time TIMESTAMP,
			PRIMARY KEY (user_mxid, api_username)
		)
	`)
	if err != nil {
		return err
	}

	// Poll cursor per (account, DID): last seen VoIP.ms message id + timestamp
	// for SMS and MMS streams. cursor is a JSON blob so the poller can evolve
	// its bookkeeping without schema migrations.
	_, err = saq.DB.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sms_poll_cursors (
			user_mxid TEXT NOT NULL,
			api_username TEXT NOT NULL,
			did TEXT NOT NULL,
			cursor TEXT NOT NULL DEFAULT '{}',
			PRIMARY KEY (user_mxid, api_username, did)
		)
	`)
	if err != nil {
		return err
	}

	// Small bridge-level key/value store (e.g. the mxc URI of the uploaded
	// network logo). Lives in the DB because ./data may be read-only for the
	// container user.
	_, err = saq.DB.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sms_bridge_kv (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`)
	return err
}

// GetKV reads a bridge-level key/value entry ("" when absent).
func (saq *SMSAccountQuery) GetKV(ctx context.Context, key string) (string, error) {
	rows, err := saq.DB.Query(ctx, dialectQuery(saq.DB.Dialect, `
		SELECT value FROM sms_bridge_kv WHERE key = ?
	`), key)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", rows.Err()
	}
	var value string
	if err := rows.Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

// SetKV upserts a bridge-level key/value entry.
func (saq *SMSAccountQuery) SetKV(ctx context.Context, key, value string) error {
	_, err := saq.DB.Exec(ctx, dialectQuery(saq.DB.Dialect, `
		INSERT INTO sms_bridge_kv (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value
	`), key, value)
	return err
}

func (saq *SMSAccountQuery) GetAccount(ctx context.Context, userMXID, apiUsername string) (*SMSAccount, error) {
	rows, err := saq.DB.Query(ctx, dialectQuery(saq.DB.Dialect, `
		SELECT user_mxid, api_username, api_password, COALESCE(dids, '[]'), created_at, last_sync_time
		FROM sms_accounts
		WHERE user_mxid = ? AND api_username = ?
	`), userMXID, apiUsername)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("database query failed: %w", err)
		}
		return nil, nil // No account found
	}

	account := &SMSAccount{}
	var didsJSON string
	var lastSync sql.NullTime
	err = rows.Scan(&account.UserMXID, &account.APIUsername, &account.APIPassword,
		&didsJSON, &account.CreatedAt, &lastSync)
	if err != nil {
		return nil, err
	}
	if lastSync.Valid {
		account.LastSyncTime = lastSync.Time
	}
	account.SetDIDsFromJSON(didsJSON)
	plain, derr := decryptString(account.APIPassword)
	if derr != nil {
		// Don't expose decryption details to prevent information disclosure
		return nil, fmt.Errorf("failed to decrypt stored credentials")
	}
	account.APIPassword = plain
	return account, nil
}

func (saq *SMSAccountQuery) GetUserAccounts(ctx context.Context, userMXID string) ([]*SMSAccount, error) {
	rows, err := saq.DB.Query(ctx, dialectQuery(saq.DB.Dialect, `
		SELECT user_mxid, api_username, api_password, COALESCE(dids, '[]'), created_at, last_sync_time
		FROM sms_accounts
		WHERE user_mxid = ?
		ORDER BY created_at ASC
	`), userMXID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []*SMSAccount
	for rows.Next() {
		account := &SMSAccount{}
		var didsJSON string
		var lastSync sql.NullTime
		err = rows.Scan(&account.UserMXID, &account.APIUsername, &account.APIPassword,
			&didsJSON, &account.CreatedAt, &lastSync)
		if err != nil {
			return nil, err
		}
		if lastSync.Valid {
			account.LastSyncTime = lastSync.Time
		}
		account.SetDIDsFromJSON(didsJSON)
		plain, derr := decryptString(account.APIPassword)
		if derr != nil {
			return nil, fmt.Errorf("failed to decrypt stored credentials")
		}
		account.APIPassword = plain
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (saq *SMSAccountQuery) UpsertAccount(ctx context.Context, account *SMSAccount) error {
	enc, err := encryptString(account.APIPassword)
	if err != nil {
		return fmt.Errorf("failed to encrypt API password: %w", err)
	}
	_, err = saq.DB.Exec(ctx, dialectQuery(saq.DB.Dialect, `
		INSERT INTO sms_accounts
		(user_mxid, api_username, api_password, dids, created_at, last_sync_time)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_mxid, api_username) DO UPDATE SET
			api_password = EXCLUDED.api_password,
			dids = EXCLUDED.dids,
			last_sync_time = EXCLUDED.last_sync_time
	`), account.UserMXID, account.APIUsername, enc,
		account.GetDIDsJSON(), account.CreatedAt, account.LastSyncTime)
	return err
}

func (saq *SMSAccountQuery) DeleteAccount(ctx context.Context, userMXID, apiUsername string) error {
	_, err := saq.DB.Exec(ctx, dialectQuery(saq.DB.Dialect, `
		DELETE FROM sms_accounts
		WHERE user_mxid = ? AND api_username = ?
	`), userMXID, apiUsername)
	if err != nil {
		return err
	}
	_, err = saq.DB.Exec(ctx, dialectQuery(saq.DB.Dialect, `
		DELETE FROM sms_poll_cursors
		WHERE user_mxid = ? AND api_username = ?
	`), userMXID, apiUsername)
	return err
}

func (saq *SMSAccountQuery) UpdateLastSync(ctx context.Context, userMXID, apiUsername string, syncTime time.Time) error {
	_, err := saq.DB.Exec(ctx, dialectQuery(saq.DB.Dialect, `
		UPDATE sms_accounts
		SET last_sync_time = ?
		WHERE user_mxid = ? AND api_username = ?
	`), syncTime, userMXID, apiUsername)
	return err
}

// GetPollCursor returns the stored poll cursor JSON for the (account, DID)
// pair, or "" when no cursor has been stored yet.
func (saq *SMSAccountQuery) GetPollCursor(ctx context.Context, userMXID, apiUsername, did string) (string, error) {
	rows, err := saq.DB.Query(ctx, dialectQuery(saq.DB.Dialect, `
		SELECT cursor FROM sms_poll_cursors
		WHERE user_mxid = ? AND api_username = ? AND did = ?
	`), userMXID, apiUsername, did)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", rows.Err()
	}
	var cursor string
	if err := rows.Scan(&cursor); err != nil {
		return "", err
	}
	return cursor, nil
}

func (saq *SMSAccountQuery) SetPollCursor(ctx context.Context, userMXID, apiUsername, did, cursor string) error {
	_, err := saq.DB.Exec(ctx, dialectQuery(saq.DB.Dialect, `
		INSERT INTO sms_poll_cursors (user_mxid, api_username, did, cursor)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_mxid, api_username, did) DO UPDATE SET
			cursor = EXCLUDED.cursor
	`), userMXID, apiUsername, did, cursor)
	return err
}
