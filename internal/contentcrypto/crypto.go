// Package contentcrypto defines the versioned, chunked wire format shared with
// CryptoKit clients. HPKE is RFC 9180 base mode, X25519/HKDF-SHA256/AES-256-GCM.
// The common private key is deliberately shared; this is not end-to-end encryption.
package contentcrypto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const ChunkSize = 1 << 20
const HeaderSize = 80
const MaxPlainSize = 1 << 40
const Scheme = "fc1"
const magic = "FCENC001"

var ErrInvalid = errors.New("invalid encrypted content or key")

type Key struct {
	ID      string `json:"key_id"`
	Public  []byte `json:"public_key"`
	Private []byte `json:"private_key"`
}

func Generate() (*Key, error) {
	private, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		return nil, err
	}
	raw, err := private.Bytes()
	if err != nil {
		return nil, err
	}
	public := private.PublicKey().Bytes()
	fingerprint := sha256.Sum256(public)
	return &Key{hex.EncodeToString(fingerprint[:]), public, raw}, nil
}

func (k *Key) Validate() error {
	private, err := hpke.DHKEM(ecdh.X25519()).NewPrivateKey(k.Private)
	if err != nil {
		return ErrInvalid
	}
	id := sha256.Sum256(k.Public)
	if !bytes.Equal(private.PublicKey().Bytes(), k.Public) || hex.EncodeToString(id[:]) != k.ID {
		return ErrInvalid
	}
	return nil
}

// expected is the fingerprint saved in PostgreSQL. A missing key is only created
// for a new installation; loss of a previously used key must never be hidden.
// Callers serialize initialization with a database advisory lock.
func LoadOrCreate(path, expected string) (*Key, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && expected == "" {
		k, err := Generate()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		raw, err = json.Marshal(k)
		if err != nil {
			return nil, err
		}
		file, err := os.CreateTemp(filepath.Dir(path), ".key-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(file.Name())
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		// Link publishes a complete file without ever replacing an existing key.
		if err := os.Link(file.Name(), path); err != nil {
			return nil, err
		}
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		err = dir.Sync()
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, errors.New("encryption key missing: restore the original key backup")
	}
	var k Key
	if json.Unmarshal(raw, &k) != nil || k.Validate() != nil {
		return nil, ErrInvalid
	}
	if expected != "" && expected != k.ID {
		return nil, errors.New("encryption key does not match this database")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("encryption key must have permissions 0600")
	}
	return &k, nil
}

func StoredSize(size int64) int64 {
	if size < 0 || size > MaxPlainSize {
		return -1
	}
	chunks := (size + ChunkSize - 1) / ChunkSize
	if chunks == 0 {
		chunks = 1
	}
	return HeaderSize + size + chunks*16
}

func (k *Key) HeaderSize(header []byte) (int64, error) {
	if len(header) != HeaderSize || string(header[:8]) != magic || hex.EncodeToString(header[8:40]) != k.ID {
		return 0, ErrInvalid
	}
	n := binary.BigEndian.Uint64(header[72:80])
	if n > MaxPlainSize {
		return 0, ErrInvalid
	}
	return int64(n), nil
}

func (k *Key) Encrypt(out io.Writer, in io.Reader, size int64, context string) error {
	if StoredSize(size) < 0 {
		return ErrInvalid
	}
	public, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(k.Public)
	if err != nil {
		return err
	}
	enc, sender, err := hpke.NewSender(public, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte("FamilyChat/v1/"+context))
	if err != nil {
		return err
	}
	header := make([]byte, HeaderSize)
	copy(header, magic)
	fingerprint, _ := hex.DecodeString(k.ID)
	copy(header[8:40], fingerprint)
	copy(header[40:72], enc)
	binary.BigEndian.PutUint64(header[72:80], uint64(size))
	if _, err := out.Write(header); err != nil {
		return err
	}
	remaining := size
	for {
		n := min(remaining, ChunkSize)
		block := make([]byte, int(n))
		if _, err := io.ReadFull(in, block); err != nil {
			return err
		}
		sealed, err := sender.Seal(header, block)
		if err != nil {
			return err
		}
		if _, err := out.Write(sealed); err != nil {
			return err
		}
		remaining -= n
		if remaining == 0 {
			break
		}
	}
	var extra [1]byte
	if n, err := in.Read(extra[:]); n != 0 || err != io.EOF {
		return ErrInvalid
	}
	return nil
}

// Decrypt writes provisional plaintext. Callers must discard the entire output
// on failure, including a missing final block or trailing bytes.
func (k *Key) Decrypt(out io.Writer, in io.Reader, context string) error {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(in, header); err != nil {
		return ErrInvalid
	}
	remaining, err := k.HeaderSize(header)
	if err != nil {
		return err
	}
	private, err := hpke.DHKEM(ecdh.X25519()).NewPrivateKey(k.Private)
	if err != nil {
		return err
	}
	recipient, err := hpke.NewRecipient(header[40:72], private, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte("FamilyChat/v1/"+context))
	if err != nil {
		return err
	}
	for {
		n := min(remaining, ChunkSize)
		block := make([]byte, int(n)+16)
		if _, err := io.ReadFull(in, block); err != nil {
			return ErrInvalid
		}
		opened, err := recipient.Open(header, block)
		if err != nil {
			return ErrInvalid
		}
		if _, err := out.Write(opened); err != nil {
			return err
		}
		remaining -= n
		if remaining == 0 {
			break
		}
	}
	var extra [1]byte
	if n, err := in.Read(extra[:]); n != 0 || err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func MessageContext(chat, sender, client string) string {
	return "message:" + strings.ToLower(chat) + ":" + strings.ToLower(sender) + ":" + strings.ToLower(client)
}

func (k *Key) EncryptText(text, context string) (string, error) {
	var out bytes.Buffer
	err := k.Encrypt(&out, strings.NewReader(text), int64(len(text)), context)
	return base64.StdEncoding.EncodeToString(out.Bytes()), err
}

func (k *Key) ValidateText(text string) error {
	if len(text) > 86000 {
		return ErrInvalid
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || len(raw) < HeaderSize {
		return ErrInvalid
	}
	n, err := k.HeaderSize(raw[:HeaderSize])
	if err != nil || n > 64000 || int64(len(raw)) != StoredSize(n) {
		return ErrInvalid
	}
	return nil
}
