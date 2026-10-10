package contentcrypto

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const PublicSecretName = "content-public-key.txt"
const PrivateSecretName = "content-private-key.txt"

// LoadSecrets never generates or writes key material. The API only reads its
// two Docker secret mounts and checks their fingerprint against PostgreSQL.
func LoadSecrets(publicPath, privatePath, expected string) (*Key, error) {
	private, err := readSecret(privatePath)
	if err != nil {
		return nil, err
	}
	public, err := readSecret(publicPath)
	if err != nil {
		return nil, err
	}
	key, err := keyFromPrivate(private)
	if err != nil || !bytes.Equal(key.Public, public) {
		return nil, ErrInvalid
	}
	if expected != "" && key.ID != expected {
		return nil, errors.New("encryption secrets do not match this database")
	}
	return key, nil
}

func keyFromPrivate(raw []byte) (*Key, error) {
	private, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	public := private.PublicKey().Bytes()
	hash := sha256.Sum256(public)
	return &Key{ID: hex.EncodeToString(hash[:]), Public: public, Private: raw}, nil
}

func readSecret(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 128 {
		return nil, errors.New("encryption secret must be a restricted regular file (0400 or 0600)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, ErrInvalid
	}
	return key, nil
}

// PrepareSecretFiles is used by the one-shot setup container, never the API.
// Existing private material wins; a partial private-only write can be resumed.
// A missing established key or a conflicting file is never replaced.
func PrepareSecretFiles(dir, legacyPath, expected string) (*Key, error) {
	publicPath, privatePath := filepath.Join(dir, PublicSecretName), filepath.Join(dir, PrivateSecretName)
	private, err := readSecret(privatePath)
	var key *Key
	if err == nil {
		key, err = keyFromPrivate(private)
	} else if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(legacyPath); statErr == nil {
			key, err = LoadOrCreate(legacyPath, expected)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		} else {
			if expected != "" {
				return nil, errors.New("existing encryption key missing: restore its original backup")
			}
			if _, publicErr := os.Stat(publicPath); !errors.Is(publicErr, os.ErrNotExist) {
				return nil, errors.New("private encryption secret missing; restore the original pair")
			}
			key, err = Generate()
		}
	}
	if err != nil {
		return nil, err
	}
	if expected != "" && key.ID != expected {
		return nil, errors.New("encryption secrets do not match this database")
	}
	// Check any existing public half before writing even the private half.
	if public, err := readSecret(publicPath); err == nil {
		if !bytes.Equal(public, key.Public) {
			return nil, ErrInvalid
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := writeSecret(privatePath, key.Private); err != nil {
		return nil, err
	}
	if err := writeSecret(publicPath, key.Public); err != nil {
		return nil, err
	}
	return LoadSecrets(publicPath, privatePath, key.ID)
}

func writeSecret(path string, raw []byte) error {
	if current, err := readSecret(path); err == nil {
		if !bytes.Equal(current, raw) {
			return ErrInvalid
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".content-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0400); err == nil {
		_, err = f.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n")
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// PrepareSecrets serializes setup with API initialization and remembers the key
// before any message can be encrypted. It does not migrate message/file data.
func PrepareSecrets(ctx context.Context, db *pgxpool.Pool, dir, legacyPath string) error {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(736429182)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(736429182)`)
	var expected string
	err = conn.QueryRow(ctx, `SELECT key_id FROM content_encryption WHERE singleton`).Scan(&expected)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	key, err := PrepareSecretFiles(dir, legacyPath, expected)
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		// Compose file-backed secrets keep host ownership; grant the API UID access.
		for _, name := range []string{PublicSecretName, PrivateSecretName} {
			if err := os.Chown(filepath.Join(dir, name), 10001, 10001); err != nil {
				return err
			}
		}
	}
	_, err = conn.Exec(ctx, `INSERT INTO content_encryption(singleton,key_id) VALUES(true,$1) ON CONFLICT DO NOTHING`, key.ID)
	return err
}
