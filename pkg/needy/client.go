/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package needy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	needyv1 "github.com/nekomeowww/rc/apis/sdk/go/needy/v1"
)

// RunSpec is what rc sends to create a run.
type RunSpec struct {
	// Name addresses the run in needy's CLI; rc keeps the returned ID.
	Name        string
	ProfileName string
	ProfileYAML string
	Args        []string
	Terminal    bool
	// WaitForRunner lets the create wait for the actor runner Pod to connect.
	WaitForRunner time.Duration
}

// Run is the part of needy's run record that rc publishes.
type Run struct {
	ID           string
	State        needyv1.RunState
	ActorState   needyv1.ActorState
	ExitCode     *int32
	Failure      string
	LastActivity time.Time
}

// IsTerminal reports whether the run has ended.
func (run Run) IsTerminal() bool {
	switch run.State {
	case needyv1.RunState_RUN_STATE_FINISHED, needyv1.RunState_RUN_STATE_STOPPED, needyv1.RunState_RUN_STATE_FAILED:
		return true
	default:
		return false
	}
}

// Daemon is the part of the needy daemon's API that the manager uses.
type Daemon interface {
	// CheckPlacement refuses a profile that would place any part on the
	// daemon's own Pod, or that needy finds invalid.
	CheckPlacement(ctx context.Context, profileYAML string) error
	// IsActorRunnerConnected reports whether a runner has connected and offers
	// Firecracker actors. needy learns a runner's capabilities only when it
	// connects, so placement is checked after that. A freshly minted runner
	// already reads as online, so the status alone does not tell.
	IsActorRunnerConnected(ctx context.Context, name string) (bool, error)
	// IsExecutorRunnerConnected reports whether a runner has connected and
	// offers at least one executor driver.
	IsExecutorRunnerConnected(ctx context.Context, name string) (bool, error)
	// MintRunnerCredential mints a credential that lets a runner named name
	// declare only role.
	MintRunnerCredential(ctx context.Context, name string, role Role, ttl time.Duration) (string, error)
	RevokeRunnerCredential(ctx context.Context, name string) error
	CreateRun(ctx context.Context, spec RunSpec) (Run, error)
	GetRun(ctx context.Context, id string) (Run, error)
	StopRun(ctx context.Context, id string, grace time.Duration) error
	Close() error
}

// Dialer connects to a daemon's control address (HOST:PORT) with a client
// token.
type Dialer interface {
	Dial(ctx context.Context, address, token string) (Daemon, error)
}

// ErrUnauthenticated means the daemon refused the client token, for example
// after it lost its state.
var ErrUnauthenticated = errors.New("needy daemon refused the client token")

// ErrNotFound means the daemon has no such run or runner.
var ErrNotFound = errors.New("needy daemon has no such object")

// GRPCDialer dials a daemon over h2c. needy's listeners have no TLS; a
// NetworkPolicy or a mesh must protect the token in transit.
type GRPCDialer struct{}

// Dial implements Dialer.
func (GRPCDialer) Dial(_ context.Context, address, token string) (Daemon, error) {
	connection, err := grpc.NewClient("dns:///"+address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(token)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial needy daemon %s: %w", address, err)
	}

	return &grpcDaemon{
		connection: connection,
		runs:       needyv1.NewRunServiceClient(connection),
		runners:    needyv1.NewRunnerServiceClient(connection),
	}, nil
}

type bearer string

func (token bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(token)}, nil
}

func (bearer) RequireTransportSecurity() bool {
	return false
}

type grpcDaemon struct {
	connection *grpc.ClientConn
	runs       needyv1.RunServiceClient
	runners    needyv1.RunnerServiceClient
}

