package authz

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/authconfig"
	"github.com/golang-jwt/jwt/v5"
)

const defaultJWKSCacheMaxAge = 5 * time.Minute

var errUnsupportedJWK = errors.New("unsupported jwk")

type OIDCValidator struct {
	issuersByURL map[string]*trustedOIDCIssuer
	httpClient   *http.Client
	now          func() time.Time
}

type trustedOIDCIssuer struct {
	Name             string
	Issuer           string
	Audiences        []string
	JWKSURL          string
	RequiredClaims   map[string]string
	AllowedAlgs      []string
	ClockSkew        time.Duration
	MaxTokenLifetime time.Duration
	keys             *jwksCache
}

type OIDCValidatorOption func(*OIDCValidator)

func WithHTTPClient(client *http.Client) OIDCValidatorOption {
	return func(v *OIDCValidator) {
		if client != nil {
			v.httpClient = client
		}
	}
}

func WithNow(now func() time.Time) OIDCValidatorOption {
	return func(v *OIDCValidator) {
		if now != nil {
			v.now = now
		}
	}
}

func NewOIDCValidator(issuers []authconfig.OIDCIssuerConfig, opts ...OIDCValidatorOption) (*OIDCValidator, error) {
	v := &OIDCValidator{
		issuersByURL: map[string]*trustedOIDCIssuer{},
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		now: time.Now,
	}
	for _, opt := range opts {
		opt(v)
	}

	for i, cfg := range issuers {
		issuer := strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
		if issuer == "" {
			return nil, fmt.Errorf("issuer[%d].issuer is required", i)
		}
		if _, exists := v.issuersByURL[issuer]; exists {
			return nil, fmt.Errorf("duplicate trusted issuer %q", issuer)
		}
		name := strings.TrimSpace(cfg.Name)
		if name == "" {
			name = issuer
		}
		allowedAlgs := trimStrings(cfg.AllowedAlgorithms)
		if len(allowedAlgs) == 0 {
			allowedAlgs = []string{"RS256"}
		}
		for _, alg := range allowedAlgs {
			if alg != "RS256" {
				return nil, fmt.Errorf("issuer[%d].allowed_algorithms contains unsupported algorithm %q", i, alg)
			}
		}
		audiences := trimStrings(cfg.Audiences)
		if len(audiences) == 0 {
			return nil, fmt.Errorf("issuer[%d].audiences must contain at least one audience", i)
		}
		requiredClaims, err := normalizeRequiredClaims(cfg.RequiredClaims, fmt.Sprintf("issuer[%d].required_claims", i))
		if err != nil {
			return nil, err
		}
		clockSkew := time.Duration(cfg.ClockSkewSeconds) * time.Second
		if clockSkew < 0 {
			return nil, fmt.Errorf("issuer[%d].clock_skew_seconds must be non-negative", i)
		}
		if clockSkew == 0 {
			clockSkew = time.Duration(authconfig.DefaultOIDCClockSkewSeconds) * time.Second
		}
		maxLifetime := time.Duration(cfg.MaxTokenLifetimeSeconds) * time.Second
		if maxLifetime < 0 {
			return nil, fmt.Errorf("issuer[%d].max_token_lifetime_seconds must be non-negative", i)
		}
		if maxLifetime == 0 {
			maxLifetime = time.Duration(authconfig.DefaultOIDCMaxTokenLifetimeSeconds) * time.Second
		}
		trusted := &trustedOIDCIssuer{
			Name:             name,
			Issuer:           issuer,
			Audiences:        audiences,
			JWKSURL:          strings.TrimSpace(cfg.JWKSURL),
			RequiredClaims:   requiredClaims,
			AllowedAlgs:      allowedAlgs,
			ClockSkew:        clockSkew,
			MaxTokenLifetime: maxLifetime,
		}
		trusted.keys = &jwksCache{
			url:        trusted.JWKSURL,
			httpClient: v.httpClient,
			keys:       map[string]any{},
			now:        v.now,
			maxAge:     defaultJWKSCacheMaxAge,
		}
		v.issuersByURL[issuer] = trusted
	}
	if len(v.issuersByURL) == 0 {
		return nil, errors.New("at least one trusted issuer is required")
	}
	return v, nil
}

