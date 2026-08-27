package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/service"
	"github.com/00101010xyz/mcpaw/internal/store"
)

// GetConnectors lists installed connectors and offers the import form.
func (s *Server) GetConnectors(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "connectors", s.page(r, "Connectors", "connectors",
		map[string]any{"Connectors": s.connectors.List(r.Context())}))
}

// PostImportConnector validates and stores a pasted manifest.
func (s *Server) PostImportConnector(w http.ResponseWriter, r *http.Request) {
	manifest := strings.TrimSpace(r.PostFormValue("manifest"))
	if manifest == "" {
		s.flashError(r, "Paste a connector manifest to import.")
		redirect(w, r, "/connectors")
		return
	}

	entry, err := s.connectors.ImportManifest(r.Context(), s.actor(r), []byte(manifest), domain.SourceManifest)
	if err != nil {
		// Validation errors list every problem at once, which is far more
		// useful to a manifest author than one error per attempt.
		s.flash(r, &Flash{
			Level:   FlashError,
			Message: "The manifest was rejected.",
			Details: validationDetails(err),
		})
		redirect(w, r, "/connectors")
		return
	}
	s.flashSuccess(r, "Imported %s %s with %d tools.",
		entry.Record.Name, entry.Record.Version, len(entry.Compiled.Tools))
	redirect(w, r, "/connectors")
}

// validationDetails unpacks a manifest validation failure into individual
// problems for display.
func validationDetails(err error) []string {
	var ve *connector.ValidationError
	if asValidation(err, &ve) {
		return ve.Problems
	}
	return []string{errorMessage(err)}
}

func asValidation(err error, target **connector.ValidationError) bool {
	for err != nil {
		if ve, ok := err.(*connector.ValidationError); ok {
			*target = ve
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// PostDeleteConnector removes an imported connector.
func (s *Server) PostDeleteConnector(w http.ResponseWriter, r *http.Request) {
	if err := s.connectors.Delete(r.Context(), s.actor(r), r.PathValue("id")); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Connector deleted.")
	}
	redirect(w, r, "/connectors")
}

// tokenRow is the render model for the token table.
type tokenRow struct {
	Token      *domain.Token
	ScopeLabel string
	Revoked    bool
}

// GetTokens renders the token management page.
func (s *Server) GetTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tokens, err := s.tokens.List(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	instances, err := s.instances.List(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	names := map[string]string{}
	for _, inst := range instances {
		names[inst.Instance.ID] = inst.Instance.Name
	}

	rows := make([]tokenRow, 0, len(tokens))
	for _, t := range tokens {
		scope := "All instances"
		if t.InstanceID != "" {
			if name, ok := names[t.InstanceID]; ok {
				scope = name
			} else {
				scope = "(deleted instance)"
			}
		}
		rows = append(rows, tokenRow{Token: t, ScopeLabel: scope, Revoked: t.RevokedAt != nil})
	}

	s.render(w, r, http.StatusOK, "tokens", s.page(r, "Tokens", "tokens", map[string]any{
		"Tokens":           rows,
		"Instances":        instances,
		"SelectedInstance": r.URL.Query().Get("instance"),
	}))
}

// PostTokens issues a bearer token and shows it exactly once.
func (s *Server) PostTokens(w http.ResponseWriter, r *http.Request) {
	ttlDays, _ := strconv.Atoi(r.PostFormValue("ttl_days"))
	var ttl time.Duration
	if ttlDays > 0 {
		ttl = time.Duration(ttlDays) * 24 * time.Hour
	}

	result, err := s.tokens.Create(r.Context(), s.actor(r), service.CreateTokenInput{
		Name:       r.PostFormValue("name"),
		InstanceID: r.PostFormValue("instance_id"),
		TTL:        ttl,
	})
	if err != nil {
		s.flashError(r, "%s", errorMessage(err))
		redirect(w, r, "/tokens")
		return
	}

	s.flash(r, &Flash{
		Level:   FlashSuccess,
		Message: fmt.Sprintf("Token %q issued.", result.Token.Name),
		Secret:  result.Plaintext,
		Details: []string{
			"Send it as: Authorization: Bearer <token>",
			"Only a keyed digest is stored, so this value cannot be recovered later.",
		},
	})
	redirect(w, r, "/tokens")
}

// PostRevokeToken permanently disables a token.
func (s *Server) PostRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.tokens.Revoke(r.Context(), s.actor(r), r.PathValue("id")); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Token revoked. It stops working immediately.")
	}
	redirect(w, r, "/tokens")
}

// auditRow is the render model for one audit entry.
type auditRow struct {
	Event       *domain.AuditEvent
	ActorLabel  string
	DetailLabel string
}

// auditActions is the filter list, kept in a stable order so the dropdown does
// not reshuffle between requests.
var auditActions = []string{
	domain.ActionLogin, domain.ActionLoginFailed, domain.ActionLogout,
	domain.ActionUserCreate, domain.ActionUserUpdate,
	domain.ActionInstanceCreate, domain.ActionInstanceUpdate, domain.ActionInstanceDelete,
	domain.ActionInstanceSecretSet, domain.ActionInstanceEgressOpen, domain.ActionInstanceTest,
	domain.ActionConnectorImport, domain.ActionConnectorDelete,
	domain.ActionTokenCreate, domain.ActionTokenRevoke,
	domain.ActionToolCall,
}

// GetAudit renders the audit log.
func (s *Server) GetAudit(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if action != "" && !validAction(action) {
		// An unknown filter value is dropped rather than passed to the store,
		// so the query cannot be steered from the URL.
		action = ""
	}

	events, err := s.audit.List(r.Context(), store.AuditFilter{Action: action, Limit: 200})
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	rows := make([]auditRow, 0, len(events))
	for _, e := range events {
		rows = append(rows, auditRow{
			Event:       e,
			ActorLabel:  actorLabel(e),
			DetailLabel: detailLabel(e.Detail),
		})
	}

	s.render(w, r, http.StatusOK, "audit", s.page(r, "Audit log", "audit", map[string]any{
		"Events": rows, "Actions": auditActions, "SelectedAction": action,
	}))
}

func validAction(action string) bool {
	for _, a := range auditActions {
		if a == action {
			return true
		}
	}
	return false
}

func actorLabel(e *domain.AuditEvent) string {
	if e.ActorID == "" {
		return e.ActorType
	}
	label := e.ActorType + " " + e.ActorID
	if e.IP != "" {
		label += " (" + e.IP + ")"
	}
	return label
}

// detailLabel renders the detail map deterministically. Map iteration order is
// random in Go, and a table whose cells reshuffle on every refresh is useless
// for spotting change.
func detailLabel(detail map[string]any) string {
	if len(detail) == 0 {
		return ""
	}
	keys := make([]string, 0, len(detail))
	for k := range detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		value := detail[k]
		if encoded, err := json.Marshal(value); err == nil {
			parts = append(parts, k+"="+strings.Trim(string(encoded), `"`))
			continue
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, " ")
}
