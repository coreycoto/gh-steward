// Package planning implements provider-free GitHub relationship and backlog
// planning. It accepts caller-supplied snapshots and policies; it performs no
// filesystem, network, or mutation operations.
//
// Run accepts these command/input combinations:
//
//   - relationship-audit: issue_graph
//   - relationship-validate: payload, issue_graph, optional hierarchy_policy
//   - relationship-delta: payload, issue_graph, optional hierarchy_policy
//   - next-item: queue_snapshot, optional candidate_issue_number
//   - rebalance-rank: issue_graph, queue_snapshot, project_snapshot, policy,
//     optional linked_prs_by_issue, and optional excluded_prefixes as
//     {"values": [prefix, ...]}
//   - rebalance-validate: payload, issue_graph, project_snapshot, optional
//     excluded_prefixes as {"values": [prefix, ...]}
//   - rebalance-plan: payload, issue_graph, queue_snapshot, project_snapshot,
//     options (queue_mode and positive rank_step; optional
//     linked_prs_by_issue), optional excluded_prefixes as {"values": [prefix,
//     ...]} and backlog_audit
//
// The hierarchy_policy object maps parent prefixes to allowed child-prefix
// arrays. It is optional; without it, validation still checks references and
// cycle additions but applies no consumer taxonomy. RelationshipTopology is
// the separate strict complete-graph validator.
// The ranking policy requires integer schema_version 1, band_targets.now and
// band_targets.next non-negative integers, positive sparse_rank_step, a weights
// object, and a body_patterns array of {name, pattern, weight} records.
// Project planning inputs must agree on repository host/owner/name and on the
// full project identity (id, number, title). Linked pull-request state is
// supplied by issue number and never fetched here.
//
// Relationship payloads and rebalance decisions use schema_version 1. This is
// an input/domain contract; the CLI owner adds the public v2 output envelope.
package planning
