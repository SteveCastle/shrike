package deps

import (
	"context"
	"fmt"
	"sync"

	"github.com/stevecastle/shrike/jobqueue"
)

// DependencyStatus represents the current state of a dependency.
type DependencyStatus string

const (
	StatusNotInstalled DependencyStatus = "not_installed"
	StatusInstalled    DependencyStatus = "installed"
	StatusOutdated     DependencyStatus = "outdated"
	StatusDownloading  DependencyStatus = "downloading"
)

// Dependency represents an external dependency that can be checked and downloaded.
type Dependency struct {
	ID            string
	Name          string
	Description   string
	TargetDir     string // Base directory for installation
	LatestVersion string
	DownloadURL   string
	ExpectedSize  int64

	// Check function verifies if dependency exists and returns its version
	Check func(ctx context.Context) (exists bool, version string, err error)

	// Download function downloads and installs the dependency
	Download func(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error
}

// DependencyRegistry stores all registered dependencies.
type DependencyRegistry map[string]*Dependency

var (
	registry DependencyRegistry
	mu       sync.RWMutex
)

func init() {
	registry = make(DependencyRegistry)
}

// Register adds a dependency to the global registry.
func Register(dep *Dependency) {
	mu.Lock()
	defer mu.Unlock()
	registry[dep.ID] = dep
}

// GetAll returns all registered dependencies.
func GetAll() []*Dependency {
	mu.RLock()
	defer mu.RUnlock()

	deps := make([]*Dependency, 0, len(registry))
	for _, d := range registry {
		deps = append(deps, d)
	}
	return deps
}

// Get retrieves a dependency by its ID.
func Get(id string) (*Dependency, bool) {
	mu.RLock()
	defer mu.RUnlock()

	dep, ok := registry[id]
	return dep, ok
}

// EnsureAvailable checks if a dependency is available.
// If not available, it returns an error indicating the dependency is missing.
// Users must manually download dependencies via the /dependencies UI.
func EnsureAvailable(ctx context.Context, q *jobqueue.Queue, depID string) error {
	dep, ok := Get(depID)
	if !ok {
		return fmt.Errorf("unknown dependency: %s", depID)
	}

	// Check if dependency exists
	exists, _, err := dep.Check(ctx)
	if err != nil {
		return fmt.Errorf("failed to check dependency %s: %w", depID, err)
	}

	if !exists {
		return fmt.Errorf("dependency %s is not installed. Please download it from the Dependencies page", dep.Name)
	}

	return nil
}
