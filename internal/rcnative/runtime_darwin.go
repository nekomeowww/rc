//go:build darwin

package rcnative

func current() Runtime {
	const home = "/Volumes/My Shared Files/home"
	return Runtime{
		Home:      home,
		Workspace: home + "/workspace",
		Run:       home + "/.rc/run",
		Endpoint:  home + "/.rc/run/rc-kube.sock",
	}
}

func scriptCommand(script string) []string {
	return []string{"/bin/sh", "-ceu", script, "rc-lifecycle"}
}
