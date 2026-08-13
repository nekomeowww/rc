---
name: use-uber-fx
description: Apply consistent Uber Fx dependency-injection patterns in Go projects. Use when adding, refactoring, or reviewing fx Modules(), constructor callbacks, fx.In/fx.Out structs, fx.Provide, fx.Invoke run entrypoints, Lifecycle hooks, command wiring, named/grouped providers, or tests for packages that use go.uber.org/fx.
---

# Use Uber Fx

## Operating Model

Use `go.uber.org/fx` as the application wiring layer. Keep business behavior in ordinary Go structs and methods. Keep graph registration in predictable `Modules()` functions.

When editing an fx codebase, first identify three layers:

1. Leaf packages: constructors and runtime objects live here.
2. Aggregator packages: `Modules()` wires leaf packages and child aggregators.
3. Command packages: `cmd/*/main.go` selects broad modules and invokes app-level run modes.

Do not hide database queries, network calls, worker behavior, or domain logic inside fx constructors unless construction genuinely requires it. Constructors build objects. `RunXxx` starts runtime loops. Methods do the business work.

## Package Layout

Every aggregation package should have a same-name `.go` file with one exported `Modules()` function.

Example:

```text
internal/
  datastore/
    datastore.go      // func Modules() fx.Option
    ent.go            // func NewEnt() ...
    redis.go          // func NewRedis() ...
  grpc/
    services/
      services.go     // func Modules() fx.Option
      users/
        users.go      // func Modules() fx.Option
      billing/
        billing.go    // func Modules() fx.Option
```

The aggregator owns registration for its direct children:

```go
package services

import "go.uber.org/fx"

func Modules() fx.Option {
	return fx.Options(
		fx.Options(users.Modules()),
		fx.Options(billing.Modules()),
		fx.Provide(NewRegister()),
	)
}
```

Apply this recursively. When adding a new package, update the closest parent aggregator. Avoid wiring leaf packages directly in a distant command package unless the leaf package is truly command-specific.

## Naming

Use names that show the layer and role:

- Domain model packages should generally be plural when they represent a collection or orchestration surface: `tasks`, `accounts`, `bilibili`, `bilibilitasks`.
- API implementation packages should include domain, version, and role/protocol: `userv1api`, `userv1pb`, `userv1http`.
- Generated protobuf imports should use the protobuf package identity and version, for example `userv1`, `tasksv1`, `bilibiliv1`.
- Internal API implementation packages can then use the role suffix, for example `bilibiliv1api`.

Prefer this style:

```go
import (
	bilibiliv1 "example.com/project/apis/sdk/go/data/bilibili/v1"
	tasksv1 "example.com/project/apis/sdk/go/tasks/v1"

	"github.com/example/project/internal/grpc/services/bilibiliv1api"
)
```

Do not use `gen*` or `gentasksv1` aliases for generated protobuf packages when the package has a stable versioned identity.

Name files after the primary object or service they implement:

- `VideoDataService` -> `video_data.go`
- `CommentDataService` -> `comment_data.go`
- Bilibili task request helpers -> `video_request.go`

Keep type definitions and constructors close to the methods for that service. If `VideoDataService` methods live in `video_data.go`, put `type VideoDataService` and `func NewVideoDataService()` in that file.

## Constructors

Use this shape for fx-managed objects:

```go
type Service struct {
	repo   *Repository
	logger *slog.Logger
}

type NewServiceParams struct {
	fx.In

	Repo   *Repository
	Logger *slog.Logger
}

func NewService(mode string) func(params NewServiceParams) (*Service, error) {
	return func(params NewServiceParams) (*Service, error) {
		return &Service{
			repo:   params.Repo,
			logger: params.Logger,
		}, nil
	}
}
```

If the constructor does not need an extra argument, still prefer:

```go
func NewService() func(params NewServiceParams) (*Service, error) {
	return func(params NewServiceParams) (*Service, error) {
		return &Service{}, nil
	}
}
```

over:

```go
func NewService(params NewServiceParams) (*Service, error)
```

The callback-returning form lets the command entrypoint choose variants without pushing every mode through `*Config`. Use it for dry-run mode, check-only mode, no-serving mode, test-mode variants, static labels, or multiple instances of the same provider.

Constructor rules:

