package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// Argon2 is the cost of Argon2id password hashes: memory in KiB, passes,
// and threads.
type Argon2 struct {
	Memory  uint32
	Time    uint32
	Threads uint8
}

// Argon2Defaults are OWASP's recommended minimum for Argon2id: 19 MiB, two
// passes, one thread — some tens of milliseconds per hash.
var Argon2Defaults = Argon2{Memory: 19 * 1024, Time: 2, Threads: 1}

// maxPassword bounds what is hashed, so a megabyte "password" isn't work
// to do.
const maxPassword = 1024

var errPasswordLength = errors.New("auth: a password is 1 to 1024 bytes")

// HashPassword encodes a password for storage with Argon2id and the default
// cost: "$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>".
func HashPassword(password string) (string, error) { return Argon2Defaults.Hash(password) }

// Hash encodes a password with Argon2id at this cost.
func (p Argon2) Hash(password string) (string, error) {
	if len(password) == 0 || len(password) > maxPassword {
		return "", errPasswordLength
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}

// CheckPassword reports whether password is the one encoded, and whether
// the encoding is stale — another algorithm (bcrypt), or a cost lower than
// cost — so that a login can store a new hash. A malformed encoding is an
// error.
func CheckPassword(encoded, password string, cost Argon2) (ok, stale bool, err error) {
	if len(password) == 0 || len(password) > maxPassword {
		return false, false, nil
	}
	switch {
	case strings.HasPrefix(encoded, "$argon2id$"):
		p, salt, sum, err := parseArgon2(encoded)
		if err != nil {
			return false, false, err
		}
		got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(sum)))
		ok := subtle.ConstantTimeCompare(got, sum) == 1
		return ok, ok && (p.Memory < cost.Memory || p.Time < cost.Time || p.Threads < cost.Threads), nil
	case strings.HasPrefix(encoded, "$2a$"), strings.HasPrefix(encoded, "$2b$"), strings.HasPrefix(encoded, "$2y$"):
		if len(password) > 72 { // bcrypt reads 72 bytes; never let the rest pass unchecked
			return false, false, nil
		}
		err := bcrypt.CompareHashAndPassword([]byte(encoded), []byte(password))
		switch {
		case err == nil:
			return true, true, nil
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			return false, false, nil
		}
		return false, false, err
	}
	return false, false, errors.New("auth: unknown password encoding")
}

func parseArgon2(s string) (p Argon2, salt, sum []byte, err error) {
	parts := strings.Split(s, "$") // "", "argon2id", "v=19", "m=…,t=…,p=…", salt, hash
	bad := errors.New("auth: malformed argon2id hash")
	if len(parts) != 6 {
		return p, nil, nil, bad
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return p, nil, nil, bad
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, bad
	}
	if p.Memory == 0 || p.Memory > 1<<20 || p.Time == 0 || p.Time > 100 || p.Threads == 0 { // a stored hash can't ask for more than 1 GiB
		return p, nil, nil, bad
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, bad
	}
	if sum, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(sum) < 16 || len(sum) > 64 {
		return p, nil, nil, bad
	}
	return p, salt, sum, nil
}
