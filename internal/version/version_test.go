package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRCKubeImageUsesTheReleaseTag(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ghcr.io/nekomeowww/rc/rc-kube:0.22.0", RCKubeImage("0.22.0"), "a release uses the image of the same version")
	assert.Equal(t, "ghcr.io/nekomeowww/rc/rc-kube:latest", RCKubeImage(""), "a development build uses the latest image")
}

func TestDefaultRCKubeImageFollowsTheBuildVersion(t *testing.T) {
	t.Parallel()
	assert.Equal(t, RCKubeImage(Version), DefaultRCKubeImage())
}
