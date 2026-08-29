package antigravity

// AccountInfo represents discovered account identity fields
type AccountInfo struct {
	ProjectID string `json:"project_id,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
	TierID    string `json:"tier_id,omitempty"`
}
