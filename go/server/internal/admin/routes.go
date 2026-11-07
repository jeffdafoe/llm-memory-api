package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/attempts"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// sessionTTL is how long a dashboard login session lasts.
const sessionTTL = 24 * time.Hour

// credentialWriteTimeout bounds the credential writes, which run on after
// the client hangs up, as Node's did.
const credentialWriteTimeout = 30 * time.Second

// The account-link throttles (LLM-670): per caller and per target, so one
// session cannot hammer and an attacker cannot spread guesses over many
// signups.
const (
	linkWindow         = 15 * time.Minute
	linkCallerAttempts = 5
	linkTargetAttempts = 10
)

// Routes are the accounts-and-configuration admin routes: login and the
// account routes, actors, agents, virtual-agent access, providers, access
// requests, invite codes, templates and config.
type Routes struct {
	*Common
	Config      *config.Store
	Sessions    *auth.Sessions
	SessionAuth TokenCache
	Providers   *providers.Providers
	Namespaces  *permissions.Namespaces
	Agents      *virtualagent.Service
	Mail        *mail.Service
	Notes       *notes.Service
	Moderator   Moderator
	// BaseURL is BASE_URL; empty means the request's own http://host.
	BaseURL string
	// Now is the clock for session expiry; nil is time.Now.
	Now func() time.Time

	linkCaller *attempts.Limiter
	linkTarget *attempts.Limiter
}

// TokenCache is the /v1 auth middleware's token cache (auth.SessionAuth):
// logout drops one token, a passphrase reset every agent token of the
// agent, actor deletion every token of the deleted actors.
type TokenCache interface {
	Forget(token string)
	ForgetAgent(agent string)
	ForgetActor(actorID int)
}

// Moderator reviews a proposed actor name (virtualagent.Moderator).
type Moderator interface {
	ModerateActorName(ctx context.Context, name string) (approved bool, reason string)
}

func (rt *Routes) now() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}

