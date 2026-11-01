package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ensureKey crea una clave ed25519 sin passphrase en path si no existe y
// devuelve la línea pública (formato authorized_keys).
func ensureKey(path string) (string, error) {
	if pemBytes, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return "", fmt.Errorf("%s ya existe pero no es una clave válida sin passphrase: %w", path, err)
		}
		return authorizedLine(signer.PublicKey(), path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, keyComment())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	return authorizedLine(sshPub, path)
}

func authorizedLine(pub ssh.PublicKey, path string) (string, error) {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + keyComment()
	return line, os.WriteFile(path+".pub", []byte(line+"\n"), 0o644)
}

// keyComment solo lleva caracteres seguros para pasarlo por shells remotos.
func keyComment() string {
	host, _ := os.Hostname()
	host = regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(host, "")
	if host == "" {
		host = "windows"
	}
	return "despeje@" + host
}
