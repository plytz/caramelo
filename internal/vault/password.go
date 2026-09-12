package vault

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

const DevPassword = "caramelo"

func PasswordSecret(dep string) string {
	up := strings.ToUpper(strings.TrimSpace(dep))
	up = strings.ReplaceAll(up, "-", "_")
	return up + "_PASSWORD"
}

const GeneratedPasswordBytes = 24

func GeneratePassword() (string, error) {
	b := make([]byte, GeneratedPasswordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