- Name params `NewXxxParams`.
- Put `fx.In` as the first field.
- Keep injected fields exported.
- Keep dependencies in private fields on `Xxx` unless they are deliberately part of the public API.
- Return `(*Xxx, error)` when construction can fail.
- Return `*Xxx` only when construction cannot fail.
- Register cleanup in the constructor if the object owns resources.

## fx.In

Use `fx.In` when a constructor needs more than one dependency or when optional/named/grouped values are involved.

```go
type NewGatewayParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    *Config
	Register  *Register
	Logger    *slog.Logger
}
```

Optional dependencies should be explicit:

```go
type NewServiceParams struct {
	fx.In

	Tracer trace.Tracer `optional:"true"`
}
```

Use optional fields only when the code has a meaningful no-dependency behavior. Do not use optional injection to paper over missing module registration.

## fx.Out

Use `fx.Out` when one constructor intentionally provides multiple related outputs.

```go
type NewClientsResult struct {
	fx.Out

	HTTPClient *http.Client
	APIClient  *APIClient
}

func NewClients() func(params NewClientsParams) (NewClientsResult, error) {
	return func(params NewClientsParams) (NewClientsResult, error) {
		httpClient := &http.Client{Timeout: params.Timeout}

		return NewClientsResult{
			HTTPClient: httpClient,
			APIClient:  NewAPIClient(httpClient),
		}, nil
	}
}
```

Prefer one output per constructor unless the outputs share construction state or lifecycle. Do not use `fx.Out` just to make a package-level container object.

## Provide Rules

Register constructors in the package `Modules()` function:

```go
func Modules() fx.Option {
	return fx.Options(
		fx.Provide(NewRepository()),
		fx.Provide(NewService("default")),
	)
}
```

Provide each concrete type once in the final graph unless using `name`, `group`, or `fx.Annotate` to disambiguate.

Bad:

```go
func Modules() fx.Option {
	return fx.Options(
		featurea.Modules(), // provides *Store
		featureb.Modules(), // also provides *Store
	)
}
```

Better:

```go
func Modules() fx.Option {
	return fx.Options(
		store.Modules(),
		featurea.Modules(),
		featureb.Modules(),
	)
}
```

If two consumers need a shared object, provide it once at the nearest common aggregation level.

## Named Providers

Use named providers when the same type has several intentional instances.

```go
func Modules() fx.Option {
	return fx.Options(
		fx.Provide(
			fx.Annotate(
				NewPrimaryClient(),
				fx.ResultTags(`name:"primary"`),
			),
			fx.Annotate(
				NewReplicaClient(),
				fx.ResultTags(`name:"replica"`),
			),
		),
	)
}

type NewStoreParams struct {
	fx.In

	Primary *Client `name:"primary"`
	Replica *Client `name:"replica"`
}
```

Use names for fixed, semantically distinct instances. If the number of instances is open-ended, use groups instead.

## Grouped Providers

Use value groups for registries, hooks, handlers, checks, middlewares, or plugins where multiple packages contribute values to one consumer.

```go
type Handler interface {
	Register(*Router)
}

func Modules() fx.Option {
	return fx.Options(
		fx.Provide(
			fx.Annotate(
				NewUserHandler(),
				fx.As(new(Handler)),
				fx.ResultTags(`group:"handlers"`),
			),
		),
	)
}

type NewRouterParams struct {
	fx.In

	Handlers []Handler `group:"handlers"`
}
```

Group consumers should tolerate empty groups only when that is a real valid mode. Otherwise, fail early during construction.

## Interfaces

Prefer injecting concrete types inside the same module boundary. Use interfaces at ownership boundaries: external clients, storage adapters, worker runners, plugin registries, or packages that should not depend on a concrete implementation.

When providing an interface, use `fx.As`:

```go
fx.Provide(
	fx.Annotate(
		NewPostgresQueue(),
		fx.As(new(Queue)),
	),
)
```

Avoid defining an interface only because fx is involved. The interface should belong to the consumer side when it expresses what the consumer needs.

## Run Entrypoints

Use `fx.Invoke` for runtime entrypoints:

- HTTP and gRPC servers
- gateways
- workers
- schedulers
- pprof
- health loops
- stream consumers
- queue pollers
- any component that starts goroutines or blocks on external work

Pattern:

```go
func RunWorker(mode RunMode) func(worker *Worker) error {
	return func(worker *Worker) error {
		go worker.Run(mode)
		return nil
	}
}

func Modules() fx.Option {
	return fx.Options(
		fx.Provide(NewWorker()),
		fx.Invoke(RunWorker(RunModeServe)),
	)
}
```

