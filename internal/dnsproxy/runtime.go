package dnsproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/miekg/dns"
)

const (
	defaultMaxObservationsPerScope  = 256
	defaultMaxConnectionsPerSandbox = 1024
)

var ErrSandboxNotRegistered = errors.New("sandbox not registered")

// RecordType describes the observed DNS resource record family.
type RecordType string

const (
	RecordTypeA     RecordType = "A"
	RecordTypeAAAA  RecordType = "AAAA"
	RecordTypeCNAME RecordType = "CNAME"
)

// Protocol values describe the transport attached to a connection decision.
const (
	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
)

// RuntimeConfig controls cache bounds for the DNS policy runtime.
type RuntimeConfig struct {
	MaxObservationsPerScope  int
	MaxConnectionsPerSandbox int
}

// Observation is a single cached DNS answer scoped to a sandbox and source IP.
type Observation struct {
	SandboxID  string
	SourceIP   netip.Addr
	QueryName  string
	Name       string
	Type       RecordType
	Address    netip.Addr
	Target     string
	Names      []string
	TTL        time.Duration
	ObservedAt time.Time
	ExpiresAt  time.Time
}

// AllowedDestination is a concrete transport destination derived from current
// DNS observations plus sandbox allowlist policy.
type AllowedDestination struct {
	Protocol  string
	Address   netip.Addr
	Port      string
	ExpiresAt time.Time
}

// Connection identifies a network flow that is being authorised.
type Connection struct {
	SandboxID  string
	SourceIP   netip.Addr
	SourcePort uint16
	DestIP     netip.Addr
	DestPort   uint16
	Protocol   string
}

// Runtime stores sandbox policies, DNS observations, and established
// connection leases.
type Runtime struct {
	mu                       sync.Mutex
	maxObservationsPerScope  int
	maxConnectionsPerSandbox int
	sandboxes                map[string]*sandboxState
}

type sandboxState struct {
	policy          *policy.CompiledPolicy
	scopes          map[netip.Addr]*scopeState
	connections     map[connectionKey]struct{}
	connectionOrder []connectionKey
}

type scopeState struct {
	observations []Observation
}

type connectionKey struct {
	sourceIP   netip.Addr
	sourcePort uint16
	destIP     netip.Addr
	destPort   uint16
	protocol   string
}

type cnameRecord struct {
	target string
	ttl    time.Duration
}

type queryPath struct {
	query    string
	names    []string
	cnameTTL time.Duration
}

// NewRuntime creates a DNS policy runtime with bounded observation and
// connection caches.
func NewRuntime(cfg RuntimeConfig) *Runtime {
	maxObservations := cfg.MaxObservationsPerScope
	if maxObservations <= 0 {
		maxObservations = defaultMaxObservationsPerScope
	}
	maxConnections := cfg.MaxConnectionsPerSandbox
	if maxConnections <= 0 {
		maxConnections = defaultMaxConnectionsPerSandbox
	}
	return &Runtime{
		maxObservationsPerScope:  maxObservations,
		maxConnectionsPerSandbox: maxConnections,
		sandboxes:                make(map[string]*sandboxState),
	}
}

// RegisterSandbox adds a sandbox policy to the runtime.
func (r *Runtime) RegisterSandbox(sandboxID string, compiled *policy.CompiledPolicy) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return errors.New("sandbox id must not be empty")
	}
	if compiled == nil {
		return errors.New("compiled policy is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.sandboxes[sandboxID]; exists {
		return fmt.Errorf("sandbox %q already registered", sandboxID)
	}
	r.sandboxes[sandboxID] = &sandboxState{
		policy:      compiled,
		scopes:      make(map[netip.Addr]*scopeState),
		connections: make(map[connectionKey]struct{}),
	}
	return nil
}

// ClearSandbox removes a sandbox policy, observations, and active connections.
func (r *Runtime) ClearSandbox(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sandboxes, strings.TrimSpace(sandboxID))
}

