//go:build darwin

package firecracker

import (
	"context"
	"fmt"

	"github.com/danthegoodman1/checker/runtime"
)

// Runtime is not supported on macOS - Firecracker requires Linux with KVM.
type Runtime struct{}

// NewRuntime returns an error on macOS as Firecracker is not supported.
func NewRuntime() (*Runtime, error) {
	return nil, fmt.Errorf("firecracker is not supported on macOS (requires Linux with KVM)")
}

func (r *Runtime) Type() runtime.RuntimeType {
	return runtime.RuntimeTypeFirecracker
}

func (r *Runtime) ParseConfig(raw []byte) (any, error) {
	return nil, fmt.Errorf("firecracker not supported on macOS")
}

func (r *Runtime) Start(ctx context.Context, opts runtime.StartOptions) (runtime.Process, error) {
	return nil, fmt.Errorf("firecracker not supported on macOS")
}

func (r *Runtime) Restore(ctx context.Context, opts runtime.RestoreOptions) (runtime.Process, error) {
	return nil, fmt.Errorf("firecracker not supported on macOS")
}

func (r *Runtime) ReconstructCheckpoint(checkpointPath string, executionID string, env map[string]string, config any, apiHostAddress string) (runtime.Checkpoint, error) {
	return nil, fmt.Errorf("firecracker not supported on macOS")
}

func (r *Runtime) Close() error {
	return nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)
