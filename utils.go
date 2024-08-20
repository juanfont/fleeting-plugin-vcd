package vcd

import (
	"crypto"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

type PrivPub interface {
	crypto.PrivateKey
	Public() crypto.PublicKey
}

// GenerateRandomBytes returns securely generated random bytes.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func GenerateRandomBytes(n int) ([]byte, error) {
	bytes := make([]byte, n)

	// Note that err == nil only if we read len(b) bytes.
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}

	return bytes, nil
}

// GenerateRandomStringURLSafe returns a URL-safe, base64 encoded
// securely generated random string.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func GenerateRandomStringURLSafe(n int) (string, error) {
	b, err := GenerateRandomBytes(n)

	return base64.RawURLEncoding.EncodeToString(b), err
}

// GenerateRandomStringVMNameSafe returns a string that can
// be used as a VM name.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func GenerateRandomStringVMNameSafe(size int) (string, error) {
	var str string
	var err error
	for len(str) < size {
		str, err = GenerateRandomStringURLSafe(size)
		if err != nil {
			return "", err
		}
		str = strings.ToLower(
			strings.ReplaceAll(strings.ReplaceAll(str, "_", ""), "-", ""),
		)
	}

	return str[:size], nil
}

func generateVMName(prefix string) (string, error) {
	randomSuffix, err := GenerateRandomStringVMNameSafe(8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", prefix, randomSuffix), nil
}

func boolPointer(value bool) *bool {
	return &value
}
