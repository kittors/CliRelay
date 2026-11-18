package interfaces

import "time"

type GCPProject struct {
	Projects []GCPProjectProjects `json:"projects"`
}

type GCPProjectLabels struct {
	GenerativeLanguage string `json:"generative-language"`
}

type GCPProjectProjects struct {
	ProjectNumber  string           `json:"projectNumber"`
	ProjectID      string           `json:"projectId"`
	LifecycleState string           `json:"lifecycleState"`
	Name           string           `json:"name"`
	Labels         GCPProjectLabels `json:"labels"`
	CreateTime     time.Time        `json:"createTime"`
}
