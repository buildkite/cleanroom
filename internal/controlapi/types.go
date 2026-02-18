package controlapi

type ExecRequest struct {
	CWD     string      `json:"cwd"`
	Backend string      `json:"backend,omitempty"`
	Command []string    `json:"command"`
	Options ExecOptions `json:"options"`
}

type ExecOptions struct {
	RunDir            string `json:"run_dir,omitempty"`
	ReadOnlyWorkspace bool   `json:"read_only_workspace,omitempty"`
	DryRun            bool   `json:"dry_run,omitempty"`
	HostPassthrough   bool   `json:"host_passthrough,omitempty"`
	LaunchSeconds     int64  `json:"launch_seconds,omitempty"`
}

type ExecResponse struct {
	RunID        string `json:"run_id"`
	PolicySource string `json:"policy_source"`
	PolicyHash   string `json:"policy_hash"`
	ExitCode     int    `json:"exit_code"`
	LaunchedVM   bool   `json:"launched_vm"`
	PlanPath     string `json:"plan_path"`
	RunDir       string `json:"run_dir"`
	Message      string `json:"message"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
