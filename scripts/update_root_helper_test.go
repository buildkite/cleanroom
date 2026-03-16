package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestUpdateCleanroomRootHelperUsesTrustedMainHistory(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("update-cleanroom-root-helper.sh")
	if err != nil {
		t.Fatalf("read update-cleanroom-root-helper.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"git fetch --quiet origin main",
		"trusted_ref=\"${1:-${CLEANROOM_ROOT_HELPER_REF:-origin/main}}\"",
		"git merge-base --is-ancestor \"$trusted_commit\" \"$origin_main_commit\"",
		"git show \"$trusted_commit:scripts/cleanroom-root-helper.sh\" >\"$tmp_helper\"",
		"install -o root -g root -m 0755 \"$tmp_helper\" \"$helper_install_path\"",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected update-cleanroom-root-helper.sh to contain %q", needle)
		}
	}
}
