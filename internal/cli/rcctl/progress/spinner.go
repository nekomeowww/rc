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

package progress

import (
	"io"
	"os"
	"time"

	"github.com/briandowns/spinner"
)

const refreshInterval = 200 * time.Millisecond

// Indicator is an interactive progress spinner. It is a no-op when the
// command's error writer is not a terminal.
type Indicator struct {
	spinner *spinner.Spinner
}

// Start begins a kollama-style spinner on an interactive error writer.
func Start(writer io.Writer, message string) *Indicator {
	writerFile, ok := writer.(*os.File)
	if !ok {
		return new(Indicator)
	}

	inner := spinner.New(
		spinner.CharSets[14],
		refreshInterval,
		spinner.WithWriterFile(writerFile),
		spinner.WithSuffix(" "+message),
	)
	_ = inner.Color("blue")
	inner.Start()

	return &Indicator{spinner: inner}
}

// Stop stops the spinner and clears its terminal line. It is safe to call
// more than once.
func (indicator *Indicator) Stop() {
	if indicator == nil || indicator.spinner == nil {
		return
	}
	indicator.spinner.Stop()
}