`fx.Invoke` is the graph entrypoint. Providers not reachable from an invoked function may not be constructed. If code appears not to run, check whether anything invokes a dependency path to it.

Do not put runtime loops in constructors. A constructor should not start serving traffic, consuming queues, or launching unbounded goroutines.

## Lifecycle Hooks

Register cleanup with `fx.Lifecycle` for anything that owns resources.

```go
type NewServerParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    *Config
}

func NewServer() func(params NewServerParams) (*Server, error) {
	return func(params NewServerParams) (*Server, error) {
		server := &Server{}

		params.Lifecycle.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				return server.Shutdown(ctx)
			},
		})

		return server, nil
	}
}
```

Use `OnStop` for graceful shutdown, cancellation, flushing, lease release, closing network connections, and worker pool cleanup.

Use `OnStart` when startup is part of resource readiness and should be ordered by fx. Use `RunXxx` when startup is an application runtime entrypoint.

If `RunXxx` starts goroutines, make sure shutdown is reachable through lifecycle hooks or context cancellation.

## Command Wiring

Command packages should select top-level modules and runtime invokes.

```go
func main() {
	app := fx.New(
		configs.Modules(),
		logger.Modules(),
		datastore.Modules(),
		services.Modules(),
		servers.Modules(),
	)

	app.Run()
}
```

If command-specific behavior is needed, pass it into constructor or run callback factories:

```go
fx.Provide(NewService(ServiceModeReadOnly))
fx.Invoke(RunServer(ServerModeCheckOnly))
```

Avoid making every mode a field on a global config when the mode is selected by the command itself.

## Testing

Default to ordinary unit tests. Construct dependencies and call the constructor callback directly.

```go
func TestService(t *testing.T) {
	repo := NewRepositoryForTest(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	service, err := NewService("test")(NewServiceParams{
		Repo:   repo,
		Logger: logger,
	})
	require.NoError(t, err)

	got, err := service.Do(context.Background())
	require.NoError(t, err)
	require.NotNil(t, got)
}
```

Use an fx app test when the graph itself is what you need to verify:

- missing providers
- named providers
- grouped values
- lifecycle start/stop behavior
- command module composition
- accidental duplicate providers

For graph tests, keep the app small:

```go
app := fx.New(
	fx.NopLogger,
	Modules(),
	fx.Invoke(func(service *Service) {
		require.NotNil(t, service)
	}),
)

require.NoError(t, app.Start(context.Background()))
require.NoError(t, app.Stop(context.Background()))
```

## Refactoring Workflow

When refactoring an existing fx package:

1. Find the package's current constructor, `Modules()`, and runtime entrypoints.
2. Identify whether the package is a leaf package or an aggregator.
3. Move dependency registration to the closest `Modules()` function.
4. Convert constructors to `NewXxx() func(params NewXxxParams) ...`.
5. Move goroutine/server startup out of constructors into `RunXxx`.
6. Add `fx.Lifecycle` hooks for resource cleanup.
7. Update tests to call constructor callbacks directly.
8. Run `go test ./...` and the repository lint command.

## Common Smells

Avoid these patterns:

- A model or service package directly imports an unrelated sibling only to avoid wiring dependencies.
- A constructor starts a server, worker, queue poller, or infinite goroutine.
- A package exposes public fields only because fx injected them.
- `Modules()` in a leaf package imports distant parent packages.
- A command package manually wires every leaf package.
- Two child modules provide the same concrete type accidentally.
- An optional dependency hides a missing provider.
- Business logic is hidden in `fx.Invoke`.
- Tests boot a full fx app for simple method behavior.
- An interface exists only to satisfy fx, not to express a boundary.

## Review Checklist

Before finishing fx work, check:

1. Does every aggregator package have one obvious `Modules()` function?
2. Is the closest parent module responsible for new child packages?
3. Are constructors callback factories?
4. Do constructor params start with `fx.In`?
5. Are injected params exported and object fields private by default?
6. Are duplicate concrete providers intentional and named/grouped?
7. Are runtime loops started through `RunXxx` and `fx.Invoke`?
8. Are owned resources stopped through lifecycle hooks?
9. Can unit tests construct the object without a full fx app?
10. Do graph tests exist only where graph behavior is under test?
