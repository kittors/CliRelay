package management

import (
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// routingSimulationResponse is what GET /routing-simulation returns. Times are
// RFC 3339 and omitted when unknown; durations are milliseconds.
type routingSimulationResponse struct {
	APIKey   routingSimulationKey `json:"api-key"`
	Endpoint string               `json:"endpoint"`
	// RequestedModel is the model the simulated request asked for.
	RequestedModel string                      `json:"requested-model"`
	RoutePath      string                      `json:"route-path,omitempty"`
	Stages         []routingSimulationStageDTO `json:"stages"`
	Rejection      *routingSimulationErrorDTO  `json:"rejection,omitempty"`
	Route          *routingSimulationRouteDTO  `json:"route,omitempty"`
	ModelPolicy    *routingSimulationPolicyDTO `json:"model-policy,omitempty"`
	// Model is the model credentials are selected for, after the model
	// policy and the route's CC Switch mapping.
	Model     string                         `json:"model,omitempty"`
	Selection *routingSimulationSelectionDTO `json:"selection,omitempty"`
}

// routingSimulationKey names the simulated key. Never its plaintext.
type routingSimulationKey struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type routingSimulationStageDTO struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

type routingSimulationErrorDTO struct {
	// Stage is the stage that rejected the request; set on rejections only.
	Stage        string `json:"stage,omitempty"`
	HTTPStatus   int    `json:"http-status,omitempty"`
	Code         string `json:"code,omitempty"`
	Type         string `json:"type,omitempty"`
	Message      string `json:"message,omitempty"`
	RetryAfterMS int64  `json:"retry-after-ms,omitempty"`
}

type routingSimulationRouteDTO struct {
	RoutePath string `json:"route-path,omitempty"`
	Group     string `json:"group,omitempty"`
	Fallback  string `json:"fallback,omitempty"`
	// CcSwitchClient is the client type of the CC Switch preset the route
	// belongs to, when it belongs to one.
	CcSwitchClient string `json:"ccswitch-client,omitempty"`
}

type routingSimulationPolicyDTO struct {
	RequestedModel string   `json:"requested-model,omitempty"`
	FinalModel     string   `json:"final-model,omitempty"`
	RouteID        string   `json:"route-id,omitempty"`
	FallbackRules  []string `json:"fallback-rules,omitempty"`
}

type routingSimulationSelectionDTO struct {
	Model       string                      `json:"model"`
	ModelKey    string                      `json:"model-key"`
	Providers   []string                    `json:"providers"`
	Groups      []routingSimulationGroupDTO `json:"groups"`
	Attempts    []routingSimulationAttempt  `json:"attempts"`
	Error       *routingSimulationErrorDTO  `json:"error,omitempty"`
	RetryWaitMS int64                       `json:"retry-wait-ms,omitempty"`
	// RetryWaitKind is "request-retry" or "busy-wait" when the request waits
	// before selection runs again, absent when it fails at once.
	RetryWaitKind string `json:"retry-wait-kind,omitempty"`
}

type routingSimulationGroupDTO struct {
	// Name is empty when the request is not narrowed to one route group.
	Name                string                           `json:"name"`
	Fallback            bool                             `json:"fallback,omitempty"`
	Distribution        string                           `json:"distribution"`
	Sticky              bool                             `json:"sticky,omitempty"`
	StickyMaxRequests   int                              `json:"sticky-max-requests,omitempty"`
	StickyReleaseAtLoad float64                          `json:"sticky-release-at-load,omitempty"`
	Credentials         []routingSimulationCredentialDTO `json:"credentials"`
	Error               *routingSimulationErrorDTO       `json:"error,omitempty"`
}

type routingSimulationCredentialDTO struct {
	AuthID            string   `json:"auth-id"`
	Label             string   `json:"label,omitempty"`
	Provider          string   `json:"provider"`
	Shared            bool     `json:"shared,omitempty"`
	Excluded          string   `json:"excluded,omitempty"`
	ModelExcludedBy   []string `json:"model-excluded-by,omitempty"`
	ModelNotAllowedBy []string `json:"model-not-allowed-by,omitempty"`
	// The fields below describe candidates (Excluded empty) only.
	Weight           int                        `json:"weight"`
	Blocked          string                     `json:"blocked,omitempty"`
	BlockedUntil     string                     `json:"blocked-until,omitempty"`
	BlockCause       string                     `json:"block-cause,omitempty"`
	BlockHTTPStatus  int                        `json:"block-http-status,omitempty"`
	InFlight         int                        `json:"in-flight"`
	ConcurrencyLimit int                        `json:"concurrency-limit,omitempty"`
	Saturated        bool                       `json:"saturated,omitempty"`
	LoadRatio        float64                    `json:"load-ratio"`
	Score            float64                    `json:"score,omitempty"`
	Reset            *routingSimulationResetDTO `json:"reset,omitempty"`
	// Attempt is the 1-based place in the failover order; 0 when the request
	// would not reach the credential.
	Attempt int `json:"attempt,omitempty"`
}

type routingSimulationResetDTO struct {
	Tier    string                       `json:"tier"`
	Used    float64                      `json:"used"`
	Windows []routingSimulationWindowDTO `json:"windows,omitempty"`
}

type routingSimulationWindowDTO struct {
	Key      string  `json:"key"`
	SpanMS   int64   `json:"span-ms,omitempty"`
	Used     float64 `json:"used"`
	ResetsAt string  `json:"resets-at,omitempty"`
}

type routingSimulationAttempt struct {
	AuthID        string `json:"auth-id"`
	Group         string `json:"group"`
	UpstreamModel string `json:"upstream-model,omitempty"`
	Queued        bool   `json:"queued,omitempty"`
}

func newRoutingSimulationResponse(key routingSimulationKey, endpoint, requestedModel, routePath string, result *RoutingSimulationResult) routingSimulationResponse {
	out := routingSimulationResponse{
		APIKey:         key,
		Endpoint:       endpoint,
		RequestedModel: requestedModel,
		RoutePath:      routePath,
		Stages:         []routingSimulationStageDTO{},
	}
	if result == nil {
		return out
	}
	rejectedBy := ""
	for _, stage := range result.Stages {
		out.Stages = append(out.Stages, routingSimulationStageDTO{Name: stage.Name, Outcome: stage.Outcome})
		if stage.Outcome == RoutingStageRejected {
			rejectedBy = stage.Name
		}
	}
	if rejection := result.Rejection; rejection != nil {
		out.Rejection = &routingSimulationErrorDTO{
			Stage:      rejectedBy,
			HTTPStatus: rejection.Status,
			Code:       rejection.ErrorCode,
			Type:       rejection.ErrorType,
			Message:    rejection.ErrorMessage,
		}
	}
	if route := result.Route; route != nil {
		out.Route = &routingSimulationRouteDTO{RoutePath: route.RoutePath, Group: route.Group, Fallback: route.Fallback}
		if route.CcSwitch != nil {
			out.Route.CcSwitchClient = route.CcSwitch.ClientType
		}
	}
	if policy := result.ModelPolicy; policy != nil {
		out.ModelPolicy = &routingSimulationPolicyDTO{
			RequestedModel: policy.RequestedModel,
			FinalModel:     policy.FinalModel,
			RouteID:        policy.RouteID,
			FallbackRules:  policy.FallbackRules,
		}
	}
	out.Model = result.Model
	if result.Selection != nil {
		out.Selection = newRoutingSimulationSelection(result.Selection)
	}
	return out
}

func newRoutingSimulationSelection(sim *coreauth.SelectionSimulation) *routingSimulationSelectionDTO {
	out := &routingSimulationSelectionDTO{
		Model:         sim.Model,
		ModelKey:      sim.ModelKey,
		Providers:     append([]string{}, sim.Providers...),
		Groups:        make([]routingSimulationGroupDTO, 0, len(sim.Groups)),
		Attempts:      make([]routingSimulationAttempt, 0, len(sim.Attempts)),
		Error:         simulatedErrorDTO(sim.Error),
		RetryWaitMS:   sim.RetryWait.Milliseconds(),
		RetryWaitKind: sim.RetryWaitKind,
	}
	for _, group := range sim.Groups {
		dto := routingSimulationGroupDTO{
			Name:                group.Name,
			Fallback:            group.Fallback,
			Distribution:        group.Distribution,
			Sticky:              group.Sticky,
			StickyMaxRequests:   group.StickyMaxRequests,
			StickyReleaseAtLoad: group.StickyReleaseAtLoad,
			Credentials:         make([]routingSimulationCredentialDTO, 0, len(group.Credentials)),
			Error:               simulatedErrorDTO(group.Error),
		}
		for _, credential := range group.Credentials {
			dto.Credentials = append(dto.Credentials, newRoutingSimulationCredential(credential))
		}
		out.Groups = append(out.Groups, dto)
	}
	for _, attempt := range sim.Attempts {
		out.Attempts = append(out.Attempts, routingSimulationAttempt{
			AuthID:        attempt.AuthID,
			Group:         attempt.Group,
			UpstreamModel: attempt.UpstreamModel,
			Queued:        attempt.Queued,
		})
	}
	return out
}

func newRoutingSimulationCredential(credential coreauth.CredentialSimulation) routingSimulationCredentialDTO {
	out := routingSimulationCredentialDTO{
		AuthID:            credential.AuthID,
		Label:             credential.Label,
		Provider:          credential.Provider,
		Shared:            credential.Shared,
		Excluded:          string(credential.Excluded),
		ModelExcludedBy:   credential.ModelExcludedBy,
		ModelNotAllowedBy: credential.ModelNotAllowedBy,
		Weight:            credential.Weight,
		Blocked:           credential.Blocked,
		BlockedUntil:      formatSimulationTime(credential.BlockedUntil),
		BlockCause:        credential.BlockCause,
		BlockHTTPStatus:   credential.BlockHTTPStatus,
		InFlight:          credential.InFlight,
		ConcurrencyLimit:  credential.ConcurrencyLimit,
		Saturated:         credential.Saturated,
		LoadRatio:         credential.LoadRatio,
		Score:             credential.Score,
		Attempt:           credential.Attempt,
	}
	if reset := credential.Reset; reset != nil {
		out.Reset = &routingSimulationResetDTO{Tier: reset.Tier, Used: reset.Used}
		for _, window := range reset.Windows {
			out.Reset.Windows = append(out.Reset.Windows, routingSimulationWindowDTO{
				Key:      window.Key,
				SpanMS:   window.Span.Milliseconds(),
				Used:     window.Used,
				ResetsAt: formatSimulationTime(window.ResetAt),
			})
		}
	}
	return out
}

func simulatedErrorDTO(err *coreauth.SimulatedError) *routingSimulationErrorDTO {
	if err == nil {
		return nil
	}
	return &routingSimulationErrorDTO{
		HTTPStatus:   err.HTTPStatus,
		Code:         err.Code,
		Message:      err.Message,
		RetryAfterMS: err.RetryAfter.Milliseconds(),
	}
}

func formatSimulationTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
