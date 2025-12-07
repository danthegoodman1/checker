package runtime

import (
	"context"
	"io"
)

// RuntimeType identifies the type of runtime (nodejs, docker, firecracker, etc.)
type RuntimeType string

const (
	RuntimeTypeNodeJS RuntimeType = "nodejs"
	RuntimeTypeDocker RuntimeType = "docker"
	// Future runtime types:
	// RuntimeTypeFirecracker RuntimeType = "firecracker"
	// RuntimeTypeBubblewrap RuntimeType = "bubblewrap"
)

// Checkpoint holds the data needed to restore a process.
// Each runtime implementation defines what this contains (file paths, serialized state, etc.)
type Checkpoint interface {
	// String returns a human-readable identifier for logging/debugging
	String() string

	// GracePeriodMs returns the time in milliseconds that the worker should wait
	// after receiving the checkpoint response before the runtime will stop the process.
	// This allows the worker to be idle (no open connections, no progress) when stopped.
	// Returns 0 if no grace period is needed.
	GracePeriodMs() int64
}

// Process represents a running execution with its own state.
// Each runtime implementation defines what this contains (PID, container ID, etc.)
type Process interface {
	// String returns a human-readable identifier for logging/debugging
	String() string

	// Checkpoint captures the current state of the execution.
	// If keepRunning is true, the process continues after checkpointing.
	// If keepRunning is false, the process is suspended after checkpointing.
	// Returns the checkpoint data needed to restore this process.
	Checkpoint(ctx context.Context, keepRunning bool) (Checkpoint, error)

	// Kill terminates the execution immediately.
	Kill(ctx context.Context) error

	// Wait blocks until the process exits and returns the exit code.
	// Returns an error if the process cannot be waited on.
	Wait(ctx context.Context) (exitCode int, err error)
}

// StartOptions contains parameters for starting a new process.
type StartOptions struct {
	// ExecutionID is a unique identifier for this execution (e.g., job ID).
	ExecutionID string

	// Env contains environment variables to pass to the process.
	// These are user/job-defined variables, not internal runtime config.
	Env map[string]string

	// Config is runtime-specific configuration (e.g., *nodejs.Config, *docker.Config).
	Config any

	// APIHostAddress is the host:port where the runtime API is listening.
	// The runtime is responsible for making this accessible to the process
	// (e.g., Docker will rewrite 127.0.0.1 to host.docker.internal).
	APIHostAddress string

	// Stdout is where process stdout will be written. If nil, stdout is discarded.
	Stdout io.Writer

	// Stderr is where process stderr will be written. If nil, stderr is discarded.
	Stderr io.Writer
}

// RestoreOptions contains parameters for restoring a process from a checkpoint.
type RestoreOptions struct {
	// Checkpoint contains the data needed to restore the process.
	Checkpoint Checkpoint

	// Stdout is where process stdout will be written. If nil, stdout is discarded.
	Stdout io.Writer

	// Stderr is where process stderr will be written. If nil, stderr is discarded.
	Stderr io.Writer
}

// Runtime is a factory that creates Process instances.
// Each runtime type (NodeJS, Docker, etc.) implements this interface, with
// platform-specific implementations (Linux with CRIU, macOS with no-ops, etc.)
type Runtime interface {
	// Type returns the runtime type this implementation handles
	Type() RuntimeType

	// ParseConfig parses runtime-specific config from JSON.
	ParseConfig(raw []byte) (any, error)

	// Start launches a new execution with the given options.
	// Returns a Process that can be used for subsequent operations.
	Start(ctx context.Context, opts StartOptions) (Process, error)

	// Restore resumes an execution from a checkpoint.
	// Returns a new Process representing the restored execution.
	Restore(ctx context.Context, opts RestoreOptions) (Process, error)

	// CheckpointGracePeriodMs returns the grace period in milliseconds that workers
	// should wait after receiving a checkpoint response before the runtime stops the process.
	// This allows the worker to be idle (no open connections) when stopped.
	// Returns 0 if no grace period is needed (e.g., Linux with CRIU).
	CheckpointGracePeriodMs() int64
}
