// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package backend encapsulates all extensibility points required to fully implement a new cloud provider.
package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/pkg/apitype"
	"github.com/pulumi/pulumi/pkg/backend/display"
	"github.com/pulumi/pulumi/pkg/diag"
	"github.com/pulumi/pulumi/pkg/engine"
	"github.com/pulumi/pulumi/pkg/operations"
	"github.com/pulumi/pulumi/pkg/resource"
	"github.com/pulumi/pulumi/pkg/resource/config"
	"github.com/pulumi/pulumi/pkg/resource/deploy"
	"github.com/pulumi/pulumi/pkg/resource/stack"
	"github.com/pulumi/pulumi/pkg/secrets"
	"github.com/pulumi/pulumi/pkg/tokens"
	"github.com/pulumi/pulumi/pkg/util/cancel"
	"github.com/pulumi/pulumi/pkg/util/result"
	"github.com/pulumi/pulumi/pkg/workspace"
)

var (
	// ErrNoPreviousDeployment is returned when there isn't a previous deployment.
	ErrNoPreviousDeployment = errors.New("no previous deployment")
)

// StackAlreadyExistsError is returned from CreateStack when the stack already exists in the backend.
type StackAlreadyExistsError struct {
	StackName string
}

func (e StackAlreadyExistsError) Error() string {
	return fmt.Sprintf("stack '%v' already exists", e.StackName)
}

// StackReference is an opaque type that refers to a stack managed by a backend.  The CLI uses the ParseStackReference
// method to turn a string like "my-great-stack" or "pulumi/my-great-stack" into a stack reference that can be used to
// interact with the stack via the backend. Stack references are specific to a given backend and different back ends
// may interpret the string passed to ParseStackReference differently
type StackReference interface {
	// fmt.Stringer's String() method returns a string of the stack identity, suitable for display in the CLI
	fmt.Stringer
	// Name is the name that will be passed to the Pulumi engine when preforming operations on this stack. This
	// name may not uniquely identify the stack (e.g. the cloud backend embeds owner information in the StackReference
	// but that informaion is not part of the StackName() we pass to the engine.
	Name() tokens.QName
}

// PolicyPackReference is an opaque type that refers to a PolicyPack managedby a backend. The CLI
// uses the ParsePolicyPackReference method to turn a string like "myOrg/mySecurityRules" into a
// PolicyPackReference that can be used to interact with the PolicyPack via the backend.
// PolicyPackReferences are specific to a given backend and different back ends may interpret the
// string passed to ParsePolicyPackReference differently.
type PolicyPackReference interface {
	// fmt.Stringer's String() method returns a string of the stack identity, suitable for display in the CLI
	fmt.Stringer
	// OrgName is the name of the organization that is managing the PolicyPack.
	OrgName() string
	// Name is the name of the PolicyPack being referenced.
	Name() tokens.QName
}

// StackSummary provides a basic description of a stack, without the ability to inspect its resources or make changes.
type StackSummary interface {
	Name() StackReference

	// LastUpdate returns when the stack was last updated, as applicable.
	LastUpdate() *time.Time
	// ResourceCount returns the stack's resource count, as applicable.
	ResourceCount() *int
}

// Backend is an interface that represents actions the engine will interact with to manage stacks of cloud resources.
// It can be implemented any number of ways to provide pluggable backend implementations of the Pulumi Cloud.
type Backend interface {
	// Name returns a friendly name for this backend.
	Name() string
	// URL returns a URL at which information about this backend may be seen.
	URL() string

	// GetPolicyPack returns a PolicyPack object tied to this backend, or nil if it cannot be found.
	GetPolicyPack(ctx context.Context, policyPack string, d diag.Sink) (PolicyPack, error)

	// ParseStackReference takes a string representation and parses it to a reference which may be used for other
	// methods in this backend.
	ParseStackReference(s string) (StackReference, error)

	// GetStack returns a stack object tied to this backend with the given name, or nil if it cannot be found.
	GetStack(ctx context.Context, stackRef StackReference) (Stack, error)
	// CreateStack creates a new stack with the given name and options that are specific to the backend provider.
	CreateStack(ctx context.Context, stackRef StackReference, opts interface{}) (Stack, error)
	// RemoveStack removes a stack with the given name.  If force is true, the stack will be removed even if it
	// still contains resources.  Otherwise, if the stack contains resources, a non-nil error is returned, and the
	// first boolean return value will be set to true.
	RemoveStack(ctx context.Context, stackRef StackReference, force bool) (bool, error)
	// ListStacks returns a list of stack summaries for all known stacks in the target backend.
	ListStacks(ctx context.Context, projectFilter *tokens.PackageName) ([]StackSummary, error)

	RenameStack(ctx context.Context, stackRef StackReference, newName tokens.QName) error

	// Preview shows what would be updated given the current workspace's contents.
	Preview(ctx context.Context, stackRef StackReference, op UpdateOperation) (engine.ResourceChanges, result.Result)
	// Update updates the target stack with the current workspace's contents (config and code).
	Update(ctx context.Context, stackRef StackReference, op UpdateOperation) (engine.ResourceChanges, result.Result)
	// Refresh refreshes the stack's state from the cloud provider.
	Refresh(ctx context.Context, stackRef StackReference, op UpdateOperation) (engine.ResourceChanges, result.Result)
	// Destroy destroys all of this stack's resources.
	Destroy(ctx context.Context, stackRef StackReference, op UpdateOperation) (engine.ResourceChanges, result.Result)

	// Query against the resource outputs in a stack's state checkpoint.
	Query(ctx context.Context, op QueryOperation) result.Result

	// GetHistory returns all updates for the stack. The returned UpdateInfo slice will be in
	// descending order (newest first).
	GetHistory(ctx context.Context, stackRef StackReference) ([]UpdateInfo, error)
	// GetLogs fetches a list of log entries for the given stack, with optional filtering/querying.
	GetLogs(ctx context.Context, stackRef StackReference, cfg StackConfiguration,
		query operations.LogQuery) ([]operations.LogEntry, error)
	// Get the configuration from the most recent deployment of the stack.
	GetLatestConfiguration(ctx context.Context, stackRef StackReference) (config.Map, error)

	// GetStackTags fetches the stack's existing tags.
	GetStackTags(ctx context.Context, stackRef StackReference) (map[apitype.StackTagName]string, error)
	// UpdateStackTags updates the stacks's tags, replacing all existing tags.
	UpdateStackTags(ctx context.Context, stackRef StackReference, tags map[apitype.StackTagName]string) error

	// ExportDeployment exports the deployment for the given stack as an opaque JSON message.
	ExportDeployment(ctx context.Context, stackRef StackReference) (*apitype.UntypedDeployment, error)
	// ImportDeployment imports the given deployment into the indicated stack.
	ImportDeployment(ctx context.Context, stackRef StackReference, deployment *apitype.UntypedDeployment) error
	// Logout logs you out of the backend and removes any stored credentials.
	Logout() error
	// Returns the identity of the current user for the backend.
	CurrentUser() (string, error)
}

