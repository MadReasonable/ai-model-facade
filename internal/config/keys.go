package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// hashPrefix names the algorithm of a key's hash, so the value says what it
// is, the way a stored password hash names its scheme.
const hashPrefix = "sha256:"

// HashKey returns the hash line value for key, as server.apiKeys takes it.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hashPrefix + hex.EncodeToString(sum[:])
}

// NewKey returns a random key: "amf-" and 32 random bytes in base64url. The
// prefix lets a secret scanner recognize a leaked key. A plain SHA-256 is a
// safe hash for it only because it is random: a salt and a slow hash protect
// keys people choose, which can be guessed, and this one can't be.
func NewKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "amf-" + base64.RawURLEncoding.EncodeToString(b)
}

// indexKeys checks server.apiKeys and indexes the entries by the SHA-256 of
// their keys, hashing those given as the key itself, so a request is matched
// the same way whichever form an entry uses. A name may have several entries,
// each a key of the same caller, such as an old and a new key while clients
// change over. Two entries with the same key are a note, not an error: the
// first one is used, as it was when keys were compared in order.
func (cfg *Config) indexKeys() error {
	s := &cfg.Server
	s.keys = map[[sha256.Size]byte]string{}
	for i, k := range s.APIKeys {
		if k.Name == "" {
			return fmt.Errorf("server.apiKeys[%d]: name is required", i)
		}
		if k.Name == UnknownClient {
			return fmt.Errorf("server.apiKeys[%d]: name '%s' is reserved for unlisted keys", i, UnknownClient)
		}
		digest, err := k.digest()
		if err != nil {
			return fmt.Errorf("server.apiKeys[%d] ('%s'): %w", i, k.Name, err)
		}
		if first, ok := s.keys[digest]; ok {
			if first == k.Name {
				cfg.Notes = append(cfg.Notes, fmt.Sprintf("server.apiKeys: '%s' has the same key in two entries; the first one will be used", k.Name))
			} else {
				cfg.Notes = append(cfg.Notes, fmt.Sprintf("server.apiKeys: key of '%s' is the same as '%s'; requests with it are named '%s'", k.Name, first, first))
			}
			continue
		}
		s.keys[digest] = k.Name
	}
	return nil
}

// digest returns the SHA-256 of the entry's key, from the key or its hash.
func (k APIKey) digest() ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	switch {
	case k.Key != nil && k.Hash != nil:
		return digest, fmt.Errorf("give key or hash, not both")
	case k.Key == nil && k.Hash == nil:
		return digest, fmt.Errorf("key or hash is required")
	case k.Key != nil && *k.Key == "":
		return digest, fmt.Errorf("key is empty; if it comes from an environment variable, that variable is set to nothing")
	case k.Key != nil:
		return sha256.Sum256([]byte(*k.Key)), nil
	case *k.Hash == "":
		return digest, fmt.Errorf("hash is empty; if it comes from an environment variable, that variable is set to nothing")
	}
	hexDigest, ok := strings.CutPrefix(*k.Hash, hashPrefix)
	if !ok || len(hexDigest) != hex.EncodedLen(sha256.Size) {
		return digest, fmt.Errorf("hash must be '%s' followed by 64 hex characters, as hash-key prints it", hashPrefix)
	}
	if _, err := hex.Decode(digest[:], []byte(hexDigest)); err != nil {
		return digest, fmt.Errorf("hash must be '%s' followed by 64 hex characters, as hash-key prints it", hashPrefix)
	}
	return digest, nil
}

// KeyName returns the name of the listed key whose SHA-256 is digest.
func (s *Server) KeyName(digest [sha256.Size]byte) (string, bool) {
	name, ok := s.keys[digest]
	return name, ok
}
