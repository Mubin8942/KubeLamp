package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Secrets KubeLamp has to send on (the SMS gateway and SMTP passwords) cannot be hashed
// like login passwords, so they are encrypted with AES-256-GCM. The key comes from
// KUBELAMP_SECRET_KEY (any text, hashed to 32 bytes) or from <data>/secret.key (32 random
// bytes, mode 600, made on first use). Keep the key with the data: without it the saved
// secrets cannot be read and have to be entered again.

type secretBox struct{ aead cipher.AEAD }

const secretPrefix = "enc:v1:"

func openSecretBox(dir string) (*secretBox, error) {
	var key []byte
	if k := os.Getenv("KUBELAMP_SECRET_KEY"); k != "" {
		h := sha256.Sum256([]byte(k))
		key = h[:]
	} else {
		p := filepath.Join(dir, "secret.key")
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			b = make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, err
			}
			_, err = f.Write(b)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		if len(b) != 32 {
			return nil, fmt.Errorf("%s must hold 32 bytes", p)
		}
		key = b
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead}, nil
}

func (b *secretBox) seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (b *secretBox) open(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, secretPrefix) {
		return "", errors.New("not an encrypted value")
	}
	raw, err := base64.StdEncoding.DecodeString(s[len(secretPrefix):])
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("damaged encrypted value")
	}
	n := b.aead.NonceSize()
	p, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt a saved secret: the secret key changed (enter the password again)")
	}
	return string(p), nil
}
