package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	// Import Kubernetes client auth plugins so kubeconfig exec and auth
	// providers are available to rcctl.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/nekomeowww/rc/internal/cli/rcctl"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	command, err := rcctl.New()
	if err == nil {
		err = command.ExecuteContext(ctx)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "rcctl: error: %v\n", err)
		os.Exit(2)
	}
}
