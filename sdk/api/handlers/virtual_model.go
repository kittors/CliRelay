package handlers

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
)

// A virtual model is a model name a tenant serves from one of several real
// models, its members: each is a model in a channel group (the routing
// config's virtual-models). The model gate resolves the name, per request, into
// a VirtualModelPlan that says which members the request may use. The
// execution entry points run the plan: they try the members one after the
// other, and move on to the next while nothing has reached the client and the
// failure is not the request's own fault (virtualModelMovesOn).

// VirtualModelPlannerContextKey is the gin context key the model gate stores
// the request's VirtualModelPlanner under.
const VirtualModelPlannerContextKey = "cliproxy.virtualModelPlanner"

// Response headers of a request a virtual model served. Requested, Routed and
// Fallback are the ones the key's model policy sets when it rewrites a model
// (internal/api/middleware/model_policy.go); a request both rewrote keeps the
// policy's, which were set first.
const (
	virtualModelHeader   = "X-CliRelay-Virtual-Model"
	modelRequestedHeader = "X-CliRelay-Model-Requested"
	modelRoutedHeader    = "X-CliRelay-Model-Routed"
	modelFallbackHeader  = "X-CliRelay-Model-Fallback"
)

// VirtualModelPlanner returns the plan of the virtual model that model names,
// or nil when it names none of the request's tenant.
type VirtualModelPlanner func(model string) *VirtualModelPlan

// VirtualModelPlan is a virtual model as one request may run it.
type VirtualModelPlan struct {
	Name string
	// Weighted picks the first member by weight; the others follow in the
	// listed order.
	Weighted bool
	Members  []VirtualModelMember
}

// VirtualModelMember is a member of a plan.
type VirtualModelMember struct {
	// Group is the member's channel group. Empty runs the member where the
	// request would run a model it named itself.
	Group string
	// Model is the model to run, with the request's thinking suffix.
	Model  string
	Weight int
	// Skip is why the request may not use the member (routetrace.MemberNotAllowed
	// or MemberDisabled), empty when it may; Detail says what refused it.
	Skip   string
	Detail string
}

// Route names the member as the request log and the response headers do:
// group/model, or the model alone when the member has no group.
func (m VirtualModelMember) Route() string {
	if m.Group == "" {
		return m.Model
	}
	return m.Group + "/" + m.Model
}

// virtualModelPlanFor returns the plan of the virtual model modelName names, or
// nil when the request has no planner (its tenant has no virtual models) or the
// name is not one of them.
func virtualModelPlanFor(ctx context.Context, modelName string) *VirtualModelPlan {
	c := ginContextFrom(ctx)
	if c == nil {
		return nil
	}
	raw, ok := c.Get(VirtualModelPlannerContextKey)
	if !ok {
		return nil
	}
	planner, _ := raw.(VirtualModelPlanner)
	if planner == nil {
		return nil
	}
	plan := planner(modelName)
	if plan == nil || len(plan.Members) == 0 {
		return nil
	}
	return plan
}

// virtualModelPick picks a number in [0, n) for a weighted first pick. Tests
// replace it.
var virtualModelPick = rand.IntN

// order returns the members the request may use, by position, in the order to
// try them, and the trace reason of the first: the listed order, or one picked
// by weight followed by the others in the listed order.
func (p *VirtualModelPlan) order(pick func(int) int) ([]int, string) {
	usable := make([]int, 0, len(p.Members))
	total := 0
	for i, member := range p.Members {
		if member.Skip != "" {
			continue
		}
		usable = append(usable, i)
		if member.Weight > 0 {
			total += member.Weight
		}
	}
	if !p.Weighted || total == 0 || len(usable) < 2 {
		return usable, routetrace.MemberFirst
	}
	n := pick(total)
	for k, i := range usable {
		weight := p.Members[i].Weight
		if weight <= 0 {
			continue
		}
		if n < weight {
			ordered := make([]int, 0, len(usable))
			ordered = append(ordered, i)
			ordered = append(ordered, usable[:k]...)
			ordered = append(ordered, usable[k+1:]...)
			return ordered, routetrace.MemberWeighted
		}
		n -= weight
	}
	return usable, routetrace.MemberFirst
}

