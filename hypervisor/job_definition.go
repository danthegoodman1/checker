package hypervisor

import (
	"fmt"

	"github.com/danthegoodman1/checker/runtime"
)

// VersionLatest is a special version string that refers to the latest registered version.
const VersionLatest = "@latest"

// RetryPolicy defines how a job should be retried on failure.
type RetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts. 0 means no retries.
	MaxRetries int `json:"max_retries"`

	// RetryDelay is the duration to wait before retrying (e.g. "1s", "500ms").
	// If empty, retries immediately.
	RetryDelay string `json:"retry_delay"`
}

// JobDefinition represents a registered job definition.
// A job definition defines what to run (runtime type, configuration) and is referenced by name and version.
type JobDefinition struct {
	// Name is the unique name of the job definition.
	Name    string
	Version string

	// RuntimeType specifies which runtime to use (podman, etc.)
	RuntimeType runtime.RuntimeType

	// Config holds the runtime-specific configuration.
	// For Podman, this would be *podman.Config.
	// The hypervisor passes this to the runtime when starting a job.
	Config any

	// RetryPolicy defines retry behavior on failure. Nil means no retries.
	RetryPolicy *RetryPolicy

	// Metadata holds arbitrary key-value metadata for the job definition.
	Metadata map[string]string
}

// Validate checks if the job definition has all required fields set.
func (jd *JobDefinition) Validate() error {
	if jd.Name == "" {
		return fmt.Errorf("job definition name is required")
	}
	if jd.Version == "" {
		return fmt.Errorf("job definition version is required")
	}
	if jd.RuntimeType == "" {
		return fmt.Errorf("job definition runtime type is required")
	}
	if jd.Config == nil {
		return fmt.Errorf("job definition config is required")
	}
	return nil
}

// Key returns a unique key for this job definition (name:version).
func (jd *JobDefinition) Key() string {
	return fmt.Sprintf("%s:%s", jd.Name, jd.Version)
}

// Clone creates a deep copy of the job definition.
func (jd *JobDefinition) Clone() *JobDefinition {
	clone := *jd

	if jd.Metadata != nil {
		clone.Metadata = make(map[string]string, len(jd.Metadata))
		for k, v := range jd.Metadata {
			clone.Metadata[k] = v
		}
	}

	// Note: Config is not deep copied as it's opaque to the hypervisor.
	// Runtimes should treat config as immutable.

	return &clone
}

// JobDefinitionRegistry manages registered job definitions.
// This is used internally by the Hypervisor.
type JobDefinitionRegistry struct {
	// definitions maps job name -> version -> JobDefinition
	definitions map[string]map[string]*JobDefinition

	// latestVersions tracks the latest version for each job name
	latestVersions map[string]string
}

// NewJobDefinitionRegistry creates a new job definition registry.
func NewJobDefinitionRegistry() *JobDefinitionRegistry {
	return &JobDefinitionRegistry{
		definitions:    make(map[string]map[string]*JobDefinition),
		latestVersions: make(map[string]string),
	}
}

// Register adds a job definition to the registry.
// If a job definition with the same name and version exists, it will be overwritten.
func (r *JobDefinitionRegistry) Register(jd *JobDefinition) error {
	if err := jd.Validate(); err != nil {
		return fmt.Errorf("invalid job definition: %w", err)
	}

	if r.definitions[jd.Name] == nil {
		r.definitions[jd.Name] = make(map[string]*JobDefinition)
	}

	r.definitions[jd.Name][jd.Version] = jd.Clone()

	// Update latest version tracking
	// Simple string comparison - assumes semantic versioning or similar
	if r.latestVersions[jd.Name] == "" || jd.Version > r.latestVersions[jd.Name] {
		r.latestVersions[jd.Name] = jd.Version
	}

	return nil
}

// Get retrieves a job definition by name and version.
// Use VersionLatest to get the latest version.
func (r *JobDefinitionRegistry) Get(name, version string) (*JobDefinition, error) {
	versions, exists := r.definitions[name]
	if !exists {
		return nil, fmt.Errorf("job definition %q not found", name)
	}

	// Handle @latest
	if version == VersionLatest {
		version = r.latestVersions[name]
		if version == "" {
			return nil, fmt.Errorf("no versions registered for job definition %q", name)
		}
	}

	jd, exists := versions[version]
	if !exists {
		return nil, fmt.Errorf("job definition %q version %q not found", name, version)
	}

	return jd.Clone(), nil
}

// List returns all registered job definitions.
func (r *JobDefinitionRegistry) List() []*JobDefinition {
	var result []*JobDefinition
	for _, versions := range r.definitions {
		for _, jd := range versions {
			result = append(result, jd.Clone())
		}
	}
	return result
}

// ListVersions returns all versions of a job definition by name.
func (r *JobDefinitionRegistry) ListVersions(name string) ([]*JobDefinition, error) {
	versions, exists := r.definitions[name]
	if !exists {
		return nil, fmt.Errorf("job definition %q not found", name)
	}

	var result []*JobDefinition
	for _, jd := range versions {
		result = append(result, jd.Clone())
	}
	return result, nil
}

// Unregister removes a job definition from the registry.
func (r *JobDefinitionRegistry) Unregister(name, version string) error {
	versions, exists := r.definitions[name]
	if !exists {
		return fmt.Errorf("job definition %q not found", name)
	}

	if _, exists := versions[version]; !exists {
		return fmt.Errorf("job definition %q version %q not found", name, version)
	}

	delete(versions, version)

	// Clean up if no versions left
	if len(versions) == 0 {
		delete(r.definitions, name)
		delete(r.latestVersions, name)
	} else {
		// Recalculate latest version
		r.latestVersions[name] = ""
		for v := range versions {
			if r.latestVersions[name] == "" || v > r.latestVersions[name] {
				r.latestVersions[name] = v
			}
		}
	}

	return nil
}
