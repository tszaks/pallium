package db

// Evidence separates mechanical citation validation from model assessment.
// Audit is an assessment of a bounded excerpt, never proof of truth.
type Evidence struct {
	Limitations   []string `json:"limitations,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty"`
	Supersedes    string   `json:"supersedes,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	Model         string   `json:"model,omitempty"`
	Snapshot      string   `json:"snapshot"`
	ParserVersion string   `json:"parser_version"`
	State         string   `json:"state"`
	AuditedAt     string   `json:"audited_at,omitempty"`
	Verdicts      string   `json:"verdicts,omitempty"`
}
