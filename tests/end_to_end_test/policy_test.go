package endtoend_test

import (
	"testing"

	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/tests/testenv"
)

func TestDefaultGlobalPolicy(t *testing.T) {
	t.Parallel()

	e := testenv.NewCLITest(t)

	e.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", e.RepoDir)

	// make sure we can read policy
	e.RunAndExpectSuccess(t, "policy", "show", "--global")

	// verify we created global policy entry

	var contents []content.InfoStruct

	mustParseJSONLines(t, e.RunAndExpectSuccess(t, "content", "ls", "--json"), &contents)

	if got, want := len(contents), 1; got != want {
		t.Fatalf("unexpected number of contents %v, want %v", got, want)
	}

	globalPolicyContentID := contents[0].GetContentID()
	e.RunAndExpectSuccess(t, "content", "show", "-jz", string(globalPolicyContentID))

	// make sure the policy is visible in the manifest list
	var manifests []manifest.EntryMetadata

	mustParseJSONLines(t, e.RunAndExpectSuccess(t, "manifest", "list", "--filter=type:policy", "--filter=policyType:global", "--json"), &manifests)

	if got, want := len(manifests), 1; got != want {
		t.Fatalf("unexpected number of manifests %v, want %v", got, want)
	}

	// make sure the policy is visible in the policy list
	var plist []policy.TargetWithPolicy

	mustParseJSONLines(t, e.RunAndExpectSuccess(t, "policy", "list", "--json"), &plist)

	if got, want := len(plist), 1; got != want {
		t.Fatalf("unexpected number of policies %v, want %v", got, want)
	}
}