func (v *OIDCValidator) Validate(ctx context.Context, tokenString string) (ValidatedToken, error) {
	tokenString = strings.TrimSpace(tokenString)
	if tokenString == "" {
		return ValidatedToken{}, errors.New("token is empty")
	}

	untrustedClaims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser(jwt.WithoutClaimsValidation()).ParseUnverified(tokenString, untrustedClaims); err != nil {
		return ValidatedToken{}, fmt.Errorf("parse token: %w", err)
	}
	issuerURL, err := untrustedClaims.GetIssuer()
	if err != nil || strings.TrimSpace(issuerURL) == "" {
		return ValidatedToken{}, errors.New("token issuer is required")
	}
	trusted, ok := v.issuersByURL[strings.TrimRight(strings.TrimSpace(issuerURL), "/")]
	if !ok {
		return ValidatedToken{}, fmt.Errorf("untrusted issuer %q", issuerURL)
	}

	parsedToken, claims, err := v.parseToken(ctx, tokenString, trusted)
	if errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		if refreshErr := trusted.keys.refresh(ctx); refreshErr == nil {
			parsedToken, claims, err = v.parseToken(ctx, tokenString, trusted)
		}
	}
	if err != nil {
		return ValidatedToken{}, fmt.Errorf("validate token: %w", err)
	}
	if parsedToken == nil || !parsedToken.Valid {
		return ValidatedToken{}, errors.New("token is invalid")
	}

	subject, err := claims.GetSubject()
	if err != nil || strings.TrimSpace(subject) == "" {
		return ValidatedToken{}, errors.New("token subject is required")
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return ValidatedToken{}, errors.New("token expiration is required")
	}
	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		return ValidatedToken{}, errors.New("token issued-at time is required")
	}
	if exp.Time.Sub(iat.Time) > trusted.MaxTokenLifetime {
		return ValidatedToken{}, fmt.Errorf("token lifetime %s exceeds maximum %s", exp.Time.Sub(iat.Time), trusted.MaxTokenLifetime)
	}
	if err := validateRequiredClaims(claims, trusted.RequiredClaims); err != nil {
		return ValidatedToken{}, err
	}

	return ValidatedToken{
		IssuerName: trusted.Name,
		Issuer:     trusted.Issuer,
		Subject:    subject,
		Claims:     copyMapStringAny(claims),
		ExpiresAt:  exp.Time,
		IssuedAt:   iat.Time,
	}, nil
}

func (v *OIDCValidator) parseToken(ctx context.Context, tokenString string, trusted *trustedOIDCIssuer) (*jwt.Token, jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods(trusted.AllowedAlgs),
		jwt.WithIssuer(trusted.Issuer),
		jwt.WithAudience(trusted.Audiences...),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(trusted.ClockSkew),
		jwt.WithTimeFunc(v.now),
	)
	parsedToken, err := parser.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		kid = strings.TrimSpace(kid)
		if kid == "" {
			return nil, errors.New("token kid header is required")
		}
		return trusted.keys.key(ctx, kid)
	})
	return parsedToken, claims, err
}

func validateRequiredClaims(claims jwt.MapClaims, required map[string]string) error {
	for name, want := range required {
		got, ok := claims[name]
		if !ok {
			return fmt.Errorf("required claim %q is missing", name)
		}
		gotString, ok := got.(string)
		if !ok || gotString != want {
			return fmt.Errorf("required claim %q does not match", name)
		}
	}
	return nil
}

type jwksCache struct {
	url        string
	httpClient *http.Client

	mu   sync.Mutex
	keys map[string]any
	at   time.Time
	now  func() time.Time

	maxAge time.Duration
}

func (c *jwksCache) key(ctx context.Context, kid string) (any, error) {
	c.mu.Lock()
	key, ok := c.keys[kid]
	fresh := c.maxAge <= 0 || (!c.at.IsZero() && c.now().Sub(c.at) < c.maxAge)
	c.mu.Unlock()
	if ok && fresh {
		return key, nil
	}

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	key, ok = c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("jwks key %q not found", kid)
	}
	return key, nil
}

func (c *jwksCache) refresh(ctx context.Context) error {
	if strings.TrimSpace(c.url) == "" {
		return errors.New("jwks_url is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("create jwks request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}

	var payload jwksPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := map[string]any{}
	for _, jwk := range payload.Keys {
		key, err := jwk.publicKey()
		if err != nil {
			if errors.Is(err, errUnsupportedJWK) {
				continue
			}
			return err
		}
		if strings.TrimSpace(jwk.KID) == "" {
			continue
		}
		keys[jwk.KID] = key
	}
	if len(keys) == 0 {
		return errors.New("jwks contained no supported RSA signing keys")
	}

	c.mu.Lock()
	c.keys = keys
	c.at = c.now()
	c.mu.Unlock()
	return nil
}

type jwksPayload struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	Alg string `json:"alg,omitempty"`
	Use string `json:"use,omitempty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwkKey) publicKey() (any, error) {
	if k.KTY != "RSA" {
		return nil, fmt.Errorf("%w: key type %q", errUnsupportedJWK, k.KTY)
	}
	if k.Use != "" && k.Use != "sig" {
		return nil, fmt.Errorf("%w: key use %q", errUnsupportedJWK, k.Use)
	}
	if k.Alg != "" && k.Alg != "RS256" {
		return nil, fmt.Errorf("%w: key algorithm %q", errUnsupportedJWK, k.Alg)
	}
	n, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode jwks modulus: %w", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode jwks exponent: %w", err)
	}
	exponent := new(big.Int).SetBytes(e)
	if !exponent.IsInt64() {
		return nil, errors.New("jwks exponent overflows int64")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(n),
		E: int(exponent.Int64()),
	}, nil
}

func trimStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func normalizeRequiredClaims(values map[string]string, label string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s contains an empty claim name", label)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s[%q] must not be empty", label, key)
		}
		if _, ok := out[key]; ok {
			return nil, fmt.Errorf("%s contains duplicate claim name %q after trimming whitespace", label, key)
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func copyMapStringAny(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
