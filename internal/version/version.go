// Package version reports the rc release that a binary was built from.
package version

// Version is the rc release version without the "v" prefix, for example
// "1.2.3". Release images set it at build time:
//
//	go build -ldflags "-X github.com/nekomeowww/rc/internal/version.Version=1.2.3"
//
// A build without the flag is a development build and has no version.
var Version string

// RCKubeRepository is the official image repository of the rc-kube image.
const RCKubeRepository = "ghcr.io/nekomeowww/rc/rc-kube"

// DefaultRCKubeImage returns the official rc-kube image for this build: the
// tag of its release, or "latest" for a development build.
func DefaultRCKubeImage() string {
	return RCKubeImage(Version)
}

// RCKubeImage returns the official rc-kube image for one release version. An
// empty version selects "latest".
func RCKubeImage(release string) string {
	tag := release
	if tag == "" {
		tag = "latest"
	}

	return RCKubeRepository + ":" + tag
}
