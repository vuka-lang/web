package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// TokenPrefix starts every bearer token auth issues, so a leaked one is
// recognisable (secret scanners, logs).
const TokenPrefix = "vka_"

// Token is a bearer token as a TokenStore keeps it: the SHA-256 of the
// token, never the token itself, which only its holder has.
type Token struct {
	UserID  string
	Name    string // what it is for: "cli", "ci"
	Created time.Time
	Expires time.Time
}

// TokenStore keeps bearer tokens by key, the hash of the token. Get returns
// nil and no error for an unknown key. DeleteUser deletes every token of a
// user.
type TokenStore interface {
	Get(ctx context.Context, key string) (*Token, error)
	Save(ctx context.Context, key string, t Token) error
	Delete(ctx context.Context, key string) error
	DeleteUser(ctx context.Context, uid string) error
}

// MemoryTokens is the default TokenStore: a map in the process. Its tokens
// stop working when the process ends — after a restart or a deploy every
// client needs a new one — and replicas don't share them. Use it for
// development and tests; otherwise implement TokenStore over the app's
// database.
type MemoryTokens struct {
	mu    sync.Mutex
	m     map[string]Token
	saves int
}

// NewMemoryTokens is an empty MemoryTokens.
func NewMemoryTokens() *MemoryTokens { return &MemoryTokens{m: map[string]Token{}} }

func (s *MemoryTokens) Get(_ context.Context, key string) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return nil, nil
	}
	return &v, nil
}

func (s *MemoryTokens) Save(_ context.Context, key string, t Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = t
	if s.saves++; s.saves%1000 == 0 {
		now := time.Now()
		for k, v := range s.m {
			if now.After(v.Expires) {
				delete(s.m, k)
			}
		}
	}
	return nil
}

func (s *MemoryTokens) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func (s *MemoryTokens) DeleteUser(_ context.Context, uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if v.UserID == uid {
			delete(s.m, k)
		}
	}
	return nil
}

// Issue makes a bearer token for user uid, lasting ttl (Config.TokenTTL
// when 0). The token is returned once: the store keeps only its hash.
func (a *Auth) Issue(ctx context.Context, uid, name string, ttl time.Duration) (string, Token, error) {
	if ttl <= 0 {
		ttl = a.cfg.TokenTTL
	}
	secret, now := newSecret(), time.Now()
	tok := TokenPrefix + secret
	t := Token{UserID: uid, Name: name, Created: now, Expires: now.Add(ttl)}
	if err := a.tokens.Save(ctx, storeKey(tok), t); err != nil {
		return "", Token{}, err
	}
	return tok, t, nil
}

// IssueToken makes a bearer token for user uid, as Auth.Issue does, for the
// app of ctx.
func IssueToken(ctx context.Context, uid, name string, ttl time.Duration) (string, error) {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return "", errNoAuth
	}
	tok, _, err := st.auth.Issue(ctx, uid, name, ttl)
	return tok, err
}

// Revoke ends a bearer token: requests carrying it are refused from now on.
func (a *Auth) Revoke(ctx context.Context, token string) error {
	if !wellFormedToken(token) {
		return errors.New("auth: not a token")
	}
	return a.tokens.Delete(ctx, storeKey(token))
}

// Revoke ends a bearer token, as Auth.Revoke does, for the app of ctx.
func Revoke(ctx context.Context, token string) error {
	st := stateOf(ctx)
	if st == nil || st.auth == nil {
		return errNoAuth
	}
	return st.auth.Revoke(ctx, token)
}

func wellFormedToken(tok string) bool {
	s, ok := strings.CutPrefix(tok, TokenPrefix)
	return ok && wellFormed(s)
}

// verifyBearer signs the request in with an opaque bearer token; one that
// doesn't verify is an error, not an anonymous request.
func (a *Auth) verifyBearer(ctx context.Context, st *state, tok string) error {
	if !wellFormedToken(tok) {
		return errBadCredentials
	}
	key := storeKey(tok)
	t, err := a.tokens.Get(ctx, key)
	if err != nil {
		return err
	}
	if t == nil || time.Now().After(t.Expires) {
		return errBadCredentials
	}
	id, err := a.load(ctx, t.UserID)
	if err != nil {
		return err
	}
	if id == nil {
		return errBadCredentials
	}
	st.id, st.via, st.key = id, viaBearer, key
	return nil
}
