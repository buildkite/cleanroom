package authconfig

const (
	DefaultOIDCClockSkewSeconds        int64 = 60
	DefaultOIDCMaxTokenLifetimeSeconds int64 = 3600
)

type OIDCConfig struct {
	Issuers []OIDCIssuerConfig `yaml:"issuers,omitempty"`
}

type OIDCIssuerConfig struct {
	Name                    string            `yaml:"name,omitempty"`
	Issuer                  string            `yaml:"issuer"`
	Audiences               []string          `yaml:"audiences,omitempty"`
	JWKSURL                 string            `yaml:"jwks_url,omitempty"`
	RequiredClaims          map[string]string `yaml:"required_claims,omitempty"`
	AllowedAlgorithms       []string          `yaml:"allowed_algorithms,omitempty"`
	ClockSkewSeconds        int64             `yaml:"clock_skew_seconds,omitempty"`
	MaxTokenLifetimeSeconds int64             `yaml:"max_token_lifetime_seconds,omitempty"`
}

type Policy struct {
	Bindings []PolicyBinding `yaml:"bindings,omitempty"`
}

func (p Policy) Configured() bool {
	return len(p.Bindings) > 0
}

type PolicyBinding struct {
	Name      string            `yaml:"name"`
	When      string            `yaml:"when,omitempty"`
	Principal PrincipalTemplate `yaml:"principal"`
	Grants    []PolicyGrant     `yaml:"grants"`
}

type PrincipalTemplate struct {
	ID    string `yaml:"id"`
	Scope string `yaml:"scope,omitempty"`
}

type PolicyGrant struct {
	Name      string   `yaml:"name,omitempty"`
	Actions   []string `yaml:"actions"`
	Resources []string `yaml:"resources"`
	Condition string   `yaml:"condition,omitempty"`
}
