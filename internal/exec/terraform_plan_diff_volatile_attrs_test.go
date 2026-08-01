package exec

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reproduction for https://github.com/cloudposse/atmos/issues/2088.
//
// `plan-diff` reports a false-positive difference for `aws_db_instance` resources because
// `latest_restorable_time` is a server-managed computed attribute that AWS advances roughly every
// five minutes. Terraform re-reads it during refresh, so a plan captured with `-out` and a plan
// generated 6-7 minutes later differ in that attribute alone even though no configuration and no
// infrastructure changed.
//
// The attribute is read-only and computed, so it cannot be suppressed with a `lifecycle`
// `ignore_changes` block (hashicorp/terraform#29543). It therefore has to be handled by Atmos.
//
// These tests drive the same code path the command uses: `comparePlansAndGenerateDiff` unmarshals
// the output of `terraform show -json`, runs it through `sortMapKeys`, and hands the result to
// `generatePlanDiff`. When `generatePlanDiff` reports `hasDiff`, the command prints the diff and
// exits with code 2, which is what fails the PR check described in the issue.

// rdsPlanJSON renders a minimal but structurally faithful `terraform show -json` document for a
// bare RDS instance. Only `latest_restorable_time` varies between the two renderings, mirroring the
// refresh-to-refresh drift reported in the issue.
func rdsPlanJSON(latestRestorableTime string) string {
	return fmt.Sprintf(`{
  "format_version": "1.2",
  "terraform_version": "1.9.5",
  "variables": {
    "instance_class": {"value": "db.t3.medium"}
  },
  "planned_values": {
    "root_module": {
      "resources": [
        {
          "address": "module.rds_instance.aws_db_instance.default[0]",
          "mode": "managed",
          "type": "aws_db_instance",
          "name": "default",
          "index": 0,
          "provider_name": "registry.terraform.io/hashicorp/aws",
          "values": {
            "identifier": "portal-rds-qa",
            "instance_class": "db.t3.medium",
            "engine": "postgres",
            "engine_version": "15.5",
            "allocated_storage": 100,
            "backup_retention_period": 7,
            "latest_restorable_time": %q
          }
        }
      ]
    }
  },
  "resource_changes": [
    {
      "address": "module.rds_instance.aws_db_instance.default[0]",
      "mode": "managed",
      "type": "aws_db_instance",
      "name": "default",
      "index": 0,
      "provider_name": "registry.terraform.io/hashicorp/aws",
      "change": {
        "actions": ["no-op"],
        "before": {
          "identifier": "portal-rds-qa",
          "instance_class": "db.t3.medium",
          "latest_restorable_time": %q
        },
        "after": {
          "identifier": "portal-rds-qa",
          "instance_class": "db.t3.medium",
          "latest_restorable_time": %q
        }
      }
    }
  ]
}`, latestRestorableTime, latestRestorableTime, latestRestorableTime)
}

// planDiffFromJSON mirrors comparePlansAndGenerateDiff: unmarshal both `terraform show -json`
// documents, sort their keys, and generate the diff.
func planDiffFromJSON(t *testing.T, origJSON, newJSON string) (string, bool) {
	t.Helper()

	var origPlan, newPlan map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(origJSON), &origPlan))
	require.NoError(t, json.Unmarshal([]byte(newJSON), &newPlan))

	return generatePlanDiff(sortMapKeys(origPlan), sortMapKeys(newPlan))
}

// TestPlanDiff_Issue2088_LatestRestorableTimeIsNotADifference reproduces the reported bug.
//
// It currently FAILS: `generatePlanDiff` returns hasDiff=true and renders
// `~ latest_restorable_time: ... => ...`, so `atmos terraform plan-diff` exits 2 on an RDS
// component that has not changed. It should pass once volatile computed attributes are excluded
// from the comparison.
func TestPlanDiff_Issue2088_LatestRestorableTimeIsNotADifference(t *testing.T) {
	// Two refreshes ~5 minutes apart, exactly as in the issue's reproduction steps.
	origJSON := rdsPlanJSON("2026-02-17T22:02:50Z")
	newJSON := rdsPlanJSON("2026-02-17T22:07:50Z")

	// Guard the fixture: the two plans must be byte-different, otherwise the test would pass
	// trivially without exercising the bug at all.
	require.NotEqual(t, origJSON, newJSON, "fixture must differ so the comparison is actually exercised")

	diff, hasDiff := planDiffFromJSON(t, origJSON, newJSON)

	assert.False(t, hasDiff,
		"plan-diff must not report a difference when only the volatile computed attribute "+
			"latest_restorable_time changed; got diff:\n%s", diff)
	assert.NotContains(t, diff, "latest_restorable_time",
		"latest_restorable_time must not be rendered as a meaningful change")
}

// TestPlanDiff_Issue2088_RealChangesAreStillReported is the negative path for the fix: suppressing
// the volatile attribute must not suppress a genuine change on the same resource. This test passes
// today and must keep passing after the fix.
func TestPlanDiff_Issue2088_RealChangesAreStillReported(t *testing.T) {
	origJSON := rdsPlanJSON("2026-02-17T22:02:50Z")
	// Same timestamp drift, plus a real configuration change to the instance class.
	newJSON := strings.ReplaceAll(
		rdsPlanJSON("2026-02-17T22:07:50Z"),
		`"db.t3.medium"`, `"db.t3.large"`,
	)

	diff, hasDiff := planDiffFromJSON(t, origJSON, newJSON)

	assert.True(t, hasDiff, "a real change to instance_class must still be reported")
	assert.Contains(t, diff, "instance_class", "the real change must appear in the diff output")
}

// TestPlanDiff_Issue2088_SkipListDoesNotSuppressHasDiff documents the second half of the bug.
//
// `processAttributeDifferences` already carries a hard-coded skip list (`content_base64sha256`,
// `content_md5`, ...), but that list only affects the rendered text. The hasDiff decision is made
// earlier, in `compareResourceSections`, by a `reflect.DeepEqual` over the whole resource map. So
// even for the attributes Atmos claims to ignore, `plan-diff` still exits 2 — it just prints a
// resource header with no attribute lines under it.
//
// This test currently FAILS, which is why an allowlist has to be applied to the comparison itself
// and not only to the formatter.
func TestPlanDiff_Issue2088_SkipListDoesNotSuppressHasDiff(t *testing.T) {
	planJSON := func(sha string) string {
		return fmt.Sprintf(`{
  "planned_values": {
    "root_module": {
      "resources": [
        {
          "address": "aws_s3_object.config",
          "mode": "managed",
          "type": "aws_s3_object",
          "name": "config",
          "values": {
            "key": "config.json",
            "content_base64sha256": %q
          }
        }
      ]
    }
  }
}`, sha)
	}

	diff, hasDiff := planDiffFromJSON(t, planJSON("abc123="), planJSON("def456="))

	assert.False(t, hasDiff,
		"attributes in the skip list must not make plan-diff exit 2; got diff:\n%s", diff)
}
