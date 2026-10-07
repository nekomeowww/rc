package maintenance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadPlanRejectsTrailingAndUnknownFields(t *testing.T) {
	for name, content := range map[string]string{"trailing": "{} {}", "unknown-field": `{"unexpected":true}`, "invalid": "["} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plan.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0600))
			_, err := readPlan(path)
			require.Error(t, err)
		})
	}
}
