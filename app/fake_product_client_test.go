package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/grpc"
)

// fakeAgentUUID is the local agent every deploy-path fake pairs. Fixed rather
// than generated so golden output needs no uuid normalization for it.
const fakeAgentUUID = "f8888e33-0000-0000-0000-000000000001"

// fakeUserUUID is the uuid GetTokenUser reports.
const fakeUserUUID = "f8888e33-0000-0000-0000-000000000009"

// fakeCall is one recorded RPC: the method name plus the request fields that
// carry meaning for an effect assertion. Requests are proto messages that are
// awkward to compare and mostly irrelevant, so what is kept is the handful of
// key/value pairs a test would actually assert on; the full request is kept in
// Req for the rare test that wants more.
type fakeCall struct {
	Method string
	Params map[string]string
	Req    interface{}
}

// fakeProductClient is a scripted pb.NuzurProductClient for the deploy/destroy
// path. It replaces the real gRPC client via the exported field on
// productclient.Client:
//
//	&Implementation{productClient: &productclient.Client{ProductClient: fake}}
//
// which is the whole seam — no interface had to be introduced for it.
//
// UNIMPLEMENTED METHODS PANIC. NuzurProductClient has ~200 methods and the
// deploy path uses eight of them, so the rest are supplied by the embedded nil
// interface: calling one dereferences a nil interface value and panics with
// "invalid memory address or nil pointer dereference". Go has no way to
// synthesize a proxy that names the method instead (you cannot build a type with
// methods at runtime, and hand-writing 200 panicking stubs against a generated
// interface is a maintenance liability that would break on every proto change),
// so the panic's STACK TRACE is the diagnostic: its first frame is the call site
// in command_deploy*.go and the frame above it names the method. That is loud
// and it is precise; it is only the message that is generic. A method this fake
// implements but has no script for uses panicUnimplemented, which does name
// itself.
type fakeProductClient struct {
	// Nil on purpose — see the type comment.
	pb.NuzurProductClient

	// --- scripted returns -------------------------------------------------

	// ProvisioningToken is what IssueProvisioningToken hands back (it reaches the
	// box's bootstrap script, so a golden that leaks it would be unstable —
	// keep it fixed).
	ProvisioningToken string
	// TokenExpiresAt is the informational expiry on the same response.
	TokenExpiresAt int64

	// Agents is the fixed ListLocalAgents result. Nil (and AgentsByCall nil) means
	// the default: one ONLINE agent with fakeAgentUUID, which is what the poll
	// loops in command_deploy.go want — waitForAgentOnline returns on the FIRST
	// query and only checks its deadline afterwards, so an already-ONLINE agent
	// costs the test zero wall-clock seconds.
	Agents []*nemgen.LocalAgent
	// AgentsByCall scripts a SEQUENCE of ListLocalAgents results, one per call
	// (the last entry repeats), and overrides Agents when set. This is how a
	// first-deploy scenario is expressed: runDeploy snapshots the existing agents
	// BEFORE provisioning (listAgentUUIDs) and then waits for one that is not in
	// the snapshot (waitForNewOnlineAgent), so the new agent must be absent from
	// the first result and present-and-ONLINE in the second.
	//
	//	AgentsByCall: [][]*nemgen.LocalAgent{{}, {onlineAgent(fakeAgentUUID)}}
	AgentsByCall [][]*nemgen.LocalAgent

	// RevisionUUID is the uuid UpsertDeployment reports as the active revision —
	// the handle every later UpdateDeploymentRevisionStatus call is keyed by.
	RevisionUUID string
	// TokenUser is what GetTokenUser returns. Nil yields a default user.
	//
	// Note: the deploy path's own login check goes through
	// auth.AuthClientImplementation.GetTokenUser, which builds its OWN
	// productclient (auth/token.go) and therefore does NOT come through this
	// fake. It is scripted here for the callers that do use the injected client,
	// and so the seam is complete if that call is ever routed through it.
	TokenUser *nemgen.User

	// --- scripted errors --------------------------------------------------
	//
	// Each fails exactly one RPC, so a test can drive a single failure arm
	// without disturbing the rest of the pipeline.

	IssueProvisioningTokenErr      error
	ListLocalAgentsErr             error
	UpdateLocalAgentConnectionsErr error
	UpsertDeploymentErr            error
	UpdateDeploymentRevisionErr    error
	GetTokenUserErr                error
	RevokeLocalAgentErr            error
	MarkDeploymentDestroyedErr     error

	// --- recording --------------------------------------------------------

	calls          []fakeCall
	listAgentsSeen int
}