// ObserveResponse records DNS answers for a sandbox and source IP.
func (r *Runtime) ObserveResponse(sandboxID string, sourceIP netip.Addr, msg *dns.Msg, now time.Time) error {
	sourceIP = normalizeAddr(sourceIP)
	if !sourceIP.IsValid() {
		return errors.New("source ip must be valid")
	}

	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[strings.TrimSpace(sandboxID)]
	if !ok {
		return ErrSandboxNotRegistered
	}

	scope := state.scopeFor(sourceIP)
	scope.pruneExpired(now)

	observations := observationsFromResponse(strings.TrimSpace(sandboxID), sourceIP, msg, now)
	if len(observations) == 0 {
		return nil
	}

	scope.observations = append(scope.observations, observations...)
	if len(scope.observations) > r.maxObservationsPerScope {
		scope.observations = append([]Observation(nil), scope.observations[len(scope.observations)-r.maxObservationsPerScope:]...)
	}

	return nil
}

// Observations returns the currently cached DNS answers for a sandbox.
func (r *Runtime) Observations(sandboxID string, now time.Time) []Observation {
	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[strings.TrimSpace(sandboxID)]
	if !ok {
		return nil
	}

	out := make([]Observation, 0)
	for _, sourceIP := range orderedScopeKeys(state.scopes) {
		scope := state.scopes[sourceIP]
		scope.pruneExpired(now)
		for _, observation := range scope.observations {
			out = append(out, cloneObservation(observation))
		}
	}

	slices.SortFunc(out, func(a, b Observation) int {
		if cmp := a.ObservedAt.Compare(b.ObservedAt); cmp != 0 {
			return cmp
		}
		if cmp := compareAddr(a.SourceIP, b.SourceIP); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.QueryName, b.QueryName); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// AllowedDestinations returns the currently permitted transport destinations
// for a sandbox/source pair after applying DNS observation TTLs and policy
// ports. The result is suitable for backend-specific firewall programming.
func (r *Runtime) AllowedDestinations(sandboxID string, sourceIP netip.Addr, now time.Time) []AllowedDestination {
	sourceIP = normalizeAddr(sourceIP)
	if !sourceIP.IsValid() {
		return nil
	}

	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[strings.TrimSpace(sandboxID)]
	if !ok {
		return nil
	}

	scope, ok := state.scopes[sourceIP]
	if !ok {
		return nil
	}
	scope.pruneExpired(now)

	type destinationKey struct {
		protocol string
		address  netip.Addr
		port     string
	}

	destinations := make(map[destinationKey]AllowedDestination)
	for _, observation := range scope.observations {
		if !observation.ExpiresAt.After(now) || !observation.Address.IsValid() {
			continue
		}
		for _, port := range state.observationAllowedPorts(observation) {
			portStr := strconv.Itoa(port)
			for _, protocol := range []string{ProtocolTCP, ProtocolUDP} {
				key := destinationKey{
					protocol: protocol,
					address:  observation.Address,
					port:     portStr,
				}
				candidate := AllowedDestination{
					Protocol:  protocol,
					Address:   observation.Address,
					Port:      portStr,
					ExpiresAt: observation.ExpiresAt,
				}
				existing, exists := destinations[key]
				if !exists || existing.ExpiresAt.Before(candidate.ExpiresAt) {
					destinations[key] = candidate
				}
			}
		}
	}

	out := make([]AllowedDestination, 0, len(destinations))
	for _, destination := range destinations {
		out = append(out, destination)
	}
	slices.SortFunc(out, func(a, b AllowedDestination) int {
		if cmp := compareAddr(a.Address, b.Address); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.Protocol, b.Protocol); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.Port, b.Port); cmp != 0 {
			return cmp
		}
		return a.ExpiresAt.Compare(b.ExpiresAt)
	})
	return out
}

