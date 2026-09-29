package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const passwordMemory = 64 * 1024

func HashPassword(password string) (string, error) {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 12 || utf8.RuneCountInString(password) > 128 || len(password) > 512 {
		return "", errors.New("password must contain 12–128 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, passwordMemory, 2, 32)
	enc := base64.RawStdEncoding
	return "argon2id:v1:" + enc.EncodeToString(salt) + ":" + enc.EncodeToString(hash), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, ":")
	if len(parts) != 4 || parts[0] != "argon2id" || parts[1] != "v1" || len(password) > 512 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := enc.DecodeString(parts[3])
	if err != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, passwordMemory, 2, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