// Compile-time proof the fake is substitutable for the generated client. Without
// this the nil embed would let a signature drift go unnoticed until a test that
// happens to exercise the method panics.
var _ pb.NuzurProductClient = (*fakeProductClient)(nil)

// newFakeProductClient returns a fake with the defaults a successful deploy
// needs: a fixed provisioning token, one ONLINE agent, a fixed revision uuid.
func newFakeProductClient() *fakeProductClient {
	return &fakeProductClient{
		ProvisioningToken: "fake-provisioning-token",
		TokenExpiresAt:    4876543210,
		RevisionUUID:      "f8888e33-0000-0000-0000-0000000000re",
	}
}

// onlineAgent builds a LocalAgent in the one status the deploy poll loops accept
// (LOCAL_AGENT_STATUS_ONLINE — see waitForAgentOnline / waitForNewOnlineAgent).
func onlineAgent(uuid string) *nemgen.LocalAgent {
	return &nemgen.LocalAgent{
		Uuid:   uuid,
		Status: nemgen.LocalAgentStatus_LOCAL_AGENT_STATUS_ONLINE,
	}
}

// offlineAgent builds a registered-but-not-yet-ONLINE agent — the state the poll
// loops time out on and warn about rather than hard-fail.
func offlineAgent(uuid string) *nemgen.LocalAgent {
	return &nemgen.LocalAgent{
		Uuid:   uuid,
		Status: nemgen.LocalAgentStatus_LOCAL_AGENT_STATUS_OFFLINE,
	}
}

// record appends one call. kv is flattened key/value pairs.
func (f *fakeProductClient) record(method string, req interface{}, kv ...string) {
	params := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		params[kv[i]] = kv[i+1]
	}
	f.calls = append(f.calls, fakeCall{Method: method, Params: params, Req: req})
}

// Calls returns every recorded call in order.
func (f *fakeProductClient) Calls() []fakeCall { return f.calls }