// AllowConnection permits an already-established flow or authorises a new flow
// when there is a still-valid DNS observation that matches sandbox policy.
func (r *Runtime) AllowConnection(conn Connection, now time.Time) bool {
	conn.SourceIP = normalizeAddr(conn.SourceIP)
	conn.DestIP = normalizeAddr(conn.DestIP)
	if !conn.SourceIP.IsValid() || !conn.DestIP.IsValid() || conn.DestPort == 0 {
		return false
	}

	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[strings.TrimSpace(conn.SandboxID)]
	if !ok {
		return false
	}

	key := connectionKey{
		sourceIP:   conn.SourceIP,
		sourcePort: conn.SourcePort,
		destIP:     conn.DestIP,
		destPort:   conn.DestPort,
		protocol:   normalizeProtocol(conn.Protocol),
	}
	if _, exists := state.connections[key]; exists {
		state.touchConnection(key)
		return true
	}

	scope, ok := state.scopes[conn.SourceIP]
	if !ok {
		return false
	}
	scope.pruneExpired(now)

	for _, observation := range scope.observations {
		if !observation.ExpiresAt.After(now) {
			continue
		}
		if observation.Address != conn.DestIP {
			continue
		}
		if !state.observationAllowsPort(observation, int(conn.DestPort)) {
			continue
		}
		state.recordConnection(key, r.maxConnectionsPerSandbox)
		return true
	}

	return false
}