// UpdateOperation is a complete stack update operation (preview, update, refresh, or destroy).
type UpdateOperation struct {
	Proj               *workspace.Project
	Root               string
	M                  *UpdateMetadata
	Opts               UpdateOptions
	SecretsManager     secrets.Manager
	StackConfiguration StackConfiguration
	Scopes             CancellationScopeSource
}

// QueryOperation configures a query operation.
type QueryOperation struct {
	Proj               *workspace.Project
	Root               string
	Opts               UpdateOptions
	SecretsManager     secrets.Manager
	StackConfiguration StackConfiguration
	Scopes             CancellationScopeSource
}

// StackConfiguration holds the configuration for a stack and it's associated decrypter.
type StackConfiguration struct {
	Config    config.Map
	Decrypter config.Decrypter
}

// UpdateOptions is the full set of update options, including backend and engine options.
type UpdateOptions struct {
	// Engine contains all of the engine-specific options.
	Engine engine.UpdateOptions
	// Display contains all of the backend display options.
	Display display.Options

	// AutoApprove, when true, will automatically approve previews.
	AutoApprove bool
	// SkipPreview, when true, causes the preview step to be skipped.
	SkipPreview bool
}

// QueryOptions configures a query to operate against a backend and the engine.
type QueryOptions struct {
	// Engine contains all of the engine-specific options.
	Engine engine.UpdateOptions
	// Display contains all of the backend display options.
	Display display.Options
}

// CancellationScope provides a scoped source of cancellation and termination requests.
type CancellationScope interface {
	// Context returns the cancellation context used to observe cancellation and termination requests for this scope.
	Context() *cancel.Context
	// Close closes the cancellation scope.
	Close()
}

// CancellationScopeSource provides a source for cancellation scopes.
type CancellationScopeSource interface {
	// NewScope creates a new cancellation scope.
	NewScope(events chan<- engine.Event, isPreview bool) CancellationScope
}

// NewBackendClient returns a deploy.BackendClient that wraps the given Backend.
func NewBackendClient(backend Backend) deploy.BackendClient {
	return &backendClient{backend: backend}
}

type backendClient struct {
	backend Backend
}

// GetStackOutputs returns the outputs of the stack with the given name.
func (c *backendClient) GetStackOutputs(ctx context.Context, name string) (resource.PropertyMap, error) {
	ref, err := c.backend.ParseStackReference(name)
	if err != nil {
		return nil, err
	}
	s, err := c.backend.GetStack(ctx, ref)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.Errorf("unknown stack %q", name)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	res, err := stack.GetRootStackResource(snap)
	if err != nil {
		return nil, errors.Wrap(err, "getting root stack resources")
	}
	if res == nil {
		return resource.PropertyMap{}, nil
	}
	return res.Outputs, nil
}

func (c *backendClient) GetStackResourceOutputs(
	ctx context.Context, name string) (resource.PropertyMap, error) {
	ref, err := c.backend.ParseStackReference(name)
	if err != nil {
		return nil, err
	}
	s, err := c.backend.GetStack(ctx, ref)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.Errorf("unknown stack %q", name)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	pm := resource.PropertyMap{}
	for _, r := range snap.Resources {
		if r.Delete {
			continue
		}

		resc := resource.PropertyMap{
			resource.PropertyKey("type"):    resource.NewStringProperty(string(r.Type)),
			resource.PropertyKey("outputs"): resource.NewObjectProperty(r.Outputs)}
		pm[resource.PropertyKey(r.URN)] = resource.NewObjectProperty(resc)
	}
	return pm, nil
}