func (daemon *grpcDaemon) CheckPlacement(ctx context.Context, profileYAML string) error {
	response, err := daemon.runs.CheckProfile(ctx, &needyv1.CheckProfileRequest{ProfileYaml: profileYAML})
	if err != nil {
		return fmt.Errorf("check profile: %w", classify(err))
	}
	var problems []string
	for _, finding := range response.GetFindings() {
		if finding.GetSeverity() == needyv1.FindingSeverity_FINDING_SEVERITY_ERROR {
			problems = append(problems, finding.GetMessage())
		}
	}
	// An unresolved runner leaves the placement unset; an empty runner is the
	// daemon itself.
	if response.GetActor().GetRunner() == "" {
		problems = append(problems, "the actor is not placed on its actor runner")
	}
	for name, placement := range response.GetExecutors() {
		if placement.GetRunner() == "" {
			problems = append(problems, fmt.Sprintf("executor %s would run on the daemon", name))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("profile cannot be placed: %s", strings.Join(problems, "; "))
	}

	return nil
}

func (daemon *grpcDaemon) IsActorRunnerConnected(ctx context.Context, name string) (bool, error) {
	return daemon.isConnected(ctx, name, func(capabilities *needyv1.RunnerCapabilities) bool {
		return slices.Contains(capabilities.GetActor(), needyv1.ActorDriver_ACTOR_DRIVER_LINUX_FIRECRACKER)
	})
}

func (daemon *grpcDaemon) IsExecutorRunnerConnected(ctx context.Context, name string) (bool, error) {
	return daemon.isConnected(ctx, name, func(capabilities *needyv1.RunnerCapabilities) bool {
		return len(capabilities.GetExecutor()) > 0
	})
}

// isConnected reports whether the runner is online and its capabilities,
// which needy learns only when the runner connects, satisfy offers.
func (daemon *grpcDaemon) isConnected(
	ctx context.Context,
	name string,
	offers func(*needyv1.RunnerCapabilities) bool,
) (bool, error) {
	response, err := daemon.runners.ListRunners(ctx, &needyv1.ListRunnersRequest{})
	if err != nil {
		return false, fmt.Errorf("list runners: %w", classify(err))
	}
	for _, runner := range response.GetRunners() {
		if runner.GetName() != name || runner.GetStatus() != needyv1.RunnerStatus_RUNNER_STATUS_ONLINE {
			continue
		}

		return offers(runner.GetCapabilities()), nil
	}

	return false, nil
}

func (daemon *grpcDaemon) MintRunnerCredential(
	ctx context.Context,
	name string,
	role Role,
	ttl time.Duration,
) (string, error) {
	roles := map[Role]needyv1.RunnerRole{
		RoleActor:    needyv1.RunnerRole_RUNNER_ROLE_ACTOR,
		RoleExecutor: needyv1.RunnerRole_RUNNER_ROLE_EXECUTOR,
	}
	wire, known := roles[role]
	if !known {
		// An empty roles list would allow every role.
		return "", fmt.Errorf("mint runner credential %s: unknown role %q", name, role)
	}
	response, err := daemon.runners.MintRunnerCredential(ctx, &needyv1.MintRunnerCredentialRequest{
		Name:  name,
		TtlMs: ttl.Milliseconds(),
		Roles: []needyv1.RunnerRole{wire},
	})
	if err != nil {
		return "", fmt.Errorf("mint runner credential %s: %w", name, classify(err))
	}

	return response.GetCredential(), nil
}

func (daemon *grpcDaemon) RevokeRunnerCredential(ctx context.Context, name string) error {
	request := &needyv1.RevokeRunnerCredentialRequest{Name: name}
	if _, err := daemon.runners.RevokeRunnerCredential(ctx, request); err != nil {
		return fmt.Errorf("revoke runner credential %s: %w", name, classify(err))
	}

	return nil
}

func (daemon *grpcDaemon) CreateRun(ctx context.Context, spec RunSpec) (Run, error) {
	request := &needyv1.CreateRunRequest{
		ProfileYaml:     spec.ProfileYAML,
		ProfileName:     spec.ProfileName,
		Name:            spec.Name,
		ActorArgs:       spec.Args,
		WaitForRunnerMs: uint64(spec.WaitForRunner.Milliseconds()),
	}
	if spec.Terminal {
		request.Tty = &needyv1.WindowSize{
			Rows: 24,
			Cols: 80,
		}
	}
	response, err := daemon.runs.CreateRun(ctx, request)
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", classify(err))
	}

	return runOf(response.GetRun()), nil
}

func (daemon *grpcDaemon) GetRun(ctx context.Context, id string) (Run, error) {
	response, err := daemon.runs.GetRun(ctx, &needyv1.GetRunRequest{RunId: id})
	if err != nil {
		return Run{}, fmt.Errorf("get run %s: %w", id, classify(err))
	}

	return runOf(response.GetRun()), nil
}

func (daemon *grpcDaemon) StopRun(ctx context.Context, id string, grace time.Duration) error {
	graceMs := grace.Milliseconds()
	if _, err := daemon.runs.StopRun(ctx, &needyv1.StopRunRequest{RunId: id, GraceMs: &graceMs}); err != nil {
		return fmt.Errorf("stop run %s: %w", id, classify(err))
	}

	return nil
}

func (daemon *grpcDaemon) Close() error {
	return daemon.connection.Close()
}

func runOf(run *needyv1.Run) Run {
	result := Run{
		ID:         run.GetRunId(),
		State:      run.GetState(),
		ActorState: run.GetActorState(),
		Failure:    run.GetFailure(),
	}
	if exit := run.GetActorExit(); exit != nil && exit.Code != nil {
		code := exit.GetCode()
		result.ExitCode = &code
	}
	if milliseconds := run.GetLastActivityMs(); milliseconds > 0 {
		result.LastActivity = time.UnixMilli(milliseconds)
	}

	return result
}

func classify(err error) error {
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	case codes.NotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	default:
		return err
	}
}