// NamesForAddress returns the DNS query names that resolved to the given
// address within a sandbox scope. It is intended for diagnostic logging of
// blocked connections.
func (r *Runtime) NamesForAddress(sandboxID string, sourceIP, destIP netip.Addr, now time.Time) []string {
	sourceIP = normalizeAddr(sourceIP)
	destIP = normalizeAddr(destIP)
	if !sourceIP.IsValid() || !destIP.IsValid() {
		return nil
	}
	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[strings.TrimSpace(sandboxID)]
	if !ok {
		return nil
	}
	scope, ok := state.scopes[sourceIP]
	if !ok {
		return nil
	}

	seen := map[string]struct{}{}
	var names []string
	for _, obs := range scope.observations {
		if !obs.ExpiresAt.After(now) || obs.Address != destIP {
			continue
		}
		name := obs.QueryName
		if name == "" {
			name = obs.Name
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// HostAllowedByPolicy checks whether the given host is in the network allow
// list for the sandbox.
func (r *Runtime) HostAllowedByPolicy(sandboxID, host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[strings.TrimSpace(sandboxID)]
	if !ok {
		return false
	}
	return state.policy.HostAllowed(host)
}

// ReleaseConnection removes an established flow from the runtime.
func (r *Runtime) ReleaseConnection(conn Connection) {
	conn.SourceIP = normalizeAddr(conn.SourceIP)
	conn.DestIP = normalizeAddr(conn.DestIP)

	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.sandboxes[strings.TrimSpace(conn.SandboxID)]
	if !ok {
		return
	}
	key := connectionKey{
		sourceIP:   conn.SourceIP,
		sourcePort: conn.SourcePort,
		destIP:     conn.DestIP,
		destPort:   conn.DestPort,
		protocol:   normalizeProtocol(conn.Protocol),
	}
	if _, exists := state.connections[key]; !exists {
		return
	}
	delete(state.connections, key)
	index := slices.Index(state.connectionOrder, key)
	if index >= 0 {
		state.connectionOrder = slices.Delete(state.connectionOrder, index, index+1)
	}
}

func (s *sandboxState) scopeFor(sourceIP netip.Addr) *scopeState {
	scope, ok := s.scopes[sourceIP]
	if ok {
		return scope
	}
	scope = &scopeState{}
	s.scopes[sourceIP] = scope
	return scope
}

func (s *sandboxState) observationAllowsPort(observation Observation, port int) bool {
	if s.policy == nil {
		return false
	}
	for _, name := range observation.Names {
		if s.policy.Allows(name, port) {
			return true
		}
	}
	if observation.Name != "" && s.policy.Allows(observation.Name, port) {
		return true
	}
	return false
}

func (s *sandboxState) observationAllowedPorts(observation Observation) []int {
	if s.policy == nil {
		return nil
	}

	ports := make([]int, 0)
	seen := make(map[int]struct{})
	for _, rule := range s.policy.Allow {
		for _, port := range rule.Ports {
			if _, exists := seen[port]; exists {
				continue
			}
			if !s.observationAllowsPort(observation, port) {
				continue
			}
			seen[port] = struct{}{}
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)
	return ports
}

func (s *sandboxState) recordConnection(key connectionKey, limit int) {
	if _, exists := s.connections[key]; exists {
		s.touchConnection(key)
		return
	}

	if len(s.connectionOrder) >= limit {
		evicted := s.connectionOrder[0]
		s.connectionOrder = s.connectionOrder[1:]
		delete(s.connections, evicted)
	}

	s.connections[key] = struct{}{}
	s.connectionOrder = append(s.connectionOrder, key)
}

func (s *sandboxState) touchConnection(key connectionKey) {
	index := slices.Index(s.connectionOrder, key)
	if index < 0 {
		return
	}
	s.connectionOrder = slices.Delete(s.connectionOrder, index, index+1)
	s.connectionOrder = append(s.connectionOrder, key)
}

func (s *scopeState) pruneExpired(now time.Time) {
	if len(s.observations) == 0 {
		return
	}

	filtered := s.observations[:0]
	for _, observation := range s.observations {
		if observation.ExpiresAt.After(now) {
			filtered = append(filtered, observation)
		}
	}
	s.observations = filtered
}

func observationsFromResponse(sandboxID string, sourceIP netip.Addr, msg *dns.Msg, now time.Time) []Observation {
	if msg == nil || msg.Rcode != dns.RcodeSuccess {
		return nil
	}

	questions := normalizedQuestions(msg.Question)
	if len(questions) == 0 {
		return nil
	}
	cnames := cnameRecords(msg.Answer)

	observations := make([]Observation, 0, len(msg.Answer))
	for _, rr := range msg.Answer {
		switch record := rr.(type) {
		case *dns.CNAME:
			owner := normalizeName(record.Hdr.Name)
			target := normalizeName(record.Target)
			if owner == "" || target == "" {
				continue
			}
			for _, path := range queryPaths(owner, questions, cnames) {
				ttl := ttlSeconds(record.Hdr.Ttl)
				observations = append(observations, Observation{
					SandboxID:  sandboxID,
					SourceIP:   sourceIP,
					QueryName:  path.query,
					Name:       owner,
					Type:       RecordTypeCNAME,
					Target:     target,
					Names:      append([]string(nil), path.names...),
					TTL:        ttl,
					ObservedAt: now,
					ExpiresAt:  now.Add(ttl),
				})
			}
		case *dns.A:
			addr, ok := netip.AddrFromSlice(record.A.To4())
			if !ok {
				continue
			}
			observations = append(observations, addressObservations(sandboxID, sourceIP, questions, cnames, normalizeName(record.Hdr.Name), RecordTypeA, addr, ttlSeconds(record.Hdr.Ttl), now)...)
		case *dns.AAAA:
			addr, ok := netip.AddrFromSlice(record.AAAA)
			if !ok {
				continue
			}
			if addr.Is4In6() {
				addr = addr.Unmap()
			}
			observations = append(observations, addressObservations(sandboxID, sourceIP, questions, cnames, normalizeName(record.Hdr.Name), RecordTypeAAAA, addr, ttlSeconds(record.Hdr.Ttl), now)...)
		}
	}

	return observations
}

func addressObservations(sandboxID string, sourceIP netip.Addr, questions []string, cnames map[string]cnameRecord, owner string, recordType RecordType, addr netip.Addr, answerTTL time.Duration, now time.Time) []Observation {
	if owner == "" || !addr.IsValid() {
		return nil
	}

	paths := queryPaths(owner, questions, cnames)
	observations := make([]Observation, 0, len(paths))
	for _, path := range paths {
		effectiveTTL := answerTTL
		if len(path.names) > 1 {
			effectiveTTL = minTTL(answerTTL, path.cnameTTL)
		}
		observations = append(observations, Observation{
			SandboxID:  sandboxID,
			SourceIP:   sourceIP,
			QueryName:  path.query,
			Name:       owner,
			Type:       recordType,
			Address:    addr,
			Names:      append([]string(nil), path.names...),
			TTL:        effectiveTTL,
			ObservedAt: now,
			ExpiresAt:  now.Add(effectiveTTL),
		})
	}
	return observations
}

func normalizedQuestions(questions []dns.Question) []string {
	out := make([]string, 0, len(questions))
	for _, question := range questions {
		name := normalizeName(question.Name)
		if name == "" {
			continue
		}
		if slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

func cnameRecords(answers []dns.RR) map[string]cnameRecord {
	out := make(map[string]cnameRecord)
	for _, rr := range answers {
		record, ok := rr.(*dns.CNAME)
		if !ok {
			continue
		}
		owner := normalizeName(record.Hdr.Name)
		target := normalizeName(record.Target)
		if owner == "" || target == "" {
			continue
		}
		out[owner] = cnameRecord{
			target: target,
			ttl:    ttlSeconds(record.Hdr.Ttl),
		}
	}
	return out
}

func queryPaths(owner string, questions []string, cnames map[string]cnameRecord) []queryPath {
	paths := make([]queryPath, 0, len(questions))
	for _, question := range questions {
		names, cnameTTL, ok := deriveNamePath(question, owner, cnames)
		if !ok {
			continue
		}
		paths = append(paths, queryPath{
			query:    question,
			names:    names,
			cnameTTL: cnameTTL,
		})
	}
	return paths
}

func deriveNamePath(question, owner string, cnames map[string]cnameRecord) ([]string, time.Duration, bool) {
	question = normalizeName(question)
	owner = normalizeName(owner)
	if question == "" || owner == "" {
		return nil, 0, false
	}
	if question == owner {
		return []string{question}, 0, true
	}

	seen := map[string]struct{}{question: {}}
	current := question
	names := []string{question}
	var ttlLimit time.Duration
	ttlLimitSet := false
	for i := 0; i < len(cnames)+1; i++ {
		record, ok := cnames[current]
		if !ok {
			return nil, 0, false
		}
		if !ttlLimitSet || record.ttl < ttlLimit {
			ttlLimit = record.ttl
			ttlLimitSet = true
		}
		current = record.target
		if _, duplicate := seen[current]; duplicate {
			return nil, 0, false
		}
		seen[current] = struct{}{}
		names = append(names, current)
		if current == owner {
			return names, ttlLimit, true
		}
	}
	return nil, 0, false
}

func orderedScopeKeys(scopes map[netip.Addr]*scopeState) []netip.Addr {
	keys := make([]netip.Addr, 0, len(scopes))
	for addr := range scopes {
		keys = append(keys, addr)
	}
	slices.SortFunc(keys, compareAddr)
	return keys
}

func cloneObservation(observation Observation) Observation {
	observation.Names = append([]string(nil), observation.Names...)
	return observation
}

func compareAddr(a, b netip.Addr) int {
	return a.Compare(b)
}

func normalizeAddr(addr netip.Addr) netip.Addr {
	if addr.Is4In6() {
		return addr.Unmap()
	}
	return addr
}

func normalizeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	return strings.TrimSuffix(name, ".")
}

func normalizeProtocol(protocol string) string {
	protocol = strings.TrimSpace(strings.ToLower(protocol))
	if protocol == "" {
		return ProtocolTCP
	}
	return protocol
}

func ttlSeconds(seconds uint32) time.Duration {
	return time.Duration(seconds) * time.Second
}

func minPositive(left, right time.Duration) time.Duration {
	switch {
	case left <= 0:
		return right
	case right <= 0:
		return left
	case left < right:
		return left
	default:
		return right
	}
}

func minTTL(left, right time.Duration) time.Duration {
	if right < left {
		return right
	}
	return left
}