// Register adds the routes to the admin mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	if rt.linkCaller == nil {
		rt.linkCaller = attempts.New(attempts.Options{MaxAttempts: linkCallerAttempts, Window: linkWindow})
		rt.linkTarget = attempts.New(attempts.Options{MaxAttempts: linkTargetAttempts, Window: linkWindow})
	}

	// These four have their own try/catch in Node, not adminRoute: a failure
	// is a 500 with the route's own message and is not in error_log.
	mux.Handle(http.MethodPost, "/admin/login", http.HandlerFunc(rt.login))
	mux.Handle(http.MethodPost, "/admin/logout", http.HandlerFunc(rt.logout))
	mux.Handle(http.MethodPost, "/admin/link-account", http.HandlerFunc(rt.linkAccount))
	mux.Handle(http.MethodPost, "/admin/change-password", http.HandlerFunc(rt.changePassword))

	rt.Handle(mux, "/admin/providers/registry", "", "", "providers-registry", rt.providersRegistry)
	rt.Handle(mux, "/admin/providers/defaults", "", "", "providers-defaults", rt.providersDefaults)
	rt.Handle(mux, "/admin/providers/openrouter/models", "", "", "openrouter-models", rt.openRouterModels)
	rt.Handle(mux, "/admin/config/list", "config", "read", "config-list", rt.configList)
	rt.Handle(mux, "/admin/config/update", "config", "write", "config-update", rt.configUpdate)
	rt.Handle(mux, "/admin/profile/visibility", "", "", "profile-visibility", rt.profileVisibility)

	rt.Handle(mux, "/admin/actors/list", "agents", "read", "actors-list", rt.actorsList)
	rt.Handle(mux, "/admin/actors/permissions/read", "agents", "read", "actors-permissions-read", rt.actorsPermissionsRead)
	rt.Handle(mux, "/admin/actors/permissions/save", "agents", "write", "actors-permissions-save", rt.actorsPermissionsSave)
	rt.Handle(mux, "/admin/actors/visibility/read", "agents", "read", "actors-visibility-read", rt.actorsVisibilityRead)
	rt.Handle(mux, "/admin/actors/visibility/save", "agents", "write", "actors-visibility-save", rt.actorsVisibilitySave)
	rt.Handle(mux, "/admin/actors/password", "agents", "write", "actors-password", rt.actorsPassword)
	rt.Handle(mux, "/admin/actors/namespaces", "agents", "read", "actors-namespaces", rt.actorsNamespaces)
	rt.Handle(mux, "/admin/actors/admin-permissions/read", "agents", "read", "actors-admin-permissions-read", rt.actorsAdminPermissionsRead)
	rt.Handle(mux, "/admin/actors/admin-permissions/save", "agents", "write", "actors-admin-permissions-save", rt.actorsAdminPermissionsSave)
	rt.Handle(mux, "/admin/actors/delete", "agents", "write", "actors-delete", rt.actorsDelete)
	rt.Handle(mux, "/admin/actors/create", "agents", "write", "actors-create", rt.actorsCreate)
	rt.Handle(mux, "/admin/actors/searchable", "notes", "read", "actors-searchable", rt.actorsSearchable)
	rt.Handle(mux, "/admin/virtual-agent-access/list", "agents", "read", "va-access-list", rt.vaAccessList)
	rt.Handle(mux, "/admin/virtual-agent-access/grant", "agents", "write", "va-access-grant", rt.vaAccessGrant)
	rt.Handle(mux, "/admin/virtual-agent-access/revoke", "agents", "write", "va-access-revoke", rt.vaAccessRevoke)

	rt.Handle(mux, "/admin/agents", "agents", "read", "agents-list", rt.agentsList)
	rt.Handle(mux, "/admin/agents/instructions/read", "agents", "read", "agents-instructions-read", rt.agentsInstructionsRead)
	rt.Handle(mux, "/admin/agents/instructions/save", "agents", "write", "agents-instructions-save", rt.agentsInstructionsSave)
	rt.Handle(mux, "/admin/agents/expertise/save", "agents", "write", "agents-expertise-save", rt.agentsExpertiseSave)
	rt.Handle(mux, "/admin/agents/reset-passphrase", "agents", "write", "agents-reset-passphrase", rt.agentsResetPassphrase)
	rt.Handle(mux, "/admin/agents/read", "agents", "read", "agents-read", rt.agentsRead)
	rt.Handle(mux, "/admin/agents/update", "agents", "write", "agents-update", rt.agentsUpdate)
	rt.Handle(mux, "/admin/agents/usage", "agents", "read", "agents-usage", rt.agentsUsage)
	rt.Handle(mux, "/admin/agents/call-detail", "agents", "read", "agents-call-detail", rt.agentsCallDetail)

	rt.Handle(mux, "/admin/templates/list", "templates", "read", "templates-list", rt.templatesList)
	rt.Handle(mux, "/admin/templates/read", "templates", "read", "templates-read", rt.templatesRead)
	rt.Handle(mux, "/admin/templates/save", "templates", "write", "templates-save", rt.templatesSave)
	rt.Handle(mux, "/admin/templates/delete", "templates", "delete", "templates-delete", rt.templatesDelete)
	rt.Handle(mux, "/admin/access-requests", "access", "read", "access-requests-list", rt.accessRequestsList)
	rt.Handle(mux, "/admin/access-requests/approve", "access", "write", "access-requests-approve", rt.accessRequestsApprove)
	rt.Handle(mux, "/admin/access-requests/reject", "access", "write", "access-requests-reject", rt.accessRequestsReject)
	rt.Handle(mux, "/admin/invite-codes", "access", "read", "invite-codes-list", rt.inviteCodesList)
	rt.Handle(mux, "/admin/invite-codes/generate", "access", "write", "invite-codes-generate", rt.inviteCodesGenerate)
	rt.Handle(mux, "/admin/invite-codes/delete", "access", "write", "invite-codes-delete", rt.inviteCodesDelete)
}

// fail is the catch block of the routes outside adminRoute: the error on
// stderr and a 500 with the route's message.
func (rt *Routes) fail(w http.ResponseWriter, prefix, message string, err error) {
	rt.Log.Errorf("%s: %s", prefix, httpapi.ErrorMessage(err))
	httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", message)
}

// unhandled is an error Node threw outside the route's try block, which
// Express's error handler answered (server.js): stderr and the last-resort
// 500 with the error's message.
func (rt *Routes) unhandled(w http.ResponseWriter, err error) {
	rt.Log.Errorf("Unhandled error: %s", httpapi.ErrorMessage(err))
	httpapi.WriteUnhandledError(w, http.StatusInternalServerError, httpapi.ErrorMessage(err))
}
