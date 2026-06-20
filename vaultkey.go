package main

import (
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/crypto/argon2"
)

var vaultSalt = []byte("mediabin-vaultblob-v1")

func deriveVaultKey(passphrase string) [32]byte {
	key := argon2.IDKey([]byte(passphrase), vaultSalt, 1, 64*1024, 4, 32)
	var mk [32]byte
	copy(mk[:], key)
	return mk
}

func vaultKeyHex(passphrase string) string {
	key := deriveVaultKey(passphrase)
	return hex.EncodeToString(key[:])
}

func deriveFileID(id, purpose string) string {
	h := sha256.Sum256([]byte(id + "|" + purpose))
	return hex.EncodeToString(h[:16])
}
