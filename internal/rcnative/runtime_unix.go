//go:build !windows && !darwin

package rcnative

func current() Runtime {
	return Runtime{
		Home:      "/home/agent",
		Workspace: "/workspace",
		Run:       "/run/rc",
		Endpoint:  "/run/rc/rc-kube.sock",
	}
}

func scriptCommand(script string) []string {
	return []string{"/bin/sh", "-ceu", script, "rc-lifecycle"}
}
