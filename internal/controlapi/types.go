package controlapi

type ExecRequest struct {
	CWD     string      `json:"cwd"`
	Backend string      `json:"backend,omitempty"`
	Command []string    `json:"command"`
	Options ExecOptions `json:"options"`
}

type LaunchCleanroomRequest struct {
	CWD     string                 `json:"cwd"`
	Backend string                 `json:"backend,omitempty"`
	Options LaunchCleanroomOptions `json:"options"`
}

type LaunchCleanroomOptions struct {
	LaunchSeconds int64 `json:"launch_seconds,omitempty"`
}

type LaunchCleanroomResponse struct {
	CleanroomID  string `json:"cleanroom_id"`
	Backend      string `json:"backend"`
	PolicySource string `json:"policy_source"`
	PolicyHash   string `json:"policy_hash"`
	Message      string `json:"message"`
}

type RunCleanroomRequest struct {
	CleanroomID string   `json:"cleanroom_id"`
	Command     []string `json:"command"`
}

type RunCleanroomResponse struct {
	CleanroomID string `json:"cleanroom_id"`
	RunID       string `json:"run_id"`
	ExitCode    int    `json:"exit_code"`
	LaunchedVM  bool   `json:"launched_vm"`
	PlanPath    string `json:"plan_path"`
	RunDir      string `json:"run_dir"`
	ImageRef    string `json:"image_ref,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	Message     string `json:"message"`
	Stdout      string `json:"stdout,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
}

type TerminateCleanroomRequest struct {
	CleanroomID string `json:"cleanroom_id"`
}

type TerminateCleanroomResponse struct {
	CleanroomID string `json:"cleanroom_id"`
	Terminated  bool   `json:"terminated"`
	Message     string `json:"message"`
}

type ExecOptions struct {
	LaunchSeconds int64 `json:"launch_seconds,omitempty"`
}

type ExecResponse struct {
	RunID        string `json:"run_id"`
	PolicySource string `json:"policy_source"`
	PolicyHash   string `json:"policy_hash"`
	ExitCode     int    `json:"exit_code"`
	LaunchedVM   bool   `json:"launched_vm"`
	PlanPath     string `json:"plan_path"`
	RunDir       string `json:"run_dir"`
	ImageRef     string `json:"image_ref,omitempty"`
	ImageDigest  string `json:"image_digest,omitempty"`
	Message      string `json:"message"`
	Stdout       string `json:"stdout,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
