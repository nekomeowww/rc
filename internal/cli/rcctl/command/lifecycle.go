/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package command

import (
	"fmt"
	"time"

	"github.com/spf13/pflag"
)

// LifecycleOptions holds the opt-in Workspace clocks used by run and workspace create.
// Omitted and zero values both disable a clock; Environments supply no defaults.
type LifecycleOptions struct {
	IdleTimeout          time.Duration
	DeleteAfterSuspended time.Duration
}

// AddFlags exposes the existing idle clock and opt-in suspended storage deletion.
func (options *LifecycleOptions) AddFlags(flags *pflag.FlagSet) {
	flags.DurationVar(&options.IdleTimeout, "idle-timeout", 0, "Suspend compute after inactivity; zero or omitted disables")
	flags.DurationVar(&options.DeleteAfterSuspended, "delete-after-suspended", 0, "Delete Workspace and owned storage after confirmed suspension; zero or omitted disables")
}

// Validate rejects invalid clocks before connecting to the cluster.
func (options *LifecycleOptions) Validate() error {
	if options.IdleTimeout < 0 || options.DeleteAfterSuspended < 0 {
		return fmt.Errorf("lifecycle durations must not be negative")
	}
	return nil
}