// CallsTo returns the recorded calls to one method, in order.
func (f *fakeProductClient) CallsTo(method string) []fakeCall {
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// MethodSequence returns just the method names, in call order — the cheap
// assertion for "did the pipeline talk to nuzur in the right order".
func (f *fakeProductClient) MethodSequence() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

// Reset clears the recording without touching the script.
func (f *fakeProductClient) Reset() {
	f.calls = nil
	f.listAgentsSeen = 0
}

// panicUnimplemented is the loud failure for a method this fake claims but has
// no script for.
func panicUnimplemented(method string) {
	panic("fakeProductClient: " + method + " was called but is not scripted; add a script for it or assert the call never happens")
}

// --- scripted deploy-path methods -------------------------------------------

func (f *fakeProductClient) IssueProvisioningToken(ctx context.Context, in *pb.IssueProvisioningTokenRequest, opts ...grpc.CallOption) (*pb.IssueProvisioningTokenResponse, error) {
	f.record("IssueProvisioningToken", in, "project_uuid", in.GetProjectUuid())
	if f.IssueProvisioningTokenErr != nil {
		return nil, f.IssueProvisioningTokenErr
	}
	return &pb.IssueProvisioningTokenResponse{
		ProvisioningToken: f.ProvisioningToken,
		ExpiresAt:         f.TokenExpiresAt,
	}, nil
}

func (f *fakeProductClient) ListLocalAgents(ctx context.Context, in *pb.ListLocalAgentsRequest, opts ...grpc.CallOption) (*pb.ListLocalAgentsResponse, error) {
	n := f.listAgentsSeen
	f.listAgentsSeen++
	f.record("ListLocalAgents", in, "call", fmt.Sprint(n))
	if f.ListLocalAgentsErr != nil {
		return nil, f.ListLocalAgentsErr
	}
	return &pb.ListLocalAgentsResponse{LocalAgents: f.agentsForCall(n)}, nil
}

// agentsForCall resolves the nth ListLocalAgents result from the script.
func (f *fakeProductClient) agentsForCall(n int) []*nemgen.LocalAgent {
	if len(f.AgentsByCall) > 0 {
		if n >= len(f.AgentsByCall) {
			n = len(f.AgentsByCall) - 1 // the last scripted result repeats
		}
		return f.AgentsByCall[n]
	}
	if f.Agents != nil {
		return f.Agents
	}
	return []*nemgen.LocalAgent{onlineAgent(fakeAgentUUID)}
}

func (f *fakeProductClient) UpdateLocalAgentConnections(ctx context.Context, in *pb.UpdateLocalAgentConnectionsRequest, opts ...grpc.CallOption) (*pb.UpdateLocalAgentConnectionsResponse, error) {
	// The catalog is a REPLACE, and the union-of-all-projects-on-the-box rule is
	// the fix for a second project's deploy wiping the first's connection — so
	// what is recorded is the full published set, in order.
	uuids := ""
	for i, c := range in.GetConnections() {
		if i > 0 {
			uuids += ","
		}
		uuids += c.GetUuid()
	}
	f.record("UpdateLocalAgentConnections", in,
		"local_agent_uuid", in.GetLocalAgentUuid(),
		"connections", uuids,
		"connection_count", fmt.Sprint(len(in.GetConnections())),
	)
	if f.UpdateLocalAgentConnectionsErr != nil {
		return nil, f.UpdateLocalAgentConnectionsErr
	}
	return &pb.UpdateLocalAgentConnectionsResponse{}, nil
}

func (f *fakeProductClient) UpsertDeployment(ctx context.Context, in *pb.UpsertDeploymentRequest, opts ...grpc.CallOption) (*pb.UpsertDeploymentResponse, error) {
	// revision_uuid empty = insert a new revision, set = finalize that one. That
	// distinction IS the two-phase progress report, so it is recorded.
	f.record("UpsertDeployment", in,
		"host", in.GetDeployment().GetHost(),
		"identifier", in.GetDeployment().GetIdentifier(),
		"project_uuid", in.GetDeployment().GetProjectUuid(),
		"revision_uuid", in.GetRevisionUuid(),
		"status", in.GetRevision().GetStatus().String(),
		"status_message", in.GetRevision().GetStatusMessage(),
		"image_name", in.GetRevision().GetImageName(),
	)
	if f.UpsertDeploymentErr != nil {
		return nil, f.UpsertDeploymentErr
	}
	// Echo the request's revision back with the uuid the caller will key later
	// status updates by (reportDeployment reads GetActiveRevision().GetUuid()).
	rev := in.GetRevision()
	if rev == nil {
		rev = &nemgen.DeploymentRevision{}
	}
	active := &nemgen.DeploymentRevision{
		Uuid:               f.RevisionUUID,
		ProjectVersionUuid: rev.GetProjectVersionUuid(),
		Status:             rev.GetStatus(),
		StatusMessage:      rev.GetStatusMessage(),
	}
	return &pb.UpsertDeploymentResponse{
		Deployment:     in.GetDeployment(),
		ActiveRevision: active,
	}, nil
}

func (f *fakeProductClient) UpdateDeploymentRevisionStatus(ctx context.Context, in *pb.UpdateDeploymentRevisionStatusRequest, opts ...grpc.CallOption) (*pb.UpdateDeploymentRevisionStatusResponse, error) {
	f.record("UpdateDeploymentRevisionStatus", in,
		"revision_uuid", in.GetRevisionUuid(),
		"status", in.GetStatus().String(),
		"status_message", in.GetStatusMessage(),
	)
	if f.UpdateDeploymentRevisionErr != nil {
		return nil, f.UpdateDeploymentRevisionErr
	}
	return &pb.UpdateDeploymentRevisionStatusResponse{
		Revision: &nemgen.DeploymentRevision{
			Uuid:          in.GetRevisionUuid(),
			Status:        in.GetStatus(),
			StatusMessage: in.GetStatusMessage(),
		},
	}, nil
}

func (f *fakeProductClient) GetTokenUser(ctx context.Context, in *pb.GetTokenUserRequest, opts ...grpc.CallOption) (*nemgen.User, error) {
	f.record("GetTokenUser", in)
	if f.GetTokenUserErr != nil {
		return nil, f.GetTokenUserErr
	}
	if f.TokenUser != nil {
		return f.TokenUser, nil
	}
	return &nemgen.User{
		Uuid:       fakeUserUUID,
		Identifier: "deploy-tests",
		Name:       "Deploy",
		LastName:   "Tests",
		Email:      "deploy-tests@example.invalid",
	}, nil
}

func (f *fakeProductClient) RevokeLocalAgent(ctx context.Context, in *pb.RevokeLocalAgentRequest, opts ...grpc.CallOption) (*pb.RevokeLocalAgentResponse, error) {
	f.record("RevokeLocalAgent", in, "local_agent_uuid", in.GetLocalAgentUuid())
	if f.RevokeLocalAgentErr != nil {
		return nil, f.RevokeLocalAgentErr
	}
	return &pb.RevokeLocalAgentResponse{}, nil
}

func (f *fakeProductClient) MarkDeploymentDestroyed(ctx context.Context, in *pb.MarkDeploymentDestroyedRequest, opts ...grpc.CallOption) (*pb.MarkDeploymentDestroyedResponse, error) {
	f.record("MarkDeploymentDestroyed", in,
		"host", in.GetHost(),
		"identifier", in.GetIdentifier(),
	)
	if f.MarkDeploymentDestroyedErr != nil {
		return nil, f.MarkDeploymentDestroyedErr
	}
	return &pb.MarkDeploymentDestroyedResponse{}, nil
}

// --- named-panic guards -------------------------------------------------------
//
// These three RPCs are the AGENT side of pairing: they are issued by the agent
// running on the box (command_agent*.go), never by the deploying CLI. A harness
// that reaches one has wired agent code into a deploy test, which is worth a
// named panic rather than the generic nil-interface one — and much better than a
// silent zero-value success that would let a bad wiring pass.

func (f *fakeProductClient) ExchangeProvisioningToken(ctx context.Context, in *pb.ExchangeProvisioningTokenRequest, opts ...grpc.CallOption) (*pb.ExchangeProvisioningTokenResponse, error) {
	panicUnimplemented("ExchangeProvisioningToken (agent-side: the deploy path must not call it)")
	return nil, nil
}

func (f *fakeProductClient) RegisterLocalAgent(ctx context.Context, in *pb.RegisterLocalAgentRequest, opts ...grpc.CallOption) (*pb.RegisterLocalAgentResponse, error) {
	panicUnimplemented("RegisterLocalAgent (agent-side: the deploy path must not call it)")
	return nil, nil
}

func (f *fakeProductClient) PublishLocalAgentCatalog(ctx context.Context, in *pb.PublishLocalAgentCatalogRequest, opts ...grpc.CallOption) (*pb.PublishLocalAgentCatalogResponse, error) {
	panicUnimplemented("PublishLocalAgentCatalog (agent-side: the deploy path publishes via UpdateLocalAgentConnections)")
	return nil, nil
}

// --- smoke tests -------------------------------------------------------------

// fakeImplementation wires the fake into an Implementation the way every later
// test will.
func fakeImplementation(fake *fakeProductClient) *Implementation {
	return &Implementation{productClient: &productclient.Client{ProductClient: fake}}
}

// The seam itself: an Implementation carrying the fake reaches real deploy-path
// code, and that code's calls come back out as recordings. listAgentUUIDs and
// waitForAgentOnline are the two smallest methods on the deploy path that need
// nothing but the product client and an auth context, so they are what proves it.
func TestFakeProductClientInjection(t *testing.T) {
	isolateHome(t) // for productclient.ClientContext()
	fake := newFakeProductClient()
	i := fakeImplementation(fake)

	set, err := i.listAgentUUIDs()
	if err != nil {
		t.Fatalf("listAgentUUIDs: %v", err)
	}
	if !set[fakeAgentUUID] {
		t.Fatalf("listAgentUUIDs = %v, want the scripted agent %s", set, fakeAgentUUID)
	}

	// An already-ONLINE agent must satisfy the poll on its FIRST query: the loop
	// only checks its deadline after querying, so anything else costs the golden
	// suite three real seconds per wait.
	start := time.Now()
	online, err := i.waitForAgentOnline(fakeAgentUUID, 30*time.Second)
	if err != nil {
		t.Fatalf("waitForAgentOnline: %v", err)
	}
	if !online {
		t.Fatal("waitForAgentOnline = false for a scripted ONLINE agent")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitForAgentOnline slept %s; a scripted ONLINE agent must return on the first query", elapsed)
	}

	if got, want := fake.MethodSequence(), []string{"ListLocalAgents", "ListLocalAgents"}; !equalStrings(got, want) {
		t.Errorf("recorded calls = %v, want %v", got, want)
	}
}

// The sequence script is what a first deploy needs: the agent does not exist when
// runDeploy snapshots, and is ONLINE by the time it waits.
func TestFakeProductClientAgentSequence(t *testing.T) {
	isolateHome(t)
	fake := newFakeProductClient()
	fake.AgentsByCall = [][]*nemgen.LocalAgent{
		{},                            // snapshot: nothing paired yet
		{offlineAgent(fakeAgentUUID)}, // registered, still coming up
		{onlineAgent(fakeAgentUUID)},  // ONLINE — and repeats from here
	}
	i := fakeImplementation(fake)

	existing, err := i.listAgentUUIDs()
	if err != nil {
		t.Fatalf("listAgentUUIDs: %v", err)
	}
	if len(existing) != 0 {
		t.Fatalf("snapshot = %v, want empty", existing)
	}

	uuid, online, err := i.waitForNewOnlineAgent(existing, 30*time.Second)
	if err != nil {
		t.Fatalf("waitForNewOnlineAgent: %v", err)
	}
	if uuid != fakeAgentUUID || !online {
		t.Fatalf("waitForNewOnlineAgent = (%q, %v), want (%q, true)", uuid, online, fakeAgentUUID)
	}
	// The last scripted result repeats, so a later poll still finds it ONLINE.
	if agents := fake.agentsForCall(99); len(agents) != 1 || agents[0].GetUuid() != fakeAgentUUID {
		t.Errorf("the last scripted ListLocalAgents result should repeat, got %v", agents)
	}
}

// Recording captures the parameters an effect assertion is written against — for
// UpdateLocalAgentConnections that is the published catalog, the one whose
// wholesale-replace semantics made a second project's deploy erase the first's
// connection.
func TestFakeProductClientRecordsCatalogPublish(t *testing.T) {
	isolateHome(t)
	fake := newFakeProductClient()
	i := fakeImplementation(fake)

	if err := i.publishConnectionCatalog(fakeAgentUUID, "conn-uuid-1", "sfapi-db", "mysql"); err != nil {
		t.Fatalf("publishConnectionCatalog: %v", err)
	}

	calls := fake.CallsTo("UpdateLocalAgentConnections")
	if len(calls) != 1 {
		t.Fatalf("UpdateLocalAgentConnections called %d times, want 1", len(calls))
	}
	if got := calls[0].Params["local_agent_uuid"]; got != fakeAgentUUID {
		t.Errorf("local_agent_uuid = %q, want %q", got, fakeAgentUUID)
	}
	if got := calls[0].Params["connections"]; got != "conn-uuid-1" {
		t.Errorf("published connections = %q, want %q", got, "conn-uuid-1")
	}
}

// A scripted error surfaces as the caller's own wrapped error, so failure arms
// can be driven one RPC at a time.
func TestFakeProductClientScriptedError(t *testing.T) {
	isolateHome(t)
	fake := newFakeProductClient()
	fake.UpdateLocalAgentConnectionsErr = fmt.Errorf("permission denied")
	i := fakeImplementation(fake)

	err := i.publishConnectionCatalog(fakeAgentUUID, "conn-uuid-1", "sfapi-db", "mysql")
	if err == nil {
		t.Fatal("publishConnectionCatalog succeeded despite a scripted error")
	}
	if got := err.Error(); got != "publishing connection catalog: permission denied" {
		t.Errorf("error = %q, want it to wrap the scripted one", got)
	}
	// The call is recorded even though it failed: "was it attempted" and "did it
	// succeed" are different questions, and the [O]-class bugs are about the first.
	if n := len(fake.CallsTo("UpdateLocalAgentConnections")); n != 1 {
		t.Errorf("a failed call was recorded %d times, want 1", n)
	}
}

// The two-phase deployment report: an insert (no revision uuid) hands back the
// uuid the finalizing call is keyed by.
func TestFakeProductClientReportsDeployment(t *testing.T) {
	isolateHome(t)
	fake := newFakeProductClient()
	i := fakeImplementation(fake)

	revUUID, err := i.reportDeployment(context.Background(), deploymentReportInput{
		Provider:    "digitalocean",
		Identifier:  "sfapi",
		ProjectUUID: "proj-uuid-1",
		Host:        "203.0.113.10",
		DBOnly:      true, // skips the on-box ports read-back (no runner in a unit test)
		Status:      nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS,
	})
	if err != nil {
		t.Fatalf("reportDeployment: %v", err)
	}
	if revUUID != fake.RevisionUUID {
		t.Fatalf("revision uuid = %q, want %q", revUUID, fake.RevisionUUID)
	}

	i.updateDeployRevision(context.Background(), revUUID,
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_ACTIVE, "done")

	upserts := fake.CallsTo("UpsertDeployment")
	if len(upserts) != 1 {
		t.Fatalf("UpsertDeployment called %d times, want 1", len(upserts))
	}
	if got := upserts[0].Params["revision_uuid"]; got != "" {
		t.Errorf("first report carried revision_uuid %q, want empty (an insert)", got)
	}
	if got := upserts[0].Params["host"]; got != "203.0.113.10" {
		t.Errorf("host = %q", got)
	}

	updates := fake.CallsTo("UpdateDeploymentRevisionStatus")
	if len(updates) != 1 {
		t.Fatalf("UpdateDeploymentRevisionStatus called %d times, want 1", len(updates))
	}
	if got := updates[0].Params["revision_uuid"]; got != fake.RevisionUUID {
		t.Errorf("status update keyed by %q, want the revision uuid %q", got, fake.RevisionUUID)
	}
	if got := updates[0].Params["status_message"]; got != "done" {
		t.Errorf("status_message = %q, want %q", got, "done")
	}
}

// The loud-failure contract: an RPC the fake declines to script fails the test
// by name, not by returning a zero value that reads as success.
func TestFakeProductClientUnscriptedMethodPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an unscripted RPC returned instead of panicking")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "RegisterLocalAgent") {
			t.Errorf("panic %q does not name the method", msg)
		}
	}()
	_, _ = newFakeProductClient().RegisterLocalAgent(context.Background(), &pb.RegisterLocalAgentRequest{})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