// virtualModelMovesOn reports whether a member's failure lets the request move
// on to the next member. A client that went away needs no other member, and a
// request the upstream refused for what it asks (a malformed body, a
// content-policy block) would be refused by the next member too: moving on
// would only route around the refusal.
func virtualModelMovesOn(ctx context.Context, errMsg *StreamErrorMessage) bool {
	if errMsg == nil || errMsg.StatusCode == 499 {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(errMsg.Error, context.Canceled) {
		return false
	}
	return !coreauth.IsRequestFault(errMsg.Error)
}

// virtualModelRun is one request running on the members of a plan.
type virtualModelRun struct {
	ctx   context.Context
	plan  *VirtualModelPlan
	trace *routetrace.Recorder
	// order holds the members to try, by position in plan.Members; next is the
	// index in order of the next one.
	order []int
	next  int
	// first is the trace reason of the first member tried.
	first  string
	shared *memberMetadata
	tried  int

	// failure is the error of the last member tried and failedRoute its route.
	failure     *StreamErrorMessage
	failedRoute string
	// passedOver says why each member that was not tried was passed over;
	// noProvider is set when one was for want of an account.
	passedOver []string
	noProvider bool
}

// virtualModelTry is a member the request turned to.
type virtualModelTry struct {
	ctx    context.Context
	member VirtualModelMember
}

// newVirtualModelRun starts running a request on plan. It passes over the
// members the request may not use and the ones no account it may use serves,
// and records them in the route trace before any member is tried.
func (h *BaseAPIHandler) newVirtualModelRun(ctx context.Context, plan *VirtualModelPlan, stream bool) *virtualModelRun {
	// All the members' scheduling shares one busy-wait budget: a virtual model
	// of five members must not wait five times as long for cooling accounts.
	ctx = coreauth.WithRetryWaitBudget(ctx)
	run := &virtualModelRun{
		ctx:    ctx,
		plan:   plan,
		trace:  routetrace.FromContext(ctx),
		shared: &memberMetadata{},
	}
	run.trace.SetRequest(plan.Name, stream, false)
	order, first := plan.order(virtualModelPick)
	run.first = first
	for i, member := range plan.Members {
		if member.Skip != "" {
			run.passOver(i, member.Skip, member.Detail)
		}
	}
	for _, i := range order {
		member := plan.Members[i]
		if _, _, errMsg := h.getRequestDetails(run.memberContext(member, false), member.Model); errMsg != nil {
			run.passOver(i, routetrace.MemberNoProvider, errorText(errMsg))
			run.noProvider = true
			continue
		}
		run.order = append(run.order, i)
	}
	return run
}

func (r *virtualModelRun) memberInfo(index int, reason string) routetrace.MemberInfo {
	member := r.plan.Members[index]
	return routetrace.MemberInfo{
		VirtualModel: r.plan.Name,
		Member:       index + 1,
		Members:      len(r.plan.Members),
		Group:        member.Group,
		Model:        member.Model,
		Reason:       reason,
	}
}

func (r *virtualModelRun) passOver(index int, reason, detail string) {
	info := r.memberInfo(index, reason)
	info.Error = detail
	r.trace.Member(info)
	r.passedOver = append(r.passedOver, fmt.Sprintf("%s: %s", r.plan.Members[index].Route(), detail))
}

// nextTry turns the request to the next member, or reports that none is left.
func (r *virtualModelRun) nextTry() (virtualModelTry, bool) {
	if r.next >= len(r.order) {
		return virtualModelTry{}, false
	}
	index := r.order[r.next]
	r.next++
	info := r.memberInfo(index, r.first)
	if r.tried > 0 {
		info.Reason = routetrace.MemberFallback
		if r.failure != nil {
			info.Status = r.failure.StatusCode
			info.Cause = coreauth.FailureCause(r.failure.Error)
			info.Error = errorText(r.failure)
		}
	}
	r.trace.Member(info)
	r.tried++
	member := r.plan.Members[index]
	return virtualModelTry{ctx: r.memberContext(member, r.next < len(r.order)), member: member}, true
}

// failed notes that the member tried last failed with errMsg, and reports
// whether the request moves on to the next one.
func (r *virtualModelRun) failed(try virtualModelTry, errMsg *StreamErrorMessage) bool {
	r.failure, r.failedRoute = errMsg, try.member.Route()
	return r.next < len(r.order) && virtualModelMovesOn(r.ctx, errMsg)
}

// served settles the trace for the member that took the request, and returns
// the headers that tell the client which member it was.
func (r *virtualModelRun) served(try virtualModelTry) http.Header {
	r.trace.SettlePending()
	headers := http.Header{}
	headers.Set(virtualModelHeader, r.plan.Name)
	headers.Set(modelRequestedHeader, r.plan.Name)
	headers.Set(modelRoutedHeader, try.member.Route())
	if r.tried > 1 && r.failedRoute != "" {
		headers.Set(modelFallbackHeader, r.failedRoute)
	}
	return headers
}

// finish ends a request no member served, with the error to report: the last
// member's failure, or why no member could be tried.
func (r *virtualModelRun) finish() *StreamErrorMessage {
	r.trace.SettlePending()
	errMsg := r.failure
	if errMsg == nil {
		detail := strings.Join(r.passedOver, "; ")
		if detail == "" {
			detail = "it has no members"
		}
		status := http.StatusForbidden
		if r.noProvider {
			// What a model no account serves answers with (getRequestDetails).
			status = http.StatusBadGateway
		}
		errMsg = &StreamErrorMessage{
			StatusCode: status,
			Error:      fmt.Errorf("virtual model %q has no member this request can use (%s)", r.plan.Name, detail),
		}
	}
	addon := errMsg.Addon.Clone()
	if addon == nil {
		addon = http.Header{}
	}
	addon.Set(virtualModelHeader, r.plan.Name)
	out := *errMsg
	out.Addon = addon
	return &out
}

// memberContext is the context a member runs under. A member of a channel
// group runs there, as if the client had asked for its model on the group's
// path; a request already on a path stays on it (the planner passes over the
// members of other groups). Every member but the last tells the scheduler the
// request has somewhere else to go (routetrace.WithCallerDecides), so a pool
// with no account free reports so at once rather than waiting for one.
func (r *virtualModelRun) memberContext(member VirtualModelMember, more bool) context.Context {
	ctx := context.WithValue(r.ctx, memberMetadataKey{}, r.shared)
	if member.Group != "" && routeContextFromExecutionContext(ctx) == nil {
		ctx = sdkrouting.WithPathRouteContext(ctx, &sdkrouting.PathRouteContext{Group: member.Group})
	}
	if more {
		ctx = routetrace.WithCallerDecides(ctx)
	}
	return ctx
}

// writeVirtualModelHeaders writes headers to a response nothing has been
// written to yet, keeping the ones already set (by the key's model policy).
// Only the goroutine that owns the writer may call it.
func writeVirtualModelHeaders(c *gin.Context, headers http.Header) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	dst := c.Writer.Header()
	for key, values := range headers {
		if dst.Get(key) != "" {
			continue
		}
		dst[key] = append([]string(nil), values...)
	}
}

