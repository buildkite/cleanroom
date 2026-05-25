package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type AuthCommand struct {
	Check AuthCheckCommand `cmd:"" help:"Check local auth policy for a token and request"`
}

type AuthCheckCommand struct {
	Config           string `help:"Runtime config path (default: $XDG_CONFIG_HOME/cleanroom/config.yaml)"`
	PolicyFile       string `name:"policy-file" help:"Auth policy path (default: auth.policy_file from runtime config)"`
	TokenFile        string `name:"token-file" required:"" help:"Path to OIDC JWT token file, or '-' for stdin"`
	Action           string `required:"" help:"Cleanroom action to check, for example sandbox.create"`
	Resource         string `help:"Resource kind (default: derived from action prefix)"`
	ResourceID       string `name:"resource-id" help:"Existing resource ID for get, stream, file, or snapshot checks"`
	OwnerPrincipalID string `name:"owner-principal-id" help:"Existing resource owner principal ID"`
	OwnerScope       string `name:"owner-scope" help:"Existing resource owner scope"`
	Request          string `help:"JSON request fixture path (default: empty object)"`
	JSON             bool   `help:"Print decision as JSON"`
}

func (c *AuthCheckCommand) Run(ctx *runtimeContext) error {
	if strings.TrimSpace(c.Action) == "" {
		return errors.New("--action is required")
	}
	configPath, err := resolveRuntimeConfigPath(ctx.CWD, c.Config)
	if err != nil {
		return err
	}
	if st, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("runtime config does not exist at %s", configPath)
		}
		return fmt.Errorf("stat %s: %w", configPath, err)
	} else if st.IsDir() {
		return fmt.Errorf("runtime config path %s is a directory", configPath)
	}

	cfg, resolvedConfigPath, err := runtimeconfig.LoadPath(configPath)
	if err != nil {
		return err
	}
	policyPath, err := resolveAuthPolicyPath(ctx.CWD, resolvedConfigPath, c.PolicyFile, cfg.Auth.PolicyFile)
	if err != nil {
		return err
	}
	policy, err := authz.LoadPolicyFile(policyPath)
	if err != nil {
		return err
	}
	validator, err := authz.NewOIDCValidator(cfg.Auth.OIDC.Issuers)
	if err != nil {
		return err
	}

	tokenString, err := readAuthToken(ctx.CWD, c.TokenFile)
	if err != nil {
		return err
	}
	token, err := validator.Validate(context.Background(), tokenString)
	if err != nil {
		return err
	}
	bound, err := policy.Bind(token)
	if err != nil {
		if errors.Is(err, authz.ErrNoBinding) {
			decision := authz.Decision{
				Allowed: false,
				Action:  strings.TrimSpace(c.Action),
				Resource: authz.Resource{
					Kind: resourceKindForAction(c.Action, c.Resource),
					ID:   strings.TrimSpace(c.ResourceID),
				},
				Reason: authz.ReasonNoBinding,
			}
			return writeAuthCheckDecision(ctx, decision, c.JSON)
		}
		return err
	}
	request, err := readAuthCheckRequest(ctx.CWD, c.Request)
	if err != nil {
		return err
	}
	decision := bound.Authorize(authz.DecisionRequest{
		Action: strings.TrimSpace(c.Action),
		Resource: authz.Resource{
			Kind: resourceKindForAction(c.Action, c.Resource),
			ID:   strings.TrimSpace(c.ResourceID),
			Owner: authz.ResourceOwner{
				PrincipalID: strings.TrimSpace(c.OwnerPrincipalID),
				Scope:       strings.TrimSpace(c.OwnerScope),
			},
		},
		Request: request,
	})
	return writeAuthCheckDecision(ctx, decision, c.JSON)
}

func writeAuthCheckDecision(ctx *runtimeContext, decision authz.Decision, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(stdout(ctx))
		enc.SetIndent("", "  ")
		if err := enc.Encode(decision); err != nil {
			return err
		}
	} else {
		status := "auth check denied"
		if decision.Allowed {
			status = "auth check allowed"
		}
		var out strings.Builder
		out.WriteString(renderStatusValueLine(status, decision.Reason, defaultTerminalPalette().info, shouldUseANSI(stdout(ctx))))
		out.WriteByte('\n')
		out.WriteString(renderKeyValueLine("", "action", decision.Action, shouldUseANSI(stdout(ctx)), defaultTerminalPalette()))
		out.WriteByte('\n')
		out.WriteString(renderKeyValueLine("", "resource", decision.Resource.Kind, shouldUseANSI(stdout(ctx)), defaultTerminalPalette()))
		out.WriteByte('\n')
		if decision.Principal.ID != "" {
			out.WriteString(renderKeyValueLine("", "principal", decision.Principal.ID, shouldUseANSI(stdout(ctx)), defaultTerminalPalette()))
			out.WriteByte('\n')
		}
		if decision.Binding != "" {
			out.WriteString(renderKeyValueLine("", "binding", decision.Binding, shouldUseANSI(stdout(ctx)), defaultTerminalPalette()))
			out.WriteByte('\n')
		}
		if decision.Grant != "" {
			out.WriteString(renderKeyValueLine("", "grant", decision.Grant, shouldUseANSI(stdout(ctx)), defaultTerminalPalette()))
			out.WriteByte('\n')
		}
		if _, err := fmt.Fprint(stdout(ctx), out.String()); err != nil {
			return err
		}
	}
	if !decision.Allowed {
		return exitCodeError{code: 1}
	}
	return nil
}

func resolveAuthPolicyPath(cwd, configPath, explicit, configured string) (string, error) {
	path := strings.TrimSpace(explicit)
	if path == "" {
		path = strings.TrimSpace(configured)
	}
	if path == "" {
		return "", errors.New("auth policy file is required (set --policy-file or auth.policy_file)")
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			if err != nil {
				return "", err
			}
			return "", errors.New("home directory is not available")
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if strings.TrimSpace(explicit) != "" {
		return filepath.Join(cwd, path), nil
	}
	return filepath.Join(filepath.Dir(configPath), path), nil
}

func readAuthToken(cwd, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("--token-file is required")
	}
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
	} else {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		raw, err = os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file %s: %w", path, err)
		}
	}
	return strings.TrimSpace(string(raw)), nil
}

func readAuthCheckRequest(cwd, path string) (map[string]any, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return map[string]any{}, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open request fixture %s: %w", path, err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	var request map[string]any
	if err := dec.Decode(&request); err != nil {
		return nil, fmt.Errorf("decode request fixture %s: %w", path, err)
	}
	return normalizeJSONNumbers(request).(map[string]any), nil
}

func normalizeJSONNumbers(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = normalizeJSONNumbers(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeJSONNumbers(item)
		}
		return out
	case json.Number:
		if i, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(v.String(), 64); err == nil {
			return f
		}
		return v.String()
	default:
		return value
	}
}

func resourceKindForAction(action, explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	action = strings.TrimSpace(action)
	switch {
	case strings.HasPrefix(action, "sandbox."):
		return "sandbox"
	case strings.HasPrefix(action, "execution."):
		return "execution"
	case strings.HasPrefix(action, "snapshot."):
		return "snapshot"
	case strings.HasPrefix(action, "cache_peer."):
		return "cache_peer"
	default:
		if idx := strings.IndexByte(action, '.'); idx > 0 {
			return action[:idx]
		}
		return action
	}
}
