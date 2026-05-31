package authz

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/interpreter"
)

const celRuntimeCostLimit uint64 = 10_000

type compiledBoolExpression struct {
	source  string
	program cel.Program
}

func compileBoolExpression(source string, rules celPathRules) (*compiledBoolExpression, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return &compiledBoolExpression{}, nil
	}
	if err := validateCELPaths(source, rules); err != nil {
		return nil, err
	}
	env, err := cel.NewEnv(
		cel.Variable("token", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("claims", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("principal", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("action", cel.StringType),
		cel.Variable("resource", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("expression must return bool, got %s", ast.OutputType())
	}
	program, err := env.Program(ast, cel.CostLimit(celRuntimeCostLimit), cel.EvalOptions(cel.OptPartialEval))
	if err != nil {
		return nil, err
	}
	return &compiledBoolExpression{
		source:  source,
		program: program,
	}, nil
}

func (e *compiledBoolExpression) eval(vars map[string]any) (bool, error) {
	if e == nil || strings.TrimSpace(e.source) == "" {
		return true, nil
	}
	val, _, err := e.program.Eval(vars)
	if err != nil {
		return false, err
	}
	native, ok := val.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression returned %T, expected bool", val.Value())
	}
	return native, nil
}

func (e *compiledBoolExpression) evalPartial(vars map[string]any, unknownPaths ...string) (bool, bool, error) {
	if e == nil || strings.TrimSpace(e.source) == "" {
		return true, true, nil
	}
	unknowns := make([]*interpreter.AttributePattern, 0, len(unknownPaths))
	for _, path := range unknownPaths {
		if pattern := celAttributePattern(path); pattern != nil {
			unknowns = append(unknowns, pattern)
		}
	}
	activation, err := cel.PartialVars(vars, unknowns...)
	if err != nil {
		return false, false, err
	}
	val, _, err := e.program.Eval(activation)
	if err != nil {
		return false, false, err
	}
	if types.IsUnknown(val) {
		return false, false, nil
	}
	native, ok := val.Value().(bool)
	if !ok {
		return false, false, fmt.Errorf("expression returned %T, expected bool", val.Value())
	}
	return native, true, nil
}

func (e *compiledBoolExpression) repositorySourceAuthorized(vars map[string]any, unknownPaths ...string) bool {
	if e == nil || strings.TrimSpace(e.source) == "" {
		return true
	}
	for _, disjunct := range sourceDisjuncts(e.source) {
		disjunctExpr, err := compileBoolExpression(disjunct, grantCELRules())
		if err != nil {
			continue
		}
		ok, known, err := disjunctExpr.evalPartial(vars, unknownPaths...)
		if err != nil || (known && !ok) {
			continue
		}
		for _, conjunct := range sourceConjuncts(disjunct) {
			if !referencesCELPath(conjunct, "request.repository.remote_url") {
				continue
			}
			compiled, err := compileBoolExpression(conjunct, grantCELRules())
			if err != nil {
				continue
			}
			ok, known, err := compiled.evalPartial(vars, unknownPaths...)
			if err == nil && known && ok {
				return true
			}
		}
	}
	return false
}

func celAttributePattern(path string) *interpreter.AttributePattern {
	parts := strings.Split(strings.TrimSpace(path), ".")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return nil
	}
	pattern := cel.AttributePattern(parts[0])
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil
		}
		pattern = pattern.QualString(part)
	}
	return pattern
}

func sourceDisjuncts(source string) []string {
	source = stripOuterParens(strings.TrimSpace(source))
	parts := splitTopLevelOr(source)
	if len(parts) == 1 && parts[0] == source {
		return []string{source}
	}
	var out []string
	for _, part := range parts {
		out = append(out, sourceDisjuncts(part)...)
	}
	return out
}

func sourceConjuncts(source string) []string {
	source = stripOuterParens(strings.TrimSpace(source))
	parts := splitTopLevelAnd(source)
	if len(parts) == 1 && parts[0] == source {
		return []string{source}
	}
	var out []string
	for _, part := range parts {
		out = append(out, sourceConjuncts(part)...)
	}
	return out
}

func splitTopLevelOr(source string) []string {
	return splitTopLevelOperator(source, '|')
}

func splitTopLevelAnd(source string) []string {
	return splitTopLevelOperator(source, '&')
}

func splitTopLevelOperator(source string, op byte) []string {
	var parts []string
	start := 0
	depth := 0
	for i := 0; i < len(source); {
		switch source[i] {
		case '\'', '"':
			next, err := skipQuoted(source, i)
			if err != nil {
				return []string{strings.TrimSpace(source)}
			}
			i = next
			continue
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case op:
			if depth == 0 && i+1 < len(source) && source[i+1] == op {
				parts = append(parts, strings.TrimSpace(source[start:i]))
				i += 2
				start = i
				continue
			}
		}
		i++
	}
	parts = append(parts, strings.TrimSpace(source[start:]))
	return parts
}

func stripOuterParens(source string) string {
	for {
		source = strings.TrimSpace(source)
		if len(source) < 2 || source[0] != '(' || source[len(source)-1] != ')' {
			return source
		}
		depth := 0
		wrapped := true
		for i := 0; i < len(source); {
			switch source[i] {
			case '\'', '"':
				next, err := skipQuoted(source, i)
				if err != nil {
					return source
				}
				i = next
				continue
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 && i != len(source)-1 {
					wrapped = false
				}
			}
			i++
		}
		if !wrapped || depth != 0 {
			return source
		}
		source = source[1 : len(source)-1]
	}
}