// mergeVirtualModelHeaders adds a virtual model's headers to the upstream
// headers a non-streaming execution returns, for the handler to write with
// them: while the request runs, a keep-alive goroutine may be writing to the
// response, so the headers cannot go on it directly.
func mergeVirtualModelHeaders(upstream, headers http.Header) http.Header {
	if upstream == nil {
		upstream = make(http.Header, len(headers))
	}
	for key, values := range headers {
		// An upstream that is itself a gateway may send these too; ours win.
		upstream[key] = append([]string(nil), values...)
	}
	return upstream
}

func errorText(errMsg *StreamErrorMessage) string {
	if errMsg == nil || errMsg.Error == nil {
		return ""
	}
	return errMsg.Error.Error()
}

type memberMetadataKey struct{}

// memberMetadata hands what the request guard decided about a virtual model's
// request from its first member's execution to the others'.
type memberMetadata struct {
	mu    sync.Mutex
	first map[string]any
}

// executionMetadata is requestExecutionMetadata for one execution of the
// request. The members of a virtual model are executions of one client
// request: the request guard reads its body for the first member, and the
// members after it are handed what it decided (coreauth.ShareRequestGuardOutcome),
// so a rewrite is the same whichever member serves.
func executionMetadata(ctx context.Context) map[string]any {
	meta := requestExecutionMetadata(ctx)
	if ctx == nil {
		return meta
	}
	shared, _ := ctx.Value(memberMetadataKey{}).(*memberMetadata)
	if shared == nil {
		return meta
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.first == nil {
		shared.first = meta
	} else {
		coreauth.ShareRequestGuardOutcome(meta, shared.first)
	}
	return meta
}
