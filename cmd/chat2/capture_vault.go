package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var captureVaultMagic = []byte("CHAT2-CAPTURE-V1\x00")

type captureVault struct {
	mu     sync.Mutex
	files  map[string]*captureKey
	closed bool
}

type captureKey struct {
	bytes [32]byte
}

func newCaptureVault() *captureVault {
	return &captureVault{files: make(map[string]*captureKey)}
}

func (v *captureVault) SealJPEG(directory string, plaintext []byte) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return "", errors.New("encrypted capture vault is closed")
	}
	key := &captureKey{}
	if _, err := rand.Read(key.bytes[:]); err != nil {
		return "", fmt.Errorf("create capture encryption key: %w", err)
	}
	stored := false
	defer func() {
		if !stored {
			clear(key.bytes[:])
		}
	}()
	gcm, err := captureAEAD(key.bytes[:])
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("create capture nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, captureVaultMagic)
	payload := make([]byte, 0, len(captureVaultMagic)+len(nonce)+len(ciphertext))
	payload = append(payload, captureVaultMagic...)
	payload = append(payload, nonce...)
	payload = append(payload, ciphertext...)
	defer clear(nonce)
	defer clear(ciphertext)
	defer clear(payload)

	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create encrypted capture directory: %w", err)
	}
	path := filepath.Join(directory, "photo-"+time.Now().Format("20060102-150405.000000000")+".jpg.enc")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", fmt.Errorf("write encrypted camera capture: %w", err)
	}
	v.files[path] = key
	stored = true
	return path, nil
}

func (v *captureVault) Open(path string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, errors.New("encrypted capture vault is closed")
	}
	key, owned := v.files[path]
	if !owned {
		return nil, fmt.Errorf("capture is not owned by this session: %s", path)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encrypted camera capture: %w", err)
	}
	defer clear(payload)
	gcm, err := captureAEAD(key.bytes[:])
	if err != nil {
		return nil, err
	}
	headerSize := len(captureVaultMagic) + gcm.NonceSize()
	if len(payload) < headerSize || string(payload[:len(captureVaultMagic)]) != string(captureVaultMagic) {
		return nil, errors.New("encrypted camera capture has an invalid header")
	}
	nonce := payload[len(captureVaultMagic):headerSize]
	plaintext, err := gcm.Open(nil, nonce, payload[headerSize:], captureVaultMagic)
	if err != nil {
		return nil, fmt.Errorf("decrypt camera capture: %w", err)
	}
	return plaintext, nil
}

func (v *captureVault) Owns(path string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, owned := v.files[path]
	return owned
}

func (v *captureVault) Remove(path string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	key, owned := v.files[path]
	if !owned {
		return nil
	}
	err := removeCaptureFile(path)
	if err == nil {
		delete(v.files, path)
		clear(key.bytes[:])
	}
	return err
}

func (v *captureVault) Destroy() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	var cleanupErrors []error
	for path, key := range v.files {
		if err := removeCaptureFile(path); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		delete(v.files, path)
		clear(key.bytes[:])
	}
	v.closed = true
	return errors.Join(cleanupErrors...)
}

func captureAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize capture encryption: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize capture authentication: %w", err)
	}
	return gcm, nil
}