func referencesCELPath(source, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for i := 0; i < len(source); {
		r := rune(source[i])
		if source[i] == '"' || source[i] == '\'' {
			next, err := skipQuoted(source, i)
			if err != nil {
				return false
			}
			i = next
			continue
		}
		if !isIdentStart(r) || (i > 0 && source[i-1] == '.') {
			i++
			continue
		}
		path, next := readPath(source, i)
		if path == target || strings.HasPrefix(path, target+".") {
			return true
		}
		i = next
	}
	return false
}

type celPathRules struct {
	allowedExact       map[string]struct{}
	allowedDynamicRoot map[string]struct{}
}

func bindingCELRules() celPathRules {
	return celPathRules{
		allowedExact: map[string]struct{}{
			"token.issuer":  {},
			"token.subject": {},
		},
		allowedDynamicRoot: map[string]struct{}{
			"claims": {},
		},
	}
}

func grantCELRules() celPathRules {
	return celPathRules{
		allowedExact: map[string]struct{}{
			"action":                                   {},
			"principal.id":                             {},
			"principal.issuer":                         {},
			"principal.subject":                        {},
			"principal.scope":                          {},
			"resource.kind":                            {},
			"resource.id":                              {},
			"resource.owner.principal_id":              {},
			"resource.owner.scope":                     {},
			"request.backend":                          {},
			"request.repository.remote_url":            {},
			"request.repository.commit":                {},
			"request.repository.branch":                {},
			"request.repository.changeset.present":     {},
			"request.repository.changeset.digest":      {},
			"request.repository.changeset.tree_digest": {},
			"request.image.ref":                        {},
			"request.image.digest":                     {},
			"request.snapshot.id":                      {},
			"request.policy.resources.vcpus":           {},
			"request.policy.resources.memory_bytes":    {},
			"request.policy.resources.disk_bytes":      {},
			"request.policy.docker.required":           {},
			"request.policy.network_default":           {},
			"request.policy.network.hosts":             {},
			"request.policy.network.ports":             {},
			"request.cache.reuse":                      {},
		},
		allowedDynamicRoot: map[string]struct{}{
			"claims": {},
		},
	}
}

var celKeywords = map[string]struct{}{
	"true":  {},
	"false": {},
	"null":  {},
	"in":    {},
}

var celComprehensionVarRE = regexp.MustCompile(`\.(?:exists|exists_one|all|map|filter)\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*,`)

func validateCELPaths(source string, rules celPathRules) error {
	localRoots := collectCELLocalRoots(source)
	for i := 0; i < len(source); {
		r := rune(source[i])
		if source[i] == '"' || source[i] == '\'' {
			next, err := skipQuoted(source, i)
			if err != nil {
				return err
			}
			i = next
			continue
		}
		if !isIdentStart(r) || (i > 0 && source[i-1] == '.') {
			i++
			continue
		}

		path, next := readPath(source, i)
		i = next
		if path == "" {
			continue
		}
		if _, ok := celKeywords[path]; ok {
			continue
		}
		if _, ok := rules.allowedExact[path]; ok {
			continue
		}
		root := strings.Split(path, ".")[0]
		if _, ok := localRoots[root]; ok {
			continue
		}
		if _, ok := rules.allowedDynamicRoot[root]; ok {
			continue
		}
		if next < len(source) && source[next] == '(' {
			if base, ok := methodBase(path); ok && celPathAllowed(base, rules, localRoots) {
				continue
			}
			if _, ok := methodBase(path); !ok {
				continue
			}
		}
		return fmt.Errorf("unknown CEL field %q", path)
	}
	return nil
}

func collectCELLocalRoots(source string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, match := range celComprehensionVarRE.FindAllStringSubmatch(source, -1) {
		if len(match) == 2 {
			out[match[1]] = struct{}{}
		}
	}
	return out
}

func methodBase(path string) (string, bool) {
	idx := strings.LastIndexByte(path, '.')
	if idx <= 0 {
		return "", false
	}
	return path[:idx], true
}

func celPathAllowed(path string, rules celPathRules, localRoots map[string]struct{}) bool {
	if _, ok := rules.allowedExact[path]; ok {
		return true
	}
	root := strings.Split(path, ".")[0]
	if _, ok := localRoots[root]; ok {
		return true
	}
	if _, ok := rules.allowedDynamicRoot[root]; ok {
		return true
	}
	return false
}

func skipQuoted(source string, start int) (int, error) {
	quote := source[start]
	escaped := false
	for i := start + 1; i < len(source); i++ {
		switch {
		case escaped:
			escaped = false
		case source[i] == '\\':
			escaped = true
		case source[i] == quote:
			return i + 1, nil
		}
	}
	return len(source), errors.New("unterminated string literal")
}

func readPath(source string, start int) (string, int) {
	var parts []string
	i := start
	for {
		if i >= len(source) || !isIdentStart(rune(source[i])) {
			break
		}
		partStart := i
		i++
		for i < len(source) && isIdentPart(rune(source[i])) {
			i++
		}
		parts = append(parts, source[partStart:i])
		if i >= len(source) || source[i] != '.' || i+1 >= len(source) || !isIdentStart(rune(source[i+1])) {
			break
		}
		i++
	}
	return strings.Join(parts, "."), i
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
