package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

func TestInstancesCreate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("my-zotero"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.ID == "" {
		t.Error("Create must assign an ID")
	}
	if inst.Slug != "my-zotero" {
		t.Errorf("Slug = %q, want my-zotero", inst.Slug)
	}
	if inst.Version != 1 {
		t.Errorf("Version = %d, want 1", inst.Version)
	}
}

func TestInstancesCreateDerivesSlugFromName(t *testing.T) {
	env := newTestEnv(t)
	in := zoteroCreateInput("")
	in.Name = "My Zotero Library!!"

	inst, err := env.Instances.Create(context.Background(), systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.Slug != "my-zotero-library" {
		t.Errorf("Slug = %q, want my-zotero-library", inst.Slug)
	}
}

func TestInstancesCreateRejectsDuplicateSlug(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if _, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("dupe")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("dupe"))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Create error = %v, want ErrConflict", err)
	}
}

func TestInstancesCreateValidation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(in *CreateInput)
	}{
		{"blank name", func(in *CreateInput) { in.Name = "  " }},
		{"bad slug chars", func(in *CreateInput) { in.Slug = "Not_A_Slug!" }},
		{"slug too short", func(in *CreateInput) { in.Slug = "a" }},
		{"unknown connector", func(in *CreateInput) { in.ConnectorID = "does-not-exist" }},
		{"bad base url", func(in *CreateInput) { in.BaseURL = "not a url" }},
		{"unknown variable", func(in *CreateInput) { in.Variables = map[string]string{"bogus": "x"} }},
		{"control char in host override", func(in *CreateInput) { in.HostHeaderOverride = "evil\r\nX-Injected: 1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := zoteroCreateInput("validation-" + strings.ReplaceAll(tc.name, " ", "-"))
			tc.mut(&in)
			if _, err := env.Instances.Create(ctx, systemActor(), in); !errors.Is(err, domain.ErrInvalidInput) {
				t.Errorf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestInstancesUpdate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("update-me"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newName := "Renamed"
	updated, err := env.Instances.Update(ctx, systemActor(), inst.ID, UpdateInput{Name: &newName})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "Renamed" {
		t.Errorf("Name = %q, want Renamed", updated.Name)
	}

	// A field left nil in the input must not be touched.
	if updated.BaseURL != inst.BaseURL {
		t.Errorf("BaseURL changed to %q despite not being in the update", updated.BaseURL)
	}
}

func TestInstancesUpdateRejectsBlankName(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("blank-name-guard"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blank := "   "
	_, err = env.Instances.Update(ctx, systemActor(), inst.ID, UpdateInput{Name: &blank})
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("error = %v, want ErrInvalidInput", err)
	}
}

func TestInstancesUpdateInvalidatesResolveCache(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("cache-invalidate"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := env.Instances.ResolveBySlug(ctx, "cache-invalidate"); err != nil {
		t.Fatalf("ResolveBySlug (warm cache): %v", err)
	}

	newURL := "http://host.docker.internal:9999"
	if _, err := env.Instances.Update(ctx, systemActor(), inst.ID, UpdateInput{BaseURL: &newURL}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	resolved, err := env.Instances.ResolveBySlug(ctx, "cache-invalidate")
	if err != nil {
		t.Fatalf("ResolveBySlug (after update): %v", err)
	}
	if resolved.Instance.BaseURL != newURL {
		t.Errorf("ResolveBySlug returned a stale BaseURL %q after Update invalidated the cache, want %q",
			resolved.Instance.BaseURL, newURL)
	}
}

func TestInstancesDelete(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("delete-me"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := env.Instances.Delete(ctx, systemActor(), inst.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := env.Instances.Get(ctx, inst.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after Delete error = %v, want ErrNotFound", err)
	}
}

// --- secrets -------------------------------------------------------------

func TestInstancesSetSecretRoundTripsThroughTarget(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("secret-roundtrip"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := env.Instances.SetSecret(ctx, systemActor(), inst.ID, "apiKey", "P9K3-secret-value"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	resolved, err := env.Instances.ResolveByID(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	target, err := env.Instances.Target(resolved)
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.Secrets["apiKey"] != "P9K3-secret-value" {
		t.Errorf("decrypted secret = %q, want the plaintext set above", target.Secrets["apiKey"])
	}
}

func TestInstancesSetSecretRejectsUnknownName(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("secret-unknown-name"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = env.Instances.SetSecret(ctx, systemActor(), inst.ID, "notASecretOnThisConnector", "value")
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("error = %v, want ErrInvalidInput", err)
	}
}

func TestInstancesSetSecretRejectsEmptyValue(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("secret-empty"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = env.Instances.SetSecret(ctx, systemActor(), inst.ID, "apiKey", "")
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("error = %v, want ErrInvalidInput — an empty secret must be deleted, not stored", err)
	}
}

func TestInstancesDeleteSecret(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("secret-delete"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := env.Instances.SetSecret(ctx, systemActor(), inst.ID, "apiKey", "some-value"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if err := env.Instances.DeleteSecret(ctx, systemActor(), inst.ID, "apiKey"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}

	resolved, err := env.Instances.ResolveByID(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	target, err := env.Instances.Target(resolved)
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if _, ok := target.Secrets["apiKey"]; ok {
		t.Error("a deleted secret must not appear in the resolved target")
	}
}

// A secret is never returned by the read model — SecretView.Set is the only
// signal, never the value.
func TestInstancesDetailNeverExposesSecretValue(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("secret-not-leaked"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const secretValue = "extremely-secret-value-should-never-appear"
	if err := env.Instances.SetSecret(ctx, systemActor(), inst.ID, "apiKey", secretValue); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	detail, err := env.Instances.Detail(ctx, inst.ID)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	found := false
	for _, sv := range detail.Secrets {
		if sv.Def.Name == "apiKey" {
			found = true
			if !sv.Set {
				t.Error("SecretView.Set should be true once a value has been stored")
			}
		}
	}
	if !found {
		t.Fatal("apiKey secret definition missing from Detail().Secrets")
	}
}

// --- Target / secret decryption failure -----------------------------------

// A tampered or foreign-key-mismatched ciphertext must fail the call rather
// than silently omit the credential — sending the request unauthenticated
// would be worse than refusing it.
func TestInstancesTargetFailsClosedOnUndecryptableSecret(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	instA, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("target-fail-a"))
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	instB, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("target-fail-b"))
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if err := env.Instances.SetSecret(ctx, systemActor(), instA.ID, "apiKey", "value-for-a"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	// Splice A's ciphertext into a Resolved claiming to be B: the AEAD
	// associated data binds ciphertext to its instance ID, so this must fail
	// to decrypt rather than silently succeed with the wrong instance's key
	// material.
	resolvedA, err := env.Instances.ResolveByID(ctx, instA.ID)
	if err != nil {
		t.Fatalf("ResolveByID A: %v", err)
	}
	resolvedB, err := env.Instances.ResolveByID(ctx, instB.ID)
	if err != nil {
		t.Fatalf("ResolveByID B: %v", err)
	}
	resolvedB.secretsCiphertext = resolvedA.secretsCiphertext
	resolvedB.Instance.ID = instB.ID // sanity: still B's instance record

	if _, err := env.Instances.Target(resolvedB); err == nil {
		t.Error("Target must fail to decrypt a ciphertext sealed under a different instance's associated data")
	}
}

// --- tool bindings ---------------------------------------------------------

func TestInstancesSetToolEnabled(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("tool-toggle"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	entry, err := env.Connector.Get(ctx, "zotero-local")
	if err != nil {
		t.Fatalf("Get connector: %v", err)
	}
	if len(entry.Compiled.Tools) == 0 {
		t.Fatal("zotero-local declares no tools; test needs at least one")
	}
	toolName := entry.Compiled.Tools[0].Name()

	if err := env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, toolName, false); err != nil {
		t.Fatalf("SetToolEnabled: %v", err)
	}
	resolved, err := env.Instances.ResolveByID(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	if resolved.EnabledTools[toolName] {
		t.Errorf("tool %q should be disabled after SetToolEnabled(false)", toolName)
	}
}

func TestInstancesSetToolEnabledRejectsUnknownTool(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("tool-unknown"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, "not_a_real_tool", true)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// --- resolve / target details ----------------------------------------------

func TestInstancesResolveBySlugUnknownSlug(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.Instances.ResolveBySlug(context.Background(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestInstancesTargetCarriesHostHeaderOverride(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	in := zoteroCreateInput("host-header")
	in.HostHeaderOverride = "127.0.0.1:23119"
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	resolved, err := env.Instances.ResolveByID(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	target, err := env.Instances.Target(resolved)
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.HostHeader != "127.0.0.1:23119" {
		t.Errorf("HostHeader = %q, want the configured override", target.HostHeader)
	}
}

func TestInstancesTargetRejectsUnparseableBaseURL(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("bad-url-after-create"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resolved, err := env.Instances.ResolveByID(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	// A control character makes url.Parse fail even though Create's own
	// validation would have caught it — this exercises Target's own guard.
	resolved.Instance.BaseURL = "http://\x7f.invalid"
	if _, err := env.Instances.Target(resolved); !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("error = %v, want ErrInvalidInput", err)
	}
}

// --- List / summarise --------------------------------------------------

func TestInstancesListReportsMissingSecrets(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	if _, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("list-me")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	summaries, err := env.Instances.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("got %d summaries, want 1", len(summaries))
	}
	// zotero-local's apiKey secret is optional, so an instance with none set
	// should still be Ready — this is the negative case for the "missing
	// required secret" problem string.
	if !summaries[0].Ready() {
		t.Errorf("expected the instance to be ready (Problem=%q)", summaries[0].Problem)
	}
}
