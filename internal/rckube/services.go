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

package rckube

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const (
	// EndpointVariable gives a service the local endpoint, so it can run
	// `rc-kube process touch` to report work.
	EndpointVariable = "RC_KUBE_SOCKET"
	// serviceIDPrefix marks the process records of resident services. An
	// underscore never appears in a Kubernetes object name, so no WorkspaceExec
	// ID can collide with a service run.
	serviceIDPrefix = "service_"
	// serviceRunHistory bounds the finished runs kept per service, so a crash
	// loop cannot fill the home volume with records.
	serviceRunHistory = 5
)

// serviceBackoff is the capped exponential delay between service restarts. A
// run that lasted at least reset starts the next delay from initial again.
type serviceBackoff struct {
	initial, max, reset time.Duration
}

var defaultServiceBackoff = serviceBackoff{
	initial: time.Second,
	max:     time.Minute,
	reset:   time.Minute,
}

// WithServices sets the resident processes that Serve starts once it listens
// and keeps running until it returns. Restart notices go to log.
func WithServices(services []processruntime.Service, log io.Writer) SupervisorOption {
	return func(supervisor *Supervisor) {
		supervisor.services = services
		if log != nil {
			supervisor.serviceLog = log
		}
	}
}

// PrepareServices decodes the services argument and resolves each value that
// Kubernetes placed in a ServiceEnvironmentPrefix variable. It then removes
// those variables from this process, so no other process inherits them.
func PrepareServices(encoded string) ([]processruntime.Service, error) {
	var services []processruntime.Service
	if encoded != "" {
		var err error
		services, err = processruntime.DecodeServices(encoded)
		if err != nil {
			return nil, err
		}
	}
	for index := range services {
		service := &services[index]
		for name, source := range service.EnvironmentFrom {
			if !strings.HasPrefix(source, processruntime.ServiceEnvironmentPrefix) {
				return nil, fmt.Errorf("service %s variable %s must come from a %s variable", service.Name, name, processruntime.ServiceEnvironmentPrefix)
			}
			value, found := os.LookupEnv(source)
			if !found {
				// Kubernetes leaves an optional reference to a missing key unset.
				continue
			}
			if service.Environment == nil {
				service.Environment = make(map[string]string)
			}
			service.Environment[name] = value
		}
		service.EnvironmentFrom = nil
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), processruntime.ServiceEnvironmentPrefix) {
			if err := os.Unsetenv(name); err != nil {
				return nil, fmt.Errorf("remove service variable %s: %w", name, err)
			}
		}
	}

	return services, nil
}

// runServices starts every configured service and keeps it running until ctx
// ends. The returned function waits until every service has stopped.
func (supervisor *Supervisor) runServices(ctx context.Context, endpoint string) func() {
	var running sync.WaitGroup
	for _, service := range supervisor.services {
		running.Go(func() { supervisor.keepServiceRunning(ctx, service, endpoint) })
	}

	return running.Wait
}

// keepServiceRunning runs one service, like a container with restartPolicy
// Always: every exit, successful or not, starts a new run after a delay.
func (supervisor *Supervisor) keepServiceRunning(ctx context.Context, service processruntime.Service, endpoint string) {
	backoff := supervisor.serviceBackoff
	delay := backoff.initial
	for ctx.Err() == nil {
		started := time.Now()
		outcome := supervisor.runService(ctx, service, endpoint, started)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= backoff.reset {
			delay = backoff.initial
		}
		_, _ = fmt.Fprintf(supervisor.serviceLog, "rc-kube: service %s %s; restarting in %s\n", service.Name, outcome, delay)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		delay = min(delay*2, backoff.max)
	}
}

// runService starts one run and waits for it to exit or for ctx to end, in
// which case it stops the run with the supervisor stop grace. It returns how
// the run ended.
func (supervisor *Supervisor) runService(ctx context.Context, service processruntime.Service, endpoint string, now time.Time) string {
	id := serviceIDPrefix + service.Name + "_" + now.UTC().Format("20060102T150405.000000000Z")
	environment := maps.Clone(service.Environment)
	if environment == nil {
		environment = make(map[string]string)
	}
	if _, exists := environment[EndpointVariable]; !exists {
		environment[EndpointVariable] = endpoint
	}
	request := processruntime.StartRequest{
		ID:               id,
		UID:              id,
		Command:          service.Command,
		WorkingDirectory: service.WorkingDirectory,
		DefaultDirectory: processruntime.DefaultDirectoryWorkspace,
		Environment:      environment,
	}
	if _, err := supervisor.start(ctx, request, false); err != nil {
		return fmt.Sprintf("could not start: %v", err)
	}
	defer supervisor.pruneServiceRuns(service.Name)
	process, err := supervisor.process(id)
	if err != nil {
		return "was removed"
	}
	select {
	case <-process.exit:
	case <-ctx.Done():
		_, _ = supervisor.Stop(id)
		return "stopped"
	}
	state, err := supervisor.Inspect(id)
	if err != nil || state.ExitCode == nil {
		return "exited"
	}
	if state.Reason != "" && state.PID == 0 {
		return fmt.Sprintf("could not start: %s", state.Reason)
	}

	return fmt.Sprintf("exited with code %d", *state.ExitCode)
}

// pruneServiceRuns keeps the newest finished runs of one service.
func (supervisor *Supervisor) pruneServiceRuns(name string) {
	prefix := serviceIDPrefix + name + "_"
	policy := processruntime.HistoryPolicy{MaxEntries: serviceRunHistory}
	_, _ = supervisor.pruneHistory(policy, time.Now(), func(id string) bool { return strings.HasPrefix(id, prefix) })
}
