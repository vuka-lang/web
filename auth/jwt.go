package auth

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// JWT verifies bearer JSON Web Tokens another service issued (an identity
// provider, a gateway): HS256 with a shared Key, or RS256 with PublicKey —
// one of them, which also pins the algorithm a token must name. The token's
// subject (the "sub" claim) is the user id Users.Load takes; its "exp" is
// required. auth never issues JWTs: its own tokens are opaque (Issue).
type JWT struct {
	Key       []byte         // HS256's secret, at least 32 bytes
	PublicKey *rsa.PublicKey // RS256's key
	Issuer    string         // the "iss" a token must carry, when set
	Audience  string         // an "aud" a token must list, when set
	Leeway    time.Duration  // clock skew allowed on exp and nbf; a minute when 0
}

func (j *JWT) validate() error {
	switch {
	case (j.Key == nil) == (j.PublicKey == nil):
		return errors.New("auth: JWT needs Key (HS256) or PublicKey (RS256), one of them")
	case j.Key != nil && len(j.Key) < 32:
		return errors.New("auth: JWT.Key is shorter than 32 bytes")
	}
	return nil
}

// isJWT tells a JWT (three dot-separated parts) from an opaque token.
func isJWT(tok string) bool {
	return strings.Count(tok, ".") == 2 && !strings.HasPrefix(tok, TokenPrefix)
}

type jwtClaims struct {
	Sub string          `json:"sub"`
	Iss string          `json:"iss"`
	Aud json.RawMessage `json:"aud"`
	Exp *float64        `json:"exp"`
	Nbf *float64        `json:"nbf"`
	Iat *float64        `json:"iat"`
}

// verifyJWT signs the request in with a JWT; one that doesn't verify is an
// error.
func (a *Auth) verifyJWT(ctx context.Context, st *state, tok string) error {
	j := a.cfg.JWT
	if j == nil {
		return errBadCredentials
	}
	c, err := j.verify(tok, time.Now())
	if err != nil {
		return errBadCredentials
	}
	iat := time.Time{}
	if c.Iat != nil {
		iat = unixTime(*c.Iat)
	}
	if a.jwtEpoch.revoked(c.Sub, iat) {
		return errBadCredentials
	}
	id, err := a.load(ctx, c.Sub)
	if err != nil {
		return err
	}
	if id == nil {
		return errBadCredentials
	}
	st.id, st.via, st.exp, st.iat = id, viaJWT, unixTime(*c.Exp), iat
	return nil
}

func unixTime(f float64) time.Time { return time.Unix(int64(f), 0) }

func (j *JWT) verify(tok string, now time.Time) (*jwtClaims, error) {
	bad := errors.New("auth: invalid JWT")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || len(tok) > 8192 {
		return nil, bad
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, bad
	}
	var h struct {
		Alg  string   `json:"alg"`
		Crit []string `json:"crit"`
	}
	if json.Unmarshal(hb, &h) != nil || len(h.Crit) > 0 {
		return nil, bad
	}
	signed := []byte(parts[0] + "." + parts[1])
	switch {
	case j.Key != nil && h.Alg == "HS256":
		mac := hmac.New(sha256.New, j.Key)
		mac.Write(signed)
		if !hmac.Equal(sig, mac.Sum(nil)) {
			return nil, bad
		}
	case j.PublicKey != nil && h.Alg == "RS256":
		sum := sha256.Sum256(signed)
		if rsa.VerifyPKCS1v15(j.PublicKey, crypto.SHA256, sum[:], sig) != nil {
			return nil, bad
		}
	default: // "none", another algorithm, or the other key's
		return nil, bad
	}
	var c jwtClaims
	if json.Unmarshal(pb, &c) != nil || c.Sub == "" || c.Exp == nil {
		return nil, bad
	}
	leeway := j.Leeway
	if leeway == 0 {
		leeway = time.Minute
	}
	if now.After(unixTime(*c.Exp).Add(leeway)) {
		return nil, bad
	}
	if c.Nbf != nil && now.Add(leeway).Before(unixTime(*c.Nbf)) {
		return nil, bad
	}
	if j.Issuer != "" && c.Iss != j.Issuer {
		return nil, bad
	}
	if j.Audience != "" && !audience(c.Aud, j.Audience) {
		return nil, bad
	}
	return &c, nil
}

// audience reports whether the aud claim (a string or a list) names want.
func audience(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// epochs are the times users were revoked, for tokens auth can't delete:
// JWTs issued before are refused. Per process.
type epochs struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (e *epochs) revoke(uid string, at time.Time) {
	e.mu.Lock()
	e.m[uid] = at
	e.mu.Unlock()
}

// revoked reports whether a token of uid issued at iat (zero: unknown) was
// revoked since.
func (e *epochs) revoked(uid string, iat time.Time) bool {
	e.mu.Lock()
	at, ok := e.m[uid]
	e.mu.Unlock()
	return ok && (iat.IsZero() || !iat.After(at))
}
