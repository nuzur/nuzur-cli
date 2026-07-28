package app

import (
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
)

// TestRequireApprovedProjectVersion pins the deploy approval gate: production
// builds from a reviewed schema, and nothing else gets through. The gate exists
// because deploy previously resolved a version by identifier/uuid with no status
// check at all, so an in-review version could (and did) end up serving traffic.
func TestRequireApprovedProjectVersion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     nemgen.ProjectVersionReviewStatus
		wantErr    bool
		wantPhrase string
	}{
		{
			name:   "approved deploys",
			status: nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_APPROVED,
		},
		{
			name:   "published deploys",
			status: nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_PUBLISHED,
		},
		{
			name:       "draft is rejected",
			status:     nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_DRAFT,
			wantErr:    true,
			wantPhrase: "still a draft",
		},
		{
			name:       "in review is rejected",
			status:     nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_IN_REVIEW,
			wantErr:    true,
			wantPhrase: "still in review",
		},
		{
			name:       "rejected is rejected",
			status:     nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_REJECTED,
			wantErr:    true,
			wantPhrase: "rejected",
		},
		{
			name:       "invalid is rejected",
			status:     nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_INVALID,
			wantErr:    true,
			wantPhrase: "review state 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireApprovedProjectVersion(&nemgen.ProjectVersion{
				Identifier:   "v_6",
				ReviewStatus: tc.status,
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("expected %v to deploy, got: %v", tc.status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %v to be refused", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantPhrase) {
				t.Errorf("error should say %q, got: %v", tc.wantPhrase, err)
			}
			// The message has to name the version and the way out — this is the
			// only feedback the user (or an agent) gets.
			if !strings.Contains(err.Error(), "v_6") {
				t.Errorf("error should name the version, got: %v", err)
			}
			if !strings.Contains(err.Error(), "review") {
				t.Errorf("error should point at review, got: %v", err)
			}
		})
	}
}

// TestDeployRequiresApprovedVersion guards the wiring: the gate is only useful if
// the deploy path actually asks for it.
func TestDeployRequiresApprovedVersion(t *testing.T) {
	if !deployResolveOptions().requireApprovedVersion {
		t.Error("deploy must resolve its project version with requireApprovedVersion set")
	}
}
