// Package token manages a persisted, auto-generated auth token so the
// service is never accidentally left running without authentication just
// because DIAPASON_AUTH_TOKEN was left unset.
package token

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// LoadOrCreate reads the token stored at path, or generates and persists a
// new random one if the file doesn't exist yet. Returns the token and
// whether it was newly generated.
func LoadOrCreate(path string) (tok string, generated bool, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		tok = strings.TrimSpace(string(data))
		if tok != "" {
			return tok, false, nil
		}
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("read token file: %w", err)
	}

	tok, err = generate()
	if err != nil {
		return "", false, fmt.Errorf("generate token: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("write token file: %w", err)
	}
	return tok, true, nil
}

func generate() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
