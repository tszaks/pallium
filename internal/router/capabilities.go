package router

import "sort"

type Capability struct {
	ID                string   `json:"id"`
	Service           string   `json:"service"`
	Description       string   `json:"description"`
	UseWhen           []string `json:"use_when"`
	AvoidWhen         []string `json:"avoid_when"`
	RequiredAuthority string   `json:"required_authority"`
	SuccessEvidence   []string `json:"success_evidence"`
}

var capabilityRegistry = []Capability{
	{
		ID: "sessions-live", Service: "session-awareness", RequiredAuthority: AuthorityObserve,
		Description:     "Discover current local Codex and Claude agent activity from process and transcript evidence.",
		UseWhen:         []string{"current sessions", "possible overlapping work", "agent process state"},
		AvoidWhen:       []string{"searching prior conversation content", "inventorying generic shells or browser tabs"},
		SuccessEvidence: []string{"provider coverage is available", "each session has an explained state", "explicit exclusions are returned"},
	},
	{
		ID: "sessions-find", Service: "session-awareness", RequiredAuthority: AuthorityObserve,
		Description:     "Find and rank sessions by completion evidence, activity age, runtime state, and natural time language.",
		UseWhen:         []string{"recently finished sessions", "unfinished sessions", "inactive sessions", "most recently updated sessions"},
		AvoidWhen:       []string{"searching for words inside a transcript", "generic operating-system process inventory"},
		SuccessEvidence: []string{"parsed interpretation is explicit", "filters are returned", "matches are ordered deterministically"},
	},
	{
		ID: "sessions-recall", Service: "session-memory", RequiredAuthority: AuthorityObserve,
		Description:     "Recover goals, decisions, files, blockers, and next actions from prior indexed agent sessions.",
		UseWhen:         []string{"resume prior work", "find where work stopped", "recover a past decision"},
		AvoidWhen:       []string{"proving what is currently running", "making an external change"},
		SuccessEvidence: []string{"source session is cited", "continuity capsule is bounded", "retrieval limitations are disclosed"},
	},
	{
		ID: "repo-review", Service: "repo-intelligence", RequiredAuthority: AuthorityObserve,
		Description:     "Inspect current changes, risk, blast radius, neighbors, and likely verification without editing.",
		UseWhen:         []string{"review a diff", "estimate change risk", "inspect affected files"},
		AvoidWhen:       []string{"the requested outcome includes implementing fixes", "peer deliberation is essential"},
		SuccessEvidence: []string{"changed files are identified", "risk reasons are explicit", "test commands are recommended"},
	},
	{
		ID: "repo-preflight", Service: "repo-intelligence", RequiredAuthority: AuthorityObserve,
		Description:     "Collect scope, risk, files, and verification evidence before selecting a more expensive execution shape.",
		UseWhen:         []string{"task is ambiguous", "repository scope is unknown", "routing confidence is low"},
		AvoidWhen:       []string{"a specialized read-only service clearly answers the question", "the task is a one-shot question"},
		SuccessEvidence: []string{"scope is bounded", "risk is classified", "next command is justified"},
	},
	{
		ID: "verify-safe", Service: "verification", RequiredAuthority: AuthorityExecute,
		Description:     "Run repository-aware local checks and return objective pass or failure evidence.",
		UseWhen:         []string{"prove tests pass", "verify a build", "check current changes"},
		AvoidWhen:       []string{"failures must also be repaired", "commands would exceed granted execution authority"},
		SuccessEvidence: []string{"command and exit code are recorded", "output is preserved", "changed files are identified"},
	},
	{
		ID: "workflow-start", Service: "workflow", RequiredAuthority: AuthorityEdit,
		Description:     "Run a durable repo-scoped plan with workers, verification, resumability, and reviewable edits.",
		UseWhen:         []string{"multi-step implementation", "edits plus verification", "work must survive interruption"},
		AvoidWhen:       []string{"one-shot read-only question", "caller has not granted edit authority"},
		SuccessEvidence: []string{"run has a durable id", "worker failures are visible", "verification and patch state are inspectable"},
	},
	{
		ID: "loop-design", Service: "loop", RequiredAuthority: AuthorityExecute,
		Description:     "Prepare a bounded repeatable cycle with explicit success, stop, budget, and stagnation conditions.",
		UseWhen:         []string{"repeat until clean", "retry across invocations", "ongoing bounded remediation"},
		AvoidWhen:       []string{"one pass is sufficient", "there is no terminal condition"},
		SuccessEvidence: []string{"terminal states are named", "each tick is bounded", "stagnation and budget limits exist"},
	},
	{
		ID: "team-start", Service: "agent-teams", RequiredAuthority: AuthorityExternal,
		Description:     "Launch persistent independent peers that coordinate through a task board and mailbox.",
		UseWhen:         []string{"peer disagreement improves the answer", "agents must coordinate", "independent reviews are requested"},
		AvoidWhen:       []string{"simple fan-out without peer interaction", "external agent launch is not authorized"},
		SuccessEvidence: []string{"team and member ids are returned", "roles are distinct", "task and message state is inspectable"},
	},
}

func Capabilities() []Capability {
	result := make([]Capability, 0, len(capabilityRegistry))
	for _, capability := range capabilityRegistry {
		result = append(result, cloneCapability(capability))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func CapabilityByID(id string) (Capability, bool) {
	for _, capability := range capabilityRegistry {
		if capability.ID == id {
			return cloneCapability(capability), true
		}
	}
	return Capability{}, false
}

func cloneCapability(capability Capability) Capability {
	capability.UseWhen = append([]string(nil), capability.UseWhen...)
	capability.AvoidWhen = append([]string(nil), capability.AvoidWhen...)
	capability.SuccessEvidence = append([]string(nil), capability.SuccessEvidence...)
	return capability
}
