// Package governance contains pure GitHub policy and execution-state rules.
//
// Run accepts only already-normalized inputs. It never reads files, invokes
// GitHub, or performs Git operations. Consumer vocabulary and identity rules
// arrive through the policy input for every command; this package supplies no
// repository, project, label, workflow, branch, actor, or milestone defaults.
//
// Supported commands and input contracts:
//
//   - label-plan: policy.labels contains ordered entries with name,
//     semantic_role, platform_owner (repo-managed or github-default),
//     governance_controlled, target_color, current_reference_color,
//     description, and notes. snapshot.labels contains normalized live labels
//     with name, color, description, and default.
//   - governance-check: policy.prefix_labels maps title prefixes to labels;
//     policy.retired_labels, status_field, valid_statuses, priority_field and
//     valid_priorities are explicit. snapshot contains repo, issues, and an
//     optional project object. Issues are normalized open issues and carry
//     state=OPEN, title, labels, and number; when a project is supplied they
//     also carry in_project and field_values.
//   - governance-summary: policy.report_sections maps report keys to display
//     labels. snapshot supplies context flags, pre_reports, post_reports, and
//     an optional fix_result. All consumer-specific labels and links are
//     caller-supplied.
//   - milestone-check: policy supplies current_milestone, next_milestone,
//     quarter_milestones, expected_milestones (title/due_on plus explicit
//     description_prefixes and description_tokens), epic_prefixes,
//     priority_field, later_priority, and rationale_markers. Optional
//     rationale_rules contains alternatives with markers (all required) and
//     require_milestone (also require the issue's milestone title). Matching
//     is case-insensitive and ORed with the literal rationale_markers. snapshot
//     contains repo, milestones, normalized open issues with state=OPEN,
//     optional project, and optional issue_details keyed by decimal issue
//     number. Each issue detail contains body and comments.
//   - execution-preflight: policy.intent and snapshot.repo, snapshot.git
//     (current_branch, head_commit, worktree_entries), active_issue_branches,
//     optional issue, and optional pull_request. Optional policy.base_branch
//     overrides inferred branch context. Finish requires explicit
//     merge_methods.default_base and merge_methods.stacked policy values;
//     policy.merge_method can override the selected value.
//   - execution-transition: policy.statuses maps done, active and todo to
//     consumer status values. snapshot.issue and snapshot.pull_request are
//     normalized; snapshot.branch_exists is required when a closed unmerged PR
//     is supplied. A missing pull_request produces no-linked-pr.
//   - execution-link-facts: policy supplies linked_issue_marker_prefix and
//     link_state_marker_prefix. snapshot has
//     issue_number, default_branch and pull_request, including body and
//     closing_issue_numbers.
//   - execution-recover: policy supplies expected_event,
//     expected_workflow_path, expected_phase and artifact_name. snapshot
//     supplies attestation, run, artifact_listing, artifact_files, sentinel,
//     log_sha256 and log_is_symlink. artifact_files is a complete normalized
//     inventory with paths, digests, and explicit symlink flags, so verification
//     never reads disk.
//   - merge-eligibility: policy supplies workflow_name, workflow_event,
//     required_label, branch_patterns, trusted_logins, pass_bucket and
//     merge_method. snapshot contains repository (nameWithOwner and
//     defaultBranch), event, pull_request, and required_checks. It only
//     evaluates eligibility; it never merges.
//
// Every command rejects malformed or duplicate identities instead of silently
// treating them as absent state. Callers should include provenance in the
// normalized snapshot they pass to this package and retain it in their public
// result envelope.
package governance
